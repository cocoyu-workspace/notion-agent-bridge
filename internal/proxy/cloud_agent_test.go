package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestAgentCloudRawRetryResume(t *testing.T) {
	oldCfg := AppConfig
	AppConfig = DefaultConfig()
	defer func() { AppConfig = oldCfg }()
	old := NotionAPIBase
	defer func() { NotionAPIBase = old }()
	var mu sync.Mutex
	creates, sends := 0, 0
	var bodies []map[string]interface{}
	seq := 2
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch r.URL.Path {
		case "/createAgentThread":
			creates++
			bodies = append(bodies, body)
			w.WriteHeader(500)
		case "/sendEventToAgentThread":
			sends++
			seq = 4
			bodies = append(bodies, body)
			w.Write([]byte(`{}`))
		default:
			patches := []interface{}{map[string]interface{}{"op": "put", "entity": map[string]interface{}{"id": "answer", "kind": "assistant_message", "sequence": 1, "content": []interface{}{map[string]string{"type": "text", "text": "cloud reply"}}}}, map[string]interface{}{"op": "put", "entity": map[string]interface{}{"id": "done", "kind": "turn_completed", "sequence": 2, "stop_reason": "end_turn"}}, map[string]interface{}{"op": "put", "entity": map[string]interface{}{"id": "native-action", "kind": "tool_result", "sequence": 1.5, "result": map[string]string{"sha": "real-receipt"}}}}
			if seq == 4 {
				patches = append(patches, map[string]interface{}{"op": "put", "entity": map[string]interface{}{"id": "later-answer", "kind": "assistant_message", "sequence": 3, "content": []interface{}{map[string]string{"type": "text", "text": "followup answer"}}}}, map[string]interface{}{"op": "put", "entity": map[string]interface{}{"id": "later-done", "kind": "turn_completed", "sequence": 4, "stop_reason": "end_turn"}})
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"type": agentTranscriptPageType, "session": map[string]interface{}{"status": "idle", "sequence": seq}, "patches": patches})
		}
	}))
	defer upstream.Close()
	NotionAPIBase = upstream.URL
	pool := NewAccountPool()
	a := &Account{UserID: "user", SpaceID: "space", TokenV2: "secret-cookie"}
	pool.AddAccount(a)
	a.EnsureAccountID()
	dir := t.TempDir()
	c := NewCloudAgent(pool, dir)
	c.client = upstream.Client()
	call := func(method, query, body string) map[string]interface{} {
		t.Helper()
		w := httptest.NewRecorder()
		c.ServeHTTP(w, httptest.NewRequest(method, "/v1/cloud-agent"+query, strings.NewReader(body)))
		var v map[string]interface{}
		if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
		v["http"] = w.Code
		return v
	}
	body := `{"requestId":"request-0001","accountId":"` + a.AccountID + `","task":"RAW TASK"}`
	first := call("POST", "", body)
	if first["status"] != "submission_unknown" {
		t.Fatal(first)
	}
	thread := first["threadId"].(string)
	_ = c.Close()
	c = NewCloudAgent(pool, dir)
	c.client = upstream.Client()
	defer c.Close()
	replay := call("POST", "", body)
	if replay["threadId"] != thread || creates != 1 {
		t.Fatal(replay, creates)
	}
	got := call("GET", "?requestId=request-0001", "")
	if got["status"] != "completed" || got["text"] != "cloud reply" || !strings.Contains(string(mustJSON(t, got["progress"])), "real-receipt") {
		t.Fatal(got)
	}
	follow := `{"requestId":"request-0002","threadId":"` + thread + `","task":"RAW FOLLOWUP"}`
	got = call("POST", "", follow)
	if got["threadId"] != thread || sends != 1 {
		t.Fatal(got, sends)
	}
	_ = call("POST", "", follow)
	if sends != 1 {
		t.Fatal("duplicate followup")
	}
	got = call("GET", "?requestId=request-0001", "")
	if got["status"] != "completed" || got["text"] != "cloud reply" || c.records["request-0001"].LastSeq != 2 || strings.Contains(string(mustJSON(t, got["progress"])), "followup answer") {
		t.Fatal("old request polluted", got)
	}
	got = call("GET", "?requestId=request-0002", "")
	if got["text"] != "followup answer" {
		t.Fatal("followup missed", got)
	}
	got = call("POST", "", strings.ReplaceAll(body, "RAW TASK", "CHANGED"))
	if got["http"] != 409 {
		t.Fatal("conflict accepted", got)
	}
	for i, b := range bodies {
		if b["browserEnabled"] != true || b["model"] != "achira-donut" || b["reasoningEffort"] != "high" || b["policies"].(map[string]interface{})["approval_mode"] != "yolo" {
			t.Fatal(b)
		}
		text := string(mustJSON(t, b))
		if strings.Contains(text, "LOCAL") || strings.Contains(text, "Continue") {
			t.Fatal("local wrapper", text)
		}
		if i == 1 && b["threadId"] != thread {
			t.Fatal("forked")
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, ".cloud_agent.state"))
	if err != nil || strings.Contains(string(data), "RAW") || strings.Contains(string(data), "secret-cookie") {
		t.Fatal("private journal content", err)
	}
	st, _ := os.Stat(filepath.Join(dir, ".cloud_agent.state"))
	if st.Mode().Perm() != 0600 {
		t.Fatal(st.Mode())
	}
}
func mustJSON(t *testing.T, v interface{}) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func TestAgentCloudFailClosed(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, ".cloud_agent.state"), []byte(`bad`), 0600)
	c := NewCloudAgent(NewAccountPool(), dir)
	defer c.Close()
	w := httptest.NewRecorder()
	c.ServeHTTP(w, httptest.NewRequest("GET", "/v1/cloud-agent?requestId=anything", nil))
	if w.Code != 503 {
		t.Fatal(w.Code)
	}
	dir = t.TempDir()
	c2 := NewCloudAgent(NewAccountPool(), dir)
	defer c2.Close()
	c3 := NewCloudAgent(NewAccountPool(), dir)
	defer c3.Close()
	if c3.loadErr == nil {
		t.Fatal("two journal owners")
	}
}

func TestAgentCloudCompletedTurnExcludesFollowups(t *testing.T) {
	var p agentTranscriptPage
	if err := json.Unmarshal([]byte(`{"type":"agent_service_transcript_page","session":{"status":"running","sequence":8},"patches":[{"op":"put","entity":{"id":"first","kind":"assistant_message","sequence":1,"content":[{"type":"text","text":"first answer"}]}},{"op":"put","entity":{"id":"first-done","kind":"turn_completed","sequence":2,"stop_reason":"end_turn"}},{"op":"put","entity":{"id":"later","kind":"assistant_message","sequence":3,"content":[{"type":"text","text":"later answer"}]}},{"op":"put","entity":{"id":"failure","kind":"turn_failed","sequence":4}}]}`), &p); err != nil {
		t.Fatal(err)
	}
	fixed := cloudCompletedPage(p, 2)
	text, done, seq, err := readAgentTranscriptSince(fixed, 0)
	if err != nil || !done || seq != 2 || text != "first answer" {
		t.Fatal(text, done, seq, err)
	}
	raw := []json.RawMessage{json.RawMessage(`{"op":"put","entity":{"id":"first-tool","sequence":1,"kind":"tool"}}`), json.RawMessage(`{"op":"patch","id":"first-tool","ops":[{"op":"add","path":"/result","value":"receipt"}]}`), json.RawMessage(`{"op":"put","entity":{"id":"later-tool","sequence":3,"kind":"tool"}}`)}
	progress := cloudTurnProgress(raw, 0, 2)
	if len(progress) != 2 || strings.Contains(string(mustJSON(t, progress)), "later-tool") {
		t.Fatal(progress)
	}
}

func TestAgentCloudInterruptedProof(t *testing.T) {
	for _, tc := range []struct {
		name, status, reason, extra string
		want                        bool
	}{
		{"idle interrupted", "idle", "interrupted", "", true},
		{"still running", "running", "interrupted", "", false},
		{"normal completed", "idle", "end_turn", "", false},
		{"failed", "idle", "error", "", false},
		{"later message", "idle", "interrupted", `,{"op":"put","entity":{"id":"later","kind":"user_message","sequence":3}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var p agentTranscriptPage
			body := `{"type":"agent_service_transcript_page","session":{"status":"` + tc.status + `","sequence":2},"patches":[{"op":"put","entity":{"id":"done","kind":"turn_completed","sequence":2,"stop_reason":"` + tc.reason + `"}}` + tc.extra + `]}`
			if err := json.Unmarshal([]byte(body), &p); err != nil {
				t.Fatal(err)
			}
			if got := cloudInterrupted(p, 0); got != tc.want {
				t.Fatal(got, tc.want)
			}
			p.HasMoreBackward = true
			if cloudInterrupted(p, 0) {
				t.Fatal("incomplete transcript accepted")
			}
		})
	}
}

func TestAgentCloudInterruptedResume(t *testing.T) {
	oldCfg := AppConfig
	AppConfig = DefaultConfig()
	defer func() { AppConfig = oldCfg }()
	old := NotionAPIBase
	defer func() { NotionAPIBase = old }()
	var mu sync.Mutex
	creates, sends := 0, 0
	var bodies []map[string]interface{}
	seq := 2
	sessionStatus := "idle"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch r.URL.Path {
		case "/createAgentThread":
			creates++
			bodies = append(bodies, body)
			w.Write([]byte(`{}`))
		case "/sendEventToAgentThread":
			sends++
			seq = 4
			bodies = append(bodies, body)
			w.Write([]byte(`{}`))
		default:
			patches := []interface{}{map[string]interface{}{"op": "put", "entity": map[string]interface{}{"id": "answer", "kind": "assistant_message", "sequence": 1, "content": []interface{}{map[string]string{"type": "text", "text": "cloud reply"}}}}, map[string]interface{}{"op": "put", "entity": map[string]interface{}{"id": "done", "kind": "turn_completed", "sequence": 2, "stop_reason": "interrupted"}}, map[string]interface{}{"op": "put", "entity": map[string]interface{}{"id": "native-action", "kind": "tool_result", "sequence": 1.5, "result": map[string]string{"sha": "real-receipt"}}}}
			if seq == 4 {
				patches = append(patches, map[string]interface{}{"op": "put", "entity": map[string]interface{}{"id": "later-answer", "kind": "assistant_message", "sequence": 3, "content": []interface{}{map[string]string{"type": "text", "text": "followup answer"}}}}, map[string]interface{}{"op": "put", "entity": map[string]interface{}{"id": "later-done", "kind": "turn_completed", "sequence": 4, "stop_reason": "end_turn"}})
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"type": agentTranscriptPageType, "session": map[string]interface{}{"status": sessionStatus, "sequence": seq}, "patches": patches})
		}
	}))
	defer upstream.Close()
	NotionAPIBase = upstream.URL
	pool := NewAccountPool()
	a := &Account{UserID: "user", SpaceID: "space", TokenV2: "secret-cookie"}
	pool.AddAccount(a)
	a.EnsureAccountID()
	dir := t.TempDir()
	c := NewCloudAgent(pool, dir)
	c.client = upstream.Client()
	call := func(method, query, body string) map[string]interface{} {
		t.Helper()
		w := httptest.NewRecorder()
		c.ServeHTTP(w, httptest.NewRequest(method, "/v1/cloud-agent"+query, strings.NewReader(body)))
		var v map[string]interface{}
		if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
		v["http"] = w.Code
		return v
	}
	body := `{"requestId":"request-0001","accountId":"` + a.AccountID + `","task":"RAW TASK"}`
	first := call("POST", "", body)
	if first["status"] != "submitted" {
		t.Fatal(first)
	}
	thread := first["threadId"].(string)
	_ = c.Close()
	c = NewCloudAgent(pool, dir)
	c.client = upstream.Client()
	defer c.Close()
	replay := call("POST", "", body)
	if replay["threadId"] != thread || creates != 1 {
		t.Fatal(replay, creates)
	}
	got := call("GET", "?requestId=request-0001", "")
	if got["status"] != "interrupted" || got["text"] != "" || !strings.Contains(string(mustJSON(t, got["progress"])), "real-receipt") {
		t.Fatal(got)
	}
	follow := `{"requestId":"request-0002","threadId":"` + thread + `","task":"RAW FOLLOWUP"}`
	mu.Lock()
	sessionStatus = "running"
	mu.Unlock()
	blocked := call("POST", "", follow)
	if blocked["http"] != 409 || sends != 0 {
		t.Fatal("running thread resumed", blocked, sends)
	}
	mu.Lock()
	sessionStatus = "idle"
	mu.Unlock()
	got = call("POST", "", follow)
	if got["http"] != 202 || got["threadId"] != thread || sends != 1 {
		t.Fatal(got, sends)
	}
	_ = call("POST", "", follow)
	if sends != 1 {
		t.Fatal("duplicate followup")
	}
	got = call("GET", "?requestId=request-0001", "")
	if got["status"] != "interrupted" || got["text"] != "" || c.records["request-0001"].LastSeq != 2 || strings.Contains(string(mustJSON(t, got["progress"])), "followup answer") {
		t.Fatal("old request polluted", got)
	}
	got = call("GET", "?requestId=request-0002", "")
	if got["text"] != "followup answer" {
		t.Fatal("followup missed", got)
	}
	got = call("POST", "", strings.ReplaceAll(body, "RAW TASK", "CHANGED"))
	if got["http"] != 409 {
		t.Fatal("conflict accepted", got)
	}
	for i, b := range bodies {
		if b["browserEnabled"] != true || b["model"] != "achira-donut" || b["reasoningEffort"] != "high" || b["policies"].(map[string]interface{})["approval_mode"] != "yolo" {
			t.Fatal(b)
		}
		text := string(mustJSON(t, b))
		if strings.Contains(text, "LOCAL") || strings.Contains(text, "Continue") {
			t.Fatal("local wrapper", text)
		}
		if i == 1 && b["threadId"] != thread {
			t.Fatal("forked")
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, ".cloud_agent.state"))
	if err != nil || strings.Contains(string(data), "RAW") || strings.Contains(string(data), "secret-cookie") {
		t.Fatal("private journal content", err)
	}
	st, _ := os.Stat(filepath.Join(dir, ".cloud_agent.state"))
	if st.Mode().Perm() != 0600 {
		t.Fatal(st.Mode())
	}
}
