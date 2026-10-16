package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
)

// Strict classification of Notion Agent replies in tool mode (issue #28).
//
// In Agent-thread mode the model answers a tool request as plain text (a JSON
// object in an assistant message). Three outcomes must stay distinct:
//
//   - a valid request for a DECLARED tool        -> real tool_use block;
//   - text that is clearly a tool request but cannot be validated (truncated,
//     undecodable, unknown tool, wrong arguments) -> explicit error, never the
//     final answer and never executed;
//   - everything else (prose, data JSON, quoted examples) -> ordinary answer.
//
// Over-escaped / HTML-entity-escaped payloads are decoded only when the
// structural `"name":` key itself is escaped, so the whole text was escaped
// uniformly; a payload that already parses is never altered.

// ErrToolBridgeMalformed marks a reply that looks like a local tool request but
// failed validation. It is definitive: the turn is not resent, the thread is not
// forked, and the text is not delivered as an answer.
var ErrToolBridgeMalformed = errors.New("tool bridge reply looked like a tool request but failed validation")

type toolBridgeMalformedError struct {
	Reason string // truncated_or_undecodable | unknown_tool | arguments_not_object | missing_required | ...
	Detail string // identifier-like detail only (tool name / missing field), never reply text
}

func (e *toolBridgeMalformedError) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("%s: reason=%s (%s)", ErrToolBridgeMalformed, e.Reason, e.Detail)
	}
	return fmt.Sprintf("%s: reason=%s", ErrToolBridgeMalformed, e.Reason)
}
func (e *toolBridgeMalformedError) Is(target error) bool { return target == ErrToolBridgeMalformed }

// toolSpec is what the bridge needs to validate a call: required argument names.
type toolSpec struct{ Required []string }

// declaredTools are the tools the client declared for this request.
type declaredTools map[string]toolSpec

func buildDeclaredTools(tools []AnthropicTool) declaredTools {
	declared := declaredTools{}
	for _, t := range tools {
		if t.Name == "" || t.Name == "DeferredToolPlaceholder" {
			continue
		}
		spec := toolSpec{}
		if schema, ok := t.InputSchema.(map[string]interface{}); ok {
			if req, ok := schema["required"].([]interface{}); ok {
				for _, r := range req {
					if s, ok := r.(string); ok && s != "" {
						spec.Required = append(spec.Required, s)
					}
				}
			}
		}
		declared[t.Name] = spec
	}
	if len(declared) == 0 {
		return nil
	}
	// Handled inside the bridge itself (see prepareToolBridgeResponse).
	for _, name := range []string{"__done__", "WebSearch", "WebFetch"} {
		if _, ok := declared[name]; !ok {
			declared[name] = toolSpec{}
		}
	}
	return declared
}

func setSessionTools(s *Session, tools declaredTools) {
	if s != nil {
		s.Tools = tools
	}
}

type toolTextKind int

const (
	toolTextPlain toolTextKind = iota
	toolTextCalls
	toolTextMalformed
)

type toolTextVerdict struct {
	Kind      toolTextKind
	Calls     []ToolCall
	Remaining string
	// Preamble is prose that precedes a trailing, validated tool request; it is
	// delivered as a text block in front of the tool_use, never dropped.
	Preamble  string
	Malformed *toolBridgeMalformedError
	Repaired  string // "" or the decoding steps that were needed (html, escape, string)
}

var (
	// A structural JSON key, possibly escaped: "name": \"name\": &quot;name&quot;:
	quoteAlt        = `\\*(?:"|&quot;|&#0*34;|&#x0*22;)`
	nameKeyRegex    = regexp.MustCompile(quoteAlt + `name` + quoteAlt + `\s*:`)
	argsKeyRegex    = regexp.MustCompile(quoteAlt + `arguments` + quoteAlt + `\s*:`)
	wrapperKeyRegex = regexp.MustCompile(quoteAlt + `tool_call` + quoteAlt + `\s*:`)
	argsFirstRegex  = regexp.MustCompile(`^\s*(?:\\*")?\{\s*` + quoteAlt + `arguments` + quoteAlt + `\s*:`)
	nameValueRegex  = regexp.MustCompile(quoteAlt + `name` + quoteAlt + `\s*:\s*` + quoteAlt + `([A-Za-z0-9_.:-]{1,64})` + quoteAlt)
	htmlEntityRegex = regexp.MustCompile(`&(?:quot|amp|lt|gt|apos|#0*3[49]|#x0*2[27]);`)
	openTagRegex    = regexp.MustCompile(`(?s)^(?:<|\\<|&lt;|\\&lt;)tool_call(?:>|\\>|&gt;|\\&gt;)(.*?)(?:(?:<|\\<|&lt;|\\&lt;)/tool_call(?:>|\\>|&gt;|\\&gt;))?$`)
	fenceWholeRegex = regexp.MustCompile("(?s)^```(?:json|tool_call)?[ \\t]*\\n?(.*?)(?:\\n?```)?$")
	htmlUnescaper   = strings.NewReplacer("&quot;", `"`, "&#34;", `"`, "&#034;", `"`, "&#x22;", `"`, "&apos;", "'", "&#39;", "'", "&#x27;", "'", "&lt;", "<", "&gt;", ">", "&amp;", "&")
	escapeUndoer    = strings.NewReplacer(`\"`, `"`, `\\`, `\`, `\n`, "\n", `\t`, "\t", `\r`, "\r", `\/`, "/")
)

// guardToolText classifies content. declared may be nil (name check skipped).
func guardToolText(content string, declared declaredTools) toolTextVerdict {
	text := strings.TrimSpace(content)
	if text == "" {
		return toolTextVerdict{Kind: toolTextPlain, Remaining: content}
	}
	payload := text
	if m := openTagRegex.FindStringSubmatch(text); m != nil {
		payload = strings.TrimSpace(m[1])
	} else if m := fenceWholeRegex.FindStringSubmatch(text); m != nil {
		payload = strings.TrimSpace(m[1])
	}

	if startsLikeObject(payload) {
		if v, ok := guardWholePayload(payload, declared); ok {
			return v
		}
	}
	return guardEmbedded(content, declared)
}

func startsLikeObject(s string) bool {
	return strings.HasPrefix(s, "{") || strings.HasPrefix(s, `"{`) || strings.HasPrefix(s, `\"{`)
}

// shapedText: does the (possibly escaped) text carry the tool-request keys?
func shapedText(s string, declared declaredTools) bool {
	if argsFirstRegex.MatchString(s) { // {"arguments":{...  (key order is not guaranteed; may be cut before "name")
		return true
	}
	if wrapperKeyRegex.MatchString(s) && nameKeyRegex.MatchString(s) {
		return true
	}
	if !nameKeyRegex.MatchString(s) {
		return false
	}
	if argsKeyRegex.MatchString(s) {
		return true
	}
	if m := nameValueRegex.FindStringSubmatch(s); m != nil && declared != nil {
		_, ok := declared[m[1]]
		return ok
	}
	return false
}

// guardWholePayload handles a reply that is (or is meant to be) only tool
// request(s). ok=false means "not a tool request at all".
func guardWholePayload(payload string, declared declaredTools) (toolTextVerdict, bool) {
	// One object (possibly multi-line pretty printed), else one object per line.
	if calls, repaired, bad, shaped := decodeCalls(payload, declared); len(calls) > 0 && bad == nil {
		return toolTextVerdict{Kind: toolTextCalls, Calls: numberCalls(calls), Repaired: repaired}, true
	} else if bad != nil {
		return toolTextVerdict{Kind: toolTextMalformed, Malformed: bad}, true
	} else if !shaped && !strings.Contains(payload, "\n") {
		return toolTextVerdict{}, false
	}
	var all []ToolCall
	repairedAny := ""
	lines := strings.Split(payload, "\n")
	nonEmpty := 0
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		nonEmpty++
		if !startsLikeObject(line) {
			if len(all) > 0 && isStandaloneXMLToolClose(line) {
				continue
			}
			// Not a pure list of requests: let the embedded rules decide.
			if len(all) > 0 {
				return toolTextVerdict{}, false
			}
			if shapedText(payload, declared) && startsLikeObject(payload) && !jsonParses(payload) {
				return toolTextVerdict{Kind: toolTextMalformed, Malformed: &toolBridgeMalformedError{Reason: "truncated_or_undecodable"}}, true
			}
			return toolTextVerdict{}, false
		}
		calls, repaired, bad, shaped := decodeCalls(line, declared)
		if bad != nil {
			return toolTextVerdict{Kind: toolTextMalformed, Malformed: bad}, true
		}
		if len(calls) == 0 {
			if shaped || (!jsonParses(line) && shapedText(payload, declared) && !jsonParses(payload)) {
				return toolTextVerdict{Kind: toolTextMalformed, Malformed: &toolBridgeMalformedError{Reason: "truncated_or_undecodable"}}, true
			}
			return toolTextVerdict{}, false
		}
		all = append(all, calls...)
		if repaired != "" {
			repairedAny = repaired
		}
	}
	if nonEmpty == 0 || len(all) == 0 {
		return toolTextVerdict{}, false
	}
	return toolTextVerdict{Kind: toolTextCalls, Calls: numberCalls(all), Repaired: repairedAny}, true
}

func jsonParses(s string) bool { return json.Valid([]byte(s)) }

// escapeJSONStringControls escapes raw ASCII control bytes (< 0x20, such as
// literal \t or \n emitted inside Edit old_string/new_string) only when they
// occur inside a JSON string literal, leaving whitespace between tokens intact.
func escapeJSONStringControls(s string) (string, bool) {
	var b strings.Builder
	inString := false
	escaped := false
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
			b.WriteByte(c)
			continue
		}
		if c < 0x20 {
			if !changed {
				changed = true
			}
			switch c {
			case '\t':
				b.WriteString(`\t`)
			case '\n':
				b.WriteString(`\n`)
			case '\r':
				b.WriteString(`\r`)
			case '\b':
				b.WriteString(`\b`)
			case '\f':
				b.WriteString(`\f`)
			default:
				fmt.Fprintf(&b, `\u%04x`, c)
			}
			continue
		}
		b.WriteByte(c)
	}
	if !changed {
		return s, false
	}
	return b.String(), true
}

func undoEscape(s string) string {
	u := strings.TrimSpace(escapeUndoer.Replace(s))
	if fixed, ok := escapeJSONStringControls(u); ok {
		return fixed
	}
	return u
}

func numberCalls(calls []ToolCall) []ToolCall {
	for i := range calls {
		calls[i].ID = fmt.Sprintf("call_%d_%s", i, generateUUIDv4()[:8])
	}
	return calls
}

// decodeCalls tries the raw text and then bounded decodings (never more than
// html/escape/string/ctrl layers, depth 3). It returns validated calls, or a
// malformed verdict when a decoding produced a tool-shaped object that failed
// validation, or shaped=true when the text carries tool keys but nothing parsed.
func decodeCalls(s string, declared declaredTools) (calls []ToolCall, repaired string, bad *toolBridgeMalformedError, shaped bool) {
	shaped = shapedText(s, declared)
	type variant struct {
		text  string
		steps string
		depth int
	}
	queue := []variant{{s, "", 0}}
	seen := map[string]bool{s: true}
	var firstBad *toolBridgeMalformedError
	for len(queue) > 0 {
		v := queue[0]
		queue = queue[1:]
		if call, b, isCall := parseOneCall(v.text, declared); isCall {
			if b == nil {
				return []ToolCall{*call}, v.steps, nil, shaped
			}
			if firstBad == nil {
				firstBad = b
			}
			continue
		}
		if v.depth >= 3 { // bounded: at most three decoding layers
			continue
		}
		next := func(t, step string) {
			if t != v.text && !seen[t] {
				seen[t] = true
				queue = append(queue, variant{t, strings.TrimPrefix(v.steps+","+step, ","), v.depth + 1})
			}
		}
		if fixed, ok := escapeJSONStringControls(v.text); ok && nameKeyRegex.MatchString(v.text) {
			next(fixed, "ctrl")
		}
		if fixed, ok := escapeInvalidJSONEscapes(v.text); ok && nameKeyRegex.MatchString(v.text) {
			next(fixed, "backslash")
		}
		if fixed, ok := repairTrailingXMLToolClose(v.text); ok && nameKeyRegex.MatchString(v.text) {
			next(fixed, "xml_close")
		}
		if fixed, ok := repairJSONKeyAngleColon(v.text); ok && nameKeyRegex.MatchString(v.text) {
			next(fixed, "key_colon")
		}
		if strings.HasPrefix(v.text, `"`) && strings.HasSuffix(v.text, `"`) {
			var inner string
			if json.Unmarshal([]byte(v.text), &inner) == nil {
				next(strings.TrimSpace(inner), "string")
			}
		}
		if htmlEntityRegex.MatchString(v.text) && nameKeyRegex.MatchString(v.text) {
			next(htmlUnescaper.Replace(v.text), "html")
		}
		if strings.Contains(v.text, `\"`) && nameKeyRegex.MatchString(v.text) {
			next(undoEscape(v.text), "escape")
		}
	}
	if firstBad != nil {
		return nil, "", firstBad, shaped
	}
	// Several objects on separate lines are handled by the caller.
	return nil, "", nil, shaped
}

// parseOneCall: isCall=false when text is not a JSON tool-request object.
func parseOneCall(text string, declared declaredTools) (*ToolCall, *toolBridgeMalformedError, bool) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &obj); err != nil {
		return nil, nil, false
	}
	if wrapped, ok := obj["tool_call"]; ok {
		var inner map[string]json.RawMessage
		if json.Unmarshal(wrapped, &inner) != nil {
			return nil, &toolBridgeMalformedError{Reason: "tool_call_not_object"}, true
		}
		obj = inner
	}
	var name string
	if raw, ok := obj["name"]; !ok || json.Unmarshal(raw, &name) != nil || name == "" {
		return nil, nil, false
	}
	argsRaw, hasArgs := obj["arguments"]
	_, declaredName := declared[name]
	if !hasArgs && declaredName {
		if inRaw, ok := obj["input"]; ok {
			argsRaw, hasArgs = inRaw, true
		} else if flat, ok := extractFlatToolArguments(obj); ok {
			argsRaw, hasArgs = flat, true
		}
	}
	if !hasArgs && !declaredName {
		return nil, nil, false // ordinary data object that happens to have a "name"
	}
	call, bad := validateCall(name, argsRaw, hasArgs, declared)
	return call, bad, true
}

// extractFlatToolArguments collects top-level fields when the model emitted a
// declared tool's parameters directly alongside "name" instead of wrapping them
// in "arguments" (observed with Sonnet 5.5 on thread 31c4699d, seq=48/54 in #85).
func extractFlatToolArguments(obj map[string]json.RawMessage) (json.RawMessage, bool) {
	flat := make(map[string]json.RawMessage, len(obj))
	for k, v := range obj {
		switch k {
		case "name", "type", "id", "tool_call_id":
			continue
		default:
			flat[k] = v
		}
	}
	if len(flat) == 0 {
		return nil, false
	}
	raw, err := json.Marshal(flat)
	if err != nil {
		return nil, false
	}
	return raw, true
}

func validateCall(name string, argsRaw json.RawMessage, hasArgs bool, declared declaredTools) (*ToolCall, *toolBridgeMalformedError) {
	spec, known := declared[name]
	if declared != nil && !known {
		return nil, &toolBridgeMalformedError{Reason: "unknown_tool", Detail: safeIdent(name)}
	}
	var args map[string]json.RawMessage
	if hasArgs {
		trimmed := strings.TrimSpace(string(argsRaw))
		if strings.HasPrefix(trimmed, `"`) { // arguments double-encoded as a JSON string
			var inner string
			if json.Unmarshal(argsRaw, &inner) == nil {
				argsRaw = json.RawMessage(strings.TrimSpace(inner))
			}
		}
		if err := json.Unmarshal(argsRaw, &args); err != nil || args == nil {
			if fixed, ok := escapeJSONStringControls(string(argsRaw)); ok {
				_ = json.Unmarshal([]byte(fixed), &args)
			}
		}
		if args == nil {
			if fixed, ok := escapeInvalidJSONEscapes(string(argsRaw)); ok {
				if again, ok2 := escapeJSONStringControls(fixed); ok2 {
					fixed = again
				}
				_ = json.Unmarshal([]byte(fixed), &args)
			}
		}
		if args == nil {
			if fixed, ok := repairJSONKeyAngleColon(string(argsRaw)); ok {
				_ = json.Unmarshal([]byte(fixed), &args)
			}
		}
		if args == nil {
			return nil, &toolBridgeMalformedError{Reason: "arguments_not_object", Detail: safeIdent(name)}
		}
	} else {
		args = map[string]json.RawMessage{}
	}
	for _, field := range spec.Required {
		if v, ok := args[field]; !ok || string(v) == "null" {
			return nil, &toolBridgeMalformedError{Reason: "missing_required", Detail: safeIdent(name) + "." + safeIdent(field)}
		}
	}
	out, _ := json.Marshal(args)
	return &ToolCall{Type: "function", Function: ToolCallFunction{Name: name, Arguments: string(out)}}, nil
}

var identRegex = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,64}$`)

func safeIdent(s string) string {
	if identRegex.MatchString(s) {
		return s
	}
	return "?"
}

// guardEmbedded handles multiple standalone requests. Prose surrounding a
// request makes the reply an explanation or report, never authorization to run
// a quoted call, even when its name and arguments match the declared schema.
func guardEmbedded(content string, declared declaredTools) toolTextVerdict {
	remaining := content
	var calls []ToolCall
	accept := func(candidate string) bool {
		cs, _, bad, _ := decodeCalls(strings.TrimSpace(candidate), declared)
		if len(cs) == 0 || bad != nil {
			return false
		}
		calls = append(calls, cs...)
		return true
	}
	for _, m := range toolCallXMLRegex.FindAllStringSubmatch(content, -1) {
		if accept(m[1]) {
			remaining = strings.Replace(remaining, m[0], "", 1)
		}
	}
	if len(calls) == 0 {
		for _, m := range mdFenceRegex.FindAllStringSubmatch(content, -1) {
			if accept(m[1]) {
				remaining = strings.Replace(remaining, m[0], "", 1)
			}
		}
	}
	if len(calls) == 0 {
		var rest []string
		var malformed *toolBridgeMalformedError
		for _, line := range strings.Split(content, "\n") {
			t := strings.TrimSpace(line)
			if t == "" {
				continue
			}
			if startsLikeObject(t) {
				if accept(t) {
					continue
				}
				if shapedText(t, declared) && !jsonParses(t) {
					malformed = &toolBridgeMalformedError{Reason: "truncated_or_undecodable"}
					continue
				}
			}
			if (len(calls) > 0 || malformed != nil) && isStandaloneXMLToolClose(t) {
				continue
			}
			rest = append(rest, t)
		}
		if malformed != nil && len(rest) == 0 {
			return toolTextVerdict{Kind: toolTextMalformed, Malformed: malformed}
		}
		if len(calls) > 0 {
			remaining = strings.Join(rest, "\n")
		}
	}
	if len(calls) == 0 || strings.TrimSpace(remaining) != "" {
		if v, ok := guardTrailingCalls(content, declared); ok {
			return v
		}
		return toolTextVerdict{Kind: toolTextPlain, Remaining: content}
	}
	return toolTextVerdict{Kind: toolTextCalls, Calls: numberCalls(calls), Remaining: strings.TrimSpace(remaining)}
}

// guardTrailingCalls accepts "prose, then tool request(s) as the very end of the
// reply": a run of single-line objects, ```json fences or <tool_call> tags that
// are the last thing in the content, every one decoding to a declared tool with
// valid arguments. The prose in front becomes Preamble. Nothing is accepted when
// text follows the request, when the prose itself carries tool-shaped JSON (a
// quoted example), or when any trailing request fails validation.
func guardTrailingCalls(content string, declared declaredTools) (toolTextVerdict, bool) {
	trimmed := strings.TrimRight(content, " \t\r\n")
	decode := func(candidate string) []ToolCall {
		cs, _, bad, _ := decodeCalls(strings.TrimSpace(candidate), declared)
		if bad != nil {
			return nil
		}
		return cs
	}
	finish := func(start int, calls []ToolCall) (toolTextVerdict, bool) {
		preamble := strings.TrimSpace(content[:start])
		if len(calls) == 0 || preamble == "" || shapedText(preamble, declared) {
			return toolTextVerdict{}, false
		}
		return toolTextVerdict{Kind: toolTextCalls, Calls: numberCalls(calls), Preamble: preamble}, true
	}

	// Tags and fences: the last match must end the reply; earlier matches join the
	// run only when separated by whitespace.
	for _, re := range []*regexp.Regexp{toolCallXMLRegex, mdFenceRegex} {
		matches := re.FindAllStringSubmatchIndex(trimmed, -1)
		var chain [][]ToolCall
		start, end := -1, len(trimmed)
		for i := len(matches) - 1; i >= 0; i-- {
			m := matches[i]
			if m[1] != end && strings.TrimSpace(trimmed[m[1]:end]) != "" {
				break
			}
			cs := decode(trimmed[m[2]:m[3]])
			if len(cs) == 0 {
				break
			}
			chain = append([][]ToolCall{cs}, chain...)
			start, end = m[0], m[0]
		}
		if start < 0 {
			continue
		}
		var all []ToolCall
		for _, cs := range chain {
			all = append(all, cs...)
		}
		if v, ok := finish(start, all); ok {
			return v, true
		}
	}

	// Bare single-line objects at the end.
	lines := strings.Split(trimmed, "\n")
	var all []ToolCall
	first := len(lines)
	for i := len(lines) - 1; i >= 0; i-- {
		t := strings.TrimSpace(lines[i])
		if t == "" || isStandaloneXMLToolClose(t) {
			continue
		}
		if !startsLikeObject(t) {
			break
		}
		cs := decode(t)
		if len(cs) == 0 {
			break
		}
		all = append(cs, all...)
		first = i
	}
	if len(all) > 0 {
		return finish(len(strings.Join(lines[:first], "\n")), all)
	}
	// Bare multi-line pretty-printed object at the end after prose preamble.
	if strings.HasSuffix(trimmed, "}") || strings.HasSuffix(trimmed, `"}`) || strings.HasSuffix(trimmed, `\"}"`) || trailingXMLToolCloseRegex.MatchString(trimmed) {
		for i := len(lines) - 1; i >= 1; i-- {
			if !startsLikeObject(strings.TrimSpace(lines[i])) {
				continue
			}
			if cs := decode(strings.Join(lines[i:], "\n")); len(cs) > 0 {
				if v, ok := finish(len(strings.Join(lines[:i], "\n")), cs); ok {
					return v, true
				}
			}
		}
	}
	return toolTextVerdict{}, false
}

// prepareToolBridgeResponseGuarded is the Agent-mode entry: same post-processing
// as prepareToolBridgeResponse (__done__, WebSearch interception) but the text is
// classified strictly. A malformed request is an error, never an answer.
func prepareToolBridgeResponseGuarded(content string, nativeToolUses []AgentValueEntry, declared declaredTools) (preparedToolBridgeResponse, error) {
	if len(nativeToolUses) > 0 {
		return prepareToolBridgeResponse(content, nativeToolUses), nil
	}
	verdict := guardToolText(content, declared)
	switch verdict.Kind {
	case toolTextMalformed:
		log.Printf("[bridge] malformed tool request (%d chars): %v; nothing executed, turn not resent", len(content), verdict.Malformed)
		return preparedToolBridgeResponse{}, verdict.Malformed
	case toolTextCalls:
		if verdict.Repaired != "" {
			log.Printf("[bridge] decoded escaped tool request (layers=%s)", verdict.Repaired)
		}
		prepared := finishPreparedCalls(verdict.Calls, verdict.Remaining)
		prepared.Preamble = verdict.Preamble
		return prepared, nil
	}
	return preparedToolBridgeResponse{Remaining: content}, nil
}
