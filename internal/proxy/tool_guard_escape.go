package proxy

import (
	"regexp"
	"strings"
)

var (
	// Models hosted behind Notion Agent (observed with Sonnet 5.5 on thread
	// 5df43d65, seq=78 in issue #61) occasionally start a tool call in the bridged
	// JSON format (`{"name":"Grep","arguments":{...}`) and finish it with Anthropic
	// XML closing tags (`</parameter>\n</invoke>`) instead of the outer `}`.
	trailingXMLToolCloseRegex   = regexp.MustCompile(`(?is)(?:\s*</(?:antml:)?(?:parameter|invoke|tool_call|function_calls|function_call|parameters|arguments)>)+\s*$`)
	standaloneXMLToolCloseRegex = regexp.MustCompile(`(?is)^(?:\s*</(?:antml:)?(?:parameter|invoke|tool_call|function_calls|function_call|parameters|arguments)>)+\s*$`)
)

func isStandaloneXMLToolClose(s string) bool {
	return standaloneXMLToolCloseRegex.MatchString(s)
}

// repairTrailingXMLToolClose strips trailing XML tool-call closing tags from a
// JSON tool-request object and closes up to two still-open outer braces when the
// last JSON value before the tag is complete. Genuinely truncated strings, arrays
// or unfinished key/colon/comma tokens are left untouched (ok=false) so they still
// fail loudly as truncated_or_undecodable.
func repairTrailingXMLToolClose(s string) (string, bool) {
	loc := trailingXMLToolCloseRegex.FindStringIndex(s)
	if loc == nil {
		return s, false
	}
	base := strings.TrimRight(s[:loc[0]], " \t\r\n")
	trimmedLeft := strings.TrimLeft(base, " \t\r\n")
	if !strings.HasPrefix(trimmedLeft, "{") {
		return s, false
	}
	inString := false
	escaped := false
	openBraces := 0
	openBrackets := 0
	for i := 0; i < len(base); i++ {
		c := base[i]
		if inString {
			if escaped {
				escaped = false
				continue
			}
			if c == '\\' {
				escaped = true
				continue
			}
			if c == '"' {
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{':
			openBraces++
		case '}':
			openBraces--
			if openBraces < 0 {
				return s, false
			}
		case '[':
			openBrackets++
		case ']':
			openBrackets--
			if openBrackets < 0 {
				return s, false
			}
		}
	}
	if inString || openBrackets != 0 || openBraces < 0 || openBraces > 2 {
		return s, false
	}
	if openBraces > 0 {
		last := base[len(base)-1]
		switch {
		case last == '"' || last == '}' || last == ']' || last == 'e' || last == 'l' || (last >= '0' && last <= '9'):
		default:
			return s, false
		}
	}
	fixed := base + strings.Repeat("}", openBraces)
	if fixed == s {
		return s, false
	}
	return fixed, true
}

// escapeInvalidJSONEscapes repairs backslashes inside JSON strings that do not
// start a valid JSON escape (issue #61). Models writing regexes, Windows paths
// or LaTeX into a Write/Edit argument often emit `\d`, `\s`, `\|`, `\(` or
// `C:\work` with a single backslash; JSON rejects those, so the whole local tool
// request was refused as truncated_or_undecodable. The only reading that keeps
// the text is a literal backslash, so it is doubled. Valid escapes
// (\" \\ \/ \b \f \n \r \t \uXXXX) are kept as they are; text outside strings
// is not touched. ok=false when nothing had to change.
func escapeInvalidJSONEscapes(s string) (string, bool) {
	var b strings.Builder
	inString := false
	changed := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !inString {
			if c == '"' {
				inString = true
			}
			b.WriteByte(c)
			continue
		}
		switch c {
		case '"':
			inString = false
			b.WriteByte(c)
		case '\\':
			if i+1 < len(s) {
				switch n := s[i+1]; n {
				case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
					b.WriteByte(c)
					b.WriteByte(n)
					i++
					continue
				case 'u':
					if i+5 < len(s) && isHex4(s[i+2:i+6]) {
						b.WriteString(s[i : i+6])
						i += 5
						continue
					}
				}
			}
			b.WriteString(`\\`)
			changed = true
		default:
			b.WriteByte(c)
		}
	}
	if !changed {
		return s, false
	}
	return b.String(), true
}

func isHex4(s string) bool {
	if len(s) != 4 {
		return false
	}
	for i := 0; i < 4; i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// repairJSONKeyAngleColon fixes a JSON object key whose separator was emitted
// as '>' instead of ':' outside string literals (e.g. `{"command">"cd /tmp"}`,
// observed with Sonnet 5.5 on thread 9ab0498d, seq=108 in #85). Characters
// inside JSON string literals are never altered.
func repairJSONKeyAngleColon(s string) (string, bool) {
	var b strings.Builder
	inString := false
	escaped := false
	justClosedString := false
	changed := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inString {
			if escaped {
				escaped = false
				b.WriteByte(c)
				continue
			}
			if c == '\\' {
				escaped = true
				b.WriteByte(c)
				continue
			}
			if c == '"' {
				inString = false
				justClosedString = true
			}
			b.WriteByte(c)
			continue
		}
		switch c {
		case '"':
			inString = true
			justClosedString = false
			b.WriteByte(c)
		case ' ', '\t', '\r', '\n':
			b.WriteByte(c)
		case '>':
			if justClosedString {
				b.WriteByte(':')
				justClosedString = false
				changed = true
			} else {
				b.WriteByte(c)
			}
		default:
			justClosedString = false
			b.WriteByte(c)
		}
	}
	if !changed {
		return s, false
	}
	return b.String(), true
}

