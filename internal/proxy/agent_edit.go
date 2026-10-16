package proxy

import (
	"fmt"
	"log"
	"strings"
	"time"
)

const (
	agentCorrectionHead = "[CLIENT TURN CORRECTION]\n" +
		"The client superseded or edited its previous prompt before that turn completed. " +
		"Disregard the superseded user request and any partial assistant response from that turn, " +
		"but keep any <system-reminder> context blocks (such as CLAUDE.md, available skills, and environment details) from earlier messages active. " +
		"Continue from the corrected prompt below.\n\n"
	syntheticResumePrompt = "Continue from where you left off."
	syntheticResumeReply  = "No response requested."
)

func isPlainUserMessage(m ChatMessage) bool {
	return m.Role == "user" && len(m.ToolCalls) == 0 && m.ToolCallID == ""
}

func isSyntheticResumeAssistant(m ChatMessage) bool {
	return m.Role == "assistant" && len(m.ToolCalls) == 0 && strings.TrimSpace(m.Content) == syntheticResumeReply
}

func editableReplacementTail(tail []ChatMessage) bool {
	if len(tail) == 0 || !isPlainUserMessage(tail[len(tail)-1]) {
		return false
	}
	if kind, _ := classifySummaryInstruction(tail[len(tail)-1]); kind != summaryNone {
		return false
	}
	for _, m := range tail {
		if !isPlainUserMessage(m) && !isSyntheticResumeAssistant(m) {
			return false
		}
	}
	return true
}

func stripSyntheticResumeMessages(messages []ChatMessage) []ChatMessage {
	hasSynthetic := false
	for _, m := range messages {
		if isSyntheticResumeAssistant(m) {
			hasSynthetic = true
			break
		}
	}
	if !hasSynthetic {
		return messages
	}
	out := make([]ChatMessage, 0, len(messages))
	for i, m := range messages {
		if isSyntheticResumeAssistant(m) {
			continue
		}
		if isPlainUserMessage(m) && i+1 < len(messages) && isSyntheticResumeAssistant(messages[i+1]) {
			trimmed := strings.TrimSpace(m.Content)
			if trimmed == syntheticResumePrompt {
				continue
			}
			for _, sep := range []string{"\n\n" + syntheticResumePrompt, "\n" + syntheticResumePrompt} {
				if strings.HasSuffix(trimmed, sep) {
					m.Content = strings.TrimSpace(strings.TrimSuffix(trimmed, sep))
					break
				}
			}
		}
		out = append(out, m)
	}
	if len(out) == 0 {
		return messages[len(messages)-1:]
	}
	return out
}

func setAgentEditableTurn(rec *agentThreadRecord, conv []string, ns []ChatMessage, attachmentStart int) {
	rec.EditPrefixHash = ""
	if len(ns) == 0 {
		return
	}
	last := ns[len(ns)-1]
	if last.Role == "user" && len(last.ToolCalls) == 0 {
		rec.EditPrefixLen = len(ns) - 1
		rec.EditPrefixHash = conv[rec.EditPrefixLen]
		rec.EditAttachStart = attachmentStart
		return
	}
	if last.Role == "tool" {
		rec.EditPrefixLen = len(ns)
		rec.EditPrefixHash = conv[rec.EditPrefixLen]
		rec.EditAttachStart = attachmentStart
	}
}

func editableAnchor(rec *agentThreadRecord, in agentPlanInput, conv []string, ns []ChatMessage) (int, int, bool) {
	if rec.EditPrefixHash != "" && len(ns) > rec.EditPrefixLen && conv[rec.EditPrefixLen] == rec.EditPrefixHash && len(in.Attachments) >= rec.EditAttachStart {
		return rec.EditPrefixLen, rec.EditAttachStart, true
	}
	if rec.EditPrefixHash == "" && rec.TurnStart > 0 && rec.ConvLen > 0 && len(ns) > rec.ConvLen && conv[rec.ConvLen] == rec.ConvHash && len(in.Attachments) >= rec.Attachments {
		return rec.ConvLen, rec.Attachments, true
	}
	return 0, 0, false
}

func interruptedAgentEdit(rec *agentThreadRecord, in agentPlanInput, conv []string, ns []ChatMessage) bool {
	// A tool-using conversation arriving on a tool-less helper binding is a
	// takeover (rebind), not an edit of the helper's message (#61).
	if rebindCandidate(rec, in) && rec.RootHash != conv[1] {
		return false
	}
	if conv[len(ns)] == rec.ConvHash {
		return false
	}
	prefixLen, _, ok := editableAnchor(rec, in, conv, ns)
	if !ok {
		return false
	}
	if rec.ClientInterrupted {
		return editableReplacementTail(ns[prefixLen:])
	}
	return lostReplyReplacedAt(rec, ns, prefixLen)
}

// lostReplyReplaced: the turn was accepted upstream (submitted) but its reply
// never reached the client because the proxy side failed (stream cut, read
// error, deadline) while the client stayed connected. The client then gave up
// on that request and sent a new prompt instead of retrying it: the history
// after the editable prefix is user text only (Claude Code merges the failed
// prompt and the new one into one user message, other clients may keep two).
// It is served like an interrupted edit: wait for the old remote turn to end,
// drain its reply without delivering it, then send the new user text once.
func lostReplyReplaced(rec *agentThreadRecord, ns []ChatMessage) bool {
	return lostReplyReplacedAt(rec, ns, rec.EditPrefixLen)
}

func lostReplyReplacedAt(rec *agentThreadRecord, ns []ChatMessage, prefixLen int) bool {
	if rec.State != agentStateSubmitted || !rec.PendingFollowUp && rec.Turns == 0 {
		return false
	}
	return editableReplacementTail(ns[prefixLen:])
}

// promoteLateAcceptedToCorrection handles a plain-user follow-up turn that stayed
// in agentStateSubmitting (because the submit HTTP response was lost) when the
// transcript probe proves upstream actually accepted it and the user has already
// sent a replacement/continuation prompt ("继续"). Once acceptance is proven, it
// is identical to lostReplyReplaced: mark it submitted and drain it via correction.
func (t *agentTurn) promoteLateAcceptedToCorrection(in agentPlanInput) bool {
	if t == nil || t.store == nil || t.rec == nil || !t.keyed {
		return false
	}
	_, conv := agentChains(in.Messages)
	ns := nonSystemMessages(in.Messages)
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	rec := t.rec
	if t.store.records[t.threadID] != rec || rec.State != agentStateSubmitting || rec.Prev == nil || !rec.PendingFollowUp {
		return false
	}
	prefixLen, _, ok := editableAnchor(rec, in, conv, ns)
	if !ok || prefixLen >= rec.ConvLen || !editableReplacementTail(ns[prefixLen:]) {
		return false
	}
	rec.State = agentStateSubmitted
	rec.UpdatedAt = time.Now()
	t.store.saveLocked()
	t.supersede = nil
	t.correction = &in
	return true
}

func (t *agentTurn) markClientInterrupted() {
	if t.store == nil || t.rec == nil || !t.keyed {
		return
	}
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	if t.store.records[t.threadID] != t.rec || t.rec.ClientMessageID != t.clientMessageID {
		return
	}
	t.rec.ClientInterrupted = true
	t.store.saveLocked()
}

// Called under the thread lock only after the superseded remote turn ends.
// The old reply is drained, never delivered; the corrected send gets a baseline
// beyond every old entity, including any old local action request.
func (t *agentTurn) prepareCorrection(maxSeq agentSeq, accepted bool) {
	s := t.store
	in := *t.correction
	full, conv := agentChains(in.Messages)
	ns := nonSystemMessages(in.Messages)
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := t.rec
	prefixLen, attachStart, _ := editableAnchor(rec, in, conv, ns)
	uncreatedFirst := !accepted && !rec.PendingFollowUp && rec.Turns == 0 && rec.Prev == nil
	if accepted {
		prev := *rec
		prev.Prev = nil
		t.prev = &prev
		rec.Prev = &prev
		if rec.State != agentStateCompleted {
			rec.Turns++
		}
	} else if rec.Prev != nil {
		prev := *rec.Prev
		prev.Prev = nil
		t.prev = &prev
		rec.Prev = &prev
	} else {
		t.prev = nil
	}
	rec.Model = in.Model
	rec.CorrectionCount++
	rec.Correction = accepted || rec.Correction
	rec.ClientInterrupted = false
	rec.Baseline = maxSeq
	rec.LastSeq = maxSeq
	if uncreatedFirst {
		rec.TurnStart = 0
		rec.TurnAttach = 0
		rec.TurnSysUpdate = false
		rec.PendingFollowUp = false
		t.mode = agentModeNew
		t.followUp = false
	} else {
		turnStart := prefixLen
		turnAttach := attachStart
		if !accepted && (rec.TurnStart > 0 || rec.PendingFollowUp) && rec.TurnStart < turnStart {
			turnStart = rec.TurnStart
			turnAttach = rec.TurnAttach
		}
		rec.TurnStart = turnStart
		rec.TurnAttach = turnAttach
		rec.TurnSysUpdate = (!accepted && rec.TurnSysUpdate) || agentSystemHash(in.Messages) != rec.SystemHash
		rec.PendingFollowUp = true
		t.mode = agentModeFollowUp
		t.followUp = true
	}
	setAgentEditableTurn(rec, conv, ns, rec.TurnAttach)
	rec.SystemHash = agentSystemHash(in.Messages)
	rec.RootHash = conv[1]
	rec.PrefixLen, rec.PrefixHash = len(in.Messages), full[len(in.Messages)]
	rec.ConvLen, rec.ConvHash = len(ns), conv[len(ns)]
	rec.Attachments = len(in.Attachments)
	rec.State = agentStateSubmitting
	rec.ClientMessageID = uuidFromSeed(fmt.Sprintf("%s|correct|%d|%s", rec.ThreadID, rec.CorrectionCount, rec.ConvHash))
	rec.UpdatedAt = time.Now()
	if in.Tooled {
		rec.Tooled = true
	}
	t.baseline = maxSeq
	t.clientMessageID = rec.ClientMessageID
	t.submit = true
	t.prompt, t.attachments = s.keyedPromptLocked(rec, in, ns)
	s.saveLocked()
	log.Printf("[agent-thread] previous turn of thread %s ended without its reply reaching the client (interrupted or lost); its reply was drained, not delivered, and the replacement user text is sent on the same thread (correction #%d)", shortID(rec.ThreadID), rec.CorrectionCount)
	t.correction = nil
}

// correctionSendUnconfirmed reports that a corrected send was recorded but its
// outcome was never confirmed (still submitting).
func (t *agentTurn) correctionSendUnconfirmed() bool {
	if t.store == nil || t.rec == nil {
		return false
	}
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	return t.rec.Correction && t.rec.State == agentStateSubmitting
}

func (t *agentTurn) correctionGraceElapsed(grace time.Duration) bool {
	if t.store == nil || t.rec == nil || grace <= 0 {
		return true
	}
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	if t.rec.UpdatedAt.IsZero() {
		return false
	}
	return time.Since(t.rec.UpdatedAt) >= grace
}

func (t *agentTurn) unconfirmedSubmit() bool {
	if t.store == nil || t.rec == nil {
		return false
	}
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	return !t.rec.Correction && t.rec.State == agentStateSubmitting
}

func (t *agentTurn) unconfirmedFirstTurn() bool {
	if t.store == nil || t.rec == nil {
		return false
	}
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	return !t.rec.Correction && !t.rec.PendingFollowUp && t.rec.Turns == 0 && t.rec.Prev == nil && t.rec.State == agentStateSubmitting
}
