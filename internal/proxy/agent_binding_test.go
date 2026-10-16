package proxy

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func sysMsg(s string) ChatMessage { return ChatMessage{Role: "system", Content: s} }

// runKeyed is runTurn with explicit effort and tool flag.
func runKeyed(t *testing.T, f *fakeAgentServer, store *agentThreadStore, acc *Account, key, effort string, tooled bool, messages []ChatMessage) (string, error) {
	t.Helper()
	var out strings.Builder
	err := callAgentThreadResumable(acc, messages, "", effort, key, tooled, func(text string, done bool, _ *UsageInfo) { out.WriteString(text) }, f.server.Client(), store)
	return out.String(), err
}

func bindingSetup(t *testing.T) (*fakeAgentServer, *agentThreadStore, *Account) {
	f := newFakeAgentServer(t)
	return f, newAgentThreadStore(""), &Account{UserID: "user", SpaceID: "space"}
}

func threadCount(f *fakeAgentServer) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.threads)
}

// Deterministic counterexample from issue #11 (previously creates=2, sends=0).
func TestAgentBindingSystemChangeKeepsSingleThread(t *testing.T) {
	f, store, acc := bindingSetup(t)
	if _, err := runTurn(t, f, store, acc, "terminal-session-1", []ChatMessage{sysMsg("system-v1"), u("one")}); err != nil {
		t.Fatal(err)
	}
	out, err := runTurn(t, f, store, acc, "terminal-session-1", []ChatMessage{sysMsg("system-v2"), u("one"), a("reply-1"), u("two")})
	if err != nil || out != "reply-2" {
		t.Fatalf("%q %v", out, err)
	}
	if c, s := f.counts(); c != 1 || s != 1 {
		t.Fatalf("creates=%d sends=%d, want 1/1", c, s)
	}
	prompt := promptText(f.sends[0]["event"].(map[string]interface{})["content"])
	if !strings.Contains(prompt, "system-v2") || !strings.Contains(prompt, "UPDATED CONTEXT") || !strings.Contains(prompt, "two") || strings.Contains(prompt, "system-v1") {
		t.Fatalf("changed system must be sent once as updated context: %q", prompt)
	}
	// Unchanged system on the next turn is not repeated.
	if _, err := runTurn(t, f, store, acc, "terminal-session-1", []ChatMessage{sysMsg("system-v2"), u("one"), a("r"), u("two"), a("r2"), u("three")}); err != nil {
		t.Fatal(err)
	}
	prompt = promptText(f.sends[1]["event"].(map[string]interface{})["content"])
	if strings.Contains(prompt, "system-v2") || strings.Contains(prompt, "UPDATED CONTEXT") {
		t.Fatalf("unchanged system context must not be resent: %q", prompt)
	}
	if c, s := f.counts(); c != 1 || s != 2 {
		t.Fatalf("creates=%d sends=%d", c, s)
	}
}

func TestAgentBindingEffortAndToolsChangeKeepThreadAndRestart(t *testing.T) {
	f := newFakeAgentServer(t)
	path := filepath.Join(t.TempDir(), "b.state")
	store := newAgentThreadStore(path)
	acc := &Account{UserID: "user", SpaceID: "space"}
	call := ToolCall{ID: "call_1", Type: "function", Function: ToolCallFunction{Name: "Read", Arguments: `{"path":"x"}`}}
	h1 := []ChatMessage{sysMsg("tools-v1"), u("read x")}
	h2 := []ChatMessage{sysMsg("tools-v2"), u("read x"), {Role: "assistant", ToolCalls: []ToolCall{call}}, {Role: "tool", ToolCallID: "call_1", Name: "Read", Content: "file text"}}
	h3 := append(append([]ChatMessage{}, h2...), a("answer"), u("next"))
	if _, err := runKeyed(t, f, store, acc, "s", "high", true, h1); err != nil {
		t.Fatal(err)
	}
	if _, err := runKeyed(t, f, store, acc, "s", "low", true, h2); err != nil {
		t.Fatal(err)
	}
	store = newAgentThreadStore(path) // proxy restart
	h3[0] = sysMsg("tools-v3")
	if _, err := runKeyed(t, f, store, acc, "s", "medium", true, h3); err != nil {
		t.Fatal(err)
	}
	if c, s := f.counts(); c != 1 || s != 2 || threadCount(f) != 1 {
		t.Fatalf("creates=%d sends=%d threads=%d", c, s, threadCount(f))
	}
	if f.sends[0]["reasoningEffort"] != "low" || f.sends[1]["reasoningEffort"] != "medium" {
		t.Fatalf("effort must follow each request: %v %v", f.sends[0]["reasoningEffort"], f.sends[1]["reasoningEffort"])
	}
	if p := promptText(f.sends[0]["event"].(map[string]interface{})["content"]); !strings.Contains(p, "file text") || !strings.Contains(p, "tools-v2") {
		t.Fatalf("tool result and updated tools must be delivered: %q", p)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "tools-v") || strings.Contains(string(raw), "file text") {
		t.Fatal("state must not store message text")
	}
}

func TestAgentBindingRejectedFollowUpKeepsBinding(t *testing.T) {
	f, store, acc := bindingSetup(t)
	base := []ChatMessage{u("one")}
	next := []ChatMessage{u("one"), a("x"), u("two")}
	runTurn(t, f, store, acc, "s", base)
	f.rejectSend, f.rejectBody = 400, `{"name":"ValidationError","message":"SECRET-UPSTREAM-TEXT cookie=abc"}`
	var logs bytes.Buffer
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)
	_, err := runTurn(t, f, store, acc, "s", next)
	var be *agentBindingError
	if !errors.As(err, &be) || be.Kind != "rejected" {
		t.Fatalf("want explicit rejected error, got %v", err)
	}
	if !strings.Contains(err.Error(), "HTTP 400") || !strings.Contains(err.Error(), "ValidationError") {
		t.Fatalf("safe machine code expected: %v", err)
	}
	if strings.Contains(err.Error()+logs.String(), "SECRET-UPSTREAM-TEXT") || strings.Contains(logs.String(), "cookie") {
		t.Fatal("upstream free text leaked into error/log")
	}
	if c, _ := f.counts(); c != 1 || threadCount(f) != 1 {
		t.Fatalf("rejection must not create a thread: creates=%d", c)
	}
	// Binding is intact: the same request works once upstream recovers.
	f.rejectSend = 0
	out, err := runTurn(t, f, store, acc, "s", next)
	if err != nil || out != "reply-2" {
		t.Fatalf("%q %v", out, err)
	}
	if c, s := f.counts(); c != 1 || s != 1 {
		t.Fatalf("creates=%d sends=%d", c, s)
	}
	// And the following turn continues too.
	if _, err := runTurn(t, f, store, acc, "s", append(next, a("y"), u("three"))); err != nil {
		t.Fatal(err)
	}
	if c, s := f.counts(); c != 1 || s != 2 {
		t.Fatalf("creates=%d sends=%d", c, s)
	}
}

func TestAgentBindingServerErrorAndTimeoutDoNotFork(t *testing.T) {
	f, store, acc := bindingSetup(t)
	runTurn(t, f, store, acc, "s", []ChatMessage{u("one")})
	next := []ChatMessage{u("one"), a("x"), u("two")}
	f.rejectSend = 503
	if _, err := runTurn(t, f, store, acc, "s", next); err == nil || !strings.Contains(err.Error(), "binding kept") {
		t.Fatalf("5xx must surface as explicit kept-binding error: %v", err)
	}
	f.rejectSend = 0
	if _, err := runTurn(t, f, store, acc, "s", next); err != nil {
		t.Fatal(err)
	}
	if c, s := f.counts(); c != 1 || s != 1 {
		t.Fatalf("creates=%d sends=%d", c, s)
	}
	// Timeout while waiting for the reply: the retry must not re-send.
	f.stall = true
	next2 := append(append([]ChatMessage{}, next...), a("y"), u("three"))
	old := AppConfig.Timeouts.InferenceTimeout
	AppConfig.Timeouts.InferenceTimeout = 1
	_, err := runTurn(t, f, store, acc, "s", next2)
	AppConfig.Timeouts.InferenceTimeout = old
	if err == nil {
		t.Fatal("stalled turn should time out")
	}
	// A different request while the previous one is unfinished is refused, not forked.
	_, err = runTurn(t, f, store, acc, "s", append(append([]ChatMessage{}, next2...), a("z"), u("four")))
	var be *agentBindingError
	if !errors.As(err, &be) || be.Kind != "pending" {
		t.Fatalf("want pending error, got %v", err)
	}
	f.stall = false
	f.mu.Lock()
	for _, th := range f.threads {
		f.finishLocked(th)
	}
	f.mu.Unlock()
	if _, err := runTurn(t, f, store, acc, "s", next2); err != nil {
		t.Fatal(err)
	}
	if c, s := f.counts(); c != 1 || s != 2 || threadCount(f) != 1 {
		t.Fatalf("creates=%d sends=%d", c, s)
	}
}

func TestAgentBindingConcurrentSameSessionSerializes(t *testing.T) {
	f, store, acc := bindingSetup(t)
	runTurn(t, f, store, acc, "s", []ChatMessage{u("one")})
	next := []ChatMessage{u("one"), a("x"), u("two")}
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = runTurn(t, f, store, acc, "s", next)
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if c, s := f.counts(); c != 1 || s != 1 || threadCount(f) != 1 {
		t.Fatalf("identical concurrent requests: creates=%d sends=%d", c, s)
	}
	// Different request while another is running: waits, then is refused or continues; never forks.
	old := agentBusyWait
	agentBusyWait = 50 * time.Millisecond
	defer func() { agentBusyWait = old }()
	store.mu.Lock()
	var rec *agentThreadRecord
	for _, r := range store.records {
		rec = r
	}
	lk := store.lockFor(rec.ThreadID)
	store.mu.Unlock()
	lk.Lock()
	_, err := runTurn(t, f, store, acc, "s", append(next, a("y"), u("three")))
	lk.Unlock()
	var be *agentBindingError
	if !errors.As(err, &be) || be.Kind != "busy" {
		t.Fatalf("want busy error, got %v", err)
	}
	if c, _ := f.counts(); c != 1 {
		t.Fatalf("busy must not fork: %d", c)
	}
}

func TestAgentBindingIsolationAcrossSessionsAccountsWorkspaces(t *testing.T) {
	f, store, acc := bindingSetup(t)
	msgs := []ChatMessage{u("hello")}
	follow := []ChatMessage{u("hello"), a("x"), u("more")}
	runTurn(t, f, store, acc, "A", msgs)
	runTurn(t, f, store, acc, "B", msgs)
	runTurn(t, f, store, &Account{UserID: "other", SpaceID: "space"}, "A", msgs)
	runTurn(t, f, store, &Account{UserID: "user", SpaceID: "space2"}, "A", msgs)
	if c, _ := f.counts(); c != 4 {
		t.Fatalf("four identities need four threads, got %d", c)
	}
	runTurn(t, f, store, acc, "A", follow)
	runTurn(t, f, store, acc, "B", follow)
	if c, s := f.counts(); c != 4 || s != 2 {
		t.Fatalf("creates=%d sends=%d", c, s)
	}
	if f.sends[0]["threadId"] == f.sends[1]["threadId"] {
		t.Fatal("sessions share a thread")
	}
}

// Issue #11 review round 2: one client session owns exactly one Notion thread
// (per account/workspace). No branch may create a second chat.
func TestAgentBindingHelperAfterMainIsRefusedWithoutCreatingChat(t *testing.T) {
	f, store, acc := bindingSetup(t)
	main1 := []ChatMessage{sysMsg("main"), u("build feature")}
	if _, err := runKeyed(t, f, store, acc, "s", "low", true, main1); err != nil {
		t.Fatal(err)
	}
	for name, msgs := range map[string][]ChatMessage{
		"tool-less helper":               {sysMsg("title writer"), u("Summarize as a title")},
		"edited first AND tools removed": {u("edited feature"), a("reply-1"), u("two")},
		"tool-less compaction summary":   {u("summary of the conversation so far")},
	} {
		_, err := runKeyed(t, f, store, acc, "s", "low", false, msgs)
		var be *agentBindingError
		if !errors.As(err, &be) || be.Kind != "diverged" {
			t.Fatalf("%s: want explicit refusal, got %v", name, err)
		}
	}
	if c, s := f.counts(); c != 1 || s != 0 || threadCount(f) != 1 {
		t.Fatalf("helpers must not create chats: creates=%d sends=%d threads=%d", c, s, threadCount(f))
	}
	// The main conversation is undisturbed.
	main2 := []ChatMessage{sysMsg("main"), u("build feature"), a("done"), u("continue")}
	out, err := runKeyed(t, f, store, acc, "s", "low", true, main2)
	if err != nil || out != "reply-2" {
		t.Fatalf("%q %v", out, err)
	}
	if c, s := f.counts(); c != 1 || s != 1 {
		t.Fatalf("creates=%d sends=%d", c, s)
	}
	if store.primaryLocked(agentPlanInput{ClientKey: "s", AccountID: agentAccountKey(acc), SpaceID: "space"}).Turns != 2 {
		t.Fatal("main binding must have two completed turns")
	}
}

func TestAgentBindingHelperFirstRebindsInPlaceWithoutSecondChat(t *testing.T) {
	f, store, acc := bindingSetup(t)
	if _, err := runKeyed(t, f, store, acc, "s", "low", false, []ChatMessage{u("title please")}); err != nil {
		t.Fatal(err)
	}
	m1 := []ChatMessage{sysMsg("tools"), u("real work")}
	out, err := runKeyed(t, f, store, acc, "s", "low", true, m1)
	if err != nil || out != "reply-2" {
		t.Fatalf("main must see only its own reply, not the helper's: %q %v", out, err)
	}
	if _, err := runKeyed(t, f, store, acc, "s", "low", true, append(append([]ChatMessage{}, m1...), a("ok"), u("more"))); err != nil {
		t.Fatal(err)
	}
	if c, s := f.counts(); c != 1 || s != 2 || threadCount(f) != 1 {
		t.Fatalf("creates=%d sends=%d threads=%d", c, s, threadCount(f))
	}
	p := promptText(f.sends[0]["event"].(map[string]interface{})["content"])
	if !strings.Contains(p, "real work") || !strings.Contains(p, "tools") || !strings.Contains(p, "unrelated") {
		t.Fatalf("rebind message must carry the whole main conversation and mark earlier content unrelated: %q", p)
	}
	// A later helper cannot take the binding back or create a chat.
	_, err = runKeyed(t, f, store, acc, "s", "low", false, []ChatMessage{u("another helper")})
	var be *agentBindingError
	if !errors.As(err, &be) || be.Kind != "diverged" {
		t.Fatalf("got %v", err)
	}
	if c, _ := f.counts(); c != 1 {
		t.Fatalf("creates=%d", c)
	}
}

func TestAgentBindingHelperAndMainConcurrentNeverCreateTwoChats(t *testing.T) {
	for i := 0; i < 20; i++ {
		f, store, acc := bindingSetup(t)
		var wg sync.WaitGroup
		var mainErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			runKeyed(t, f, store, acc, "s", "low", false, []ChatMessage{u("title please")})
		}()
		go func() {
			defer wg.Done()
			_, mainErr = runKeyed(t, f, store, acc, "s", "low", true, []ChatMessage{sysMsg("tools"), u("real work")})
		}()
		wg.Wait()
		if mainErr != nil {
			t.Fatalf("run %d: main request must succeed in either order: %v", i, mainErr)
		}
		if c, _ := f.counts(); c != 1 || threadCount(f) != 1 {
			t.Fatalf("run %d: creates=%d threads=%d", i, c, threadCount(f))
		}
	}
}

func TestAgentBindingNoConversationMessagesNeverCreatesChat(t *testing.T) {
	f, store, acc := bindingSetup(t)
	runKeyed(t, f, store, acc, "s", "low", true, []ChatMessage{u("one")})
	if _, err := runKeyed(t, f, store, acc, "s", "low", true, []ChatMessage{sysMsg("only system")}); err == nil {
		t.Fatal("expected refusal")
	}
	if c, _ := f.counts(); c != 1 {
		t.Fatalf("creates=%d", c)
	}
}

func TestAgentBindingFailedFirstTurnKeepsThreadAndRetriesInIt(t *testing.T) {
	f, store, acc := bindingSetup(t)
	msgs := []ChatMessage{u("boom")}
	f.failTurn = true
	if _, err := runKeyed(t, f, store, acc, "s", "low", true, msgs); err == nil {
		t.Fatal("failure accepted")
	}
	f.failTurn = false
	out, err := runKeyed(t, f, store, acc, "s", "low", true, msgs)
	if err != nil || out == "" {
		t.Fatalf("%q %v", out, err)
	}
	if c, s := f.counts(); c != 1 || s != 1 || threadCount(f) != 1 {
		t.Fatalf("a failed first turn must retry inside the same thread: creates=%d sends=%d", c, s)
	}
	if p := promptText(f.sends[0]["event"].(map[string]interface{})["content"]); !strings.Contains(p, "boom") {
		t.Fatalf("retry must carry the full first request: %q", p)
	}
	// The conversation continues normally afterwards.
	if _, err := runKeyed(t, f, store, acc, "s", "low", true, []ChatMessage{u("boom"), a("x"), u("next")}); err != nil {
		t.Fatal(err)
	}
	if c, s := f.counts(); c != 1 || s != 2 {
		t.Fatalf("creates=%d sends=%d", c, s)
	}
}

func TestAgentBindingUnreadableBoundThreadIsNotReplaced(t *testing.T) {
	f, store, acc := bindingSetup(t)
	msgs := []ChatMessage{u("one")}
	runKeyed(t, f, store, acc, "s", "low", true, msgs)
	f.mu.Lock()
	f.threads = map[string]*fakeThread{} // transcript now answers 404
	f.mu.Unlock()
	if _, err := runKeyed(t, f, store, acc, "s", "low", true, msgs); err == nil {
		t.Fatal("expected an explicit error")
	}
	if c, _ := f.counts(); c != 1 {
		t.Fatalf("creates=%d", c)
	}
}

func TestAgentThreadModelRejectsHelperModelsBeforeAnyChatIsCreated(t *testing.T) {
	// Claude Code runs its side queries on the small/fast (haiku) model. Only
	// auto/opus-5.5/sonnet-5.5 map to the Agent protocol, so such a request is
	// rejected before callAgentThreadResumable can create a remote chat.
	for _, model := range []string{"claude-haiku-4-5", "haiku-4.5", "claude-3-5-haiku-20241022"} {
		if _, err := agentThreadModelID(model); err == nil {
			t.Fatalf("%s must be rejected by the Agent transport", model)
		}
	}
}

func TestAgentBindingDivergedRequestsAreRefusedNotForked(t *testing.T) {
	f, store, acc := bindingSetup(t)
	runTurn(t, f, store, acc, "s", []ChatMessage{u("hello")})
	good := []ChatMessage{u("hello"), a("x"), u("more")}
	runTurn(t, f, store, acc, "s", good)
	cases := map[string][]ChatMessage{
		"edited last":   {u("hello"), a("x"), u("edited")},
		"rollback":      {u("hello"), a("x"), u("other branch"), a("y"), u("next")},
		"no reply echo": {u("hello"), u("two users")},
		"truncated":     {u("hello")},
	}
	for name, msgs := range cases {
		_, err := runTurn(t, f, store, acc, "s", msgs)
		var be *agentBindingError
		if name == "truncated" {
			// Same conversation, older state: an exact replay of turn 1 is a retry.
			continue
		}
		if !errors.As(err, &be) || be.Kind != "diverged" {
			t.Fatalf("%s: want diverged error, got %v", name, err)
		}
	}
	if c, s := f.counts(); c != 1 || s != 1 || threadCount(f) != 1 {
		t.Fatalf("creates=%d sends=%d", c, s)
	}
	// The binding still continues.
	if _, err := runTurn(t, f, store, acc, "s", append(good, a("y"), u("three"))); err != nil {
		t.Fatal(err)
	}
	if c, s := f.counts(); c != 1 || s != 2 {
		t.Fatalf("creates=%d sends=%d", c, s)
	}
}

func TestAgentBindingFailedFollowUpTurnKeepsBinding(t *testing.T) {
	f, store, acc := bindingSetup(t)
	runTurn(t, f, store, acc, "s", []ChatMessage{u("one")})
	next := []ChatMessage{u("one"), a("x"), u("two")}
	f.failTurn = true
	if _, err := runTurn(t, f, store, acc, "s", next); err == nil {
		t.Fatal("failed turn accepted")
	}
	f.failTurn = false
	out, err := runTurn(t, f, store, acc, "s", next)
	if err != nil || out == "" {
		t.Fatalf("%q %v", out, err)
	}
	if c, s := f.counts(); c != 1 || s != 2 || threadCount(f) != 1 {
		t.Fatalf("failure must keep the binding: creates=%d sends=%d", c, s)
	}
	if f.sends[0]["clientEventId"] == f.sends[1]["clientEventId"] {
		t.Fatal("a retry after a failed turn needs a fresh client event id")
	}
}

func TestAgentBindingSameRequestRetryProbesInsteadOfResending(t *testing.T) {
	f, store, acc := bindingSetup(t)
	runTurn(t, f, store, acc, "s", []ChatMessage{u("one")})
	next := []ChatMessage{u("one"), a("x"), u("two")}
	f.dropAckSend = true // upstream accepts the message but the proxy sees a failure
	if _, err := runTurn(t, f, store, acc, "s", next); err == nil {
		t.Fatal("expected ack failure")
	}
	f.dropAckSend = false
	if _, err := runTurn(t, f, store, acc, "s", next); err != nil {
		t.Fatal(err)
	}
	if c, s := f.counts(); c != 1 || s != 1 {
		t.Fatalf("accepted-but-unacknowledged send must not be repeated: creates=%d sends=%d", c, s)
	}
}

func TestExtractAnthropicClientSessionID(t *testing.T) {
	cases := []struct {
		name string
		meta map[string]interface{}
		want string
	}{
		{"claude code json", map[string]interface{}{"user_id": `{"device_id":"d","session_id":"sess-9"}`}, "sess-9"},
		{"plain user id is a user not a session", map[string]interface{}{"user_id": "user-123"}, ""},
		{"json without session", map[string]interface{}{"user_id": `{"device_id":"d"}`}, ""},
		{"explicit session id", map[string]interface{}{"session_id": "s-1"}, "s-1"},
		{"conversation id", map[string]interface{}{"conversation_id": "c-1"}, "c-1"},
		{"cache key is never an identity", map[string]interface{}{"prompt_cache_key": "shared-cache"}, ""},
		{"cache key must not beat real session A", map[string]interface{}{"prompt_cache_key": "shared-cache", "user_id": `{"device_id":"d","session_id":"terminal-A"}`}, "terminal-A"},
		{"cache key must not beat real session B", map[string]interface{}{"prompt_cache_key": "shared-cache", "user_id": `{"device_id":"d","session_id":"terminal-B"}`}, "terminal-B"},
		{"json session beats explicit fields", map[string]interface{}{"session_id": "explicit", "user_id": `{"session_id":"from-user-id"}`}, "from-user-id"},
		{"plain user id plus session id", map[string]interface{}{"user_id": "user-1", "session_id": "s-2"}, "s-2"},
		{"empty", nil, ""},
	}
	for _, c := range cases {
		if got := extractAnthropicClientSessionID(c.meta); got != c.want {
			t.Errorf("%s: %q want %q", c.name, got, c.want)
		}
	}
}

func TestAgentErrorCodeFromBodyKeepsOnlyMachineCodes(t *testing.T) {
	cases := map[string]string{
		`{"name":"ValidationError","message":"free text"}`:      "ValidationError",
		`{"error":{"code":"invalid_request","message":"x y"}}`:  "invalid_request",
		`{"message":"only text, with spaces and a cookie=abc"}`: "",
		`{"name":"has spaces and secrets"}`:                     "",
		`not json`:                                              "",
		`{"code":"` + strings.Repeat("A", 200) + `"}`:           "",
		`{"error":"rate_limited"}`:                              "rate_limited",
	}
	for body, want := range cases {
		if got := agentErrorCodeFromBody([]byte(body)); got != want {
			t.Errorf("%q: got %q want %q", body, got, want)
		}
	}
}

// Review round 1 (PR #12): an edited first message of a tool-using main
// conversation is ambiguous (edit/rollback/compaction) and must never be
// guessed to be a helper request.
func TestAgentBindingEditedFirstMessageIsRefusedNotForked(t *testing.T) {
	f, store, acc := bindingSetup(t)
	if _, err := runKeyed(t, f, store, acc, "s", "low", true, []ChatMessage{u("one")}); err != nil {
		t.Fatal(err)
	}
	_, err := runKeyed(t, f, store, acc, "s", "low", true, []ChatMessage{u("edited-one"), a("reply-1"), u("two")})
	var be *agentBindingError
	if !errors.As(err, &be) || be.Kind != "diverged" {
		t.Fatalf("want diverged error, got %v", err)
	}
	if c, s := f.counts(); c != 1 || s != 0 || threadCount(f) != 1 {
		t.Fatalf("creates=%d sends=%d", c, s)
	}
	// Binding intact.
	if _, err := runKeyed(t, f, store, acc, "s", "low", true, []ChatMessage{u("one"), a("reply-1"), u("two")}); err != nil {
		t.Fatal(err)
	}
	if c, s := f.counts(); c != 1 || s != 1 {
		t.Fatalf("creates=%d sends=%d", c, s)
	}
}

func TestAgentBindingToollessChatDifferentRootIsRefused(t *testing.T) {
	f, store, acc := bindingSetup(t)
	runKeyed(t, f, store, acc, "plain", "low", false, []ChatMessage{u("one")})
	runKeyed(t, f, store, acc, "plain", "low", false, []ChatMessage{u("one"), a("r"), u("two")})
	var be *agentBindingError
	_, err := runKeyed(t, f, store, acc, "plain", "low", false, []ChatMessage{u("totally different"), a("r"), u("x")})
	if !errors.As(err, &be) || be.Kind != "diverged" {
		t.Fatalf("tool-less vs tool-less different root must be refused, got %v", err)
	}
	// Tool-using request against a multi-turn tool-less binding: no in-place rebind either.
	_, err = runKeyed(t, f, store, acc, "plain", "low", true, []ChatMessage{u("new root"), a("r"), u("x")})
	if !errors.As(err, &be) || be.Kind != "diverged" {
		t.Fatalf("rebind only allowed for a fresh helper binding, got %v", err)
	}
	if c, _ := f.counts(); c != 1 {
		t.Fatalf("creates=%d", c)
	}
}

func TestAgentBindingLegacyRecordWithoutRootHash(t *testing.T) {
	f, store, acc := bindingSetup(t)
	base := []ChatMessage{u("one")}
	runKeyed(t, f, store, acc, "s", "low", true, base)
	for _, rec := range store.records { // simulate a state file written before root_hash existed
		rec.RootHash = ""
	}
	// Ambiguous request: refused explicitly, never silently forked.
	for name, msgs := range map[string][]ChatMessage{
		"edited first": {u("edited"), a("r"), u("two")},
	} {
		_, err := runKeyed(t, f, store, acc, "s", "low", true, msgs)
		var be *agentBindingError
		if !errors.As(err, &be) || be.Kind != "diverged" {
			t.Fatalf("%s: want diverged error, got %v", name, err)
		}
	}
	_, err := runKeyed(t, f, store, acc, "s", "low", false, []ChatMessage{u("some helper")})
	var be *agentBindingError
	if !errors.As(err, &be) || be.Kind != "diverged" {
		t.Fatalf("unrecognizable request against a legacy binding must be refused, got %v", err)
	}
	if c, _ := f.counts(); c != 1 {
		t.Fatalf("creates=%d", c)
	}
	// A true continuation still works and backfills the root hash.
	if _, err := runKeyed(t, f, store, acc, "s", "low", true, []ChatMessage{u("one"), a("r"), u("two")}); err != nil {
		t.Fatal(err)
	}
	for _, rec := range store.records {
		if rec.RootHash == "" {
			t.Fatal("root hash must be backfilled after a verified continuation")
		}
	}
	if c, s := f.counts(); c != 1 || s != 1 {
		t.Fatalf("creates=%d sends=%d", c, s)
	}
	// With the root known, a helper is still refused (never a second chat).
	if _, err := runKeyed(t, f, store, acc, "s", "low", false, []ChatMessage{u("some helper")}); err == nil {
		t.Fatal("helper must be refused")
	}
	if c, _ := f.counts(); c != 1 {
		t.Fatalf("creates=%d", c)
	}
}

// Review round 1 (PR #12): the typed upstream error must survive the binding
// wrapper so authoritative quota confirmation still drives account failover.
func TestAgentBindingRejectedFollowUpKeepsTypedCauseForQuota(t *testing.T) {
	for _, status := range []int{402, 403} {
		f, store, acc := bindingSetup(t)
		runKeyed(t, f, store, acc, "s", "low", true, []ChatMessage{u("one")})
		f.rejectSend = status
		next := []ChatMessage{u("one"), a("x"), u("two")}

		calls := stubQuota(t, false, nil)
		_, err := runKeyed(t, f, store, acc, "s", "low", true, next)
		var be *agentBindingError
		var he *agentHTTPError
		if !errors.As(err, &be) || !errors.As(err, &he) || he.Status != status {
			t.Fatalf("%d: binding error must unwrap to agentHTTPError: %v", status, err)
		}
		if got := classifyAgentQuotaFailure(acc, err); !errors.Is(got, ErrQuotaExhausted) {
			t.Fatalf("%d: confirmed ineligible account must classify as quota exhausted: %v", status, got)
		}
		if *calls != 1 {
			t.Fatalf("quota API is the only authority, calls=%d", *calls)
		}
		// Without authoritative confirmation nothing changes.
		stubQuota(t, true, nil)
		if got := classifyAgentQuotaFailure(acc, err); errors.Is(got, ErrQuotaExhausted) {
			t.Fatalf("%d: eligible account must not be marked exhausted", status)
		}
		stubQuota(t, false, errors.New("quota api down"))
		if got := classifyAgentQuotaFailure(acc, err); errors.Is(got, ErrQuotaExhausted) {
			t.Fatalf("%d: unverifiable quota must not switch accounts", status)
		}
		// Binding was restored.
		f.rejectSend = 0
		if _, err := runKeyed(t, f, store, acc, "s", "low", true, next); err != nil {
			t.Fatal(err)
		}
		if c, s := f.counts(); c != 1 || s != 1 {
			t.Fatalf("%d: creates=%d sends=%d", status, c, s)
		}
	}
}

func TestAgentBindingFollowUp500ThenProbeTurnFailedRestoresBinding(t *testing.T) {
	f, store, acc := bindingSetup(t)
	if _, err := runKeyed(t, f, store, acc, "s", "low", true, []ChatMessage{u("one")}); err != nil {
		t.Fatal(err)
	}
	next := []ChatMessage{u("one"), a("reply-1"), u("two")}
	f.dropAckSend = true
	f.failTurn = true
	if _, err := runKeyed(t, f, store, acc, "s", "low", true, next); err == nil {
		t.Fatal("want 500 dropAckSend failure")
	}
	// On retry, probe sees the dropped-ack turn ended in turn_failed and rolls back.
	f.dropAckSend = false
	f.failTurn = false
	if _, err := runKeyed(t, f, store, acc, "s", "low", true, next); err == nil {
		t.Fatal("probe of failed turn must report failure and roll back")
	}
	if len(store.records) != 1 {
		t.Fatalf("probe rollback must not discard the bound thread: records=%d", len(store.records))
	}
	out, err := runKeyed(t, f, store, acc, "s", "low", true, next)
	if err != nil || out != "reply-3" {
		t.Fatalf("retry after probe rollback must succeed on the same thread: out=%q err=%v", out, err)
	}
	if c, _ := f.counts(); c != 1 {
		t.Fatalf("creates=%d, want 1", c)
	}
}

func TestAgentBindingFollowUp500ThenRejectedRetryRestoresBinding(t *testing.T) {
	f, store, acc := bindingSetup(t)
	if _, err := runKeyed(t, f, store, acc, "s", "low", true, []ChatMessage{u("one")}); err != nil {
		t.Fatal(err)
	}
	bad := []ChatMessage{u("one"), a("reply-1"), u("two-bad")}
	f.rejectSend = 500
	if _, err := runKeyed(t, f, store, acc, "s", "low", true, bad); err == nil {
		t.Fatal("want 500 failure")
	}
	f.rejectSend = 400
	f.rejectBody = `{"code":"UserValidationError"}`
	if _, err := runKeyed(t, f, store, acc, "s", "low", true, bad); err == nil {
		t.Fatal("want 400 failure")
	}
	f.rejectSend = 0
	f.rejectBody = ""
	fixed := []ChatMessage{u("one"), a("reply-1"), u("two-fixed")}
	out, err := runKeyed(t, f, store, acc, "s", "low", true, fixed)
	if err != nil || out != "reply-2" {
		t.Fatalf("400 after 500 must restore the previous completed turn: out=%q err=%v", out, err)
	}
	if c, _ := f.counts(); c != 1 {
		t.Fatalf("creates=%d, want 1", c)
	}
}

// Claude Code inserts a "<total_tokens>N tokens left</total_tokens>" system entry
// at every turn (the count changes). It is bookkeeping, not project context: it
// must not make every follow-up resend the whole system/tool context.
func TestAgentBindingTokenBudgetEntriesDoNotResendSystemContext(t *testing.T) {
	f, store, acc := bindingSetup(t)
	budget := func(n int) ChatMessage { return sysMsg(fmt.Sprintf("<total_tokens>%d tokens left</total_tokens>", n)) }
	if _, err := runTurn(t, f, store, acc, "budget", []ChatMessage{sysMsg("system-v1"), u("one"), budget(15000000)}); err != nil {
		t.Fatal(err)
	}
	if _, err := runTurn(t, f, store, acc, "budget", []ChatMessage{sysMsg("system-v1"), u("one"), budget(15000000), a("reply-1"), u("two"), budget(14998000)}); err != nil {
		t.Fatal(err)
	}
	prompt := promptText(f.sends[0]["event"].(map[string]interface{})["content"])
	if strings.Contains(prompt, "UPDATED CONTEXT") || strings.Contains(prompt, "system-v1") || !strings.Contains(prompt, "two") {
		t.Fatalf("a new token-budget entry must not resend the system context: %q", prompt)
	}
	// A real change inside an entry that also carries the budget line still counts.
	if _, err := runTurn(t, f, store, acc, "budget", []ChatMessage{sysMsg("system-v1"), u("one"), budget(15000000), a("r"), u("two"), budget(14998000), a("r2"), u("three"), sysMsg("<total_tokens>14990000 tokens left</total_tokens>\n\nAvailable agent types: x")}); err != nil {
		t.Fatal(err)
	}
	prompt = promptText(f.sends[1]["event"].(map[string]interface{})["content"])
	if !strings.Contains(prompt, "UPDATED CONTEXT") || !strings.Contains(prompt, "Available agent types: x") {
		t.Fatalf("a real system change must still be sent: %q", prompt)
	}
	if c, s := f.counts(); c != 1 || s != 2 {
		t.Fatalf("creates=%d sends=%d", c, s)
	}
}

// Notion records a provider_handoff ("model changed to ...") for every follow-up
// event that carries a model field, even the thread's own model. The binding is
// per model, so follow-ups omit it and keep only the effort.
func TestAgentFollowUpOmitsModelButKeepsEffort(t *testing.T) {
	f, store, acc := bindingSetup(t)
	run := func(effort string, msgs ...ChatMessage) {
		t.Helper()
		if err := callAgentThreadResumable(acc, msgs, "achira-donut", effort, "model-key", false, func(string, bool, *UsageInfo) {}, f.server.Client(), store); err != nil {
			t.Fatal(err)
		}
	}
	run("low", u("one"))
	run("high", u("one"), a("reply-1"), u("two"))
	if c, s := f.counts(); c != 1 || s != 1 {
		t.Fatalf("creates=%d sends=%d", c, s)
	}
	if f.creates[0]["model"] != "achira-donut" {
		t.Fatalf("the thread must be created with the model: %v", f.creates[0]["model"])
	}
	if _, ok := f.sends[0]["model"]; ok {
		t.Fatalf("a follow-up must not repeat the model: %v", f.sends[0])
	}
	if f.sends[0]["reasoningEffort"] != "high" {
		t.Fatalf("the follow-up must keep its effort: %v", f.sends[0]["reasoningEffort"])
	}
}

// Notion compacts a large thread on its own (tool entity context_compaction).
// The server-side summary may drop the project context and local action
// protocol sent earlier, so the next follow-up re-sends them once.
func TestAgentNotionCompactionResendsContextOnce(t *testing.T) {
	f, store, acc := bindingSetup(t)
	f.compactOn = map[int]bool{2: true}
	msgs := []ChatMessage{sysMsg("protocol-v1"), u("one")}
	if _, err := runTurn(t, f, store, acc, "nc", msgs); err != nil {
		t.Fatal(err)
	}
	msgs = append(msgs, a("reply-1"), u("two"))
	if _, err := runTurn(t, f, store, acc, "nc", msgs); err != nil { // Notion compacts during reply-2
		t.Fatal(err)
	}
	msgs = append(msgs, a("reply-2"), u("three"))
	if _, err := runTurn(t, f, store, acc, "nc", msgs); err != nil {
		t.Fatal(err)
	}
	msgs = append(msgs, a("reply-3"), u("four"))
	if _, err := runTurn(t, f, store, acc, "nc", msgs); err != nil {
		t.Fatal(err)
	}
	prompt := func(i int) string { return promptText(f.sends[i]["event"].(map[string]interface{})["content"]) }
	if strings.Contains(prompt(0), "UPDATED CONTEXT") {
		t.Fatalf("no resend before the compaction: %q", prompt(0))
	}
	if p := prompt(1); !strings.Contains(p, "UPDATED CONTEXT") || !strings.Contains(p, "protocol-v1") || !strings.Contains(p, "three") {
		t.Fatalf("the follow-up after a Notion compaction must re-send the context: %q", p)
	}
	if strings.Contains(prompt(2), "protocol-v1") {
		t.Fatalf("the context is re-sent once only: %q", prompt(2))
	}
	if c, s := f.counts(); c != 1 || s != 3 {
		t.Fatalf("creates=%d sends=%d", c, s)
	}
}

func TestAgentBindingOutputStyleLineMoveDoesNotResendSystemContext(t *testing.T) {
	f, store, acc := bindingSetup(t)
	styleLine := "team-attention-span output style is active. Remember to follow the specific guidelines for this style."
	turn0Sys := sysMsg("x-anthropic-billing-header: cc_version=2.1.296; cch=11111;\nsystem-v1\n\n<total_tokens>15000000 tokens left</total_tokens>\n\n" + styleLine)
	turn1SysMain := sysMsg("x-anthropic-billing-header: cc_version=2.1.296; cch=22222;\nsystem-v1")
	turn1SysTail := sysMsg("<total_tokens>14998000 tokens left</total_tokens>\n\n" + styleLine)
	if _, err := runTurn(t, f, store, acc, "style", []ChatMessage{turn0Sys, u("one")}); err != nil {
		t.Fatal(err)
	}
	if _, err := runTurn(t, f, store, acc, "style", []ChatMessage{turn1SysMain, u("one"), a("reply-1"), u("two"), turn1SysTail}); err != nil {
		t.Fatal(err)
	}
	prompt := promptText(f.sends[0]["event"].(map[string]interface{})["content"])
	if strings.Contains(prompt, "UPDATED CONTEXT") || strings.Contains(prompt, "system-v1") || !strings.Contains(prompt, "two") {
		t.Fatalf("moving output style active line or changing cch billing header must not resend system context: %q", prompt)
	}
}

func TestConvertAnthropicMessagesExtractsToolReferenceAndSkipsDeferredPlaceholder(t *testing.T) {
	msgs := []AnthropicMessage{
		{
			Role: "assistant",
			Content: []interface{}{
				map[string]interface{}{
					"type":  "tool_use",
					"id":    "toolu_search_1",
					"name":  "ToolSearch",
					"input": map[string]interface{}{"query": "select:mcp__openviking__health"},
				},
			},
		},
		{
			Role: "user",
			Content: []interface{}{
				map[string]interface{}{
					"type":        "tool_result",
					"tool_use_id": "toolu_search_1",
					"content": []interface{}{
						map[string]interface{}{
							"type":      "tool_reference",
							"tool_name": "mcp__openviking__health",
						},
					},
				},
			},
		},
	}
	converted, _ := convertAnthropicMessages("sys", msgs)
	var toolMsg *ChatMessage
	for i := range converted {
		if converted[i].Role == "tool" {
			toolMsg = &converted[i]
			break
		}
	}
	if toolMsg == nil {
		t.Fatalf("expected converted tool message, got %#v", converted)
	}
	if !strings.Contains(toolMsg.Content, "mcp__openviking__health") {
		t.Fatalf("expected tool_reference tool_name in converted tool result, got %q", toolMsg.Content)
	}

	declared := buildDeclaredTools([]AnthropicTool{
		{Name: "ToolSearch"},
		{Name: "DeferredToolPlaceholder"},
	})
	if _, ok := declared["DeferredToolPlaceholder"]; ok {
		t.Fatal("DeferredToolPlaceholder must not be registered as a declared callable tool")
	}
	if _, ok := declared["ToolSearch"]; !ok {
		t.Fatal("ToolSearch must be registered as a declared callable tool")
	}
}

func TestAgentBindingModelSwitchSendsModelOnce(t *testing.T) {
	f, store, acc := bindingSetup(t)
	runWithModel := func(model string, msgs []ChatMessage) {
		t.Helper()
		notionModel, err := agentThreadModelID(model)
		if err != nil {
			t.Fatalf("agentThreadModelID(%q): %v", model, err)
		}
		var out strings.Builder
		err = callAgentThreadResumable(acc, msgs, notionModel, "low", "session-model-switch", true, func(text string, done bool, _ *UsageInfo) { out.WriteString(text) }, f.server.Client(), store)
		if err != nil {
			t.Fatalf("model=%s err=%v", model, err)
		}
	}

	m1 := []ChatMessage{u("turn 1")}
	m2 := []ChatMessage{u("turn 1"), a("r1"), u("turn 2")}
	m3 := []ChatMessage{u("turn 1"), a("r1"), u("turn 2"), a("r2"), u("turn 3")}
	m4 := []ChatMessage{u("turn 1"), a("r1"), u("turn 2"), a("r2"), u("turn 3"), a("r3"), u("turn 4")}
	m5 := []ChatMessage{u("turn 1"), a("r1"), u("turn 2"), a("r2"), u("turn 3"), a("r3"), u("turn 4"), a("r4"), u("turn 5")}

	runWithModel("sonnet-5.5", m1)
	runWithModel("sonnet-5.5", m2)
	runWithModel("opus-5.5", m3)
	runWithModel("opus-5.5", m4)
	runWithModel("gpt-6.1-sol[1m]", m5)

	if c, s := f.counts(); c != 1 || s != 4 {
		t.Fatalf("creates=%d sends=%d, want 1/4", c, s)
	}
	if got := f.sends[0]["model"]; got != nil {
		t.Fatalf("unchanged model on turn 2 must omit model field, got %v", got)
	}
	if got := f.sends[1]["model"]; got != "albuquerque-quinn" {
		t.Fatalf("switched model on turn 3 must send albuquerque-quinn, got %v", got)
	}
	if got := f.sends[2]["model"]; got != nil {
		t.Fatalf("unchanged model on turn 4 must omit model field, got %v", got)
	}
	if got := f.sends[3]["model"]; got != "omniberry-sundae" {
		t.Fatalf("switched model on turn 5 must send omniberry-sundae, got %v", got)
	}
}

func TestAgentBindingWrappedSystemReminderAndUserPromptSubmitHookDoNotResendSystemContext(t *testing.T) {
	f, store, acc := bindingSetup(t)
	styleBlock := "<system-reminder>\nteam-attention-span output style is active. Remember to follow the specific guidelines for this style.\n</system-reminder>"
	turn0Sys := sysMsg("x-anthropic-billing-header: cc_version=2.1.280.a00; cc_entrypoint=sdk-cli;\nsystem-v1")
	turn0Env := sysMsg("<system-reminder>\nSessionStart hook additional context: init\n</system-reminder>\n# Environment\n\n<system-reminder>\n<total_tokens>15000000 tokens left</total_tokens>\n</system-reminder>\n\n" + styleBlock + "\n\n<system-reminder>\nUserPromptSubmit hook additional context: <openviking>turn1</openviking>\n</system-reminder>")
	turn1Env := sysMsg("<system-reminder>\nSessionStart hook additional context: init\n</system-reminder>\n# Environment")
	turn1Tail := sysMsg("<system-reminder>\n<total_tokens>14995000 tokens left</total_tokens>\n</system-reminder>\n\n" + styleBlock + "\n\n<system-reminder>\nUserPromptSubmit hook additional context: <openviking>turn2</openviking>\n</system-reminder>")

	if _, err := runTurn(t, f, store, acc, "wrapped-style", []ChatMessage{turn0Sys, u("one"), turn0Env}); err != nil {
		t.Fatal(err)
	}
	if _, err := runTurn(t, f, store, acc, "wrapped-style", []ChatMessage{turn0Sys, u("one"), turn1Env, a("reply-1"), u("two"), turn1Tail}); err != nil {
		t.Fatal(err)
	}
	prompt := promptText(f.sends[0]["event"].(map[string]interface{})["content"])
	if strings.Contains(prompt, "UPDATED CONTEXT") || strings.Contains(prompt, "system-v1") || !strings.Contains(prompt, "two") {
		t.Fatalf("wrapped <system-reminder> bookkeeping and UserPromptSubmit hooks must not resend 89KB system context: %q", prompt)
	}
}

func TestAgentThreadPreferredAccountKeyedEditAndCompaction(t *testing.T) {
	f, store, acc := bindingSetup(t)
	if _, err := runTurn(t, f, store, acc, "keyed-pref", []ChatMessage{sysMsg("tools"), u("first"), a("r1"), u("second")}); err != nil {
		t.Fatal(err)
	}
	wantID := agentAccountKey(acc)
	// Edited last user message (same root hash conv[1] == rec.RootHash) still prefers the bound account.
	if got := store.preferredAccount([]ChatMessage{u("first"), a("r1"), u("second-edited")}, "keyed-pref", "albuquerque-quinn"); got != wantID {
		t.Fatalf("edited keyed turn got preferredAccount=%q, want %q", got, wantID)
	}
	// Post-compaction summary re-base still prefers the bound account.
	compacted := ChatMessage{
		Role:          "user",
		StringContent: true,
		Content:       "This session is being continued from a previous conversation that ran out of context. The summary below covers the earlier portion of the conversation.\n\n<summary>\nSummary text\n</summary>\n\nContinue.",
	}
	if got := store.preferredAccount([]ChatMessage{compacted}, "keyed-pref", "achira-donut"); got != wantID {
		t.Fatalf("compacted keyed turn got preferredAccount=%q, want %q", got, wantID)
	}
}

func TestResolveModelAgentThreadModels(t *testing.T) {
	oldMap := SnapshotModelMap()
	ReplaceModelMap(DefaultConfig().ModelMap)
	t.Cleanup(func() { ReplaceModelMap(oldMap) })

	cases := map[string]string{
		"sonnet-5.5":        "achira-donut",
		"claude-sonnet-5-5": "achira-donut",
		"opus-5.5":          "albuquerque-quinn",
		"claude-opus-5-5":   "albuquerque-quinn",
		"gpt-6.1-sol":       "omniberry-sundae",
		"gpt-6.1-sol[1m]":   "omniberry-sundae",
	}
	for input, want := range cases {
		if got := ResolveModel(input); got != want {
			t.Fatalf("ResolveModel(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestAgentEmptyTurnNativeToolCollisionAndCompactionRecoversInPlace(t *testing.T) {
	f, store, acc := bindingSetup(t)
	f.compactOn = map[int]bool{2: true}
	f.nativeToolOn = map[int]string{2: "Bash"}
	f.replyFn = func(n int) string {
		if n == 2 {
			return ""
		}
		return fmt.Sprintf("reply-%d", n)
	}

	msgs := []ChatMessage{sysMsg("protocol-v1: LOCAL ACTION PROTOCOL"), u("one")}
	if out, err := runTurn(t, f, store, acc, "empty-recover", msgs); err != nil || out != "reply-1" {
		t.Fatalf("turn 1: out=%q err=%v", out, err)
	}

	msgs = append(msgs, a("reply-1"), u("run git status"))
	out, err := runTurn(t, f, store, acc, "empty-recover", msgs)
	if err != nil || out != "reply-3" {
		t.Fatalf("turn 2 should recover in-place on reply-3: out=%q err=%v", out, err)
	}
	if c, s := f.counts(); c != 1 || s != 2 {
		t.Fatalf("creates=%d sends=%d, want 1/2 (initial follow-up + 1 in-place continuation)", c, s)
	}
	if got := f.sends[0]["enableSuggestedEditsTools"]; got != false {
		t.Fatalf("follow-up must set enableSuggestedEditsTools=false, got %v", got)
	}
	contPrompt := promptText(f.sends[1]["event"].(map[string]interface{})["content"])
	for _, want := range []string{
		"UPDATED CONTEXT",
		"protocol-v1: LOCAL ACTION PROTOCOL",
		"Bash",
		"unavailable or isolated from the user's computer",
		"never invoke Notion's built-in/hosted tools directly",
	} {
		if !strings.Contains(contPrompt, want) {
			t.Fatalf("continuation prompt missing %q: %q", want, contPrompt)
		}
	}
	if strings.Contains(contPrompt, "run git status") {
		t.Fatalf("continuation prompt must not duplicate user prompt: %q", contPrompt)
	}

	// Next turn must not redundantly re-send UPDATED CONTEXT because the continuation already consumed it.
	msgs = append(msgs, a("reply-3"), u("next question"))
	if out, err := runTurn(t, f, store, acc, "empty-recover", msgs); err != nil || out != "reply-4" {
		t.Fatalf("turn 3: out=%q err=%v", out, err)
	}
	nextPrompt := promptText(f.sends[2]["event"].(map[string]interface{})["content"])
	if strings.Contains(nextPrompt, "UPDATED CONTEXT") || strings.Contains(nextPrompt, "protocol-v1") {
		t.Fatalf("turn 3 must not re-send UPDATED CONTEXT after continuation already sent it: %q", nextPrompt)
	}
}

func TestAgentEmptyTurnDoubleEmptyRollsBackAndDedupesContinuePrompt(t *testing.T) {
	for _, mode := range []string{"merged", "separate"} {
		t.Run(mode, func(t *testing.T) {
			f := newFakeAgentServer(t)
			statePath := filepath.Join(t.TempDir(), "bindings.state")
			store := newAgentThreadStore(statePath)
			acc := &Account{UserID: "user", SpaceID: "space"}

			f.compactOn = map[int]bool{2: true}
			f.replyFn = func(n int) string {
				if n == 2 || n == 3 {
					return ""
				}
				return fmt.Sprintf("reply-%d", n)
			}

			base := []ChatMessage{sysMsg("protocol-v1"), u("one")}
			if _, err := runTurn(t, f, store, acc, "dedupe-key", base); err != nil {
				t.Fatal(err)
			}

			userQuestion := "unique-long-question-already-committed-in-notion"
			failedTurn := []ChatMessage{sysMsg("protocol-v1"), u("one"), a("reply-1"), u(userQuestion)}
			if _, err := runTurn(t, f, store, acc, "dedupe-key", failedTurn); !errors.Is(err, ErrEmptyResponse) {
				t.Fatalf("double empty turn must return ErrEmptyResponse, got %v", err)
			}
			if c, s := f.counts(); c != 1 || s != 2 {
				t.Fatalf("creates=%d sends=%d, want 1/2", c, s)
			}

			// Reload store from disk to verify committed dedupe fields and compaction flag survive restart.
			store = newAgentThreadStore(statePath)

			var continueMsgs []ChatMessage
			if mode == "merged" {
				// Claude Code merges consecutive user messages when the previous turn errored without an assistant reply.
				continueMsgs = []ChatMessage{sysMsg("protocol-v1"), u("one"), a("reply-1"), u(userQuestion + "\n继续")}
			} else {
				continueMsgs = []ChatMessage{sysMsg("protocol-v1"), u("one"), a("reply-1"), u(userQuestion), u("继续")}
			}

			out, err := runTurn(t, f, store, acc, "dedupe-key", continueMsgs)
			if err != nil || out != "reply-4" {
				t.Fatalf("continue turn failed: out=%q err=%v", out, err)
			}
			if c, s := f.counts(); c != 1 || s != 3 {
				t.Fatalf("creates=%d sends=%d, want 1/3", c, s)
			}
			sent := promptText(f.sends[2]["event"].(map[string]interface{})["content"])
			if strings.Contains(sent, userQuestion) {
				t.Fatalf("continue turn must not duplicate already-committed user question %q: %q", userQuestion, sent)
			}
			if !strings.Contains(sent, "继续") {
				t.Fatalf("continue turn must send the new user suffix '继续': %q", sent)
			}
			if !strings.Contains(sent, "UPDATED CONTEXT") || !strings.Contains(sent, "protocol-v1") {
				t.Fatalf("compaction from failed turn must be preserved so next turn resends UPDATED CONTEXT: %q", sent)
			}

			rawState, err := os.ReadFile(statePath)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(rawState), userQuestion) || strings.Contains(string(rawState), "继续") {
				t.Fatalf("state file must never store raw message text: %s", string(rawState))
			}
		})
	}
}

func TestAgentBindingCodingSessionNamingHelperIsUntracked(t *testing.T) {
	f, store, acc := bindingSetup(t)
	const key = "session-naming-test"

	// 1. Naming helper arrives BEFORE Turn 1 (via system prompt).
	namingBySystem := []ChatMessage{
		sysMsg("You are naming a coding session based on the initial user prompt."),
		u("Please fix issue #116 in notion-code-bridge"),
	}
	if out, err := runKeyed(t, f, store, acc, key, "low", false, namingBySystem); err != nil || out == "" {
		t.Fatalf("pre-Turn-1 naming helper failed: out=%q err=%v", out, err)
	}
	store.mu.Lock()
	if rec := store.primaryLocked(agentPlanInput{ClientKey: key, AccountID: agentAccountKey(acc), SpaceID: "space"}); rec != nil {
		store.mu.Unlock()
		t.Fatalf("naming helper must not create a primary binding, got thread %s", rec.ThreadID)
	}
	store.mu.Unlock()

	// 2. Real Turn 1 with tools arrives and binds cleanly to its own primary thread.
	turn1 := []ChatMessage{sysMsg("LOCAL ACTION PROTOCOL"), u("Turn 1 real user request")}
	out1, err := runKeyed(t, f, store, acc, key, "low", true, turn1)
	if err != nil || out1 == "" {
		t.Fatalf("Turn 1 failed: out=%q err=%v", out1, err)
	}
	store.mu.Lock()
	primary := store.primaryLocked(agentPlanInput{ClientKey: key, AccountID: agentAccountKey(acc), SpaceID: "space"})
	store.mu.Unlock()
	if primary == nil || !primary.Tooled || primary.Turns != 1 {
		t.Fatalf("expected primary tooled binding at turn 1, got %+v", primary)
	}
	mainThreadID := primary.ThreadID

	// 3. Naming helper arrives AFTER Turn 1 (via <session> template without system match).
	namingByTemplate := []ChatMessage{
		u("<session>\nTurn 1 real user request\n</session>\n\nWrite the title in the predominant language of the session."),
	}
	if out, err := runKeyed(t, f, store, acc, key, "low", false, namingByTemplate); err != nil || out == "" {
		t.Fatalf("post-Turn-1 naming helper must not fail with diverged: out=%q err=%v", out, err)
	}

	// 4. Turn 2 continues the bound primary thread normally.
	turn2 := append(append([]ChatMessage{}, turn1...), a(out1), u("Turn 2 follow-up"))
	if out2, err := runKeyed(t, f, store, acc, key, "low", true, turn2); err != nil || out2 == "" {
		t.Fatalf("Turn 2 follow-up failed: out=%q err=%v", out2, err)
	}
	store.mu.Lock()
	after := store.primaryLocked(agentPlanInput{ClientKey: key, AccountID: agentAccountKey(acc), SpaceID: "space"})
	store.mu.Unlock()
	if after == nil || after.ThreadID != mainThreadID || after.Turns != 2 {
		t.Fatalf("primary binding disturbed: got %+v, want thread %s with 2 turns", after, mainThreadID)
	}
}

func TestAgentBindingTransientRemindersAndDiskChangeNotesDoNotResendSystemContext(t *testing.T) {
	f, store, acc := bindingSetup(t)
	baseSys := sysMsg("x-anthropic-billing-header: cc_version=2.1.296; cc_entrypoint=cli;\nLOCAL ACTION PROTOCOL\nproject-rules-v1")
	silentPlain := sysMsg("The user hasn't heard from you in a while. As you continue, keep them updated when there's something to tell — a finding, a change of plan.")
	silentWrapped := sysMsg("<system-reminder>\nThe user hasn't heard from you in a while. As you continue, keep them updated when there's something to tell — a finding, a change of plan.\n</system-reminder>")
	hookPlain := sysMsg("UserPromptSubmit hook additional context: <openviking-context>\nRelevant Memories:\n1. test\n</openviking-context>")
	diskNotePlain := sysMsg("Note: /workspace/example-project/docs/README.md changed on disk since you last read it. That's usually deliberate, so take it as the current state rather than reverting it; if that creates a conflict with the task, mention it.\n\n@@ -1,3 +1,4 @@\n+new line")
	diskNoteWrapped := sysMsg("<system-reminder>\nNote: /tmp/a.go changed on disk since you last read it. That's usually deliberate, so take it as the current state rather than reverting it; if that creates a conflict with the task, mention it.\n</system-reminder>")

	msgs := []ChatMessage{baseSys, u("step-1")}
	if _, err := runTurn(t, f, store, acc, "transient-sys", msgs); err != nil {
		t.Fatal(err)
	}

	steps := [][]ChatMessage{
		{baseSys, u("step-1"), a("reply-1"), silentPlain, u("step-2")},
		{baseSys, u("step-1"), a("reply-1"), silentPlain, u("step-2"), a("reply-2"), silentWrapped, hookPlain, u("step-3")},
		{baseSys, u("step-1"), a("reply-1"), silentPlain, u("step-2"), a("reply-2"), silentWrapped, hookPlain, u("step-3"), a("reply-3"), diskNotePlain, diskNoteWrapped, u("step-4")},
	}
	for i, step := range steps {
		if _, err := runTurn(t, f, store, acc, "transient-sys", step); err != nil {
			t.Fatalf("step %d: %v", i+2, err)
		}
		prompt := promptText(f.sends[i]["event"].(map[string]interface{})["content"])
		if strings.Contains(prompt, "UPDATED CONTEXT") || strings.Contains(prompt, "project-rules-v1") {
			t.Fatalf("step %d must not resend UPDATED CONTEXT on transient client reminders: %q", i+2, prompt)
		}
	}
}

func TestAgentBindingDesktopTitleGeneratorHelperIsUntracked(t *testing.T) {
	f, store, acc := bindingSetup(t)
	const key = "desktop-session-title-test"

	desktopTitleHelper := []ChatMessage{
		u(`Generate a concise, sentence-case title (3-7 words) that captures the main topic or goal of this coding session. Also generate a git branch suffix. Return a JSON object with two fields: "title" and "branch".`),
	}
	if out, err := runKeyed(t, f, store, acc, key, "low", false, desktopTitleHelper); err != nil || out == "" {
		t.Fatalf("pre-Turn-1 desktop title helper failed: out=%q err=%v", out, err)
	}
	store.mu.Lock()
	if rec := store.primaryLocked(agentPlanInput{ClientKey: key, AccountID: agentAccountKey(acc), SpaceID: "space"}); rec != nil {
		store.mu.Unlock()
		t.Fatalf("desktop title helper must not create a primary binding, got thread %s", rec.ThreadID)
	}
	store.mu.Unlock()

	turn1 := []ChatMessage{sysMsg("LOCAL ACTION PROTOCOL"), u("Help me refactor the auth middleware")}
	out1, err := runKeyed(t, f, store, acc, key, "low", true, turn1)
	if err != nil || out1 == "" {
		t.Fatalf("Turn 1 failed: out=%q err=%v", out1, err)
	}
	store.mu.Lock()
	primary := store.primaryLocked(agentPlanInput{ClientKey: key, AccountID: agentAccountKey(acc), SpaceID: "space"})
	store.mu.Unlock()
	if primary == nil || !primary.Tooled || primary.Turns != 1 {
		t.Fatalf("expected primary tooled binding at turn 1, got %+v", primary)
	}
	mainThreadID := primary.ThreadID

	if out, err := runKeyed(t, f, store, acc, key, "low", false, desktopTitleHelper); err != nil || out == "" {
		t.Fatalf("post-Turn-1 desktop title helper must not fail with diverged: out=%q err=%v", out, err)
	}
	desktopV2TitleHelper := []ChatMessage{
		sysMsg("You write short session titles. Reply with only the tagged fields the prompt asks for."),
		u("Here is the session description:\n<description>Help me refactor the auth middleware</description>\nPlease generate a title and branch name for this session."),
	}
	if out, err := runKeyed(t, f, store, acc, key, "low", false, desktopV2TitleHelper); err != nil || out == "" {
		t.Fatalf("post-Turn-1 desktop v2 title helper must not fail with diverged: out=%q err=%v", out, err)
	}

	turn2 := append(append([]ChatMessage{}, turn1...), a(out1), u("Now add unit tests"))
	if out2, err := runKeyed(t, f, store, acc, key, "low", true, turn2); err != nil || out2 == "" {
		t.Fatalf("Turn 2 follow-up failed: out=%q err=%v", out2, err)
	}
	store.mu.Lock()
	after := store.primaryLocked(agentPlanInput{ClientKey: key, AccountID: agentAccountKey(acc), SpaceID: "space"})
	store.mu.Unlock()
	if after == nil || after.ThreadID != mainThreadID || after.Turns != 2 {
		t.Fatalf("primary binding disturbed: got %+v, want thread %s with 2 turns", after, mainThreadID)
	}
}





