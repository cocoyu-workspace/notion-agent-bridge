package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// --- merged from agent_tool_text_test.go ---
// Issue #28: a model reply that is a tool request must reach Claude Code as a
// real tool_use block, a reply that only LOOKS like a tool request but cannot be
// validated must fail loudly (never become the final answer, never run), and an
// ordinary answer must stay an answer. Fake Notion service only: these tests
// prove the bridge's decoding/classification, not what the real model emits.

const editArgs = `{"file_path":"/work/a.go","old_string":"x := 1","new_string":"x := 2 // <b>\"q\"</b>"}`

func toolTextHandler(acc *Account, tools declaredTools, stream bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sess := &Session{ClientKey: "client-1"}
		setSessionTools(sess, tools)
		msgs := []ChatMessage{u("fix it")}
		if stream {
			handleAnthropicStream(w, acc, msgs, "auto", "msg_test", true, false, false, nil, false, nil, nil, sess, r.Context().Done())
		} else {
			handleAnthropicNonStream(w, acc, msgs, "auto", "msg_test", true, false, false, nil, false, nil, nil, sess, r.Context().Done())
		}
	})
}

type toolTextResult struct {
	toolUses []map[string]interface{} // {name, input}
	text     string
	order    []string // content block types in emission order
	errMsg   string
	status   int
}

func runToolText(t *testing.T, reply string, tools declaredTools, stream bool) (toolTextResult, *fakeAgentServer) {
	t.Helper()
	env := newRecoveryEnv(t)
	useRecoveryStore(t, "")
	env.f.replyFn = func(int) string { return reply }
	srv := httptest.NewServer(toolTextHandler(recAcc(), tools, stream))
	defer srv.Close()
	var res toolTextResult
	if stream {
		call := startSSE(t, srv.URL)
		evs := call.drain(10 * time.Second)
		res.status = call.Status
		var inputs = map[int]*strings.Builder{}
		var order []int
		for _, e := range evs {
			idx := -1
			if v, ok := e.Data["index"].(float64); ok {
				idx = int(v)
			}
			switch e.Name {
			case "content_block_start":
				if cb, _ := e.Data["content_block"].(map[string]interface{}); cb != nil {
					if typ, _ := cb["type"].(string); typ != "" {
						res.order = append(res.order, typ)
					}
				}
				if cb, _ := e.Data["content_block"].(map[string]interface{}); cb["type"] == "tool_use" {
					res.toolUses = append(res.toolUses, map[string]interface{}{"name": cb["name"]})
					inputs[idx] = &strings.Builder{}
					order = append(order, idx)
				}
			case "content_block_delta":
				if d, _ := e.Data["delta"].(map[string]interface{}); d["type"] == "input_json_delta" {
					inputs[idx].WriteString(d["partial_json"].(string))
				}
			case "error":
				m, _ := e.Data["error"].(map[string]interface{})
				res.errMsg, _ = m["message"].(string)
			}
		}
		for i, idx := range order {
			var in interface{}
			if err := json.Unmarshal([]byte(inputs[idx].String()), &in); err != nil {
				t.Fatalf("tool_use input is not valid JSON: %q", inputs[idx].String())
			}
			res.toolUses[i]["input"] = in
		}
		res.text = textOf(evs)
		if call.Body != "" {
			var eb struct {
				Error struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			json.Unmarshal([]byte(call.Body), &eb)
			res.errMsg = eb.Error.Message
		}
		return res, env.f
	}
	resp, err := http.Post(srv.URL, "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	res.status = resp.StatusCode
	var body struct {
		Content []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("non-JSON body %q", raw)
	}
	res.errMsg = body.Error.Message
	for _, c := range body.Content {
		res.order = append(res.order, c.Type)
		switch c.Type {
		case "text":
			res.text += c.Text
		case "tool_use":
			var in interface{}
			json.Unmarshal(c.Input, &in)
			res.toolUses = append(res.toolUses, map[string]interface{}{"name": c.Name, "input": in})
		}
	}
	return res, env.f
}

func wantEdit(t *testing.T, res toolTextResult) {
	t.Helper()
	if len(res.toolUses) != 1 || res.toolUses[0]["name"] != "Edit" {
		t.Fatalf("want exactly one Edit tool_use, got %v text=%q err=%q", res.toolUses, res.text, res.errMsg)
	}
	in, _ := res.toolUses[0]["input"].(map[string]interface{})
	if in["file_path"] != "/work/a.go" || in["old_string"] != "x := 1" || in["new_string"] != `x := 2 // <b>"q"</b>` {
		t.Fatalf("decoded arguments differ from what the model meant: %v", in)
	}
	if res.text != "" {
		t.Fatalf("a tool request must not also leak as answer text: %q", res.text)
	}
}

func wantLoudFailure(t *testing.T, res toolTextResult, f *fakeAgentServer) {
	t.Helper()
	if len(res.toolUses) != 0 {
		t.Fatalf("an unvalidated reply must never become a tool_use: %v", res.toolUses)
	}
	if strings.Contains(res.text, "name") || strings.Contains(res.text, "Edit") {
		t.Fatalf("an unexecuted tool request was delivered as the final answer: %q", res.text)
	}
	if !strings.Contains(res.errMsg, "[bridge:malformed_tool_call]") {
		t.Fatalf("want a malformed_tool_call error, got %q (status %d)", res.errMsg, res.status)
	}
	if c, s := f.counts(); c != 1 || s != 0 {
		t.Fatalf("the failure must not resend or fork: creates=%d sends=%d", c, s)
	}
}

func jsonQuoteBody(s string) string { b, _ := json.Marshal(s); return string(b) }

func TestAgentToolTextRepairsOverEscapedEdit(t *testing.T) {
	plain := `{"name":"Edit","arguments":` + editArgs + `}`
	variants := map[string]string{
		"raw":                  plain,
		"escaped quotes":       strings.ReplaceAll(plain, `"`, `\"`),
		"html entities":        strings.NewReplacer(`"`, "&quot;", "<", "&lt;", ">", "&gt;").Replace(plain),
		"escaped then entity":  strings.NewReplacer(`"`, `\&quot;`).Replace(plain),
		"json string wrapped":  jsonQuoteBody(plain),
		"arguments as string":  `{"name":"Edit","arguments":` + jsonQuoteBody(editArgs) + `}`,
		"fenced":               "```json\n" + plain + "\n```",
		"fenced and escaped":   "```json\n" + strings.ReplaceAll(plain, `"`, `\"`) + "\n```",
		"tool_call tag":        "<tool_call>" + plain + "</tool_call>",
		"escaped tag":          `\<tool_call\>` + plain + `\</tool_call\>`,
		"entity tag":           "&lt;tool_call&gt;" + plain + "&lt;/tool_call&gt;",
		"escaped with newline": strings.ReplaceAll(plain, `"`, `\"`) + "\n",
	}
	tools := declaredTools{"Edit": {Required: []string{"file_path", "old_string", "new_string"}}}
	for name, reply := range variants {
		for _, stream := range []bool{true, false} {
			t.Run(name, func(t *testing.T) {
				res, _ := runToolText(t, reply, tools, stream)
				wantEdit(t, res)
			})
		}
	}
}

func TestAgentToolTextMalformedRequestsFailLoudly(t *testing.T) {
	tools := declaredTools{"Edit": {Required: []string{"file_path", "old_string", "new_string"}}, "Bash": {Required: []string{"command"}}}
	plain := `{"name":"Edit","arguments":` + editArgs + `}`
	cases := map[string]string{
		"truncated":               plain[:len(plain)-9],
		"truncated before name":   `{"arguments":{"file_path":"/work/a.go","old_str`,
		"truncated before name 2": `{\"arguments\":{\"file_path\":\"/work/a.go\",\"old_str`,
		"truncated escaped":       strings.ReplaceAll(plain[:len(plain)-9], `"`, `\"`),
		"arguments not json":      `{"name":"Edit","arguments":not-json}`,
		"arguments is array":      `{"name":"Edit","arguments":["a"]}`,
		"arguments missing":       `{"name":"Edit"}`,
		"missing required":        `{"name":"Edit","arguments":{"file_path":"/work/a.go"}}`,
		"undeclared tool":         `{"name":"DeleteEverything","arguments":{"command":"rm -rf /"}}`,
		"fenced truncated":        "```json\n" + plain[:len(plain)-9] + "\n```",
		"tag truncated":           "<tool_call>" + plain[:len(plain)-9],
		"mixed layers":            `{\"name\":\"Edit\",\"arguments\":{"file_path":"/x"}}`,
		"entity truncated":        strings.ReplaceAll(plain[:len(plain)-9], `"`, "&quot;"),
		"second call malformed":   `{"name":"Bash","arguments":{"command":"echo hi"}}` + "\n" + `{"name":"Edit","arguments":{"file_path":`,
		"pretty truncated":        "{\n  \"name\": \"Edit\",\n  \"arguments\": {\n    \"file_path\": \"/work/a.go\"",
		"declared name, no args":  `{"name":"Bash","arguments":{}}`,
		"wrapper without payload": `{"tool_call":{"name":"Bash"}}`,
	}
	for name, reply := range cases {
		for _, stream := range []bool{true, false} {
			t.Run(name, func(t *testing.T) {
				res, f := runToolText(t, reply, tools, stream)
				wantLoudFailure(t, res, f)
			})
		}
	}
}

func TestAgentToolTextOrdinaryAnswersStayAnswers(t *testing.T) {
	tools := declaredTools{"Edit": {Required: []string{"file_path", "old_string", "new_string"}}, "Bash": {Required: []string{"command"}}}
	answers := map[string]string{
		"prose":              "Done. I changed x to 2 and the tests pass.",
		"json data answer":   `{"files":["a.go","b.go"],"count":2}`,
		"json with a name":   `{"name":"widget","size":3}`,
		"explains protocol":  "The reply looked like {\"name\":\"Edit\" ... and was cut off, so nothing ran.",
		"quoted undeclared":  "Example of a bad call:\n```json\n{\"name\":\"DeleteEverything\",\"arguments\":{\"command\":\"rm -rf /\"}}\n```\nIt is rejected.",
		"array of objects":   `[{"name":"a","arguments":"b"}]`,
		"markdown with code": "Fixed:\n```go\nx := 2\n```\n",
	}
	for name, reply := range answers {
		for _, stream := range []bool{true, false} {
			t.Run(name, func(t *testing.T) {
				res, _ := runToolText(t, reply, tools, stream)
				if len(res.toolUses) != 0 || res.errMsg != "" || strings.TrimSpace(res.text) != strings.TrimSpace(reply) {
					t.Fatalf("ordinary answer was altered: tools=%v err=%q text=%q", res.toolUses, res.errMsg, res.text)
				}
			})
		}
	}
}

func TestAgentToolTextValidParallelCallsStillRun(t *testing.T) {
	tools := declaredTools{"Read": {Required: []string{"file_path"}}, "Bash": {Required: []string{"command"}}}
	reply := `{"name":"Read","arguments":{"file_path":"/a"}}` + "\n" + `{"name":"Bash","arguments":{"command":"ls"}}`
	res, _ := runToolText(t, reply, tools, true)
	if len(res.toolUses) != 2 || res.toolUses[0]["name"] != "Read" || res.toolUses[1]["name"] != "Bash" || res.errMsg != "" {
		t.Fatalf("parallel calls: %v err=%q", res.toolUses, res.errMsg)
	}
}

// A declared, schema-valid call in a report is an example, not authorization.
// Exercise both HTTP response paths: neither may emit a tool_use to the CLI.
func TestAgentToolTextReportQuotesDeclaredCall(t *testing.T) {
	call := `{"name":"Edit","arguments":` + editArgs + `}`
	cases := []string{
		"Escaped tag in report:\n\\<tool_call\\>" + call + "\\</tool_call\\>",
		"Truncated line in report:\n" + call[:len(call)-9],
		call + "\nThis is a quoted example, not a request.",
	}
	for _, stream := range []bool{false, true} {
		for _, reply := range cases {
			res, f := runToolText(t, reply, declaredTools{"Edit": {Required: []string{"file_path", "old_string", "new_string"}}}, stream)
			if len(res.toolUses) != 0 || res.text != reply || res.errMsg != "" {
				t.Fatalf("quoted call executed or changed: stream=%t tools=%v text=%q err=%q", stream, res.toolUses, res.text, res.errMsg)
			}
			if c, s := f.counts(); c != 1 || s != 0 {
				t.Fatalf("report resent or forked: creates=%d sends=%d", c, s)
			}
		}
	}
}

func TestAgentToolTextRepairsRawTabsAndNewlinesInEdit(t *testing.T) {
	tools := declaredTools{"Edit": {Required: []string{"file_path", "old_string", "new_string"}}}
	// Real-world failure shape from issue #28: model emits literal 0x09 tabs
	// (or literal 0x0a newlines) inside old_string/new_string when editing Go code.
	rawTabPayload := "{\"name\": \"Edit\", \"arguments\": {\"file_path\": \"/work/store.go\", \"old_string\": \"\tif t.store.records[t.threadID] == t.rec {\\n\t\tt.rec.Prev = nil\\n\t}\", \"new_string\": \"\tif t.store.records[t.threadID] == t.rec {\\n\t\tt.rec.Prev = nil\\n\t\tt.store.saveLocked()\\n\t}\"}}"
	rawNewlineAndTabPayload := "{\n  \"name\": \"Edit\",\n  \"arguments\": {\n    \"file_path\": \"/work/store.go\",\n    \"old_string\": \"\tif t.store.records[t.threadID] == t.rec {\n\t\tt.rec.Prev = nil\n\t}\",\n    \"new_string\": \"\tif t.store.records[t.threadID] == t.rec {\n\t\tt.rec.Prev = nil\n\t\tt.store.saveLocked()\n\t}\"\n  }\n}"
	overEscapedWithTabs := strings.ReplaceAll(rawTabPayload, `"`, `\"`)
	for name, reply := range map[string]string{
		"raw_tabs":              rawTabPayload,
		"raw_newlines_and_tabs": rawNewlineAndTabPayload,
		"over_escaped_tabs":     overEscapedWithTabs,
	} {
		for _, stream := range []bool{true, false} {
			t.Run(name, func(t *testing.T) {
				res, _ := runToolText(t, reply, tools, stream)
				if len(res.toolUses) != 1 || res.toolUses[0]["name"] != "Edit" || res.errMsg != "" || res.text != "" {
					t.Fatalf("want Edit tool_use, got tools=%v text=%q err=%q", res.toolUses, res.text, res.errMsg)
				}
				in, _ := res.toolUses[0]["input"].(map[string]interface{})
				wantOld := "\tif t.store.records[t.threadID] == t.rec {\n\t\tt.rec.Prev = nil\n\t}"
				wantNew := "\tif t.store.records[t.threadID] == t.rec {\n\t\tt.rec.Prev = nil\n\t\tt.store.saveLocked()\n\t}"
				if in["file_path"] != "/work/store.go" || in["old_string"] != wantOld || in["new_string"] != wantNew {
					t.Fatalf("unexpected decoded Edit arguments: %#v", in)
				}
			})
		}
	}
}

// --- merged from agent_tool_text_preamble_test.go ---
// Issue #28 follow-up (real CLI finding): one sentence of prose followed by a
// valid, declared tool request at the END of the reply is a tool call, with the
// prose delivered as a text block in front of the tool_use. Anything less
// clear-cut (prose after the call, a quoted example in the middle, undeclared
// tools, data JSON, invalid arguments) keeps the strict classification.

var preambleTools = declaredTools{
	"Edit":        {Required: []string{"file_path", "old_string", "new_string"}},
	"Bash":        {Required: []string{"command"}},
	"SendMessage": {Required: []string{"to", "message"}},
}

const (
	sendCall = `{"name":"SendMessage","arguments":{"to":"agent-1","message":"go on"}}`
	bashCall = `{"name":"Bash","arguments":{"command":"which go"}}`
	editCall = `{"name":"Edit","arguments":` + editArgs + `}`
)

func TestAgentToolTextProsePreambleThenTrailingCallIsToolUse(t *testing.T) {
	cases := map[string]struct {
		reply, prose string
		names        []string
	}{
		"line, blank line between": {"Step 1 is done, so now step 2: resuming the same subagent with SendMessage.\n\n" + sendCall,
			"Step 1 is done, so now step 2: resuming the same subagent with SendMessage.", []string{"SendMessage"}},
		"line, single newline": {"Go isn't on PATH, so I'll look for a Go install...\n" + bashCall,
			"Go isn't on PATH, so I'll look for a Go install...", []string{"Bash"}},
		"line, trailing newline": {"Checking.\n" + bashCall + "\n", "Checking.", []string{"Bash"}},
		"json fence":             {"Looking for go.\n```json\n" + bashCall + "\n```", "Looking for go.", []string{"Bash"}},
		"tool_call tag":          {"Looking for go.\n<tool_call>" + bashCall + "</tool_call>", "Looking for go.", []string{"Bash"}},
		"two trailing lines":     {"Doing two things.\n" + bashCall + "\n" + sendCall, "Doing two things.", []string{"Bash", "SendMessage"}},
		"earlier code fence is prose": {"Fixed:\n```go\nx := 2\n```\nNow verifying.\n" + bashCall,
			"Fixed:\n```go\nx := 2\n```\nNow verifying.", []string{"Bash"}},
		"pretty-printed multiline json": {
			"I will update the file now:\n{\n  \"name\": \"Edit\",\n  \"arguments\": {\n    \"file_path\": \"/work/a.go\",\n    \"old_string\": \"x := 1\",\n    \"new_string\": \"x := 2\"\n  }\n}",
			"I will update the file now:", []string{"Edit"}},
		"pretty-printed multiline edit with raw tab": {
			"Applying indentation fix:\n{\n  \"name\": \"Edit\",\n  \"arguments\": {\n    \"file_path\": \"/work/a.go\",\n    \"old_string\": \"\tx := 1\",\n    \"new_string\": \"\tx := 2\"\n  }\n}",
			"Applying indentation fix:", []string{"Edit"}},
	}
	for name, c := range cases {
		for _, stream := range []bool{true, false} {
			t.Run(name, func(t *testing.T) {
				res, f := runToolText(t, c.reply, preambleTools, stream)
				if res.errMsg != "" || len(res.toolUses) != len(c.names) {
					t.Fatalf("want %v tool_use, got tools=%v text=%q err=%q", c.names, res.toolUses, res.text, res.errMsg)
				}
				for i, n := range c.names {
					if res.toolUses[i]["name"] != n {
						t.Fatalf("tool %d = %v, want %s", i, res.toolUses[i]["name"], n)
					}
				}
				if res.text != c.prose {
					t.Fatalf("prose preamble must be delivered as text: got %q want %q", res.text, c.prose)
				}
				if len(res.order) == 0 || res.order[0] != "text" || res.order[len(res.order)-1] != "tool_use" {
					t.Fatalf("text block must come before tool_use: %v", res.order)
				}
				if cr, se := f.counts(); cr != 1 || se != 0 {
					t.Fatalf("must not resend or fork: creates=%d sends=%d", cr, se)
				}
			})
		}
	}
}

func TestAgentToolTextProsePreambleVerdict(t *testing.T) {
	v := guardToolText("Now step 2.\n\n"+sendCall, preambleTools)
	if v.Kind != toolTextCalls || len(v.Calls) != 1 || v.Preamble != "Now step 2." || v.Remaining != "" {
		t.Fatalf("verdict = %+v", v)
	}
	if prep, err := prepareToolBridgeResponseGuarded("Now step 2.\n"+sendCall, nil, preambleTools); err != nil || !prep.HasCalls || prep.Preamble != "Now step 2." {
		t.Fatalf("prepared = %+v err=%v", prep, err)
	}
}

func TestAgentToolTextProseAroundCallsStaysStrict(t *testing.T) {
	cases := map[string]string{
		"prose after the call":        "Running it.\n" + bashCall + "\nThat should do it.",
		"call then prose":             bashCall + "\nThis is a quoted example, not a request.",
		"example in the middle":       "Example:\n" + bashCall + "\nand then I am done.",
		"fence in the middle":         "Example:\n```json\n" + bashCall + "\n```\nIt works like that.",
		"tag in the middle":           "Example:\n<tool_call>" + bashCall + "</tool_call>\nOnly an example.",
		"two calls split by prose":    bashCall + "\nand then\n" + sendCall,
		"quoted call and a real call": "Earlier I ran " + bashCall + "\nNow:\n" + sendCall,
		"truncated trailing call":     "Running it.\n" + bashCall[:len(bashCall)-5],
		"escaped tag at the end":      "Report:\n" + `\<tool_call\>` + bashCall + `\</tool_call\>`,
		"undeclared tool":             `Cleaning up.` + "\n" + `{"name":"DeleteEverything","arguments":{"command":"rm -rf /"}}`,
		"missing required":            "Editing.\n" + `{"name":"Edit","arguments":{"file_path":"/a"}}`,
		"data json":                   "Result:\n" + `{"name":"widget","size":3}`,
		"data json no name":           "Result:\n" + `{"files":["a.go"],"count":1}`,
	}
	for name, reply := range cases {
		for _, stream := range []bool{true, false} {
			t.Run(name, func(t *testing.T) {
				res, f := runToolText(t, reply, preambleTools, stream)
				if len(res.toolUses) != 0 {
					t.Fatalf("must not become a tool_use: %v", res.toolUses)
				}
				if res.errMsg == "" && strings.TrimSpace(res.text) != strings.TrimSpace(reply) {
					t.Fatalf("plain answer altered: %q", res.text)
				}
				if cr, se := f.counts(); cr != 1 || se != 0 {
					t.Fatalf("must not resend or fork: creates=%d sends=%d", cr, se)
				}
			})
		}
	}
}

// --- merged from tool_guard_escape_test.go ---
func escapeDeclared() declaredTools {
	return declaredTools{"Write": {Required: []string{"file_path", "content"}}, "Bash": {Required: []string{"command"}}, "Edit": {Required: []string{"file_path", "old_string", "new_string"}}}
}

// Issue #61: a Write of a report containing regexes with single backslashes was
// refused as truncated_or_undecodable.
func TestToolGuardRepairsInvalidBackslashEscapes(t *testing.T) {
	cases := []struct{ name, reply, field, want string }{
		{"regex", `{"name":"Write","arguments":{"file_path":"r.md","content":"pattern ^\d{4}\s*\| and \(x\)"}}`, "content", `pattern ^\d{4}\s*\| and \(x\)`},
		{"windows path", `{"name":"Bash","arguments":{"command":"dir C:\work\data"}}`, "command", `dir C:\work\data`},
		{"mixed with valid escapes", `{"name":"Write","arguments":{"file_path":"a","content":"line1\nq=\"x\" \d tab\tend \u4e2d"}}`, "content", "line1\nq=\"x\" \\d tab\tend 中"},
		{"bad unicode escape", `{"name":"Write","arguments":{"file_path":"a","content":"\u12 and \uZZZZ"}}`, "content", `\u12 and \uZZZZ`},
		{"raw newline and bad escape", "{\"name\":\"Write\",\"arguments\":{\"file_path\":\"a\",\"content\":\"a\\d\nb\"}}", "content", "a\\d\nb"},
		{"after preamble", "I'll write it now.\n" + `{"name":"Write","arguments":{"file_path":"r.md","content":"\d+"}}`, "content", `\d+`},
	}
	for _, c := range cases {
		v := guardToolText(c.reply, escapeDeclared())
		if v.Kind != toolTextCalls || len(v.Calls) != 1 {
			t.Fatalf("%s: kind=%v malformed=%v", c.name, v.Kind, v.Malformed)
		}
		var args map[string]string
		if err := json.Unmarshal([]byte(v.Calls[0].Function.Arguments), &args); err != nil {
			t.Fatalf("%s: arguments not JSON: %v", c.name, err)
		}
		if args[c.field] != c.want {
			t.Fatalf("%s: %s=%q want %q", c.name, c.field, args[c.field], c.want)
		}
	}
}

func TestToolGuardEscapeRepairKeepsValidAndPlainText(t *testing.T) {
	if out, ok := escapeInvalidJSONEscapes(`{"a":"x\\d \" \/ \b\f\n\r\t \u00e9"} \q outside`); ok {
		t.Fatalf("valid escapes changed: %s", out)
	}
	// A prose answer with backslashes is still plain text, not a tool call.
	v := guardToolText(`Use the regex ^\d{4}\s*\| to match it.`, escapeDeclared())
	if v.Kind != toolTextPlain {
		t.Fatalf("kind=%v", v.Kind)
	}
	// A truly truncated request is still refused.
	v = guardToolText(`{"name":"Write","arguments":{"file_path":"r.md","content":"\d+ and more`, escapeDeclared())
	if v.Kind != toolTextMalformed {
		t.Fatalf("truncated accepted: kind=%v", v.Kind)
	}
}

func TestToolGuardIssue85FlatArgumentsAndKeyAngleTypo(t *testing.T) {
	declared := escapeDeclared()
	cases := []struct {
		name     string
		reply    string
		wantCmd  string
		wantDesc string
	}{
		{
			name:     "flat top-level Bash arguments from thread 31c4699d seq 48",
			reply:    `{"name":"Bash","description":"List recently modified log files in the repo","command":"cd /workspace/example-project && find . -maxdepth 4 \\( -name node_modules -o -name .git \\) -prune -o -type f \\( -name '*.log' -o -name '*.log.*' \\) -mtime -2 -print 2>/dev/null | head -40; ls -lt /tmp/example-logs 2>/dev/null | head -5; date"}`,
			wantCmd:  `cd /workspace/example-project && find . -maxdepth 4 \( -name node_modules -o -name .git \) -prune -o -type f \( -name '*.log' -o -name '*.log.*' \) -mtime -2 -print 2>/dev/null | head -40; ls -lt /tmp/example-logs 2>/dev/null | head -5; date`,
			wantDesc: "List recently modified log files in the repo",
		},
		{
			name:     "Anthropic input field instead of arguments",
			reply:    `{"name":"Bash","input":{"command":"echo ok","description":"say ok"}}`,
			wantCmd:  "echo ok",
			wantDesc: "say ok",
		},
		{
			name:     "key angle-bracket typo from thread 9ab0498d seq 108",
			reply:    `{"name":"Bash","arguments":{"command">"cd /tmp/ark-master-wt && grep -rln 'reporter_context' services/*_test.go | head; go test ./services -run 'ReporterContext|Reporter' -count=1 2>&1 | tail -25","description":"Run reporter_context tests on origin/master worktree","timeout":300000}}`,
			wantCmd:  `cd /tmp/ark-master-wt && grep -rln 'reporter_context' services/*_test.go | head; go test ./services -run 'ReporterContext|Reporter' -count=1 2>&1 | tail -25`,
			wantDesc: "Run reporter_context tests on origin/master worktree",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := guardToolText(c.reply, declared)
			if v.Kind != toolTextCalls || len(v.Calls) != 1 {
				t.Fatalf("kind=%v calls=%d malformed=%v", v.Kind, len(v.Calls), v.Malformed)
			}
			var args map[string]interface{}
			if err := json.Unmarshal([]byte(v.Calls[0].Function.Arguments), &args); err != nil {
				t.Fatalf("arguments not JSON: %v", err)
			}
			if args["command"] != c.wantCmd || args["description"] != c.wantDesc {
				t.Fatalf("args=%v", args)
			}
			if _, hasName := args["name"]; hasName {
				t.Fatalf("top-level name leaked into arguments: %v", args)
			}
		})
	}
}
