package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- merged from agent_subagent_test.go ---
const subagentParent = "7e35c602-974d-402d-9a00-49dd137d1e63"

func parentMeta(session string) map[string]interface{} {
	return map[string]interface{}{"user_id": `{"device_id":"d","account_uuid":"","session_id":"` + session + `"}`}
}

func agentHeader(id string) http.Header {
	h := http.Header{}
	if id != "" {
		h.Set("x-claude-code-agent-id", id)
	}
	return h
}

// Seam 1 (identity): header + body metadata -> binding identity.
// Catches: main/child confusion, unsafe fallbacks, header validation.
// Misses: the real CLI actually sending this header (verified by Codex locally).
func TestAgentSubagentIdentityFromHeader(t *testing.T) {
	main := anthropicClientSessionIDForRequest(agentHeader(""), parentMeta(subagentParent))
	if main != subagentParent {
		t.Fatalf("main identity must stay the plain client session, got %q", main)
	}
	childA := anthropicClientSessionIDForRequest(agentHeader("a1b2c3d4e5f60718"), parentMeta(subagentParent))
	childA2 := anthropicClientSessionIDForRequest(agentHeader("a1b2c3d4e5f60718"), parentMeta(subagentParent))
	childB := anthropicClientSessionIDForRequest(agentHeader("ffeeddccbbaa0099"), parentMeta(subagentParent))
	otherParent := anthropicClientSessionIDForRequest(agentHeader("a1b2c3d4e5f60718"), parentMeta("another-session"))
	if childA == "" || childA == main {
		t.Fatalf("child must get its own non-empty identity, got %q (main %q)", childA, main)
	}
	if childA != childA2 {
		t.Fatal("same parent+child must be stable across requests")
	}
	if childA == childB {
		t.Fatal("different children of one parent must differ")
	}
	if childA == otherParent {
		t.Fatal("same child id under another parent session must differ")
	}
	if strings.Contains(childA, "a1b2c3d4e5f60718") || strings.Contains(childA, subagentParent) {
		t.Fatalf("derived identity must not embed raw ids: %q", childA)
	}
}

func TestAgentSubagentInvalidHeaderNeverFallsBackToMainThread(t *testing.T) {
	long := strings.Repeat("a", 129)
	for name, id := range map[string]string{
		"too long":       long,
		"space":          "abc def",
		"newline":        "abc\ndef",
		"slash":          "abc/def",
		"non ascii":      "agent-ß",
		"only blanks":    "   ",
		"control":        "abc\x01",
		"colon injected": "x:y",
	} {
		h := http.Header{}
		h.Set("x-claude-code-agent-id", id)
		if got := anthropicClientSessionIDForRequest(h, parentMeta(subagentParent)); got == subagentParent {
			t.Errorf("%s: invalid child header must not be merged into the main thread", name)
		}
	}
	// A child without a usable parent session cannot be bound safely.
	if got := anthropicClientSessionIDForRequest(agentHeader("a1b2c3d4"), nil); got != "" {
		t.Fatalf("child without parent session must have no binding, got %q", got)
	}
}

// Seam 2 (binding): identities produced by the seam-1 helper drive the real
// store + fake Notion server. Catches: forked main history, shared threads,
// lost resume, state leaking raw ids. Misses: real Notion behavior.
func TestAgentSubagentOwnThreadAndNoMainHistoryFork(t *testing.T) {
	f := newFakeAgentServer(t)
	statePath := filepath.Join(t.TempDir(), "bindings.state")
	store := newAgentThreadStore(statePath)
	acc := &Account{UserID: "user", SpaceID: "space"}
	key := func(child string) string {
		return agentClientKey(anthropicClientSessionIDForRequest(agentHeader(child), parentMeta(subagentParent)))
	}
	const childA, childB = "agentA-0001", "agentB-0002"

	mainMsgs := []ChatMessage{sysMsg("main system"), u("MAIN-SECRET-77 read the marker file")}
	if _, err := runKeyed(t, f, store, acc, key(""), "low", true, mainMsgs); err != nil {
		t.Fatal(err)
	}
	subA1 := []ChatMessage{sysMsg("subagent system"), u("SUB-TASK-A read /tmp/marker")}
	if out, err := runKeyed(t, f, store, acc, key(childA), "low", true, subA1); err != nil {
		var be *agentBindingError
		if errors.As(err, &be) {
			t.Fatalf("child request was refused as %q of the main binding: %v", be.Kind, err)
		}
		t.Fatalf("%q %v", out, err)
	}
	subA2 := append(append([]ChatMessage{}, subA1...), a("marker is MK-1"), u("SUB-TASK-A what was the marker?"))
	if _, err := runKeyed(t, f, store, acc, key(childA), "low", true, subA2); err != nil {
		t.Fatal(err)
	}
	subB1 := []ChatMessage{sysMsg("subagent system"), u("SUB-TASK-B independent task")}
	if _, err := runKeyed(t, f, store, acc, key(childB), "low", true, subB1); err != nil {
		t.Fatal(err)
	}
	mainMsgs2 := append(append([]ChatMessage{}, mainMsgs...), a("sub reported MK-1"), u("MAIN-NEXT continue"))
	if _, err := runKeyed(t, f, store, acc, key(""), "low", true, mainMsgs2); err != nil {
		t.Fatal(err)
	}

	// main + A + B => 3 threads; A resumed with a send, not a new chat; main resumed.
	if c, s := f.counts(); c != 3 || s != 2 || threadCount(f) != 3 {
		t.Fatalf("creates=%d sends=%d threads=%d, want 3/2/3", c, s, threadCount(f))
	}
	var created []string
	for _, c := range f.creates {
		raw, _ := json.Marshal(c)
		created = append(created, string(raw))
	}
	if len(created) != 3 {
		t.Fatalf("expected 3 create requests, got %d", len(created))
	}
	for i, want := range []string{"MAIN-SECRET-77", "SUB-TASK-A", "SUB-TASK-B"} {
		if !strings.Contains(created[i], want) {
			t.Fatalf("create %d lacks its own task %q", i, want)
		}
	}
	for i := 1; i < 3; i++ {
		if strings.Contains(created[i], "MAIN-SECRET-77") || strings.Contains(created[i], "MAIN-NEXT") {
			t.Fatalf("subagent create %d forked the main history", i)
		}
	}
	if strings.Contains(created[2], "SUB-TASK-A") || strings.Contains(created[2], "MK-1") {
		t.Fatal("second subagent saw the first subagent's context")
	}
	if f.sends[0]["threadId"] == f.sends[1]["threadId"] {
		t.Fatal("main and child continuation must go to different threads")
	}
	for _, s := range f.sends {
		p := promptText(s["event"].(map[string]interface{})["content"])
		if strings.Contains(p, "SUB-TASK-A") && strings.Contains(p, "MAIN-NEXT") {
			t.Fatal("main and child deltas mixed into one thread")
		}
	}

	state, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{childA, childB, subagentParent} {
		if strings.Contains(string(state), raw) {
			t.Fatalf("binding state stores a raw id %q", raw)
		}
	}
}

func TestAgentSubagentSessionSaltSeparatesChildrenWithIdenticalPrompts(t *testing.T) {
	meta := parentMeta(subagentParent)
	main := anthropicSessionSaltForRequest(agentHeader(""), meta)
	if main != subagentParent {
		t.Fatalf("main salt must be unchanged, got %q", main)
	}
	a := anthropicSessionSaltForRequest(agentHeader("agentA-0001"), meta)
	b := anthropicSessionSaltForRequest(agentHeader("agentB-0002"), meta)
	if a == main || b == main || a == b {
		t.Fatalf("salts must differ: main=%q a=%q b=%q", main, a, b)
	}
	msgs := []ChatMessage{sysMsg("sub"), u("same task")}
	if computeSessionFingerprintForRequest(msgs, a, "sonnet-5.5") == computeSessionFingerprintForRequest(msgs, b, "sonnet-5.5") {
		t.Fatal("identical child prompts must not share one in-memory session")
	}
}

func TestApplyAgentSubagentDefaultsSetsSonnetHigh(t *testing.T) {
	// Main request (no subagent header): unchanged model and effort.
	m, out := applyAgentSubagentDefaults(agentHeader(""), "sonnet-5.5", &AnthropicOutputConfig{Effort: "low"})
	if m != "sonnet-5.5" || out == nil || out.Effort != "low" {
		t.Fatalf("main request mutated: model=%q out=%+v", m, out)
	}
	// Subagent request carrying inherited low effort: promoted to sonnet-5.5 + high.
	m, out = applyAgentSubagentDefaults(agentHeader("agentA-0001"), "sonnet-5.5", &AnthropicOutputConfig{Effort: "low"})
	if m != "sonnet-5.5" || out == nil || out.Effort != "high" {
		t.Fatalf("subagent default failed: model=%q out=%+v", m, out)
	}
	// Subagent request with auto or empty model and nil OutputConfig: defaults to sonnet-5.5 + high.
	m, out = applyAgentSubagentDefaults(agentHeader("agentA-0001"), "auto", nil)
	if m != "sonnet-5.5" || out == nil || out.Effort != "high" {
		t.Fatalf("subagent auto->sonnet-5.5 high failed: model=%q out=%+v", m, out)
	}
	// Subagent request with unsupported model (e.g. haiku or external ocx model): falls back to sonnet-5.5 + high.
	for _, bad := range []string{"haiku", "claude-haiku-4-5-20251001", "ocx-claude-native--gpt-6.1-sol--fast"} {
		m, out = applyAgentSubagentDefaults(agentHeader("agentA-0001"), bad, &AnthropicOutputConfig{Effort: "low"})
		if m != "sonnet-5.5" || out == nil || out.Effort != "high" {
			t.Fatalf("subagent unsupported model %q should fall back to sonnet-5.5 + high, got model=%q out=%+v", bad, m, out)
		}
	}
	// Subagent request with supported non-default model (e.g. opus-5.5): preserves model, sets effort high.
	m, out = applyAgentSubagentDefaults(agentHeader("agentA-0001"), "opus-5.5", &AnthropicOutputConfig{Effort: "low"})
	if m != "opus-5.5" || out == nil || out.Effort != "high" {
		t.Fatalf("subagent opus-5.5 failed: model=%q out=%+v", m, out)
	}
}

// --- merged from agent_subagent_resume_test.go ---
// ASSUMED, not captured on a real CLI (needs Codex's probe): a subagent that the
// main agent resumes (documented to go through SendMessage) sends its follow-up
// request with the SAME x-claude-code-agent-id as before, and its message list
// continues the child's earlier messages. These tests only prove what the proxy
// does GIVEN that shape; they do not prove the CLI produces it.

func subKey(child string) string {
	return agentClientKey(anthropicClientSessionIDForRequest(agentHeader(child), parentMeta(subagentParent)))
}

func threadOfCreate(t *testing.T, f *fakeAgentServer, marker string) string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.creates {
		raw, _ := json.Marshal(c)
		if strings.Contains(string(raw), marker) {
			return c["threadId"].(string)
		}
	}
	t.Fatalf("no create request carries %q", marker)
	return ""
}

func lastSendThread(f *fakeAgentServer) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sends[len(f.sends)-1]["threadId"].(string)
}

func lastSendPrompt(f *fakeAgentServer) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return promptText(f.sends[len(f.sends)-1]["event"].(map[string]interface{})["content"])
}

// Seam 2: same child header later -> same child thread (even across other
// children and a proxy restart); a different child header -> its own new thread;
// the main thread is never touched by child follow-ups.
func TestAgentSubagentFollowUpReusesChildThreadAcrossRestartAndSiblings(t *testing.T) {
	f := newFakeAgentServer(t)
	path := filepath.Join(t.TempDir(), "bindings.state")
	store := newAgentThreadStore(path)
	acc := &Account{UserID: "user", SpaceID: "space"}
	run := func(st *agentThreadStore, key string, msgs []ChatMessage) {
		t.Helper()
		if _, err := runKeyed(t, f, st, acc, key, "low", true, msgs); err != nil {
			t.Fatal(err)
		}
	}
	mainMsgs := []ChatMessage{sysMsg("main system"), u("MAIN-1 delegate to A then B")}
	a1 := []ChatMessage{sysMsg("child system"), u("A-TASK read marker one")}
	b1 := []ChatMessage{sysMsg("child system"), u("B-TASK read marker two")}
	run(store, subKey(""), mainMsgs)
	run(store, subKey("child-A"), a1)
	run(store, subKey("child-B"), b1)
	tMain, tA, tB := threadOfCreate(t, f, "MAIN-1"), threadOfCreate(t, f, "A-TASK"), threadOfCreate(t, f, "B-TASK")
	if tMain == tA || tA == tB || tMain == tB {
		t.Fatalf("main/A/B must own different threads: %s %s %s", tMain, tA, tB)
	}

	// Proxy restart: a new store reads the persisted bindings.
	store = newAgentThreadStore(path)

	a2 := append(append([]ChatMessage{}, a1...), a("A saw MK-1"), u("A-FOLLOW what was the marker?"))
	run(store, subKey("child-A"), a2)
	if got := lastSendThread(f); got != tA {
		t.Fatalf("A follow-up went to %s, want A's thread %s", got, tA)
	}
	if p := lastSendPrompt(f); !strings.Contains(p, "A-FOLLOW") || strings.Contains(p, "MAIN-1") || strings.Contains(p, "B-TASK") {
		t.Fatalf("A delta must carry only A's new message: %q", p)
	}
	b2 := append(append([]ChatMessage{}, b1...), a("B saw MK-2"), u("B-FOLLOW again"))
	run(store, subKey("child-B"), b2)
	if got := lastSendThread(f); got != tB {
		t.Fatalf("B follow-up went to %s, want %s", got, tB)
	}
	main2 := append(append([]ChatMessage{}, mainMsgs...), a("A said MK-1, B said MK-2"), u("MAIN-2 summarize"))
	run(store, subKey(""), main2)
	if got := lastSendThread(f); got != tMain {
		t.Fatalf("main follow-up went to %s, want %s", got, tMain)
	}
	if p := lastSendPrompt(f); strings.Contains(p, "A-FOLLOW") || strings.Contains(p, "B-FOLLOW") || strings.Contains(p, "A-TASK") {
		t.Fatalf("main delta polluted by child content: %q", p)
	}
	// A third, brand-new child gets a fresh thread and sees nothing else.
	run(store, subKey("child-C"), []ChatMessage{sysMsg("child system"), u("C-TASK fresh")})
	tC := threadOfCreate(t, f, "C-TASK")
	if tC == tMain || tC == tA || tC == tB {
		t.Fatal("new child must not reuse an existing thread")
	}
	if c, s := f.counts(); c != 4 || s != 3 || threadCount(f) != 4 {
		t.Fatalf("creates=%d sends=%d threads=%d, want 4/3/4", c, s, threadCount(f))
	}
	f.mu.Lock()
	raw, _ := json.Marshal(f.creates[len(f.creates)-1])
	f.mu.Unlock()
	for _, leak := range []string{"MAIN-1", "A-TASK", "A-FOLLOW", "B-TASK", "MK-1", "MK-2"} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("new child create leaked %q", leak)
		}
	}
}

// A resumed child may arrive with a changed system/tool context. That must not
// open a second chat for the same child header (existing keyed-binding rule).
func TestAgentSubagentFollowUpWithChangedSystemKeepsChildThread(t *testing.T) {
	f, store, acc := bindingSetup(t)
	a1 := []ChatMessage{sysMsg("child system v1"), u("A-TASK one")}
	if _, err := runKeyed(t, f, store, acc, subKey("child-A"), "low", true, a1); err != nil {
		t.Fatal(err)
	}
	tA := threadOfCreate(t, f, "A-TASK")
	a2 := []ChatMessage{sysMsg("child system v2 with SendMessage"), u("A-TASK one"), a("done"), u("A-FOLLOW two")}
	if _, err := runKeyed(t, f, store, acc, subKey("child-A"), "low", true, a2); err != nil {
		t.Fatal(err)
	}
	if c, s := f.counts(); c != 1 || s != 1 || lastSendThread(f) != tA {
		t.Fatalf("creates=%d sends=%d thread=%s want 1/1/%s", c, s, lastSendThread(f), tA)
	}
}

// Wire level: the header may arrive in any case and as the raw lowercase name
// Claude Code sends; the derived identity must still be the child's.
func TestAgentSubagentHeaderOverTheWireIsCaseInsensitive(t *testing.T) {
	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, anthropicClientSessionIDForRequest(r.Header, parentMeta(subagentParent)))
		mu.Unlock()
	}))
	defer srv.Close()
	send := func(name string) {
		req, _ := http.NewRequest("POST", srv.URL, nil)
		if name != "" {
			req.Header[name] = []string{"child-A"} // bypass canonicalization: raw name on the wire
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	send("")
	send("x-claude-code-agent-id")
	send("X-CLAUDE-CODE-AGENT-ID")
	if got[0] != subagentParent {
		t.Fatalf("main identity changed: %q", got[0])
	}
	if got[1] == subagentParent || got[1] == "" || got[1] != got[2] {
		t.Fatalf("lowercase/uppercase child headers must give one child identity: %q %q", got[1], got[2])
	}
}

// Race seam: main and two children running at once never share or duplicate a thread.
func TestAgentSubagentConcurrentMainAndChildrenKeepThreeThreads(t *testing.T) {
	for i := 0; i < 10; i++ {
		f, store, acc := bindingSetup(t)
		var wg sync.WaitGroup
		errs := make(chan error, 3)
		for _, c := range []struct{ child, task string }{{"", "MAIN-X"}, {"child-A", "A-X"}, {"child-B", "B-X"}} {
			wg.Add(1)
			go func(child, task string) {
				defer wg.Done()
				_, err := runKeyed(t, f, store, acc, subKey(child), "low", true, []ChatMessage{sysMsg("s"), u(task)})
				errs <- err
			}(c.child, c.task)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("run %d: %v", i, err)
			}
		}
		if c, _ := f.counts(); c != 3 || threadCount(f) != 3 {
			t.Fatalf("run %d: creates=%d threads=%d, want 3/3", i, c, threadCount(f))
		}
	}
}

// --- merged from agent_subagent_reliability_test.go ---
// Issue #61: exact payload from Notion thread 5df43d65-839f-4f73-9902-001e7d953032
// at seq=78 (event_0djreegeewekjx2ca3y1gxxxcy), where sonnet-5.5 emitted a hybrid
// JSON tool call closed by XML tags `</parameter>\n</invoke>` instead of the outer `}`.
const issue61Seq78Payload = "{\"name\":\"Grep\",\"arguments\":{\"pattern\":\"merge_plan|plan_bug_merge\",\"path\":\"/workspace/example-project\",\"glob\":\"!{artifacts,draft_*,node_modules}/**\",\"output_mode\":\"files_with_matches\",\"head_limit\":40}</parameter>\n</invoke>"

func TestAgentToolTextRepairsTrailingXMLToolClose(t *testing.T) {
	tools := declaredTools{
		"Grep": {Required: []string{"pattern"}},
		"Edit": {Required: []string{"file_path", "old_string", "new_string"}},
	}

	t.Run("issue61_seq78_exact_and_variants", func(t *testing.T) {
		variants := map[string]string{
			"seq78_exact":              issue61Seq78Payload,
			"balanced_with_xml_close":  `{"name":"Grep","arguments":{"pattern":"merge_plan|plan_bug_merge","path":"/workspace/example-project","glob":"!{artifacts,draft_*,node_modules}/**","output_mode":"files_with_matches","head_limit":40}}</parameter>` + "\n</invoke>",
			"two_open_braces_xml":      `{"name":"Grep","arguments":{"pattern":"merge_plan|plan_bug_merge","path":"/workspace/example-project","glob":"!{artifacts,draft_*,node_modules}/**","output_mode":"files_with_matches","head_limit":40</parameter>` + "\n</invoke>",
			"antml_namespace_xml":      `{"name":"Grep","arguments":{"pattern":"merge_plan|plan_bug_merge","path":"/workspace/example-project","glob":"!{artifacts,draft_*,node_modules}/**","output_mode":"files_with_matches","head_limit":40}</antml:parameter>` + "\n</antml:invoke>",
			"xml_plus_invalid_escapes": `{"name":"Grep","arguments":{"pattern":"merge_plan\|plan_bug_merge","path":"/workspace/example-project","glob":"!{artifacts,draft_*,node_modules}/**","output_mode":"files_with_matches","head_limit":40}</parameter>` + "\n</invoke>",
		}
		for name, reply := range variants {
			for _, stream := range []bool{true, false} {
				t.Run(name, func(t *testing.T) {
					res, _ := runToolText(t, reply, tools, stream)
					if len(res.toolUses) != 1 || res.toolUses[0]["name"] != "Grep" || res.errMsg != "" || res.text != "" {
						t.Fatalf("want 1 Grep tool_use, got tools=%v text=%q err=%q", res.toolUses, res.text, res.errMsg)
					}
					in, _ := res.toolUses[0]["input"].(map[string]interface{})
					wantPattern := "merge_plan|plan_bug_merge"
					if name == "xml_plus_invalid_escapes" {
						wantPattern = `merge_plan\|plan_bug_merge`
					}
					if in["pattern"] != wantPattern || in["path"] != "/workspace/example-project" || in["head_limit"] != float64(40) {
						t.Fatalf("unexpected decoded Grep arguments: %#v", in)
					}
				})
			}
		}
	})

	t.Run("preamble_plus_seq78_xml_close", func(t *testing.T) {
		reply := "Let me search for merge_plan references.\n" + issue61Seq78Payload
		for _, stream := range []bool{true, false} {
			res, _ := runToolText(t, reply, tools, stream)
			if len(res.toolUses) != 1 || res.toolUses[0]["name"] != "Grep" || res.errMsg != "" {
				t.Fatalf("want preamble + Grep tool_use, got tools=%v text=%q err=%q", res.toolUses, res.text, res.errMsg)
			}
			if strings.TrimSpace(res.text) != "Let me search for merge_plan references." {
				t.Fatalf("preamble lost or corrupted: %q", res.text)
			}
		}
	})

	t.Run("genuinely_truncated_before_xml_still_fails_loudly", func(t *testing.T) {
		badCases := map[string]string{
			"unclosed_string": `{"name":"Grep","arguments":{"pattern":"merge_plan</parameter>` + "\n</invoke>",
			"trailing_colon":  `{"name":"Grep","arguments":{"pattern":</parameter>` + "\n</invoke>",
			"trailing_comma":  `{"name":"Grep","arguments":{"pattern":"merge_plan",</parameter>` + "\n</invoke>",
			"unclosed_array":  `{"name":"Grep","arguments":{"pattern":["merge_plan"</parameter>` + "\n</invoke>",
		}
		for name, reply := range badCases {
			for _, stream := range []bool{true, false} {
				t.Run(name, func(t *testing.T) {
					res, f := runToolText(t, reply, tools, stream)
					wantLoudFailure(t, res, f)
				})
			}
		}
	})
}

func TestAgentSubagentMalformedToolCallDoesNotWedgeRetryOrSendMessage(t *testing.T) {
	tools := declaredTools{
		"Read": {Required: []string{"file_path"}},
		"Grep": {Required: []string{"pattern"}},
	}

	postMessages := func(t *testing.T, srvURL string, acc *Account, store *agentThreadStore, msgs []ChatMessage, reply string) (int, map[string]interface{}) {
		t.Helper()
		sess := &Session{ClientKey: "subagent:agent-ab11395a83bd1851e", SubagentID: "ab11395a83bd1851e"}
		setSessionTools(sess, tools)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, srvURL+"/v1/messages", bytes.NewReader([]byte("{}")))
		handleAnthropicNonStream(rec, acc, msgs, "sonnet-5.5", "msg_sub", true, false, false, nil, false, nil, nil, sess, req.Context().Done())
		var body map[string]interface{}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return rec.Code, body
	}

	t.Run("follow_up_malformed_rolls_back_for_retry_and_send_message", func(t *testing.T) {
		env := newRecoveryEnv(t)
		store := useRecoveryStore(t, "")
		acc := recAcc()

		// Turn 1 succeeds with a Read tool call.
		env.f.replyFn = func(int) string {
			return `{"name":"Read","arguments":{"file_path":"/work/a.go"}}`
		}
		m1 := []ChatMessage{sysMsg("subagent system"), u("investigate merge_plan")}
		code, body := postMessages(t, env.front.URL, acc, store, m1, "")
		if code != http.StatusOK {
			t.Fatalf("turn 1 failed: code=%d body=%v", code, body)
		}

		// Turn 2 sends tool_result and Notion replies with a genuinely truncated tool call.
		env.f.replyFn = func(int) string {
			return `{"name":"Grep","arguments":{"pattern":"unclosed`
		}
		a1 := ChatMessage{
			Role: "assistant",
			ToolCalls: []ToolCall{{
				ID:       "call_0_0b68d12f",
				Type:     "function",
				Function: ToolCallFunction{Name: "Read", Arguments: `{"file_path":"/work/a.go"}`},
			}},
		}
		tr1 := ChatMessage{Role: "tool", ToolCallID: "call_0_0b68d12f", Name: "Read", Content: "package main"}
		m2 := append(append([]ChatMessage{}, m1...), a1, tr1)

		code, body = postMessages(t, env.front.URL, acc, store, m2, "")
		if code != http.StatusBadGateway || !strings.Contains(stringFromErrorBody(body), "[bridge:malformed_tool_call]") {
			t.Fatalf("turn 2 must fail with 502 malformed_tool_call: code=%d body=%v", code, body)
		}
		if c, s := env.f.counts(); c != 1 || s != 1 {
			t.Fatalf("after failed turn 2: creates=%d sends=%d", c, s)
		}

		// 1. Retrying the identical Turn 2 request must re-send a fresh follow-up
		// event past the broken turn (not replay the broken turn from seq=78).
		env.f.replyFn = func(int) string {
			return `{"name":"Grep","arguments":{"pattern":"merge_plan"}}`
		}
		// Before running the retry to completion, also verify that if Turn 2 failed
		// and the coordinator instead resumed the subagent via SendMessage (appending
		// a user message after tr1), it is accepted as a valid follow-up on the same thread.
		env.f.replyFn = func(int) string {
			return `{"name":"Grep","arguments":{"pattern":"unclosed_again`
		}
		code, _ = postMessages(t, env.front.URL, acc, store, m2, "")
		if code != http.StatusBadGateway {
			t.Fatalf("turn 2 retry with malformed reply must still return 502, got %d", code)
		}
		if c, s := env.f.counts(); c != 1 || s != 2 {
			t.Fatalf("retry must re-send on same thread rather than replaying broken reply: creates=%d sends=%d", c, s)
		}

		// 2. Coordinator resumes the aborted subagent via SendMessage: Claude Code
		// strips the synthetic isApiErrorMessage assistant entry and appends a user
		// message after tr1.
		env.f.replyFn = func(int) string {
			return "Completed the merge_plan investigation."
		}
		mSendMsg := append(append([]ChatMessage{}, m2...), u("Continue and summarize findings without calling Grep."))
		code, body = postMessages(t, env.front.URL, acc, store, mSendMsg, "")
		if code != http.StatusOK {
			t.Fatalf("SendMessage resumption after malformed tool call must succeed, got code=%d body=%v", code, body)
		}
		if c, s := env.f.counts(); c != 1 || s != 3 {
			t.Fatalf("SendMessage must stay on the bound Notion thread: creates=%d sends=%d", c, s)
		}
	})

	t.Run("first_turn_malformed_allows_send_message_continuation", func(t *testing.T) {
		env := newRecoveryEnv(t)
		store := useRecoveryStore(t, "")
		acc := recAcc()

		env.f.replyFn = func(int) string {
			return `{"name":"Grep","arguments":{"pattern":"unclosed`
		}
		m1 := []ChatMessage{sysMsg("subagent system"), u("initial subagent task")}
		code, body := postMessages(t, env.front.URL, acc, store, m1, "")
		if code != http.StatusBadGateway {
			t.Fatalf("want 502 on malformed first turn, got %d (%v)", code, body)
		}

		// Coordinator resumes the subagent via SendMessage after first turn failed.
		env.f.replyFn = func(int) string {
			return "Recovered on first-turn SendMessage."
		}
		m2 := append(append([]ChatMessage{}, m1...), u("Please answer in plain text."))
		code, body = postMessages(t, env.front.URL, acc, store, m2, "")
		if code != http.StatusOK {
			t.Fatalf("SendMessage after failed first turn must succeed: code=%d body=%v", code, body)
		}
		if c, s := env.f.counts(); c != 1 || s != 1 {
			t.Fatalf("must reuse the same thread: creates=%d sends=%d", c, s)
		}
	})
}

func stringFromErrorBody(body map[string]interface{}) string {
	if errObj, ok := body["error"].(map[string]interface{}); ok {
		if msg, ok := errObj["message"].(string); ok {
			return msg
		}
	}
	return ""
}

func buildAgentProgressSummaryPrompt(prev string) string {
	mid := ""
	if prev != "" {
		b, _ := json.Marshal(prev)
		mid = "\nPrevious: " + string(b) + " \u2014 say something NEW.\n"
	}
	return agentProgressSummaryHead + mid + agentProgressSummaryTail
}

func TestAgentSubagentProgressSummaryRefusedWithoutLockOrHijack(t *testing.T) {
	f, store, acc := bindingSetup(t)
	key := "subagent:agent-ab11395a83bd1851e"

	// 1. Before any binding exists: must not create a Notion thread or binding.
	for _, prev := range []string{"", "Reading runAgent.ts"} {
		_, err := runKeyed(t, f, store, acc, key, "low", false, []ChatMessage{
			u("subagent task"),
			a("working"),
			u(buildAgentProgressSummaryPrompt(prev)),
		})
		var be *agentBindingError
		if !errors.As(err, &be) || be.Kind != "diverged" {
			t.Fatalf("want diverged error for progress summary (prev=%q), got %v", prev, err)
		}
	}
	if c, s := f.counts(); c != 0 || s != 0 {
		t.Fatalf("progress summary must not create or send: creates=%d sends=%d", c, s)
	}

	// 2. Bind the subagent on its real first turn.
	m1 := []ChatMessage{sysMsg("tools"), u("subagent task")}
	out1, err := runKeyed(t, f, store, acc, key, "low", true, m1)
	if err != nil {
		t.Fatalf("subagent turn 1 failed: %v", err)
	}

	// 3. While the subagent's thread lock is held (simulating an in-flight 60s turn),
	// AgentSummary must be refused immediately without waiting on the thread lock.
	in := agentPlanInput{ClientKey: key, AccountID: agentAccountKey(acc), SpaceID: "space"}
	store.mu.Lock()
	rec := store.primaryLocked(in)
	lk := store.lockFor(rec.ThreadID)
	store.mu.Unlock()

	oldWait := agentBusyWait
	agentBusyWait = 5 * time.Second
	defer func() { agentBusyWait = oldWait }()

	lk.Lock()
	start := time.Now()
	_, err = runKeyed(t, f, store, acc, key, "low", false, append(append([]ChatMessage{}, m1...), a(out1), u(buildAgentProgressSummaryPrompt("Reading merge_plan.go"))))
	elapsed := time.Since(start)
	lk.Unlock()

	var be *agentBindingError
	if !errors.As(err, &be) || be.Kind != "diverged" {
		t.Fatalf("want diverged while lock held, got %v", err)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("progress summary blocked on thread lock for %v", elapsed)
	}

	// 4. Merged block form (appended as LastTextBlock after a tool_result user message)
	// and while binding is in submitting/submitted states must also be refused without
	// hijacking the binding via unacceptedTurnSuperseded or interruptedAgentEdit.
	mergedSummary := ChatMessage{
		Role:            "user",
		Content:         "<system-reminder>note</system-reminder>" + buildAgentProgressSummaryPrompt(""),
		TextBlocks:      2,
		LastTextBlock:   buildAgentProgressSummaryPrompt(""),
		AfterToolResult: true,
	}
	_, err = runKeyed(t, f, store, acc, key, "low", false, append(append([]ChatMessage{}, m1...), a(out1), mergedSummary))
	if !errors.As(err, &be) || be.Kind != "diverged" {
		t.Fatalf("want diverged for merged block progress summary, got %v", err)
	}

	// 5. Real subagent follow-up turn still succeeds cleanly on the same thread.
	m2 := append(append([]ChatMessage{}, m1...), a(out1), u("next real step"))
	if _, err := runKeyed(t, f, store, acc, key, "low", true, m2); err != nil {
		t.Fatalf("real subagent follow-up failed: %v", err)
	}
	if c, s := f.counts(); c != 1 || s != 1 {
		t.Fatalf("want creates=1 sends=1, got creates=%d sends=%d", c, s)
	}
}
