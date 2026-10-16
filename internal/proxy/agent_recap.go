package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// Exact instruction observed in Claude Code 2.1.280's away-summary request.
// Tool availability is not a request identity: ordinary tool-less follow-ups
// must still advance the main conversation. A quoted or edited instruction
// therefore stays an ordinary request rather than being classified by a prefix.
const agentAwayRecapInstruction = "The user stepped away and is coming back. Recap in under 40 words, 1-2 plain sentences, no markdown. Lead with the overall goal and current task, then the one next action. Skip root-cause narrative, fix internals, secondary to-dos, and em-dash tangents."

// Exact instruction emitted by Claude Code 2.1.280's 30s background subagent
// progress timer (querySource: "agent_summary"). Unlike away-summary, it fires
// every 30s with the subagent's own x-claude-code-agent-id and forked history
// (same RootHash), so if it is not refused before acquiring the thread lock it
// blocks on the subagent's turn and can hijack the subagent's binding on
// follow-up, unaccepted-turn supersede, or interrupted-edit checks (issue #61).
const (
	agentProgressSummaryHead       = "Describe your most recent action in 3-5 words using present tense (-ing). Name the file or function, not the branch. Do not use tools.\n"
	agentProgressSummaryTail       = "\nGood: \"Reading runAgent.ts\"\nGood: \"Fixing null check in validate.ts\"\nGood: \"Running auth module tests\"\nGood: \"Adding retry logic to fetchUser\"\n\nBad (past tense): \"Analyzed the branch diff\"\nBad (too vague): \"Investigating the issue\"\nBad (too long): \"Reviewing full branch diff and AgentTool.tsx integration\"\nBad (branch name): \"Analyzed adam/background-summary branch diff\""
	agentProgressSummaryPrevPrefix = "\nPrevious: "
	agentProgressSummaryPrevSuffix = " \u2014 say something NEW.\n"
)

func matchAgentProgressSummaryText(text string) bool {
	if !strings.HasPrefix(text, agentProgressSummaryHead) || !strings.HasSuffix(text, agentProgressSummaryTail) {
		return false
	}
	if len(text) < len(agentProgressSummaryHead)+len(agentProgressSummaryTail) {
		return false
	}
	mid := text[len(agentProgressSummaryHead) : len(text)-len(agentProgressSummaryTail)]
	if mid == "" {
		return true
	}
	if !strings.HasPrefix(mid, agentProgressSummaryPrevPrefix) || !strings.HasSuffix(mid, agentProgressSummaryPrevSuffix) {
		return false
	}
	prevJSON := mid[len(agentProgressSummaryPrevPrefix) : len(mid)-len(agentProgressSummaryPrevSuffix)]
	var prev string
	if json.Unmarshal([]byte(prevJSON), &prev) != nil || prev == "" || len(prev) > 400 || strings.ContainsAny(prev, "\r\n") {
		return false
	}
	return true
}

func isAgentProgressSummary(m ChatMessage) bool {
	if m.Role != "user" {
		return false
	}
	if matchAgentProgressSummaryText(m.Content) {
		return true
	}
	if !m.StringContent && m.LastTextBlock != "" && strings.HasSuffix(m.Content, m.LastTextBlock) {
		return matchAgentProgressSummaryText(m.LastTextBlock)
	}
	return false
}

// awayRecapClientKey isolates the auxiliary inference from its parent's binding.
// Each input snapshot has its own persistent key, so retries (including after a
// restart) reuse its result, while a later recap of different history cannot
// replay an old summary. Assistant text matters here even though it is ignored
// by the main conversation's acknowledgement matcher. Only digests are stored.
func awayRecapClientKey(in agentPlanInput) (string, bool) {
	if in.ClientKey == "" {
		return "", false
	}
	ns := nonSystemMessages(in.Messages)
	if len(ns) == 0 {
		return "", false
	}
	last := ns[len(ns)-1]
	if last.Role != "user" || last.Content != agentAwayRecapInstruction {
		return "", false
	}
	messages := append([]ChatMessage(nil), in.Messages...)
	for i := range messages {
		if messages[i].Role == "system" {
			messages[i].Content = normalizeSessionSystemContent(messages[i].Content)
		}
	}
	data, err := json.Marshal(struct {
		Parent      string
		Messages    []ChatMessage
		Attachments int
	}{in.ClientKey, messages, len(in.Attachments)})
	if err != nil {
		return "", false
	}
	sum := sha256.Sum256(append([]byte("agent-away-recap-v1:"), data...))
	return "away-recap:" + hex.EncodeToString(sum[:]), true
}
