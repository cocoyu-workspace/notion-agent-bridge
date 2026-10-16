package proxy

import (
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"unicode/utf8"
)

// Notion message size guard (issue #28, long sessions).
//
// Evidence (real service, Claude Code reading a 230KB file; ASCII text, so bytes
// equal runes): a follow-up (sendEventToAgentThread) prompt of about 132KB is
// accepted, 136KB/138KB/140KB answer HTTP 500; a create (createAgentThread) prompt
// of about 185KB is accepted, 200KB answers HTTP 400 ValidationError. A follow-up
// that fails like that leaves the turn "submitting" and the session stuck.
//
// Before a prompt is sent, its UTF-8 byte size is checked against a budget per
// message kind (create / follow-up, configurable, with margin below the measured
// limits). When it does not fit:
//
//  1. tool results contained in this very message are shortened, largest first,
//     keeping head and tail and inserting an explicit marker, until it fits (a
//     result is never shortened below agentTruncMinKeep bytes);
//  2. if it still does not fit (a huge pasted user message, many results), the
//     request is refused BEFORE anything is sent with [bridge:too_large] (HTTP
//     413): no thread write, no pending state, the binding stays usable.
//
// Only the text sent to Notion is shortened. The binding's hashes are computed
// from the client's original messages before any prompt is built, so the
// client's next request (which carries the unshortened history) still continues
// the bound thread. The result depends only on the prompt and the budget, so an
// identical retry rebuilds the identical prompt (retry/lost-acknowledgement
// handling is unchanged). Logs carry sizes only.

const (
	defaultAgentCreateMaxBytes   = 170000
	defaultAgentFollowUpMaxBytes = 120000
	agentTruncMinKeep            = 4096 // bytes of a shortened tool result that are always kept (head + tail)
	agentCtxMinKeep              = 1024 // same for the project context block (stage 3); its full text is attached or already in the thread
	agentDeepTruncMinKeep        = 128  // minimum kept bytes per historical block in multi-turn deep truncation (stage 5)
)

func agentCreateMaxBytes() int {
	if AppConfig != nil && AppConfig.Proxy.AgentCreateMaxBytes > 0 {
		return AppConfig.Proxy.AgentCreateMaxBytes
	}
	return defaultAgentCreateMaxBytes
}

// Notion accepts at most 20 content blocks per message: one text block plus
// files. Offloaded outputs share the remaining slots with image attachments.
const agentMaxOffloadFiles = 8

func agentOffloadMaxFiles(images int) int {
	if AppConfig != nil && AppConfig.Proxy.AgentOffloadDisabled {
		return 0
	}
	n := 19 - images
	if n > agentMaxOffloadFiles {
		n = agentMaxOffloadFiles
	}
	if n < 0 {
		n = 0
	}
	return n
}

func agentFollowUpMaxBytes() int {
	if AppConfig != nil && AppConfig.Proxy.AgentFollowUpMaxBytes > 0 {
		return AppConfig.Proxy.AgentFollowUpMaxBytes
	}
	return defaultAgentFollowUpMaxBytes
}

type agentFitReport struct {
	Before, After int // prompt bytes before / after shortening
	Shortened     int // tool results shortened
	Omitted       int // bytes removed from them (before adding markers)
	Offloaded     int // shortened blocks whose full text is attached as a file
}

// toolResultBlockHeader is exactly what writeAgentMessages emits in front of a
// tool result's content.
func toolResultBlockHeader(m ChatMessage) string {
	return fmt.Sprintf("LOCAL TOOL RESULT (call_id=%s, name=%s)\n", m.ToolCallID, m.Name)
}

func truncationMarker(omitted, total int) string {
	return fmt.Sprintf("[notion-bridge: tool output truncated, %d of %d bytes omitted to fit the Notion message size limit; re-run with a narrower range (Read offset/limit, head/tail/grep) if needed]", omitted, total)
}

func deepTruncationMarker(omitted, total int) string {
	return fmt.Sprintf("[truncated %d of %d bytes]", omitted, total)
}

// truncateMiddle keeps the head and tail of s and replaces roughly omit bytes in
// the middle by the marker, cutting only on UTF-8 boundaries. It returns the new
// text and the number of bytes actually omitted.
func truncateMiddle(s string, omit int) (string, int) {
	return truncateMiddleWith(s, omit, truncationMarker)
}

// offloadMarker replaces the omitted middle of a block whose full text is
// attached to the same message as a file.
func offloadMarker(omitted, total int, name string) string {
	return fmt.Sprintf("[notion-bridge: %d of %d bytes of this output are not shown inline (Notion message size limit). The FULL output is attached to this message as the file %s; read the omitted part from that attachment with your file tools when you need it instead of asking for it again]", omitted, total, name)
}

// contextOffloadMarker replaces the omitted middle of the project context /
// client instructions whose full text is attached as a file.
func contextOffloadMarker(omitted, total int, name string) string {
	return fmt.Sprintf("[notion-bridge: %d of %d bytes of this project context are not shown inline (Notion message size limit). The FULL context is attached to this message as the file %s and stays in force exactly as if it were inline: read it from that attachment with your file tools before acting]", omitted, total, name)
}

func contextTruncationMarker(omitted, total int) string {
	return fmt.Sprintf("[notion-bridge: %d of %d bytes of this project context omitted to fit the Notion message size limit; the earlier full version in this thread still applies]", omitted, total)
}

func offloadFileName(n int, label string) string {
	clean := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, label)
	if len(clean) > 48 {
		clean = clean[:48]
	}
	return fmt.Sprintf("bridge-output-%d-%s.txt", n, clean)
}

func truncateMiddleWith(s string, omit int, marker func(omitted, total int) string) (string, int) {
	keep := len(s) - omit
	if keep < 0 {
		keep = 0
	}
	head := keep / 2
	for head > 0 && head < len(s) && !utf8.RuneStart(s[head]) {
		head--
	}
	tail := len(s) - (keep - keep/2)
	for tail < len(s) && !utf8.RuneStart(s[tail]) {
		tail++
	}
	if tail < head {
		tail = head
	}
	omitted := tail - head
	return s[:head] + "\n" + marker(omitted, len(s)) + "\n" + s[tail:], omitted
}

// fitAgentPrompt returns prompt unchanged when it fits limit bytes; otherwise it
// shortens the tool results of msgs that appear in it, largest first, until it
// fits. When even that is not enough it returns a too_large error and no text.
func fitAgentPrompt(prompt string, msgs []ChatMessage, limit int) (string, agentFitReport, error) {
	out, rep, _, err := fitAgentPromptOffload(prompt, msgs, limit, 0)
	return out, rep, err
}

// fitAgentPromptOffload is fitAgentPrompt that can also offload: up to
// maxFiles of the shortened blocks are returned as text files (their FULL
// content) to attach to the same message, and their marker names the file
// instead of asking for a re-run. Notion's agent reads attachments with its own
// file tools (measured: a 500 KB text attachment, exact line recall), so nothing
// is lost and the inline part still fits the message budget. Blocks beyond
// maxFiles are only truncated. The output depends only on the inputs, so an
// identical retry rebuilds the identical prompt and files.
func fitAgentPromptOffload(prompt string, msgs []ChatMessage, limit, maxFiles int) (string, agentFitReport, []FileAttachment, error) {
	rep := agentFitReport{Before: len(prompt), After: len(prompt)}
	if len(prompt) <= limit {
		return prompt, rep, nil, nil
	}
	var files []FileAttachment
	type cand struct {
		block string // header + content, as rendered
		head  string
		body  string
		n     int    // occurrences in the prompt
		label string // what the block is, for the attachment name
		ctx   bool   // project context / client instructions (stage 3)
	}
	var cands []cand
	seen := map[string]bool{}
	replacedBody := map[string]string{}
	for _, m := range msgs {
		if m.Role != "tool" || len(m.Content) <= agentTruncMinKeep+len(truncationMarker(len(m.Content), len(m.Content))) {
			continue
		}
		head := toolResultBlockHeader(m)
		block := head + m.Content
		if seen[block] {
			continue
		}
		seen[block] = true
		if n := strings.Count(prompt, block); n > 0 {
			cands = append(cands, cand{block: block, head: head, body: m.Content, n: n, label: "tool-" + m.ToolCallID})
		}
	}
	out := prompt
	remaining := len(prompt) - limit
	shorten := func(cands []cand) {
		sort.SliceStable(cands, func(i, j int) bool { return len(cands[i].body) > len(cands[j].body) })
		for _, c := range cands {
			if remaining <= 0 {
				break
			}
			// Re-count in out: an earlier candidate (e.g. raw system message with
			// trailing newline vs normalized systemMessages without it) may have
			// already replaced the same underlying block in out (#116).
			actual := strings.Count(out, c.block)
			if actual == 0 {
				continue
			}
			per := (remaining + actual - 1) / actual // bytes to save in each occurrence
			maxOmit := len(c.body) - agentTruncMinKeep
			if c.ctx {
				maxOmit = len(c.body) - agentCtxMinKeep
			}
			marker := func(omitted, total int) string { return truncationMarker(omitted, total) }
			if c.ctx {
				marker = func(omitted, total int) string { return contextTruncationMarker(omitted, total) }
			}
			var file *FileAttachment
			if len(files) < maxFiles {
				name := offloadFileName(len(files)+1, c.label)
				file = &FileAttachment{Data: []byte(c.body), FileName: name, ContentType: "text/plain"}
				marker = func(omitted, total int) string { return offloadMarker(omitted, total, name) }
				if c.ctx {
					marker = func(omitted, total int) string { return contextOffloadMarker(omitted, total, name) }
				}
			}
			omit := per + len(marker(len(c.body), len(c.body)))
			var short string
			var omitted int
			for i := 0; i < 8; i++ {
				if omit > maxOmit {
					omit = maxOmit
				}
				short, omitted = truncateMiddleWith(c.body, omit, marker)
				saved := len(c.body) - len(short)
				if saved >= per || omit >= maxOmit {
					break
				}
				omit += per - saved
			}
			saved := (len(c.body) - len(short)) * actual
			if saved <= 0 {
				continue
			}
			out = strings.ReplaceAll(out, c.block, c.head+short)
			replacedBody[c.body] = short
			if file != nil {
				files = append(files, *file)
				rep.Offloaded++
			}
			remaining = len(out) - limit
			rep.Shortened += actual
			rep.Omitted += omitted * actual
		}
	}
	shorten(cands)
	if remaining > 0 {
		// Stage 2: shorten subagent <task-notification> outputs, historical
		// assistant text, and historical assistant LOCAL TOOL REQUESTS blocks,
		// while never altering plain user text or the final message.
		var stage2 []cand
		addStage2 := func(head, body, label string) {
			if len(body) <= agentTruncMinKeep+len(truncationMarker(len(body), len(body))) {
				return
			}
			block := head + body
			if seen[block] {
				return
			}
			seen[block] = true
			if n := strings.Count(out, block); n > 0 {
				stage2 = append(stage2, cand{block: block, head: head, body: body, n: n, label: label})
			}
		}
		for i, m := range msgs {
			if m.Role == "user" && strings.Contains(m.Content, "<task-notification>") {
				addStage2("USER\n", m.Content, fmt.Sprintf("message-%d", i+1))
			} else if m.Role == "assistant" && i < len(msgs)-1 {
				addStage2("ASSISTANT\n", m.Content, fmt.Sprintf("message-%d", i+1))
				if len(m.ToolCalls) > 0 {
					if data, err := json.Marshal(m.ToolCalls); err == nil {
						addStage2("\nLOCAL TOOL REQUESTS\n", string(data), fmt.Sprintf("tool-req-%d", i+1))
					}
				}
			}
		}
		shorten(stage2)
	}
	if remaining > 0 {
		// Stage 3 (issue #58): the project context / client instructions block.
		// It is large (~25-80 KB with Claude Code) and is re-sent inline on a
		// helper-first rebind, after a Notion server-side compaction, or when it
		// changes; together with a big user message or <system-reminder> context
		// it can exceed the budget although the user text alone fits. Check both
		// raw system messages (used by create / rebind prompts) and normalized
		// systemMessages (used by updated-context follow-ups). Its full text goes
		// into an attachment (it stays in force); only when no attachment slot is
		// left is it shortened in the middle.
		var stage3 []cand
		sysCandidates := make([]ChatMessage, 0, len(msgs))
		for _, m := range msgs {
			if m.Role == "system" {
				sysCandidates = append(sysCandidates, m)
			}
		}
		sysCandidates = append(sysCandidates, systemMessages(msgs)...)
		for i, m := range sysCandidates {
			if len(m.Content) <= agentCtxMinKeep+len(contextTruncationMarker(len(m.Content), len(m.Content))) {
				continue
			}
			head := "PROJECT CONTEXT AND LOCAL CLIENT INSTRUCTIONS\n"
			block := head + m.Content
			if seen[block] {
				continue
			}
			seen[block] = true
			if n := strings.Count(out, block); n > 0 {
				stage3 = append(stage3, cand{block: block, head: head, body: m.Content, n: n, label: fmt.Sprintf("context-%d", i+1), ctx: true})
			}
		}
		shorten(stage3)
	}
	if remaining > 0 {
		// Stage 4: Claude Code injects large <system-reminder>...</system-reminder>
		// blocks (CLAUDE.md, skills index, SessionStart hook output) at the start
		// of user messages. Offload/shorten those blocks before failing with 413,
		// while leaving the user's own message text untouched.
		var stage4 []cand
		remIdx := 0
		for _, m := range msgs {
			if m.Role != "user" || !strings.Contains(m.Content, "<system-reminder>") {
				continue
			}
			for _, rem := range extractSystemReminders(m.Content) {
				remIdx++
				if len(rem) <= agentCtxMinKeep+len(contextTruncationMarker(len(rem), len(rem))) {
					continue
				}
				if seen[rem] {
					continue
				}
				seen[rem] = true
				if n := strings.Count(out, rem); n > 0 {
					stage4 = append(stage4, cand{block: rem, head: "", body: rem, n: n, label: fmt.Sprintf("reminder-%d", remIdx), ctx: true})
				}
			}
		}
		shorten(stage4)
	}
	if remaining > 0 && len(msgs) > 4 {
		// Stage 5 (#116): multi-turn historical deep truncation. Long sessions
		// (e.g. /compact or fresh-thread failover after many turns) can still
		// exceed the budget when dozens of historical blocks each retain 1-4 KB.
		// Keep len(msgs) <= 4 untouched so single-turn paste limits stay strict.
		curOf := func(orig string) string {
			if s, ok := replacedBody[orig]; ok {
				return s
			}
			return orig
		}
		for i := 0; i < len(msgs)-1 && remaining > 0; i++ {
			m := msgs[i]
			if m.Role != "user" || !strings.Contains(m.Content, "<system-reminder>") {
				continue
			}
			for _, rem := range extractSystemReminders(m.Content) {
				if remaining <= 0 {
					break
				}
				curRem := curOf(rem)
				if curRem == "" {
					continue
				}
				if n := strings.Count(out, curRem); n > 0 {
					out = strings.ReplaceAll(out, curRem, "")
					if curRem == rem {
						rep.Shortened += n
					}
					rep.Omitted += len(curRem) * n
					replacedBody[rem] = ""
					remaining = len(out) - limit
				}
			}
		}
		deepTruncate := func(head, origBody, curBody string) {
			if remaining <= 0 || len(origBody) <= agentDeepTruncMinKeep+len(deepTruncationMarker(len(origBody), len(origBody))) {
				return
			}
			curBlock := head + curBody
			n := strings.Count(out, curBlock)
			if n == 0 {
				return
			}
			per := (remaining + n - 1) / n
			maxOmit := len(origBody) - agentDeepTruncMinKeep
			targetSavedFromOrig := (len(origBody) - len(curBody)) + per
			omit := targetSavedFromOrig + len(deepTruncationMarker(len(origBody), len(origBody)))
			var short string
			for iter := 0; iter < 8; iter++ {
				if omit > maxOmit {
					omit = maxOmit
				}
				short, _ = truncateMiddleWith(origBody, omit, deepTruncationMarker)
				savedFromOrig := len(origBody) - len(short)
				if savedFromOrig >= targetSavedFromOrig || omit >= maxOmit {
					break
				}
				omit += targetSavedFromOrig - savedFromOrig
			}
			savedNow := (len(curBody) - len(short)) * n
			if savedNow <= 0 {
				return
			}
			out = strings.ReplaceAll(out, curBlock, head+short)
			if curBody == origBody {
				rep.Shortened += n
			}
			rep.Omitted += savedNow
			replacedBody[origBody] = short
			remaining = len(out) - limit
		}
		for i := 0; i < len(msgs)-1 && remaining > 0; i++ {
			m := msgs[i]
			if m.Role == "tool" {
				deepTruncate(toolResultBlockHeader(m), m.Content, curOf(m.Content))
			}
		}
		for i := 0; i < len(msgs)-1 && remaining > 0; i++ {
			m := msgs[i]
			if m.Role == "assistant" {
				deepTruncate("ASSISTANT\n", m.Content, curOf(m.Content))
				if len(m.ToolCalls) > 0 {
					if data, err := json.Marshal(m.ToolCalls); err == nil {
						s := string(data)
						deepTruncate("\nLOCAL TOOL REQUESTS\n", s, curOf(s))
					}
				}
			}
		}
		for i := 0; i < len(msgs)-1 && remaining > 0; i++ {
			m := msgs[i]
			if m.Role != "user" {
				continue
			}
			origUser := m.Content
			curUser := curOf(m.Content)
			if strings.Count(out, "USER\n"+curUser) == 0 && strings.Contains(m.Content, "<system-reminder>") {
				if stripped := stripSystemReminders(m.Content); stripped != "" && strings.Count(out, "USER\n"+stripped) > 0 {
					origUser = stripped
					curUser = stripped
				} else {
					rebuilt := m.Content
					for _, rem := range extractSystemReminders(m.Content) {
						rebuilt = strings.ReplaceAll(rebuilt, rem, curOf(rem))
					}
					if strings.Count(out, "USER\n"+rebuilt) > 0 {
						origUser = rebuilt
						curUser = rebuilt
					}
				}
			}
			deepTruncate("USER\n", origUser, curUser)
		}
		if remaining > 0 {
			sysCandidates := make([]ChatMessage, 0, len(msgs))
			for _, m := range msgs {
				if m.Role == "system" {
					sysCandidates = append(sysCandidates, m)
				}
			}
			sysCandidates = append(sysCandidates, systemMessages(msgs)...)
			for _, m := range sysCandidates {
				if remaining <= 0 {
					break
				}
				deepTruncate("PROJECT CONTEXT AND LOCAL CLIENT INSTRUCTIONS\n", m.Content, curOf(m.Content))
			}
		}
		if remaining > 0 {
			// Stage 6 (#120): when hundreds of already-truncated historical turns
			// still exceed the budget, drop the oldest intermediate message blocks
			// one by one while keeping the first non-system message and the final
			// message intact.
			firstNonSys := -1
			for i, m := range msgs {
				if m.Role != "system" {
					firstNonSys = i
					break
				}
			}
			for i := 0; i < len(msgs)-1 && remaining > 0; i++ {
				if i == firstNonSys || msgs[i].Role == "system" {
					continue
				}
				m := msgs[i]
				var block string
				switch m.Role {
				case "tool":
					block = toolResultBlockHeader(m) + curOf(m.Content) + "\n\n"
				case "assistant":
					block = "ASSISTANT\n" + curOf(m.Content)
					if len(m.ToolCalls) > 0 {
						if data, err := json.Marshal(m.ToolCalls); err == nil {
							s := string(data)
							block += "\nLOCAL TOOL REQUESTS\n" + curOf(s)
						}
					}
					block += "\n\n"
				case "user":
					curUser := curOf(m.Content)
					if strings.Index(out, "USER\n"+curUser+"\n\n") < 0 && strings.Contains(m.Content, "<system-reminder>") {
						if stripped := stripSystemReminders(m.Content); stripped != "" {
							curUser = curOf(stripped)
						}
					}
					block = "USER\n" + curUser + "\n\n"
				default:
					continue
				}
				if idx := strings.Index(out, block); idx >= 0 {
					out = out[:idx] + out[idx+len(block):]
					rep.Shortened++
					rep.Omitted += len(block)
					remaining = len(out) - limit
				}
			}
		}
	}
	rep.After = len(out)
	if len(out) > limit {
		files = nil
		return "", rep, nil, &agentBindingError{Kind: "too_large", Msg: fmt.Sprintf("the message this request would send to Notion is %d bytes (UTF-8) and its budget is %d bytes (Notion rejects larger messages, with HTTP 500 or 400). Shortening %d block(s) (tool results, subagent results, project context) to their minimum still leaves %d bytes: the rest is text that cannot be shortened (for example a very large pasted message). Nothing was sent, the thread was not changed and the binding is kept. Shorten or split the message and send it again; proxy.agent_follow_up_max_bytes / proxy.agent_create_max_bytes set the budgets", rep.Before, limit, rep.Shortened, rep.After)}
	}
	return out, rep, files, nil
}

// fitTurnPrompt applies the guard to the prompt a turn is about to send.
func fitTurnPrompt(turn *agentTurn, messages []ChatMessage) error {
	limit, kind := agentCreateMaxBytes(), "create"
	if turn.followUp {
		limit, kind = agentFollowUpMaxBytes(), "follow-up"
	}
	// A second attempt of the same turn sees the already fitted text; fit the
	// original again so the same attachments are rebuilt. Any other prompt is new.
	if turn.unfitPrompt == "" || (turn.prompt != turn.fittedPrompt && turn.prompt != turn.offloadFallback) {
		turn.unfitPrompt = turn.prompt
	}
	maxOffloads := agentOffloadMaxFiles(len(turn.attachments))
	if turn.sideReply != nil || isCompactSummarisationRequest(messages) {
		maxOffloads = 0
	}
	out, rep, files, err := fitAgentPromptOffload(turn.unfitPrompt, messages, limit, maxOffloads)
	if err != nil {
		log.Printf("[agent-thread] %s message of %d bytes exceeds the %d-byte budget and cannot be shortened enough; refused before sending (thread %s, binding kept)", kind, rep.Before, limit, shortID(turn.threadID))
		return err
	}
	if rep.Shortened > 0 {
		log.Printf("[agent-thread] %s message was %d bytes, budget %d: shortened %d block(s) (%d bytes not inline, %d attached in full as text files), now %d bytes (thread %s)", kind, rep.Before, limit, rep.Shortened, rep.Omitted, rep.Offloaded, rep.After, shortID(turn.threadID))
	}
	turn.prompt = out
	turn.fittedPrompt = out
	turn.offloads = files
	turn.offloadFallback = ""
	if len(files) > 0 {
		// Used when the attachments cannot be uploaded: same budget, plain
		// truncation (cannot fail, the offload markers are longer).
		turn.offloadFallback, _, _ = fitAgentPrompt(turn.unfitPrompt, messages, limit)
	}
	return nil
}

func extractSystemReminders(s string) []string {
	const (
		openTag  = "<system-reminder>"
		closeTag = "</system-reminder>"
	)
	var out []string
	for {
		start := strings.Index(s, openTag)
		if start < 0 {
			break
		}
		end := strings.Index(s[start:], closeTag)
		if end < 0 {
			break
		}
		end += start + len(closeTag)
		out = append(out, s[start:end])
		s = s[end:]
	}
	return out
}

