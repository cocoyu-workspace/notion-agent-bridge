package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"
)

// Claude Code /compact on a bound Notion thread (issue #13).
//
// What a real /compact looks like on the wire (Claude Code 2.1.280 and 2.1.296,
// observed with a local fake upstream; same client session ID on all three):
//
//  1. ordinary requests, as before;
//  2. a summarisation request: the history so far plus the assistant reply and
//     a last user message whose content is the full summary instruction as a
//     plain STRING (it carries the session's client tools like any request);
//     the reply carries <analysis>/<summary>;
//  3. a request whose first user message was REWRITTEN into several text blocks
//     (joined by the bridge into one string): an attribution <system-reminder>
//     block (also present on the original first message), the continuation
//     preface, "Summary:" and the summary, the transcript-pointer paragraph and
//     the "Continue the conversation ..." sentence, a caveat block, the /compact
//     command block, a stdout block, and finally the new user text.
//
// There is NO trusted compaction ID or metadata. Everything above can also be
// typed or pasted by a user, so the handling is conservative and auditable
// rather than unforgeable (see AGENT_THREAD.md, "Trust limits"):
//
//   - step 2 is recognised by the versioned instruction fingerprint (length +
//     SHA-256 of the full text, or, for "/compact <focus>", of the bytes before
//     and after the inserted "Additional Instructions:" block; the continuation
//     has the same layout either way; agent_summary_fingerprint.go) on top of the
//     conversation-prefix/completed-state checks, and served on a one-shot
//     Notion thread (never the bound thread); only a hash of the summary it
//     produced and a counter are remembered on the binding;
//   - step 3 re-bases the bound thread ONLY when every condition holds: same
//     session with a completed binding, exactly one message, every block of the
//     observed structure present in order, new user text present, a summarisation
//     side request was seen for this very conversation state, and the summary in
//     the request matches the one that side request produced. The thread then
//     receives the summary plus the new message, never the old history, never
//     the boilerplate blocks.
//
// Anything else is refused as "diverged": nothing is sent, the binding is kept,
// no second chat is created.
//
// Auto-compact (2.1.296) differs in two ways: the summarisation request has no
// assistant reply after the history; the instruction is a second text block of the
// PENDING user message (classifySummaryInstruction); and the continuation is the
// summary message alone (the client keeps no messages): preface, "Summary:", the
// pointer paragraph, the "Continue the conversation ..." sentence, then at most
// <system-reminder> blocks; no /compact command, caveat, stdout block or user text
// (parseAutoCompactTail). The summary hash, single use and one-message rules are
// the same.

const (
	claudeCompactionPrefix  = "This session is being continued from a previous conversation that ran out of context."
	claudeCompactionSummary = "Summary:\n"
	claudeCompactionPointer = "\n\nIf you need specific details from before compaction"
	// The pointer paragraph's fixed text (up to the transcript path) and the
	// continuation sentence the client appends when it suppresses follow-up
	// questions (the "control" prompt arm, as observed; other arms add guidance
	// text nobody has seen and stay refused for an auto-compact).
	claudeCompactionPointerHead = claudeCompactionPointer + " (like exact code snippets, error messages, or content you generated), read the full transcript at: "
	// claudeCompactionKept follows the continuation sentence when the client
	// kept the most recent messages verbatim after the summary (2.1.296 auto-
	// compact: the reply the summarisation request dropped, then the new
	// user message).
	claudeCompactionKept     = "\n\nThe messages after this summary are the most recent messages from before compaction, kept verbatim. The summary was written without seeing them, so something it says has not happened yet may already have happened in them."
	claudeCompactionContinue = "Continue the conversation from where it left off without asking the user any further questions. Resume directly \u2014 do not acknowledge the summary, do not recap what was happening, do not preface with \"I'll continue\" or similar. Pick up the last task as if the break never happened."
)

// agentCompactionDetector decides whether a request that does not continue the
// bound conversation is a verified Claude Code compaction of it. nil disables
// the re-base completely (every such request is refused). The default is the
// structural verification below; it is a variable so tests and operators can
// switch it off.
var agentCompactionDetector = func(rec *agentThreadRecord, ns []ChatMessage) bool {
	_, ok, _ := verifyClaudeCompaction(rec, ns)
	return ok
}

type claudeCompaction struct {
	Summary string // the summary text carried by the continuation message
	NewUser string // what the user typed after /compact (empty for an auto-compact)
	// Auto marks the continuation of an AUTO-compact: the client keeps no
	// message, so the request is the summary message alone, and nothing was
	// typed. Resume is the client's continuation sentence and Restored the
	// <system-reminder> blocks it re-attached after it (e.g. re-read files).
	Auto     bool
	Resume   string
	Restored string
	// Kept: the continuation message says recent messages follow verbatim.
	Kept bool
	// KeptNew: the kept messages the bound thread has not seen yet (tool results
	// of the kept reply's calls and/or new user text), forwarded after the summary.
	KeptNew []ChatMessage
}

var leadingReminderPattern = regexp.MustCompile(`(?s)^\s*<system-reminder>.*?</system-reminder>`)

// dropLeadingReminders removes the <system-reminder> blocks Claude Code puts in
// front of a conversation's first user message (observed in 2.1.296: an
// attribution block). Only LEADING blocks are removed.
func dropLeadingReminders(s string) string {
	for {
		loc := leadingReminderPattern.FindStringIndex(s)
		if loc == nil {
			return strings.TrimLeft(s, " \t\r\n")
		}
		s = s[loc[1]:]
	}
}

// parseClaudeCompaction parses the rewritten first user message. shaped reports
// that it starts with the continuation preface; when shaped and not ok, reason
// names what is missing. Nothing but the observed layout is accepted.
func parseClaudeCompaction(content string) (cc claudeCompaction, shaped, ok bool, reason string) {
	text := dropLeadingReminders(content)
	if !strings.HasPrefix(text, claudeCompactionPrefix) {
		return cc, false, false, ""
	}
	rest := text[len(claudeCompactionPrefix):]
	i := strings.Index(rest, claudeCompactionSummary)
	if i < 0 {
		return cc, true, false, "the \"Summary:\" section is missing"
	}
	after := rest[i+len(claudeCompactionSummary):]
	j := strings.Index(after, claudeCompactionPointer)
	if j < 0 {
		return cc, true, false, "the transcript-pointer paragraph is missing"
	}
	body := strings.TrimSpace(after[:j])
	if summaryDigest(body) == "" {
		return cc, true, false, "the summary is empty"
	}
	tail := after[j:]
	if cc, why, isAuto := parseAutoCompactTail(tail); isAuto {
		cc.Summary = body
		if why != "" {
			return claudeCompaction{}, true, false, why
		}
		return cc, true, true, ""
	}
	pos := 0
	for _, step := range []struct{ name, token string }{
		{"caveat", "<local-command-caveat>"},
		{"caveat end", "</local-command-caveat>"},
		{"/compact command", "<command-name>/compact</command-name>"},
		{"stdout", "<local-command-stdout>Compacted"},
		{"stdout end", "</local-command-stdout>"},
	} {
		k := strings.Index(tail[pos:], step.token)
		if k < 0 {
			return cc, true, false, fmt.Sprintf("the %s block of a /compact continuation is missing or out of order", step.name)
		}
		pos += k + len(step.token)
	}
	newUser := strings.TrimSpace(tail[pos:])
	if newUser == "" {
		return cc, true, false, "there is no user message after the /compact blocks"
	}
	return claudeCompaction{Summary: body, NewUser: newUser}, true, true, ""
}

var restoredReminderPattern = regexp.MustCompile(`(?s)^\s*<system-reminder>.*?</system-reminder>`)

// parseAutoCompactTail recognises the part after the summary of an auto-compact
// continuation (Claude Code 2.1.296, compactConversation with isAutoCompact):
// the transcript-pointer paragraph, the client's continuation sentence and then
// at most <system-reminder> blocks (files and state the client re-attaches after
// compacting). There is no /compact command, no caveat and no stdout block and
// no user text. isAuto is false when the tail has the layout of a manual
// /compact (a caveat or command block is present); when it is true and why is
// not empty the tail is rejected for that reason.
func parseAutoCompactTail(tail string) (cc claudeCompaction, why string, isAuto bool) {
	if strings.Contains(tail, "<local-command-caveat>") || strings.Contains(tail, "<command-name>") || strings.Contains(tail, "<local-command-stdout>") {
		return cc, "", false
	}
	const reasonPrefix = "the layout after the summary is neither a /compact continuation nor the auto-compact one this bridge verified: "
	if !strings.HasPrefix(tail, claudeCompactionPointerHead) {
		return cc, reasonPrefix + "the transcript-pointer paragraph is not in its known form", true
	}
	rest := tail[len(claudeCompactionPointerHead):]
	nl := strings.IndexByte(rest, '\n')
	if nl <= 0 {
		return cc, reasonPrefix + "the transcript path is missing", true
	}
	rest = rest[nl+1:]
	if !strings.HasPrefix(rest, claudeCompactionContinue) {
		return cc, reasonPrefix + "the continuation sentence is missing or worded differently from the one verified", true
	}
	rest = rest[len(claudeCompactionContinue):]
	kept := false
	if strings.HasPrefix(rest, claudeCompactionKept) {
		kept = true
		rest = rest[len(claudeCompactionKept):]
	}
	var restored strings.Builder
	for strings.TrimSpace(rest) != "" {
		loc := restoredReminderPattern.FindStringIndex(rest)
		if loc == nil {
			return cc, reasonPrefix + "text other than <system-reminder> blocks follows the continuation sentence (an auto-compact has no user text)", true
		}
		restored.WriteString(strings.TrimSpace(rest[:loc[1]]))
		restored.WriteString("\n")
		rest = rest[loc[1]:]
	}
	return claudeCompaction{Auto: true, Kept: kept, Resume: claudeCompactionContinue, Restored: strings.TrimSpace(restored.String())}, "", true
}

var (
	analysisBlockPattern    = regexp.MustCompile(`(?s)<analysis>.*?</analysis>`)
	trailingHookFooterRegex = regexp.MustCompile(`(?s)\r?\n----\r?\n上一轮 干活\s.*$`)
)

func stripTrailingHookFooter(s string) string {
	return strings.TrimSpace(trailingHookFooterRegex.ReplaceAllString(s, ""))
}

// normalizeSummary reduces a summarisation reply, or the summary carried by the
// continuation, to comparable text: scratch analysis dropped, <summary> tags
// unwrapped in place (matching Claude Code's formatCompactSummary), trailing
// hook footers stripped, and whitespace collapsed.
func normalizeSummary(s string) string {
	s = analysisBlockPattern.ReplaceAllString(s, "")
	if i := strings.Index(s, "<summary>"); i >= 0 {
		prefix := s[:i]
		inner := s[i+len("<summary>"):]
		suffix := ""
		if j := strings.LastIndex(inner, "</summary>"); j >= 0 {
			suffix = inner[j+len("</summary>"):]
			inner = inner[:j]
		}
		s = prefix + " " + inner + " " + suffix
	}
	s = stripTrailingHookFooter(s)
	s = strings.TrimSpace(s)
	s = strings.TrimSpace(strings.TrimPrefix(s, "Summary:"))
	return strings.Join(strings.Fields(s), " ")
}

// summaryDigest is the only thing kept of a summary: a hash.
func summaryDigest(s string) string {
	n := normalizeSummary(s)
	if n == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(n))
	return hex.EncodeToString(sum[:])
}

// verifyClaudeCompaction applies every condition for a re-base. reason is the
// user-visible explanation when ok is false and the request looks like a
// compaction at all.
func verifyClaudeCompaction(rec *agentThreadRecord, ns []ChatMessage) (cc claudeCompaction, ok bool, reason string) {
	if len(ns) == 0 || ns[0].Role != "user" {
		return cc, false, ""
	}
	cc, shaped, parsed, why := parseClaudeCompaction(ns[0].Content)
	switch {
	case !shaped:
		return cc, false, ""
	case !parsed:
		return cc, false, why
	case cc.Auto && hasManualCompactMarkers(ns[1:]):
		newUser, why := manualCompactKeptTail(ns[1:])
		if why != "" {
			return claudeCompaction{}, false, why
		}
		cc.Auto = false
		cc.Kept = true
		cc.NewUser = newUser
	case cc.Auto && (cc.Kept || (rec != nil && rec.AuxRewound && len(ns) > 1)):
		kept, why := autoCompactKeptTail(ns[1:])
		if why != "" {
			return claudeCompaction{}, false, why
		}
		cc.Kept = true
		cc.KeptNew = kept
	case len(ns) != 1:
		return cc, false, "a /compact continuation must consist of exactly one message"
	}
	switch {
	case rec.AuxSummaryHash == "" || rec.AuxConvHash == "" || rec.AuxConvHash != rec.ConvHash:
		return cc, false, "no matching summarisation request was seen for this conversation (or the conversation moved on since); a pasted or edited message cannot re-base it"
	case summaryDigest(cc.Summary) != rec.AuxSummaryHash:
		return cc, false, "the summary in this request does not match the summary produced for this conversation"
	}
	return cc, true, ""
}

func hasManualCompactMarkers(rest []ChatMessage) bool {
	for _, m := range rest {
		if m.Role == "user" && (strings.Contains(m.Content, "<local-command-caveat>") || strings.Contains(m.Content, "<command-name>") || strings.Contains(m.Content, "<local-command-stdout>")) {
			return true
		}
	}
	return false
}

// manualCompactKeptTail parses the messages after the summary when a manual
// /compact kept the bound thread's last assistant reply verbatim (observed
// whenever the conversation had >= 2 assistant messages, e.g. after any tool
// turn): the kept assistant reply is ns[1], and the /compact caveat, command,
// stdout and new user text are merged into ns[2:].
func manualCompactKeptTail(rest []ChatMessage) (newUser string, why string) {
	if len(rest) < 2 {
		return "", "expected the kept assistant reply followed by the /compact blocks and a new user message"
	}
	if rest[0].Role != "assistant" || len(rest[0].ToolCalls) > 0 || strings.TrimSpace(rest[0].Content) == "" {
		return "", "the first kept message is not a plain assistant reply"
	}
	var parts []string
	for _, m := range rest[1:] {
		if m.Role != "user" || len(m.ToolCalls) > 0 || m.ToolCallID != "" {
			return "", "only user messages may follow the kept reply"
		}
		if m.Content != "" {
			parts = append(parts, m.Content)
		}
	}
	tail := dropLeadingReminders(strings.Join(parts, ""))
	if !strings.HasPrefix(tail, "<local-command-caveat>") {
		return "", "the caveat block of a /compact continuation is missing or out of order"
	}
	pos := 0
	for _, step := range []struct{ name, token string }{
		{"caveat", "<local-command-caveat>"},
		{"caveat end", "</local-command-caveat>"},
		{"/compact command", "<command-name>/compact</command-name>"},
		{"stdout", "<local-command-stdout>Compacted"},
		{"stdout end", "</local-command-stdout>"},
	} {
		k := strings.Index(tail[pos:], step.token)
		if k < 0 {
			return "", fmt.Sprintf("the %s block of a /compact continuation is missing or out of order", step.name)
		}
		pos += k + len(step.token)
	}
	newUser = strings.TrimSpace(tail[pos:])
	if newUser == "" || normalizeSessionUserContent(newUser) == "" {
		return "", "there is no user message after the /compact blocks"
	}
	return newUser, ""
}

// autoCompactKeptTail checks the messages an auto-compact continuation keeps
// verbatim after the summary, in the layouts observed with 2.1.296. The first is
// the bound thread's last reply, which the summarisation request had dropped:
// plain text (compaction before the next prompt) or tool calls (compaction in
// the middle of a tool loop, after the results came back). What follows is new
// to the bound thread: tool results for exactly those calls and/or user text.
// The thread already holds the reply, so only the new messages are returned.
func autoCompactKeptTail(rest []ChatMessage) (newMsgs []ChatMessage, why string) {
	const p = "the messages kept verbatim after an auto-compact summary are not in the layout this bridge verified: "
	if len(rest) < 2 {
		return nil, p + "expected the last reply followed by new messages"
	}
	reply := rest[0]
	if reply.Role != "assistant" || strings.TrimSpace(reply.Content) == "" && len(reply.ToolCalls) == 0 {
		return nil, p + "the first kept message is not an assistant reply"
	}
	calls := map[string]bool{}
	for _, c := range reply.ToolCalls {
		calls[c.ID] = true
	}
	results, text := 0, false
	for _, m := range rest[1:] {
		switch {
		case m.Role == "tool":
			if !calls[m.ToolCallID] {
				return nil, p + "a kept tool result does not answer a call of the kept reply"
			}
			results++
		case m.Role == "user" && len(m.ToolCalls) == 0 && m.ToolCallID == "":
			if normalizeSessionUserContent(m.Content) != "" {
				text = true
			}
		default:
			return nil, p + "only tool results and user messages may follow the kept reply"
		}
	}
	if len(calls) > 0 && results == 0 {
		return nil, p + "the kept reply requested tools but no result follows"
	}
	if len(calls) == 0 && !text {
		return nil, p + "there is no new user message"
	}
	return append([]ChatMessage(nil), rest[1:]...), ""
}

// compactionShaped is the cheap pre-lock gate: a request that parses as a full
// compaction continuation may have to wait for the thread lock (a summarisation
// side request holds it); the real decision is compactionCandidate.
func compactionShaped(ns []ChatMessage) bool {
	if agentCompactionDetector == nil || len(ns) == 0 || ns[0].Role != "user" {
		return false
	}
	_, _, ok, _ := parseClaudeCompaction(ns[0].Content)
	return ok
}

// compactionCandidate: the installed detector accepts this request as a
// compaction of the bound conversation.
func compactionCandidate(rec *agentThreadRecord, ns []ChatMessage) bool {
	return agentCompactionDetector != nil && rec.RootHash != "" && len(ns) > 0 && agentCompactionDetector(rec, ns)
}

// agentRootChangeError explains a refused change of the conversation root. A
// request shaped like a /compact continuation names the failed condition;
// every other root change keeps the generic error, which also names compaction
// as a possible cause.
func agentRootChangeError(rec *agentThreadRecord, ns []ChatMessage) error {
	if len(ns) > 0 && ns[0].Role == "user" {
		if _, shaped, _, _ := parseClaudeCompaction(ns[0].Content); shaped {
			reason := "compaction re-base is switched off"
			if agentCompactionDetector != nil {
				if _, _, r := verifyClaudeCompaction(rec, ns); r != "" {
					reason = r
				} else {
					reason = "the request was not accepted as a verified compaction"
				}
			}
			return &agentBindingError{Kind: "diverged", Msg: "this request is the continuation of a Claude Code compaction, /compact or auto-compact (its first message was rewritten into a summary), and cannot be matched to the bound Notion thread: " + reason + ". Nothing was sent, the binding is kept and no second chat was created. Retry the original request, run /compact again (the bridge supports the summarisation instruction of " + knownSummaryVersions() + "; an auto-compact continuation is accepted only in the layout verified for 2.1.296) or start a new client session"}
		}
	}
	return differentRootError()
}

const agentCompactionNotice = "CLIENT CONTEXT COMPACTION\nThe local client compacted its own history. Its conversation now starts with the summary below, which was produced from this conversation. This thread still holds your earlier messages; use them only as background. The summary and the message after it are authoritative: rules, decisions, markers and unfinished work stated there stay in force, and any earlier local action request whose result is not shown below is closed. Never invent tool results.\n\n"

// agentCompactionAutoNotice replaces agentCompactionNotice for an auto-compact:
// nothing was typed after it, the client resumes the work pending in the summary.
const agentCompactionAutoNotice = "CLIENT CONTEXT COMPACTION (automatic)\nThe local client compacted its own history automatically because its context was full. Its conversation now starts with the summary below, which was produced from this conversation, including the user message that was pending at that moment. This thread still holds your earlier messages; use them only as background. The summary is authoritative: rules, decisions, markers and unfinished work stated there stay in force, and any earlier local action request whose result is not shown below is closed. No new user message follows: the client asks you to resume the work that was pending. Never invent tool results.\n\n"

// agentCompactionAutoKeptNotice: an auto-compact whose continuation kept the
// last reply and carries the new user message.
const agentCompactionAutoKeptNotice = "CLIENT CONTEXT COMPACTION (automatic)\nThe local client compacted its own history automatically because its context was full. Its conversation now starts with the summary below, which was produced from this conversation. This thread still holds your earlier messages; use them only as background. The summary and the message after it are authoritative: rules, decisions, markers and unfinished work stated there stay in force, and any earlier local action request whose result is not shown below is closed. Never invent tool results.\n\n"

// agentThreadCompactionPrompt renders the summary plus the new user message
// (and the changed system/tool context, when it changed). The boilerplate
// blocks of the continuation message are not forwarded. For an auto-compact
// there is no user message: the client's continuation sentence and the context
// it re-attached are forwarded instead.
func agentThreadCompactionPrompt(updated []ChatMessage, cc claudeCompaction) string {
	var prompt strings.Builder
	if cc.Auto && len(cc.KeptNew) > 0 {
		prompt.WriteString(agentCompactionAutoKeptNotice)
	} else if cc.Auto {
		prompt.WriteString(agentCompactionAutoNotice)
	} else {
		prompt.WriteString(agentCompactionNotice)
	}
	if len(updated) > 0 {
		prompt.WriteString("UPDATED CONTEXT: the project context and local action protocol below replace every earlier version for the rest of this thread.\n\n")
		writeAgentMessages(&prompt, updated)
	}
	prompt.WriteString("CLIENT SUMMARY\n\n")
	prompt.WriteString(stripTrailingHookFooter(cc.Summary))
	if cc.Auto && len(cc.KeptNew) > 0 {
		if cc.Restored != "" {
			prompt.WriteString("\n\nCONTEXT THE CLIENT RE-ATTACHED AFTER COMPACTING\n\n")
			prompt.WriteString(cc.Restored)
		}
		prompt.WriteString("\n\nThe summary was written without your last reply in this thread; the client kept that reply verbatim, so it stands (including any local action it requested). The new items below follow it.\n\nNEW MESSAGES\n\n")
		writeAgentMessages(&prompt, cc.KeptNew)
		prompt.WriteString(agentThreadPromptTail)
		return prompt.String()
	}
	if cc.Auto {
		prompt.WriteString("\n\nCLIENT RESUME INSTRUCTION\n\n")
		prompt.WriteString(cc.Resume)
		if cc.Restored != "" {
			prompt.WriteString("\n\nCONTEXT THE CLIENT RE-ATTACHED AFTER COMPACTING\n\n")
			prompt.WriteString(cc.Restored)
		}
		prompt.WriteString("\n\n")
		prompt.WriteString(agentThreadPromptTail)
		return prompt.String()
	}
	if cc.Restored != "" {
		prompt.WriteString("\n\nCONTEXT THE CLIENT RE-ATTACHED AFTER COMPACTING\n\n")
		prompt.WriteString(cc.Restored)
	}
	prompt.WriteString("\n\nNEW MESSAGE\n\n")
	if cc.NewUser != "" {
		writeAgentMessages(&prompt, []ChatMessage{{Role: "user", Content: cc.NewUser}})
	}
	prompt.WriteString(agentThreadPromptTail)
	return prompt.String()
}

// compactionFromMessages re-derives the compaction content of a pending turn.
func compactionFromMessages(ns []ChatMessage) claudeCompaction {
	if len(ns) > 0 {
		if cc, _, ok, _ := parseClaudeCompaction(ns[0].Content); ok {
			if cc.Auto && hasManualCompactMarkers(ns[1:]) {
				cc.Auto = false
				cc.Kept = true
				cc.NewUser, _ = manualCompactKeptTail(ns[1:])
			} else if cc.Auto && (cc.Kept || len(ns) > 1) {
				cc.Kept = true
				cc.KeptNew, _ = autoCompactKeptTail(ns[1:])
			}
			return cc
		}
		return claudeCompaction{Summary: ns[0].Content}
	}
	return claudeCompaction{}
}

// beginCompactionLocked re-bases the binding on a verified compaction. s.mu is
// held and t.unlock is the thread lock to release; both are released here on
// error. The turn is recorded as a follow-up (the thread already exists). The
// summary on record is consumed.
func (s *agentThreadStore) beginCompactionLocked(t *agentTurn, rec *agentThreadRecord, in agentPlanInput, full, conv []string, ns []ChatMessage, sysHash string) (*agentTurn, error) {
	if rec.State != agentStateCompleted {
		s.mu.Unlock()
		t.release()
		return nil, &agentBindingError{Kind: "pending", Msg: "the earlier request of this session has not completed; retry shortly (binding kept)"}
	}
	cc := compactionFromMessages(ns)
	var updated []ChatMessage
	sysChanged := sysHash != rec.SystemHash || rec.ResendContext
	rec.ResendContext = false
	if sysChanged {
		updated = systemMessages(in.Messages)
	}
	n := len(ns)
	prev := *rec
	prev.Prev = nil
	t.prev = &prev
	rec.Prev = &prev
	rec.Model = in.Model
	rec.ClientInterrupted = false
	rec.Correction = false
	setAgentEditableTurn(rec, conv, ns, 0)
	rec.RootHash = conv[1]
	rec.Baseline = rec.LastSeq
	rec.TurnStart, rec.TurnAttach = 0, 0
	rec.TurnSysUpdate = sysChanged
	rec.TurnCompacted = true
	rec.Compactions++
	rec.clearAux()
	rec.PendingFollowUp = true
	rec.PrefixLen, rec.PrefixHash = len(in.Messages), full[len(in.Messages)]
	rec.ConvLen, rec.ConvHash = n, conv[n]
	rec.SystemHash = sysHash
	if in.Tooled {
		rec.Tooled = true
	}
	rec.Attachments = len(in.Attachments)
	rec.State = agentStateSubmitting
	rec.ClientMessageID = uuidFromSeed(fmt.Sprintf("%s|compact|%s|%d", rec.ThreadID, conv[n], rec.Failures))
	rec.UpdatedAt = time.Now()
	s.saveLocked()
	t.mode, t.followUp, t.submit = agentModeFollowUp, true, true
	t.baseline = rec.Baseline
	t.clientMessageID = rec.ClientMessageID
	t.prompt = agentThreadCompactionPrompt(updated, cc)
	t.attachments = append([]UploadedAttachment(nil), in.Attachments...)
	s.mu.Unlock()
	kind := "/compact"
	if cc.Auto {
		kind = "auto-compact"
	}
	log.Printf("[agent-thread] verified Claude %s re-based bound thread %s (compaction #%d, summary hash %s, %d new message char(s); no second thread)", kind, shortID(rec.ThreadID), rec.Compactions, shortID(summaryDigest(cc.Summary)), len(cc.NewUser)+keptChars(cc.KeptNew))
	return t, nil
}

// clearAux forgets the summarisation side request recorded for the binding.
func (rec *agentThreadRecord) clearAux() {
	rec.AuxSummaryHash, rec.AuxConvHash, rec.AuxRewound = "", "", false
}

// beginSidecarLocked serves a summarisation side request on a one-shot Notion
// thread: the bound thread, its transcript, baseline and hashes are not
// touched. Thread and message IDs are deterministic for the same conversation
// state (a retry finds the same thread) and change after a failed attempt. When
// the reply arrives only its hash is recorded on the binding. s.mu is held and
// released here; t.unlock (the thread lock) travels with the returned turn.
func (s *agentThreadStore) beginSidecarLocked(t *agentTurn, rec *agentThreadRecord, in agentPlanInput, conv []string, n int, rewound bool) (*agentTurn, error) {
	seed := fmt.Sprintf("%s|aux-v2|%s|%d", rec.ThreadID, conv[n], rec.AuxFailures)
	convHash, auxLen := rec.ConvHash, n
	side := &agentTurn{
		mode: agentModeNew, threadID: uuidFromSeed(seed + "|thread"), title: formatAgentThreadTitle("旁", "杂项", "上下文压缩摘要"), clientMessageID: uuidFromSeed(seed + "|message"),
		submit: true, probe: true, prompt: agentThreadSummaryPrompt(in.Messages), attachments: in.Attachments, unlock: t.unlock,
	}
	side.sideReply = func(text string) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if cur, ok := s.records[rec.ThreadID]; !ok || cur != rec || rec.ConvHash != convHash || rec.State != agentStateCompleted {
			return
		}
		h := summaryDigest(text)
		if h == "" {
			rec.clearAux()
		} else {
			if h != rec.AuxSummaryHash || rec.AuxConvHash != convHash {
				rec.AuxCount++
			}
			rec.AuxSummaryHash, rec.AuxConvHash, rec.AuxRewound = h, convHash, rewound
		}
		s.saveLocked()
		log.Printf("[agent-thread] summarisation reply for thread %s recorded (summary hash %s, side request #%d); nothing was added to the bound thread", shortID(rec.ThreadID), shortID(h), rec.AuxCount)
	}
	side.sideFail = func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if cur, ok := s.records[rec.ThreadID]; ok && cur == rec {
			rec.AuxFailures++
			s.saveLocked()
		}
	}
	s.mu.Unlock()
	log.Printf("[agent-thread] summarisation side request (%d message(s)) for thread %s runs on a one-shot Notion thread %s; the bound thread and its binding are untouched", auxLen, shortID(rec.ThreadID), shortID(side.threadID))
	return side, nil
}

func keptChars(ms []ChatMessage) int {
	n := 0
	for _, m := range ms {
		n += len(m.Content)
	}
	return n
}
