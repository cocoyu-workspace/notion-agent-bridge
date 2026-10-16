package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Stable session binding (issue #11).
//
// With a real client session ID (Claude Code: metadata.user_id.session_id) the
// binding identity is (session, account, workspace). One session keeps one
// Notion thread. Message hashes only decide what is new (delta), whether a
// request is a retry (dedupe) and whether it still belongs to the conversation:
//
//   - system/tools are NOT part of the identity; when they change, the new text
//     is sent once in the follow-up message as updated context;
//   - effort and model are per-message settings, not identity;
//   - a client session owns exactly one Notion thread per account/workspace:
//     no branch below ever creates a second chat for a bound session. A request
//     that does not continue the bound conversation (helper/title calls, edited
//     first message, compaction) is refused with an explicit error and leaves
//     the binding untouched; it is never guessed to be a helper and never
//     answered on a throw-away chat;
//   - the only in-place exception is helper-first ordering: a tool-using
//     request that arrives after a tool-less single-turn binding is moved onto
//     that same thread (sent as a follow-up that marks the earlier content as
//     unrelated), so the main conversation does not need a second chat;
//   - a request that shares the root but is not a continuation (edit, rollback,
//     compaction of the same conversation) fails with an explicit error; a bound
//     thread is never silently forked;
//   - a failed first turn keeps its thread: the identical retry is sent into
//     the same thread (transcript position moved past the failure);
//   - a failed follow-up (4xx/5xx/timeout) keeps the binding.

// agentBusyWait bounds how long a request waits for another in-flight request
// of the same session before returning a busy error.
var agentBusyWait = 2 * time.Minute

// agentBindingError is a recoverable, explicit refusal. The binding (if any) is
// left untouched.
type agentBindingError struct {
	Kind  string // busy | diverged | pending | rejected
	Msg   string
	Cause error // typed upstream error (for example *agentHTTPError), kept for errors.As
}

func (e *agentBindingError) Error() string { return "Notion Agent session binding: " + e.Msg }

// Unwrap keeps the upstream error type reachable so authoritative quota
// confirmation (classifyAgentQuotaFailure) still sees a 402/403/429.
func (e *agentBindingError) Unwrap() error { return e.Cause }

var agentErrorCodePattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,64}$`)

// agentErrorCode extracts a machine-readable code/type/name from an upstream
// error body. Only short identifier-like values are returned; message text,
// cookies and request content are never read into logs or errors.
func agentErrorCode(resp *http.Response) string {
	code, _ := agentErrorCodeAndKeys(resp)
	return code
}

// agentErrorCodeAndKeys also returns the error body's key names (no values).
func agentErrorCodeAndKeys(resp *http.Response) (string, string) {
	if resp == nil || resp.Body == nil {
		return "", ""
	}
	reader, cleanup, err := decompressBody(resp)
	if err != nil {
		return "", ""
	}
	if cleanup != nil {
		defer cleanup()
	}
	data, err := io.ReadAll(io.LimitReader(reader, 16<<10))
	if err != nil || len(data) == 0 {
		return "", ""
	}
	return agentErrorCodeFromBody(data), agentErrorBodyKeys(data)
}

func agentErrorCodeFromBody(data []byte) string {
	var body map[string]interface{}
	if json.Unmarshal(data, &body) != nil {
		return ""
	}
	pick := func(m map[string]interface{}) string {
		// The routing error is recognized by its name even when another identifier
		// field comes first.
		for _, key := range []string{"code", "name", "type", "errorType"} {
			if v, ok := m[key].(string); ok && v == crossCellErrorName {
				return v
			}
		}
		for _, key := range []string{"code", "name", "type", "errorType"} {
			if v, ok := m[key].(string); ok && agentErrorCodePattern.MatchString(v) {
				return v
			}
		}
		return ""
	}
	if c := pick(body); c != "" {
		return c
	}
	if inner, ok := body["error"].(map[string]interface{}); ok {
		return pick(inner)
	}
	if v, ok := body["error"].(string); ok && agentErrorCodePattern.MatchString(v) {
		return v
	}
	return ""
}

func lockWithTimeout(lk *sync.Mutex, d time.Duration) bool {
	if lk.TryLock() {
		return true
	}
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		if lk.TryLock() {
			return true
		}
	}
	return false
}

func nonSystemMessages(messages []ChatMessage) []ChatMessage {
	out := make([]ChatMessage, 0, len(messages))
	for _, m := range messages {
		if m.Role != "system" {
			out = append(out, m)
		}
	}
	return out
}

func systemMessages(messages []ChatMessage) []ChatMessage {
	var out []ChatMessage
	for _, m := range messages {
		if m.Role != "system" {
			continue
		}
		// Claude Code adds a "<total_tokens>N tokens left</total_tokens>" entry,
		// a per-request "x-anthropic-billing-header: ... cch=..." line, moves
		// "<style> output style is active..." between blocks across turns, and
		// attaches per-turn "UserPromptSubmit hook additional context: ...",
		// silent-turn reminders, and disk-change notices. Strip this client
		// bookkeeping (and any <system-reminder> wrapper left empty) from the
		// context hash and forwarded updates so follow-up turns do not resend
		// the 89 KB system/tool context.
		if strings.Contains(m.Content, "<total_tokens>") ||
			strings.Contains(m.Content, "output style is active.") ||
			strings.Contains(m.Content, "UserPromptSubmit hook additional context:") ||
			strings.Contains(m.Content, "The user hasn't heard from you in a while.") ||
			strings.Contains(m.Content, "changed on disk since you last read it.") {
			m.Content = totalTokensLine.ReplaceAllString(m.Content, "")
			m.Content = outputStyleActiveLine.ReplaceAllString(m.Content, "")
			m.Content = userPromptSubmitReminderBlock.ReplaceAllString(m.Content, "")
			m.Content = userPromptSubmitPlainBlock.ReplaceAllString(m.Content, "")
			m.Content = silentTurnReminderLine.ReplaceAllString(m.Content, "")
			m.Content = editedTextFileReminderBlock.ReplaceAllString(m.Content, "")
			m.Content = editedTextFilePlainBlock.ReplaceAllString(m.Content, "")
			m.Content = emptySystemReminderBlock.ReplaceAllString(m.Content, "")
		}
		m.Content = normalizeSessionSystemContent(m.Content)
		if m.Content == "" {
			continue
		}
		out = append(out, m)
	}
	return out
}

var (
	totalTokensLine               = regexp.MustCompile(`(?m)^[ \t]*<total_tokens>\d+ tokens left</total_tokens>[ \t]*$`)
	outputStyleActiveLine         = regexp.MustCompile(`(?m)^[ \t]*\S+ output style is active\. Remember to follow the specific guidelines for this style\.[ \t]*$`)
	userPromptSubmitReminderBlock = regexp.MustCompile(`(?s)<system-reminder>\s*UserPromptSubmit hook additional context:.*?</system-reminder>`)
	userPromptSubmitPlainBlock    = regexp.MustCompile(`(?s)^\s*UserPromptSubmit hook additional context:.*\z`)
	silentTurnReminderLine        = regexp.MustCompile(`(?m)^[ \t]*The user hasn't heard from you in a while\.[^\n]*$`)
	editedTextFileReminderBlock   = regexp.MustCompile(`(?s)<system-reminder>\s*Note: [^\n]+ changed on disk since you last read it\..*?</system-reminder>`)
	editedTextFilePlainBlock      = regexp.MustCompile(`(?s)^\s*Note: [^\n]+ changed on disk since you last read it\..*\z`)
	emptySystemReminderBlock      = regexp.MustCompile(`(?s)<system-reminder>\s*</system-reminder>`)
)

// agentSystemHash commits to all system/tool-protocol messages (order kept).
func agentSystemHash(messages []ChatMessage) string {
	var digests []string
	for _, m := range systemMessages(messages) {
		digests = append(digests, agentMessageDigest(m))
	}
	chain := agentChain(digests)
	return chain[len(chain)-1]
}

// agentThreadFollowUpPromptWithContext renders new messages plus, when the
// system/tool context changed, its updated text as ordinary message content.
// No protocol field is invented: the update travels inside the user message.
func agentThreadFollowUpPromptWithContext(updated []ChatMessage, delta []ChatMessage) string {
	if len(updated) == 0 {
		return agentThreadFollowUpPrompt(delta)
	}
	var prompt strings.Builder
	prompt.WriteString(agentThreadFollowUpHead)
	prompt.WriteString("UPDATED CONTEXT: the project context and local action protocol below replace every earlier version for the rest of this thread.\n\n")
	writeAgentMessages(&prompt, updated)
	prompt.WriteString("NEW MESSAGES\n\n")
	writeAgentMessages(&prompt, stripSyntheticResumeMessages(delta))
	prompt.WriteString(agentThreadPromptTail)
	return prompt.String()
}

func (s *agentThreadStore) primaryLocked(in agentPlanInput) *agentThreadRecord {
	var best *agentThreadRecord
	for _, rec := range s.records {
		if rec.ClientKey != in.ClientKey || rec.AccountID != in.AccountID || rec.SpaceID != in.SpaceID {
			continue
		}
		if best == nil || rec.UpdatedAt.After(best.UpdatedAt) {
			best = rec
		}
	}
	return best
}

func (s *agentThreadStore) newKeyedTurnLocked(in agentPlanInput, full, conv []string, reason string) *agentTurn {
	t := s.newTurnLocked(in, full, conv)
	t.keyed = true
	log.Printf("[agent-thread] binding client session to new thread %s (%s)", shortID(t.threadID), reason)
	return t
}

// isUntrackedHelperRequest detects Claude Code and Claude Desktop built-in
// coding-session naming helper requests (tool-less requests carrying the
// session-naming system prompt, <session> template, or Desktop title/branch JSON
// generator prompt). They run on a one-shot untracked thread so they never
// occupy the session's primary binding before Turn 1 or fail with [bridge:diverged]
// after Turn 1 (#116).
func isUntrackedHelperRequest(in agentPlanInput) bool {
	if in.Tooled {
		return false
	}
	for _, m := range in.Messages {
		lower := strings.ToLower(m.Content)
		if strings.Contains(lower, "you are naming a coding session") ||
			strings.Contains(lower, "you write short session titles") {
			return true
		}
		if strings.Contains(lower, "generate a concise, sentence-case title (3-7 words) that captures the main topic or goal of this coding session") {
			return true
		}
		if strings.Contains(lower, "generate a concise, sentence-case title") &&
			strings.Contains(lower, `json object with two fields: "title" and "branch"`) {
			return true
		}
		if m.Role != "system" &&
			strings.Contains(m.Content, "<description>") &&
			strings.Contains(lower, "please generate a title") {
			return true
		}
		if m.Role != "system" &&
			strings.Contains(m.Content, "<session>") &&
			strings.Contains(lower, "write the title in the predominant language of the session") {
			return true
		}
	}
	return false
}

// beginKeyed implements the stable-session policy described above.
func (s *agentThreadStore) beginKeyed(in agentPlanInput) (*agentTurn, error) {
	if isUntrackedHelperRequest(in) {
		return newUntrackedAgentTurn(in), nil
	}
	full, conv := agentChains(in.Messages)
	ns := nonSystemMessages(in.Messages)
	n := len(ns)
	if n == 0 {
		return nil, &agentBindingError{Kind: "diverged", Msg: "request has no conversation messages; refusing to create another Notion thread for this session"}
	}
	if isAgentProgressSummary(ns[n-1]) {
		return nil, &agentBindingError{Kind: "diverged", Msg: "request is a background subagent progress summary fork; refusing to run it on the bound Notion thread or create a separate chat"}
	}
	sysHash := agentSystemHash(in.Messages)
	for attempt := 0; attempt < 6; attempt++ {
		s.mu.Lock()
		rec := s.primaryLocked(in)
		if rec == nil {
			switch kind, version := classifySummaryInstruction(ns[n-1]); kind {
			case summaryKnown:
				// Nothing is bound (state lost or first request seen): answer on a
				// throw-away thread without creating a binding.
				s.mu.Unlock()
				log.Printf("[agent-thread] summarisation request (%s) for a session without binding runs on a one-shot Notion thread; no binding created", version)
				turn := newUntrackedAgentTurn(in)
				turn.prompt = agentThreadSummaryPrompt(in.Messages)
				return turn, nil
			case summarySuspected:
				s.mu.Unlock()
				return nil, summaryInstructionNotRecognisedError()
			}
			t := s.newKeyedTurnLocked(in, full, conv, "no binding yet for this client session/account/workspace")
			s.fillMetaLocked(t.rec, conv, sysHash, in)
			s.saveLocked()
			s.mu.Unlock()
			return t, nil
		}
		if rec.RootHash != "" && rec.RootHash != conv[1] && !rebindCandidate(rec, in) && !interruptedAgentEdit(rec, in, conv, ns) && !compactionShaped(ns) && truncatedSummaryRequest(rec, ns, conv) == summaryNone {
			err := agentRootChangeError(rec, ns)
			s.mu.Unlock()
			return nil, err
		}
		lk := s.lockFor(rec.ThreadID)
		s.mu.Unlock()
		if !lockWithTimeout(lk, agentBusyWait) {
			return nil, &agentBindingError{Kind: "busy", Msg: "another request of this session is still running on the bound thread; retry shortly (binding kept)"}
		}
		s.mu.Lock()
		if cur, ok := s.records[rec.ThreadID]; !ok || cur != rec {
			s.mu.Unlock()
			lk.Unlock()
			continue
		}
		t := &agentTurn{store: s, rec: rec, keyed: true, threadID: rec.ThreadID, unlock: lk.Unlock, clientMessageID: rec.ClientMessageID}
		legacy := rec.RootHash == ""
		if interruptedAgentEdit(rec, in, conv, ns) {
			t.correction = &in
			t.baseline = rec.Baseline
			s.mu.Unlock()
			return t, nil
		}
		if rec.RootHash != "" && rec.RootHash != conv[1] {
			// Different root. The only way forward is helper-first rebinding,
			// re-checked now that this request owns the thread lock.
			if !rebindCandidate(rec, in) {
				if compactionCandidate(rec, ns) {
					return s.beginCompactionLocked(t, rec, in, full, conv, ns, sysHash)
				}
				if kind := truncatedSummaryRequest(rec, ns, conv); kind != summaryNone {
					if rec.State != agentStateCompleted {
						s.mu.Unlock()
						lk.Unlock()
						return nil, &agentBindingError{Kind: "pending", Msg: "the previous turn of this session has not completed (outcome unknown); retry the previous request first (binding kept)"}
					}
					if kind == summarySuspected {
						s.mu.Unlock()
						lk.Unlock()
						return nil, summaryInstructionNotRecognisedError()
					}
					return s.beginSidecarLocked(t, rec, in, conv, n, true)
				}
				err := agentRootChangeError(rec, ns)
				s.mu.Unlock()
				lk.Unlock()
				return nil, err
			}
			if rec.State != agentStateCompleted {
				// Issue #61: the helper's turn (Claude Code's tool-less title query)
				// never completed from the bridge's view and nobody holds its lock:
				// the client abandoned it. Refusing with "pending" wedged the main
				// conversation (10 client retries, all 409). The helper thread is
				// not part of the conversation, so the main request gets its own
				// thread and the abandoned helper binding is dropped (a late retry
				// of the helper then meets the main binding and is refused).
				delete(s.records, rec.ThreadID)
				lk.Unlock()
				t := s.newKeyedTurnLocked(in, full, conv, "the tool-less helper turn bound first never completed; not waiting for it")
				s.fillMetaLocked(t.rec, conv, sysHash, in)
				s.saveLocked()
				s.mu.Unlock()
				return t, nil
			}
			prev := *rec
			prev.Prev = nil
			t.prev = &prev
			rec.Prev = &prev
			rec.Model = in.Model
			rec.RootHash = conv[1]
			rec.Baseline = rec.LastSeq
			rec.TurnStart, rec.TurnAttach, rec.TurnSysUpdate, rec.TurnCompacted = 0, 0, false, false
			rec.clearAux()
			rec.PendingFollowUp = true
			rec.PrefixLen, rec.PrefixHash = len(in.Messages), full[len(in.Messages)]
			rec.ConvLen, rec.ConvHash = n, conv[n]
			rec.SystemHash = sysHash
			rec.Tooled = true
			rec.Attachments = len(in.Attachments)
			rec.State = agentStateSubmitting
			rec.ClientMessageID = uuidFromSeed(fmt.Sprintf("%s|rebind|%s|%d", rec.ThreadID, conv[n], rec.Failures))
			rec.UpdatedAt = time.Now()
			s.saveLocked()
			t.mode, t.followUp, t.submit = agentModeFollowUp, true, true
			t.baseline = rec.Baseline
			t.clientMessageID = rec.ClientMessageID
			t.title = agentTurnTitle(in)
			t.prompt = agentThreadRebindPrompt(in.Messages)
			t.attachments = append([]UploadedAttachment(nil), in.Attachments...)
			s.mu.Unlock()
			log.Printf("[agent-thread] tool-using conversation takes over the tool-less single-turn binding in place on thread %s (no second thread)", shortID(rec.ThreadID))
			return t, nil
		}
		if failedFirstTurnRetryOrContinuation(rec, in, conv, ns) {
			// The first turn failed. Retry the identical request — or continue
			// when the client/coordinator appended user follow-up message(s)
			// after the failed turn (e.g. SendMessage to a failed subagent) —
			// inside the same thread, past the recorded failure, with a fresh
			// message ID.
			prev := *rec
			prev.Prev = nil
			t.prev = &prev
			rec.Prev = &prev
			rec.Model = in.Model
			rec.Baseline = rec.LastSeq
			rec.TurnStart, rec.TurnAttach, rec.TurnSysUpdate, rec.TurnCompacted = 0, 0, false, false
			rec.clearAux()
			rec.PendingFollowUp = true
			rec.PrefixLen, rec.PrefixHash = len(in.Messages), full[len(in.Messages)]
			rec.ConvLen, rec.ConvHash = n, conv[n]
			rec.SystemHash = sysHash
			rec.Attachments = len(in.Attachments)
			if in.Tooled {
				rec.Tooled = true
			}
			if len(ns) > 1 && len(in.Attachments) > 0 {
				rec.EditPrefixHash = ""
			} else {
				setAgentEditableTurn(rec, conv, ns, 0)
			}
			rec.State = agentStateSubmitting
			rec.ClientMessageID = uuidFromSeed(fmt.Sprintf("%s|retry|%s|%d", rec.ThreadID, conv[n], rec.Failures))
			rec.UpdatedAt = time.Now()
			s.saveLocked()
			t.mode, t.followUp, t.submit = agentModeFollowUp, true, true
			t.baseline = rec.Baseline
			t.clientMessageID = rec.ClientMessageID
			t.title = agentTurnTitle(in)
			t.prompt = agentThreadPrompt(in.Messages)
			t.attachments = append([]UploadedAttachment(nil), in.Attachments...)
			s.mu.Unlock()
			log.Printf("[agent-thread] retrying the failed first turn inside thread %s", shortID(rec.ThreadID))
			return t, nil
		}
		switch {
		case rec.ConvLen == n && conv[n] == rec.ConvHash && rec.Attachments == len(in.Attachments) && rec.State != agentStateFailed:
			t.mode = agentModeReplay
			t.baseline = rec.Baseline
			t.followUp = rec.PendingFollowUp
			if rec.State != agentStateCompleted && rec.Prev != nil {
				prev := *rec.Prev
				t.prev = &prev
			}
			if legacy {
				rec.RootHash = conv[1] // chain verified: backfill the conversation root
				s.saveLocked()
			}
			if rec.State == agentStateSubmitting {
				t.submit, t.probe = true, true
				t.prompt, t.attachments = s.keyedPromptLocked(rec, in, ns)
				if rec.TurnStart == 0 && !rec.TurnCompacted {
					t.title = agentTurnTitle(in)
				}
			}
			s.mu.Unlock()
			log.Printf("[agent-thread] same request seen again; reusing bound thread %s (state=%s) without re-sending", shortID(rec.ThreadID), rec.State)
			return t, nil
		case unacceptedTurnSuperseded(rec, in, conv, ns):
			// The sender side failed (5xx / dropped connection) before the turn was
			// confirmed, the client gave up and sent something else that continues
			// the last stable state. Plan nothing yet: the caller probes the
			// transcript and only then drops the unaccepted turn (supersedeUnaccepted).
			t.supersede = &in
			t.baseline = rec.Baseline
			s.mu.Unlock()
			log.Printf("[agent-thread] request continues the last stable state of thread %s while the previous turn was never confirmed; probing the transcript before deciding", shortID(rec.ThreadID))
			return t, nil
		case rewoundSummaryRequest(ns, conv, rec.ConvLen, rec.ConvHash) != summaryNone:
			// Real Claude Code 2.1.280/2.1.296 reactive compaction (/compact and
			// auto-compact when >= 2 assistant messages exist): the request drops the
			// bound thread's last assistant reply and appends the summary instruction
			// as the final text block of the preceding user or tool_result message.
			if rec.State != agentStateCompleted {
				s.mu.Unlock()
				lk.Unlock()
				return nil, &agentBindingError{Kind: "pending", Msg: "the previous turn of this session has not completed (outcome unknown); retry the previous request first (binding kept)"}
			}
			if rewoundSummaryRequest(ns, conv, rec.ConvLen, rec.ConvHash) == summarySuspected {
				s.mu.Unlock()
				lk.Unlock()
				return nil, summaryInstructionNotRecognisedError()
			}
			return s.beginSidecarLocked(t, rec, in, conv, n, true)
		case rec.ConvLen > 0 && rec.ConvLen < n && conv[rec.ConvLen] == rec.ConvHash && validFollowUp(ns, rec.ConvLen):
			if rec.State != agentStateCompleted {
				s.mu.Unlock()
				lk.Unlock()
				return nil, &agentBindingError{Kind: "pending", Msg: "the previous turn of this session has not completed (outcome unknown); retry the previous request first (binding kept)"}
			}
			if len(in.Attachments) < rec.Attachments {
				s.mu.Unlock()
				lk.Unlock()
				return nil, &agentBindingError{Kind: "diverged", Msg: "attachments of the bound conversation are missing from this request; not appending to the thread (binding kept)"}
			}
			switch kind, _ := classifySummaryInstruction(ns[n-1]); kind {
			case summaryKnown:
				return s.beginSidecarLocked(t, rec, in, conv, n, false)
			case summarySuspected:
				s.mu.Unlock()
				lk.Unlock()
				return nil, summaryInstructionNotRecognisedError()
			}
			prev := *rec
			prev.Prev = nil
			t.prev = &prev
			rec.Prev = &prev
			rec.Model = in.Model
			var updated []ChatMessage
			sysChanged := sysHash != rec.SystemHash || rec.ResendContext
			rec.ResendContext = false
			if sysChanged {
				updated = systemMessages(in.Messages)
			}
			if legacy {
				rec.RootHash = conv[1]
			}

			rec.ClientInterrupted = false
			rec.Correction = false
			setAgentEditableTurn(rec, conv, ns, rec.Attachments)
			rec.Baseline = rec.LastSeq
			rec.TurnStart = rec.ConvLen + 1
			rec.TurnAttach = rec.Attachments
			rec.TurnSysUpdate = sysChanged
			rec.TurnCompacted = false
			rec.TurnUserNormLen = 0
			if isPlainUserMessage(ns[n-1]) {
				rec.TurnUserNormLen = len(normalizeSessionUserContent(ns[n-1].Content))
			}
			delta := trimCommittedFollowUpDelta(rec, conv, ns)
			rec.clearAux() // the conversation moved on: an earlier summary no longer justifies a re-base
			rec.PendingFollowUp = true
			rec.PrefixLen, rec.PrefixHash = len(in.Messages), full[len(in.Messages)]
			rec.ConvLen, rec.ConvHash = n, conv[n]
			rec.SystemHash = sysHash
			rec.Attachments = len(in.Attachments)
			rec.State = agentStateSubmitting
			rec.ClientMessageID = uuidFromSeed(fmt.Sprintf("%s|%s|%d", rec.ThreadID, conv[n], rec.Failures))
			rec.UpdatedAt = time.Now()
			if in.Tooled {
				rec.Tooled = true
			}
			s.saveLocked()
			t.mode, t.followUp, t.submit = agentModeFollowUp, true, true
			t.baseline = rec.Baseline
			t.clientMessageID = rec.ClientMessageID
			t.prompt = agentThreadFollowUpPromptWithContext(updated, delta)
			t.attachments = append([]UploadedAttachment(nil), in.Attachments[rec.TurnAttach:]...)
			s.mu.Unlock()
			log.Printf("[agent-thread] continuing bound thread %s with %d new message(s)%s", shortID(rec.ThreadID), len(delta), map[bool]string{true: " and updated system/tool context", false: ""}[sysChanged])
			return t, nil
		case truncatedSummaryRequest(rec, ns, conv) != summaryNone:
			// Compaction fallback (e.g. Claude Code compactConversation retrying
			// with fewer or microcompacted historical messages after an oversized
			// summarisation attempt): run on a one-shot sidecar thread without
			// touching the bound thread.
			if rec.State != agentStateCompleted {
				s.mu.Unlock()
				lk.Unlock()
				return nil, &agentBindingError{Kind: "pending", Msg: "the previous turn of this session has not completed (outcome unknown); retry the previous request first (binding kept)"}
			}
			if truncatedSummaryRequest(rec, ns, conv) == summarySuspected {
				s.mu.Unlock()
				lk.Unlock()
				return nil, summaryInstructionNotRecognisedError()
			}
			return s.beginSidecarLocked(t, rec, in, conv, n, true)
		case rec.ConvLen > 0 && rec.ConvLen < n && conv[rec.ConvLen] == rec.ConvHash && s.supersededByOtherAccountLocked(rec, conv, n):
			delete(s.records, rec.ThreadID)
			lk.Unlock()
			t := s.newKeyedTurnLocked(in, full, conv, "session advanced on another account and switched back; starting fresh thread with full history")
			s.fillMetaLocked(t.rec, conv, sysHash, in)
			s.saveLocked()
			s.mu.Unlock()
			return t, nil
		}
		s.mu.Unlock()
		lk.Unlock()
		return nil, &agentBindingError{Kind: "diverged", Msg: "this request edits, rolls back or compacts the conversation bound to its Notion thread; refusing to fork or append blindly (binding kept). Retry the original request or start a new client session"}
	}
	return nil, &agentBindingError{Kind: "busy", Msg: "binding is busy; retry shortly"}
}

func (s *agentThreadStore) supersededByOtherAccountLocked(rec *agentThreadRecord, conv []string, n int) bool {
	if s == nil || rec == nil || rec.ClientKey == "" {
		return false
	}
	for _, other := range s.records {
		if other == nil || other == rec || other.ClientKey != rec.ClientKey || other.AccountID == rec.AccountID {
			continue
		}
		if other.ConvLen > rec.ConvLen && other.ConvLen <= n && conv[other.ConvLen] == other.ConvHash {
			return true
		}
	}
	return false
}

func trimCommittedFollowUpDelta(rec *agentThreadRecord, conv []string, ns []ChatMessage) []ChatMessage {
	if rec.TurnStart <= 0 || rec.TurnStart > len(ns) {
		return nil
	}
	delta := ns[rec.TurnStart:]
	if rec.CommittedConvLen <= rec.TurnStart || rec.CommittedConvHash == "" {
		return delta
	}
	n := len(ns)
	if n == rec.CommittedConvLen && conv[n] == rec.CommittedConvHash {
		return delta
	}
	if n > rec.CommittedConvLen && conv[rec.CommittedConvLen] == rec.CommittedConvHash {
		return ns[rec.CommittedConvLen:]
	}
	if n == rec.CommittedConvLen && rec.CommittedUserNormLen > 0 && isPlainUserMessage(ns[n-1]) {
		fullNorm := normalizeSessionUserContent(ns[n-1].Content)
		if len(fullNorm) > rec.CommittedUserNormLen {
			prefixNorm := strings.TrimSpace(fullNorm[:rec.CommittedUserNormLen])
			tailNorm := strings.TrimSpace(fullNorm[rec.CommittedUserNormLen:])
			if tailNorm != "" && chainNext(conv[n-1], agentMessageDigest(ChatMessage{Role: "user", Content: prefixNorm})) == rec.CommittedConvHash {
				trimmed := ns[n-1]
				trimmed.Content = tailNorm
				return []ChatMessage{trimmed}
			}
		}
	}
	return delta
}

func (s *agentThreadStore) fillMetaLocked(rec *agentThreadRecord, conv []string, sysHash string, in agentPlanInput) {
	rec.RootHash = conv[1]
	rec.SystemHash = sysHash
	rec.Tooled = in.Tooled
	rec.TurnStart = 0
	ns := nonSystemMessages(in.Messages)
	if len(ns) > 1 && len(in.Attachments) > 0 {
		// Flat attachment inputs cannot prove where historical images end.
		// A later normal turn will record a verified attachment boundary.
		rec.EditPrefixHash = ""
	} else {
		setAgentEditableTurn(rec, conv, ns, 0)
	}
}

func (s *agentThreadStore) keyedPromptLocked(rec *agentThreadRecord, in agentPlanInput, ns []ChatMessage) (string, []UploadedAttachment) {
	attach := in.Attachments
	if rec.TurnAttach > 0 && rec.TurnAttach <= len(attach) {
		attach = attach[rec.TurnAttach:]
	}
	if rec.PendingFollowUp && rec.TurnStart == 0 && rec.TurnCompacted {
		// Interrupted (or corrected) compaction re-base: resend the same summary message.
		var updated []ChatMessage
		if rec.TurnSysUpdate {
			updated = systemMessages(in.Messages)
		}
		prompt := agentThreadCompactionPrompt(updated, compactionFromMessages(ns))
		if rec.Correction {
			prompt = agentCorrectionHead + prompt
		}
		return prompt, append([]UploadedAttachment(nil), in.Attachments...)
	}
	if rec.PendingFollowUp && rec.TurnStart > 0 && rec.TurnStart <= len(ns) {
		var updated []ChatMessage
		if rec.TurnSysUpdate {
			updated = systemMessages(in.Messages)
		}
		_, conv := agentChains(in.Messages)
		prompt := agentThreadFollowUpPromptWithContext(updated, trimCommittedFollowUpDelta(rec, conv, ns))
		if rec.Correction {
			prompt = agentCorrectionHead + prompt
		}
		return prompt, append([]UploadedAttachment(nil), attach...)
	}
	if rec.PendingFollowUp && rec.TurnStart == 0 && rec.Turns >= 1 {
		// Interrupted helper-first rebind: resend the same whole-conversation message.
		return agentThreadRebindPrompt(in.Messages), append([]UploadedAttachment(nil), in.Attachments...)
	}
	return agentThreadPrompt(in.Messages), append([]UploadedAttachment(nil), in.Attachments...)
}

// rollbackRejected restores the binding after the upstream definitively refused
// a follow-up (nothing was added to the thread).
func (t *agentTurn) rollbackRejected() {
	if t.store == nil || t.rec == nil || t.prev == nil {
		return
	}
	t.store.restore(t.rec, t.prev, 0, false)
}

// rollbackFailed restores the binding after a turn that reached the thread but
// ended in failure. The transcript position moves past the failure so a retry
// is not confused by it, and the retry gets a fresh client message ID.
func (t *agentTurn) rollbackFailed(maxSeq agentSeq) {
	if t.store == nil || t.rec == nil || t.prev == nil {
		t.discard()
		return
	}
	t.store.restore(t.rec, t.prev, maxSeq, true)
}

func (s *agentThreadStore) restore(rec, prev *agentThreadRecord, maxSeq agentSeq, failed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur, ok := s.records[rec.ThreadID]; !ok || cur != rec {
		return
	}
	failedConvLen := rec.ConvLen
	failedConvHash := rec.ConvHash
	failedUserNormLen := rec.TurnUserNormLen
	resendCtx := rec.ResendContext || prev.ResendContext
	notionCompactions := rec.NotionCompactions
	if prev.NotionCompactions > notionCompactions {
		notionCompactions = prev.NotionCompactions
	}
	*rec = *prev
	rec.Prev = nil
	rec.ResendContext = resendCtx
	rec.NotionCompactions = notionCompactions
	rec.UpdatedAt = time.Now()
	if failed {
		rec.Failures++
		if maxSeq > rec.LastSeq {
			if failedConvLen > rec.ConvLen && failedConvHash != "" {
				rec.CommittedConvLen = failedConvLen
				rec.CommittedConvHash = failedConvHash
				rec.CommittedUserNormLen = failedUserNormLen
			}
			rec.LastSeq = maxSeq
		}
	}
	s.saveLocked()
}

// failed handles a turn whose transcript shows a failure or no usable reply.
// A bound follow-up keeps its binding; anything else is dropped as before.
func (t *agentTurn) failed(maxSeq agentSeq) {
	if t.sideFail != nil {
		t.sideFail()
		return
	}
	if t.keyed && t.prev != nil {
		t.rollbackFailed(maxSeq)
		return
	}
	if t.keyed && t.store != nil && t.rec != nil && t.rec.State != agentStateCompleted {
		// Bound session, first turn failed: keep the thread (no second chat).
		t.store.markFailed(t.rec, maxSeq)
		return
	}
	t.discard()
}

func (s *agentThreadStore) markFailed(rec *agentThreadRecord, maxSeq agentSeq) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur, ok := s.records[rec.ThreadID]; !ok || cur != rec {
		return
	}
	rec.State = agentStateFailed
	rec.Prev = nil
	rec.Failures++
	if maxSeq > rec.LastSeq {
		rec.LastSeq = maxSeq
	}
	rec.UpdatedAt = time.Now()
	s.saveLocked()
}

// rebindCandidate: helper-first ordering. The bound thread carries a tool-less
// request with at most one turn and the new request is tool-using.
func rebindCandidate(rec *agentThreadRecord, in agentPlanInput) bool {
	return in.Tooled && !rec.Tooled && rec.Turns <= 1 && rec.RootHash != ""
}

func differentRootError() error {
	return &agentBindingError{Kind: "diverged", Msg: "this request does not continue the conversation bound to its Notion thread (first message differs: helper/title call, edited first message, or a client compaction this bridge could not verify); it is not served on a second chat and the binding is kept. If you ran /compact, its continuation did not have the verified layout (the bridge supports the summarisation instruction of " + knownSummaryVersions() + "): retry the original request, run /compact again, or start a new client session. Otherwise retry the original request or start a new client session"}
}

// agentThreadRebindPrompt moves a tool-using conversation onto a thread that
// only carried an unrelated, tool-less single-turn request.
func agentThreadRebindPrompt(messages []ChatMessage) string {
	var prompt strings.Builder
	prompt.WriteString("Earlier messages in this thread belong to an unrelated, finished request. Ignore them completely. The conversation below starts here and is the only one that matters from now on.\n\n")
	prompt.WriteString(agentThreadPrompt(messages))
	return prompt.String()
}

func failedFirstTurnRetryOrContinuation(rec *agentThreadRecord, in agentPlanInput, conv []string, ns []ChatMessage) bool {
	if rec.State != agentStateFailed || rec.ConvLen <= 0 || len(ns) < rec.ConvLen || conv[rec.ConvLen] != rec.ConvHash {
		return false
	}
	if len(ns) == rec.ConvLen {
		return rec.Attachments == len(in.Attachments)
	}
	if len(in.Attachments) < rec.Attachments {
		return false
	}
	if kind, _ := classifySummaryInstruction(ns[len(ns)-1]); kind != summaryNone {
		return false
	}
	for _, m := range ns[rec.ConvLen:] {
		if !isPlainUserMessage(m) {
			return false
		}
	}
	return true
}

// unacceptedTurnSuperseded: the bound record holds a plain follow-up that was sent
// but never confirmed (state submitting), the request is not its retry, and it
// continues the stable state recorded before that follow-up (rec.Prev). That is how
// a client that abandoned a failed turn continues: its history carries the failed
// turn's messages (or not) plus new ones, but no assistant reply for the failed turn.
func unacceptedTurnSuperseded(rec *agentThreadRecord, in agentPlanInput, conv []string, ns []ChatMessage) bool {
	prev := rec.Prev
	if rec.State != agentStateSubmitting || prev == nil || !rec.PendingFollowUp || rec.TurnStart <= 0 || rec.TurnCompacted || rec.Correction {
		return false
	}
	n := len(ns)
	return prev.State == agentStateCompleted && prev.ConvLen > 0 && prev.ConvLen < n &&
		conv[prev.ConvLen] == prev.ConvHash && validFollowUp(ns, prev.ConvLen) &&
		len(in.Attachments) >= prev.Attachments
}

// supersedeUnaccepted runs under the thread lock after the transcript proved the
// failed turn was never accepted and the thread is idle: the binding goes back to
// the last stable state and the new request is planned from there as an ordinary
// follow-up (its delta already contains the failed turn's messages, sent once).
func (s *agentThreadStore) supersedeUnaccepted(t *agentTurn, in agentPlanInput) (*agentTurn, error) {
	s.mu.Lock()
	rec := t.rec
	if cur, ok := s.records[rec.ThreadID]; !ok || cur != rec || rec.State != agentStateSubmitting || rec.Prev == nil || rec.ClientMessageID != t.clientMessageID {
		s.mu.Unlock()
		return nil, &agentBindingError{Kind: "pending", Msg: "the previous turn of this session changed while it was being checked; retry shortly (binding kept)"}
	}
	prev := *rec.Prev
	prev.Prev = nil
	*rec = prev
	rec.UpdatedAt = time.Now()
	s.saveLocked()
	s.mu.Unlock()
	t.release() // the thread lock is re-taken by the regular planning below
	log.Printf("[agent-thread] previous turn of thread %s was never accepted upstream (no new entity, thread idle); continuing from the last stable state", shortID(rec.ThreadID))
	return s.beginKeyed(in)
}

// rewoundSummaryRequest reports whether ns is the bound conversation (chain
// hash want over convLen messages) whose final assistant reply was dropped by
// Claude Code's compaction, with the summary instruction appended as the final
// text block of the preceding user or tool_result message.
func rewoundSummaryRequest(ns []ChatMessage, conv []string, convLen int, want string) summaryKind {
	n := len(ns)
	if n == 0 || len(conv) < n {
		return summaryNone
	}
	if convLen == n {
		last := ns[n-1]
		if last.Role != "user" || last.StringContent || last.TextBlocks < 2 || last.LastTextBlock == "" {
			return summaryNone
		}
		pending := last
		pending.Content = strings.TrimSuffix(last.Content, last.LastTextBlock)
		if pending.Content == last.Content || normalizeSessionUserContent(pending.Content) == "" {
			return summaryNone
		}
		if chainNext(conv[n-1], agentMessageDigest(pending)) != want {
			return summaryNone
		}
		kind, _ := classifySummaryInstruction(last)
		return kind
	}
	if convLen > 0 && convLen+1 == n && conv[convLen] == want && ns[convLen-1].Role == "tool" {
		last := ns[n-1]
		if last.Role != "user" || last.StringContent || !last.AfterToolResult || last.TextBlocks < 1 || last.LastTextBlock == "" {
			return summaryNone
		}
		before := strings.TrimSuffix(last.Content, last.LastTextBlock)
		if before == last.Content || normalizeSessionUserContent(before) != "" {
			return summaryNone
		}
		kind, _ := classifySummaryInstruction(last)
		return kind
	}
	return summaryNone
}

func autoCompactRewoundLastReply(ns []ChatMessage, conv []string, want string) bool {
	return rewoundSummaryRequest(ns, conv, len(ns), want) == summaryKnown
}

// truncatedSummaryRequest reports whether ns is a Claude Code summarisation
// request whose historical messages were dropped or microcompacted (for example
// when compactConversation retries with a truncated history after a 413). Same
// length + unchanged root is left to rewoundSummaryRequest so an edited pending
// message at the exact bound length stays refused.
func truncatedSummaryRequest(rec *agentThreadRecord, ns []ChatMessage, conv []string) summaryKind {
	n := len(ns)
	if rec == nil || rec.ConvLen <= 0 || n == 0 || len(conv) < n {
		return summaryNone
	}
	if n == rec.ConvLen && (rec.RootHash == "" || rec.RootHash == conv[1]) {
		return summaryNone
	}
	kind, _ := classifySummaryInstruction(ns[n-1])
	return kind
}

func chainNext(prev, digest string) string {
	next := sha256.Sum256([]byte(prev + "\x00" + digest))
	return hex.EncodeToString(next[:])
}
