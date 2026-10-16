package proxy

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

// Credential- and content-free diagnostics for Agent submit failures (issue #28).
//
// sendEventToAgentThread/createAgentThread answered HTTP 400 (UserValidationError)
// or repeated 500 for some requests while others on the same thread worked, and
// the existing error carried only status+code. The shape below records what the
// REQUEST looked like (sizes and counts of risky characters, never text) and
// which top-level KEYS the error body had (never values), so the next live
// occurrence can be compared against a passing request without copying private
// content into logs or issues. It does not claim a cause.

type agentRequestShape struct {
	Endpoint     string
	BodyBytes    int
	PromptBytes  int
	PromptRunes  int
	PromptLines  int
	MaxLineBytes int
	ToolResults  int // "LOCAL TOOL RESULT (" sections
	ToolRequests int // "LOCAL TOOL REQUESTS" sections
	NulChars     int
	CtrlChars    int // C0 controls other than \t \n \r
	BadUTF8      int // invalid UTF-8 still present in the marshalled prompt (json.Marshal normally rewrites it)
	Replacement  int // U+FFFD: what json.Marshal turns invalid UTF-8 into before sending
	NonBMP       int
	RichMarker   int // U+2023, the marker Notion rich text uses for mentions
	Files        int // file attachment blocks
	FollowUp     bool
	Model        string
	Effort       string
}

func (s agentRequestShape) String() string {
	return fmt.Sprintf("endpoint=%s body=%dB prompt=%dB/%drunes lines=%d maxline=%dB tool_results=%d tool_requests=%d nul=%d ctrl=%d bad_utf8=%d replacement=%d non_bmp=%d rich_marker=%d files=%d follow_up=%t model=%s effort=%s",
		s.Endpoint, s.BodyBytes, s.PromptBytes, s.PromptRunes, s.PromptLines, s.MaxLineBytes, s.ToolResults, s.ToolRequests, s.NulChars, s.CtrlChars, s.BadUTF8, s.Replacement, s.NonBMP, s.RichMarker, s.Files, s.FollowUp, shapeIdent(s.Model), shapeIdent(s.Effort))
}

func shapeIdent(s string) string {
	if s == "" {
		return "-"
	}
	if agentErrorCodePattern.MatchString(s) {
		return s
	}
	return "?"
}

// describeAgentRequestShape summarises a marshalled request body.
func describeAgentRequestShape(endpoint string, data []byte) agentRequestShape {
	shape := agentRequestShape{Endpoint: shapeIdent(endpoint), BodyBytes: len(data)}
	var body map[string]interface{}
	if json.Unmarshal(data, &body) != nil {
		return shape
	}
	shape.Model, _ = body["model"].(string)
	shape.Effort, _ = body["reasoningEffort"].(string)
	content, _ := body["content"].([]interface{})
	if event, ok := body["event"].(map[string]interface{}); ok {
		shape.FollowUp = true
		content, _ = event["content"].([]interface{})
	}
	var prompt strings.Builder
	for _, item := range content {
		block, _ := item.(map[string]interface{})
		switch block["type"] {
		case "file":
			shape.Files++
		case "text":
			rows, _ := block["text"].([]interface{})
			for _, row := range rows {
				cells, _ := row.([]interface{})
				for _, cell := range cells {
					if s, ok := cell.(string); ok {
						prompt.WriteString(s)
					}
				}
			}
		}
	}
	text := prompt.String()
	shape.PromptBytes = len(text)
	shape.PromptRunes = utf8.RuneCountInString(text)
	shape.ToolResults = strings.Count(text, "LOCAL TOOL RESULT (")
	shape.ToolRequests = strings.Count(text, "LOCAL TOOL REQUESTS")
	if text != "" {
		shape.PromptLines = strings.Count(text, "\n") + 1
	}
	for _, line := range strings.Split(text, "\n") {
		if len(line) > shape.MaxLineBytes {
			shape.MaxLineBytes = len(line)
		}
	}
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			shape.BadUTF8++
		case r == '\uFFFD':
			shape.Replacement++
		case r == 0:
			shape.NulChars++
		case r < 0x20 && r != '\t' && r != '\n' && r != '\r':
			shape.CtrlChars++
		case r > 0xFFFF:
			shape.NonBMP++
		case r == '\u2023':
			shape.RichMarker++
		}
		i += size
	}
	return shape
}

// agentErrorBodyKeys returns the sorted identifier-like top-level (and one level
// nested "error") keys of an upstream error body. Values are never read.
func agentErrorBodyKeys(data []byte) string {
	var body map[string]interface{}
	if json.Unmarshal(data, &body) != nil {
		return ""
	}
	var keys []string
	add := func(prefix string, m map[string]interface{}) {
		for k := range m {
			if agentErrorCodePattern.MatchString(k) {
				keys = append(keys, prefix+k)
			}
		}
	}
	add("", body)
	if inner, ok := body["error"].(map[string]interface{}); ok {
		add("error.", inner)
	}
	sort.Strings(keys)
	if len(keys) > 12 {
		keys = keys[:12]
	}
	return strings.Join(keys, ",")
}
