package proxy

import (
	"encoding/json"
	"log"
	"unicode/utf8"
)

// Token usage for the Agent-thread transport.
//
// The Notion Agent transcript carries no token counts and the proxy never reads
// any (callAgentThreadResumable reports usage=nil), so Anthropic responses used to
// say input_tokens=0/output_tokens=0. Claude Code decides on auto-compaction from
// those numbers, so a long session would grow without bound. When the upstream
// gives no reading the proxy therefore reports an ESTIMATE:
//
//	input_tokens  = estimate of the whole client request (system + tools +
//	                every message incl. tool_use/tool_result/thinking blocks)
//	output_tokens = estimate of the text, thinking and tool request returned
//
// Rule (counted in quarter-token units, rounded up once at the end):
//
//	ASCII byte                      = 1 unit  (4 chars per token)
//	other rune (CJK, accents, ...)  = 6 units (1.5 tokens per char, conservative)
//	rune >= U+10000 (emoji, ...)    = 8 units (2 tokens per char)
//	base64 payload (images/files)   = imageTokenEstimate, never its length
//	each message                    = messageOverheadTokens
//
// It deliberately leans high so compaction triggers a little early rather than
// late. These are estimates, not upstream readings; logs mark them "estimated".
const (
	imageTokenEstimate    = 1500
	messageOverheadTokens = 4
)

func textUnits(s string) int {
	units := 0
	for _, r := range s {
		switch {
		case r < utf8.RuneSelf:
			units++
		case r >= 0x10000:
			units += 8
		default:
			units += 6
		}
	}
	return units
}

func unitsToTokens(units int) int { return (units + 3) / 4 }

// estimateTextTokens estimates the tokens of plain text (see the rule above).
func estimateTextTokens(s string) int { return unitsToTokens(textUnits(s)) }

// valueUnits walks a decoded JSON value (strings, arrays, objects, scalars).
func valueUnits(v interface{}) int {
	switch t := v.(type) {
	case nil:
		return 0
	case string:
		return textUnits(t)
	case float64, bool:
		return 4
	case []interface{}:
		n := 0
		for _, e := range t {
			n += valueUnits(e)
		}
		return n
	case map[string]interface{}:
		n := 0
		_, isBase64 := t["data"].(string)
		isBase64 = isBase64 && t["type"] == "base64"
		for k, e := range t {
			if isBase64 && k == "data" {
				n += imageTokenEstimate * 4
				continue
			}
			n += textUnits(k) + valueUnits(e)
		}
		return n
	default:
		raw, err := json.Marshal(t)
		if err != nil {
			return 0
		}
		var generic interface{}
		if json.Unmarshal(raw, &generic) != nil {
			return 0
		}
		return valueUnits(generic)
	}
}

// estimateAnthropicRequestTokens estimates the full client request.
func estimateAnthropicRequestTokens(req *AnthropicRequest) int {
	if req == nil {
		return 0
	}
	units := valueUnits(req.System)
	for _, m := range req.Messages {
		units += messageOverheadTokens*4 + textUnits(m.Role) + valueUnits(m.Content)
	}
	for _, tool := range req.Tools {
		units += valueUnits(tool)
	}
	return unitsToTokens(units)
}

// estimateChatMessagesTokens is the fallback when only the converted messages are
// at hand (no tool definitions): system and history incl. tool calls/results.
func estimateChatMessagesTokens(msgs []ChatMessage) int {
	units := 0
	for _, m := range msgs {
		units += messageOverheadTokens*4 + textUnits(m.Role) + textUnits(m.Content) + textUnits(m.Name)
		for _, tc := range m.ToolCalls {
			units += textUnits(tc.Function.Name) + textUnits(tc.Function.Arguments)
		}
	}
	return unitsToTokens(units)
}

// requestInputEstimate: the request-level estimate recorded on the session, else
// the converted messages.
func requestInputEstimate(session *Session, messages []ChatMessage) int {
	if session != nil && session.InputTokenEstimate > 0 {
		return session.InputTokenEstimate
	}
	return estimateChatMessagesTokens(messages)
}

// resolveAnthropicUsage returns the numbers for the Anthropic usage block: the
// upstream's own counts when it reported any, else the estimate. A non-empty
// requestID logs which one was used.
func resolveAnthropicUsage(real *UsageInfo, session *Session, messages []ChatMessage, outputText string, requestID string) (in, out int) {
	if real != nil && (real.PromptTokens > 0 || real.CompletionTokens > 0) {
		return real.PromptTokens, real.CompletionTokens
	}
	in, out = requestInputEstimate(session, messages), estimateTextTokens(outputText)
	if requestID != "" {
		log.Printf("[usage] %s estimated (upstream reported no token counts): input_tokens=%d output_tokens=%d", requestID, in, out)
	}
	return in, out
}

func thinkingBlocksText(blocks []ThinkingBlock) string {
	n := 0
	for _, b := range blocks {
		n += len(b.Content)
	}
	if n == 0 {
		return ""
	}
	buf := make([]byte, 0, n)
	for _, b := range blocks {
		buf = append(buf, b.Content...)
	}
	return string(buf)
}
