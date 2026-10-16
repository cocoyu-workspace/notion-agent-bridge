package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const capturedShape = `{"type":"agent_service_transcript_page","patches":[{"op":"committed","sequence":21},{"op":"put","entity":{"id":"reply","kind":"assistant_message","sequence":22,"source":"committed","content":[{"type":"text","text":"answer"}]}},{"op":"remove","id":"rate-limit-status:test"},{"op":"put","entity":{"id":"done","kind":"turn_completed","sequence":24,"source":"committed","stop_reason":"end_turn"}},{"op":"session","session":{"status":"idle","sequence":24}}],"session":{"status":"idle","sequence":24},"has_more_backward":false}`

func TestAgentTranscriptCapturedShape(t *testing.T) {
	var page agentTranscriptPage
	if err := json.Unmarshal([]byte(capturedShape), &page); err != nil {
		t.Fatal(err)
	}
	text, done, err := readAgentTranscript(page)
	if err != nil || !done || text != "answer" {
		t.Fatalf("%q %v %v", text, done, err)
	}
	page.Session.Status = "running"
	if _, done, err = readAgentTranscript(page); err != nil || done {
		t.Fatalf("running completed: %v %v", done, err)
	}
	page.Session.Status = "failed"
	if _, _, err = readAgentTranscript(page); err == nil {
		t.Fatal("failed accepted")
	}
	page.Session.Status = "idle"
	page.HasMoreBackward = true
	if _, _, err = readAgentTranscript(page); err == nil {
		t.Fatal("partial page accepted")
	}
	page.HasMoreBackward = false
	page.Patches[3].Entity.StopReason = "error"
	if _, _, err = readAgentTranscript(page); err == nil {
		t.Fatal("failure stop accepted")
	}
}

func TestAgentThreadOptInAndLoopbackConfiguration(t *testing.T) {
	if DefaultConfig().Proxy.AgentThread {
		t.Fatal("new transport must be opt-in")
	}
	t.Setenv("AGENT_THREAD", "true")
	t.Setenv("HOST", "127.0.0.1")
	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Proxy.AgentThread || cfg.Server.Host != "127.0.0.1" {
		t.Fatal("transport and listener overrides were not loaded")
	}
}

func TestAgentThreadOpus55SelectionMatchesObservedBrowserRequest(t *testing.T) {
	for _, alias := range []string{"opus-5.5", "claude-opus-5-5", "claude-opus-5.5", "albuquerque-quinn"} {
		id, err := agentThreadModelID(alias)
		if err != nil || id != "albuquerque-quinn" {
			t.Fatalf("%q resolved to %q: %v", alias, id, err)
		}
		body := agentThreadBody(&Account{SpaceID: "space"}, nil, id)
		if body["model"] != "albuquerque-quinn" || body["reasoningEffort"] != "medium" {
			t.Fatal("explicit model selection did not match captured protocol")
		}
	}
	if _, err := agentThreadModelID("opus-5.6"); err == nil {
		t.Fatal("unknown Opus alias silently fell back to an older model")
	}
	old := AppConfig
	AppConfig = DefaultConfig()
	AppConfig.Proxy.AgentThread = true
	defer func() { AppConfig = old }()
	tools := []Tool{{Function: ToolFunction{Name: "Read", Parameters: map[string]interface{}{"type": "object"}}}}
	messages := injectToolsIntoMessages([]ChatMessage{{Role: "user", Content: "read"}}, tools, "opus-5.5", nil)
	if len(messages) != 2 || !strings.Contains(messages[0].Content, "LOCAL ACTION PROTOCOL") {
		t.Fatal("Opus selection did not use the verified local tool bridge")
	}
}

func TestAgentTranscriptUpdatesRemovalsOrdering(t *testing.T) {
	var page agentTranscriptPage
	raw := `{"type":"agent_service_transcript_page","session":{"status":"idle"},"patches":[{"op":"put","entity":{"id":"b","kind":"assistant_message","sequence":4,"content":[{"type":"text","text":"stale"}]}},{"op":"put","entity":{"id":"a","kind":"assistant_message","sequence":2,"content":[{"type":"text","text":"first"}]}},{"op":"put","entity":{"id":"b","kind":"assistant_message","sequence":4,"content":[{"type":"text","text":"last"}]}},{"op":"put","entity":{"id":"x","kind":"assistant_message","content":[{"type":"text","text":"removed"}]}},{"op":"remove","id":"x"},{"op":"put","entity":{"id":"done","kind":"turn_completed","stop_reason":"end_turn"}}]}`
	if err := json.Unmarshal([]byte(raw), &page); err != nil {
		t.Fatal(err)
	}
	text, done, err := readAgentTranscript(page)
	if err != nil || !done || text != "first\nlast" {
		t.Fatalf("%q %v %v", text, done, err)
	}
	page.Patches = page.Patches[:5]
	if _, done, err = readAgentTranscript(page); err != nil || done {
		t.Fatal("no completion accepted")
	}
}

func TestAgentThreadBodyAutoHistoryAndFreshThreads(t *testing.T) {
	messages := []ChatMessage{{Role: "user", Content: "read file"}, {Role: "tool", ToolCallID: "call", Content: "file body"}}
	a := agentThreadBody(&Account{SpaceID: "space"}, messages, "")
	b := agentThreadBody(&Account{SpaceID: "space"}, messages, "")
	if _, ok := a["model"]; ok {
		t.Fatal("Auto must omit model")
	}
	if a["threadId"] == b["threadId"] {
		t.Fatal("threads reused")
	}
	encoded, _ := json.Marshal(a)
	if !strings.Contains(string(encoded), "file body") || !strings.Contains(string(encoded), "read file") {
		t.Fatal("history lost")
	}
}

func TestAgentThreadAutoPreservesTools(t *testing.T) {
	old := AppConfig
	AppConfig = DefaultConfig()
	AppConfig.Proxy.AgentThread = true
	defer func() { AppConfig = old }()
	tools := []Tool{{Function: ToolFunction{Name: "Read", Parameters: map[string]interface{}{"type": "object"}}}}
	messages := injectToolsIntoMessages([]ChatMessage{{Role: "user", Content: "read"}}, tools, "auto", nil)
	if len(messages) != 2 || !strings.Contains(messages[0].Content, "Read") || !strings.Contains(messages[0].Content, "local client") {
		t.Fatal("Auto tools stripped")
	}
	calls, _, ok := parseToolCalls(`{"name":"Read","arguments":{"file_path":"/tmp/example"}}`)
	if !ok || len(calls) != 1 || calls[0].Function.Name != "Read" {
		t.Fatal("direct JSON incompatible")
	}
}

func TestAgentThreadTransportBoundedAndCompleted(t *testing.T) {
	oldCfg, oldBase := AppConfig, NotionAPIBase
	AppConfig = DefaultConfig()
	AppConfig.Proxy.AgentThread = true
	AppConfig.Timeouts.InferenceTimeout = 1
	defer func() { AppConfig, NotionAPIBase = oldCfg, oldBase }()
	complete := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "createAgentThread") {
			w.Write([]byte(`{}`))
			return
		}
		if complete {
			w.Write([]byte(capturedShape))
			return
		}
		w.Write([]byte(`{"type":"agent_service_transcript_page","session":{"status":"running"}}`))
	}))
	defer server.Close()
	NotionAPIBase = server.URL
	started := time.Now()
	called := false
	err := callAgentThreadWithClient(&Account{}, nil, "", "", func(string, bool, *UsageInfo) { called = true }, server.Client())
	if err == nil || called || time.Since(started) > 3*time.Second {
		t.Fatalf("deadline failed %v, called=%v", err, called)
	}
	complete = true
	var output string
	var done bool
	if err := callAgentThreadWithClient(&Account{}, nil, "", "", func(text string, finished bool, _ *UsageInfo) { output += text; done = finished }, server.Client()); err != nil {
		t.Fatal(err)
	}
	if output != "answer" || !done {
		t.Fatalf("%q %v", output, done)
	}
}

func TestAgentThreadPlainConversationPreservesToolsAndResults(t *testing.T) {
	messages := []ChatMessage{
		{Role: "system", Content: "Project rule: test changes"},
		{Role: "user", Content: "Read /tmp/file.py"},
		{Role: "assistant", Content: "reading", ToolCalls: []ToolCall{{ID: "call_read", Type: "function", Function: ToolCallFunction{Name: "Read", Arguments: `{"file_path":"/tmp/file.py"}`}}}},
		{Role: "tool", ToolCallID: "call_read", Name: "Read", Content: "def f():\n    return 1\n"},
	}
	prompt := agentThreadPrompt(messages)
	for _, want := range []string{"PROJECT CONTEXT", "Project rule: test changes", "USER\nRead /tmp/file.py", "ASSISTANT\nreading", "LOCAL TOOL REQUESTS", "call_read", "LOCAL TOOL RESULT (call_id=call_read, name=Read)", "def f():\n    return 1\n", "user's computer", "do not try Notion's hosted filesystem", "Wait for the returned tool message"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	if strings.Contains(prompt, `"role":"system"`) {
		t.Fatal("whole conversation still JSON encoded")
	}
}

func TestAgentThreadToolChoiceAndSchemaPreserved(t *testing.T) {
	old := AppConfig
	AppConfig = DefaultConfig()
	AppConfig.Proxy.AgentThread = true
	defer func() { AppConfig = old }()
	schema := map[string]interface{}{"type": "object", "properties": map[string]interface{}{"file_path": map[string]string{"type": "string"}}, "required": []string{"file_path"}}
	tools := []Tool{{Function: ToolFunction{Name: "Read", Parameters: schema}}}
	choice := map[string]interface{}{"type": "tool", "name": "Read"}
	messages := injectToolsIntoMessages([]ChatMessage{{Role: "user", Content: "read"}}, tools, "auto", nil, choice)
	prompt := messages[0].Content
	for _, want := range []string{"file_path", "required", `"type":"tool"`, `"name":"Read"`, "local client executes", "Never invent results"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("missing %q", want)
		}
	}
}

func TestAgentThreadAutoLowEffortReachesUpstream(t *testing.T) {
	oldCfg, oldBase := AppConfig, NotionAPIBase
	AppConfig = DefaultConfig()
	defer func() { AppConfig, NotionAPIBase = oldCfg, oldBase }()
	checked := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "createAgentThread") {
			var body map[string]interface{}
			json.NewDecoder(r.Body).Decode(&body)
			if body["reasoningEffort"] != "low" {
				t.Error("low effort was lost")
			}
			if _, ok := body["model"]; ok {
				t.Error("Auto gained an explicit model")
			}
			checked = true
			w.Write([]byte(`{}`))
		} else {
			w.Write([]byte(capturedShape))
		}
	}))
	defer server.Close()
	NotionAPIBase = server.URL
	err := callAgentThreadWithClient(&Account{}, nil, "", "low", func(string, bool, *UsageInfo) {}, server.Client())
	if err != nil || !checked {
		t.Fatalf("%v checked=%v", err, checked)
	}
}

func TestAgentThreadSubagentRoutingProtocol(t *testing.T) {
	old := AppConfig
	AppConfig = DefaultConfig()
	AppConfig.Proxy.AgentThread = true
	defer func() { AppConfig = old }()

	tools := []Tool{
		{Function: ToolFunction{Name: "Read", Parameters: map[string]interface{}{"type": "object"}}},
		{Function: ToolFunction{Name: "Agent", Parameters: map[string]interface{}{"type": "object"}}},
		{Function: ToolFunction{Name: "SendMessage", Parameters: map[string]interface{}{"type": "object"}}},
	}
	messages := injectToolsIntoMessages([]ChatMessage{{Role: "user", Content: "开一个子 Agent 查一下"}}, tools, "sonnet-5.5", nil)
	protocol := messages[0].Content
	for _, want := range []string{
		`{"name":"Agent"`,
		"本地子 Agent",
		"云端子 Agent",
		"Notion Spawn Session",
		"haiku",
	} {
		if !strings.Contains(protocol, want) {
			t.Errorf("LOCAL ACTION PROTOCOL missing %q", want)
		}
	}

	firstPrompt := agentThreadPrompt(messages)
	followUpPrompt := agentThreadFollowUpPrompt([]ChatMessage{{Role: "user", Content: "开云端子 Agent 查一下"}})
	for name, p := range map[string]string{
		"firstPrompt":    firstPrompt,
		"followUpPrompt": followUpPrompt,
	} {
		for _, want := range []string{"本地子 Agent", "云端子 Agent", `"name":"Agent"`} {
			if !strings.Contains(p, want) {
				t.Errorf("%s missing %q", name, want)
			}
		}
	}
}

func TestAgentTranscriptDropsPreToolChatterAfterNativeToolCollision(t *testing.T) {
	var page agentTranscriptPage
	raw := `{
		"type":"agent_service_transcript_page",
		"session":{"status":"idle","sequence":150},
		"patches":[
			{"op":"put","entity":{"id":"u1","kind":"user_message","sequence":125}},
			{"op":"put","entity":{"id":"c1","kind":"tool","name":"context_compaction","sequence":127}},
			{"op":"put","entity":{"id":"a_shrug","kind":"assistant_message","sequence":128,"content":[{"type":"text","text":"\\_(ツ)_/¯"}]}},
			{"op":"put","entity":{"id":"t_bash","kind":"tool","name":"bash","sequence":129}},
			{"op":"put","entity":{"id":"t_hist","kind":"tool","name":"search_session_history","sequence":135}},
			{"op":"put","entity":{"id":"t_read","kind":"tool","name":"read_session_event","sequence":143}},
			{"op":"put","entity":{"id":"a_real","kind":"assistant_message","sequence":148,"content":[{"type":"text","text":"{\"name\":\"Bash\",\"arguments\":{\"command\":\"git status\",\"description\":\"Check status\"}}"}]}},
			{"op":"put","entity":{"id":"done","kind":"turn_completed","sequence":150,"stop_reason":"end_turn"}}
		]
	}`
	if err := json.Unmarshal([]byte(raw), &page); err != nil {
		t.Fatal(err)
	}
	text, done, maxSeq, err := readAgentTranscriptSince(page, 124)
	if err != nil || !done || maxSeq != 150 {
		t.Fatalf("done=%v maxSeq=%v err=%v", done, maxSeq, err)
	}
	want := `{"name":"Bash","arguments":{"command":"git status","description":"Check status"}}`
	if text != want {
		t.Fatalf("got %q, want %q (pre-tool chatter must be dropped)", text, want)
	}
}

func TestAgentSubmitTimeoutProbesAndRetriesWedgedSend(t *testing.T) {
	oldTimeout, oldMax := agentSubmitAttemptTimeout, agentSubmitMaxAttempts
	agentSubmitAttemptTimeout = 60 * time.Millisecond
	agentSubmitMaxAttempts = 3
	defer func() {
		agentSubmitAttemptTimeout = oldTimeout
		agentSubmitMaxAttempts = oldMax
	}()

	t.Run("retries wedged send when not yet accepted", func(t *testing.T) {
		f, store, acc := bindingSetup(t)
		if _, err := runTurn(t, f, store, acc, "wedged-send", []ChatMessage{u("turn-1")}); err != nil {
			t.Fatal(err)
		}
		release := make(chan struct{})
		sendAttempts := 0
		var eventIDs []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "sendEventToAgentThread") {
				f.mu.Lock()
				sendAttempts++
				attempt := sendAttempts
				f.mu.Unlock()
				if attempt == 1 {
					select {
					case <-release:
					case <-r.Context().Done():
					}
					return
				}
			}
			f.handle(w, r)
		}))
		defer srv.Close()
		defer close(release)
		oldBase := NotionAPIBase
		NotionAPIBase = srv.URL
		defer func() { NotionAPIBase = oldBase }()

		var out strings.Builder
		err := callAgentThreadResumable(acc, []ChatMessage{u("turn-1"), a("reply-1"), u("turn-2")}, "", "", "wedged-send", false, func(text string, _ bool, _ *UsageInfo) {
			out.WriteString(text)
		}, srv.Client(), store)
		if err != nil || out.String() != "reply-2" {
			t.Fatalf("out=%q err=%v", out.String(), err)
		}
		if sendAttempts != 2 {
			t.Fatalf("sendAttempts=%d, want 2", sendAttempts)
		}
		for _, s := range f.sends {
			if id, ok := s["clientEventId"].(string); ok && id != "" {
				eventIDs = append(eventIDs, id)
			}
		}
		if len(eventIDs) != 1 {
			t.Fatalf("expected exactly 1 accepted send on fake server, got %d", len(eventIDs))
		}
	})

	t.Run("does not duplicate send when wedged request was already accepted", func(t *testing.T) {
		f, store, acc := bindingSetup(t)
		if _, err := runTurn(t, f, store, acc, "wedged-accepted", []ChatMessage{u("turn-1")}); err != nil {
			t.Fatal(err)
		}
		release := make(chan struct{})
		sendAttempts := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "sendEventToAgentThread") {
				f.mu.Lock()
				sendAttempts++
				attempt := sendAttempts
				f.mu.Unlock()
				if attempt == 1 {
					rec := httptest.NewRecorder()
					f.handle(rec, r)
					select {
					case <-release:
					case <-r.Context().Done():
					}
					return
				}
			}
			f.handle(w, r)
		}))
		defer srv.Close()
		defer close(release)
		oldBase := NotionAPIBase
		NotionAPIBase = srv.URL
		defer func() { NotionAPIBase = oldBase }()

		var out strings.Builder
		err := callAgentThreadResumable(acc, []ChatMessage{u("turn-1"), a("reply-1"), u("turn-2")}, "", "", "wedged-accepted", false, func(text string, _ bool, _ *UsageInfo) {
			out.WriteString(text)
		}, srv.Client(), store)
		if err != nil || out.String() != "reply-2" {
			t.Fatalf("out=%q err=%v", out.String(), err)
		}
		if sendAttempts != 1 {
			t.Fatalf("sendAttempts=%d, want 1 (probe must detect upstream acceptance without resending)", sendAttempts)
		}
	})
}


