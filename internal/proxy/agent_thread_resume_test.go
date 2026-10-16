package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeAgentServer mimics the createAgentThread / sendEventToAgentThread /
// getThreadTranscript protocol shapes used by the bridge, with per-thread
// sequence numbers. It records every request body for assertions.
type fakeAgentServer struct {
	mu           sync.Mutex
	t            *testing.T
	threads      map[string]*fakeThread
	creates      []map[string]interface{}
	sends        []map[string]interface{}
	rejectSend   int    // HTTP status for sendEventToAgentThread (0 = accept)
	rejectBody   string // optional JSON body for rejected sends
	dropAckSend  bool   // accept the send, then answer 500 (ack lost)
	rejectCreate int    // HTTP status for createAgentThread (0 = accept); rejectBody is its optional body too
	createTries  int    // every createAgentThread request, accepted or not
	sendTries    int    // every sendEventToAgentThread request, accepted or not
	stall        bool   // keep new turns running (no reply)
	failTurn     bool   // new turns end with a failure entity
	hasMore      bool   // report has_more_backward
	replyN       int
	replyFn      func(n int) string // optional: assistant text of the n-th reply (default "reply-n")
	maxCreate    int                // >0: createAgentThread prompts longer than this many bytes get HTTP 400 (Notion limit model)
	maxFollowUp  int                // >0: sendEventToAgentThread prompts longer than this many bytes get HTTP 500
	sizeRejected int                // requests refused for size
	compactOn    map[int]bool       // reply numbers during which Notion compacts the thread context
	nativeToolOn map[int]string     // reply numbers during which Notion emits a native tool entity
	thinkingOn   map[int][]string   // reply numbers during which Notion emits kind=thinking entities (text in top-level content_text)
	titles       map[string]string  // threadID -> latest title written via saveTransactionsFanout
	userSetTitle map[string]bool    // threadID -> user_set_title flag written via saveTransactionsFanout
	server       *httptest.Server
}

type fakeThread struct {
	seq      int
	entities []map[string]interface{}
	status   string
}

func newFakeAgentServer(t *testing.T) *fakeAgentServer {
	f := &fakeAgentServer{
		t:            t,
		threads:      map[string]*fakeThread{},
		titles:       map[string]string{},
		userSetTitle: map[string]bool{},
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.server.Close)
	oldCfg, oldBase, oldEmptyDelay := AppConfig, NotionAPIBase, agentEmptyTurnRetryDelay
	oldSubmitDelay, oldInferenceDelay := agentSubmitRetryDelay, agentInferenceRetryDelay
	AppConfig = DefaultConfig()
	AppConfig.Proxy.AgentThread = true
	AppConfig.Timeouts.InferenceTimeout = 2
	NotionAPIBase = f.server.URL
	agentEmptyTurnRetryDelay = 0
	agentSubmitRetryDelay = 0
	agentInferenceRetryDelay = 0
	t.Cleanup(func() {
		AppConfig, NotionAPIBase, agentEmptyTurnRetryDelay = oldCfg, oldBase, oldEmptyDelay
		agentSubmitRetryDelay, agentInferenceRetryDelay = oldSubmitDelay, oldInferenceDelay
	})
	return f
}

func (f *fakeAgentServer) startTurn(th *fakeThread, userText string) {
	th.seq++
	th.entities = append(th.entities, map[string]interface{}{"id": fmt.Sprintf("u%d", th.seq), "kind": "user_message", "sequence": th.seq, "content": []map[string]string{{"type": "text", "text": userText}}})
	if f.stall {
		th.status = "running"
		return
	}
	f.replyN++
	th.seq++
	if f.failTurn {
		th.entities = append(th.entities, map[string]interface{}{"id": fmt.Sprintf("e%d", th.seq), "kind": "turn_failed", "sequence": th.seq})
	} else {
		reply := fmt.Sprintf("reply-%d", f.replyN)
		if f.replyFn != nil {
			reply = f.replyFn(f.replyN)
		}
		if f.compactOn[f.replyN] {
			th.entities = append(th.entities, map[string]interface{}{"id": fmt.Sprintf("tool:context-compaction:%d", th.seq), "kind": "tool", "name": "context_compaction", "sequence": th.seq})
			th.seq++
		}
		for _, think := range f.thinkingOn[f.replyN] {
			th.entities = append(th.entities, map[string]interface{}{"id": fmt.Sprintf("th%d", th.seq), "kind": "thinking", "sequence": th.seq, "content_text": think})
			th.seq++
		}
		if toolName := f.nativeToolOn[f.replyN]; toolName != "" {
			th.entities = append(th.entities, map[string]interface{}{"id": fmt.Sprintf("tool:native:%d", th.seq), "kind": "tool", "name": toolName, "sequence": th.seq})
			th.seq++
		}
		if reply != "" {
			th.entities = append(th.entities, map[string]interface{}{"id": fmt.Sprintf("a%d", th.seq), "kind": "assistant_message", "sequence": th.seq, "content": []map[string]string{{"type": "text", "text": reply}}})
			th.seq++
		}
		th.entities = append(th.entities, map[string]interface{}{"id": fmt.Sprintf("d%d", th.seq), "kind": "turn_completed", "sequence": th.seq, "stop_reason": "end_turn"})
	}
	// The real protocol keeps a bookkeeping "status" entity with a fractional
	// sequence between turns (observed: 26.5). Model it so every flow below
	// also proves fractional sequences are accepted.
	th.entities = append(th.entities, map[string]interface{}{"id": fmt.Sprintf("s%d", th.seq), "kind": "status", "sequence": float64(th.seq) + 0.5})
	th.status = "idle"
}

// finishLocked completes a stalled turn the way the real service eventually does.
func (f *fakeAgentServer) finishLocked(th *fakeThread) {
	if th.status != "running" {
		return
	}
	f.replyN++
	th.seq++
	th.entities = append(th.entities, map[string]interface{}{"id": fmt.Sprintf("a%d", th.seq), "kind": "assistant_message", "sequence": th.seq, "content": []map[string]string{{"type": "text", "text": fmt.Sprintf("reply-%d", f.replyN)}}})
	th.seq++
	th.entities = append(th.entities, map[string]interface{}{"id": fmt.Sprintf("d%d", th.seq), "kind": "turn_completed", "sequence": th.seq, "stop_reason": "end_turn"})
	th.status = "idle"
}

func (f *fakeAgentServer) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var body map[string]interface{}
	json.NewDecoder(r.Body).Decode(&body)
	endpoint := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	switch endpoint {
	case "createAgentThread":
		f.createTries++
		if f.maxCreate > 0 && len(promptText(body["content"])) > f.maxCreate {
			f.sizeRejected++
			w.WriteHeader(400)
			w.Write([]byte(`{"name":"ValidationError"}`))
			return
		}
		if f.rejectCreate != 0 {
			w.WriteHeader(f.rejectCreate)
			if f.rejectBody != "" {
				w.Write([]byte(f.rejectBody))
			}
			return
		}
		f.creates = append(f.creates, body)
		id := body["threadId"].(string)
		if _, ok := f.threads[id]; !ok {
			th := &fakeThread{status: "idle"}
			f.threads[id] = th
			f.startTurn(th, promptText(body["content"]))
		}
		w.Write([]byte(`{}`))
	case "sendEventToAgentThread":
		f.sendTries++
		if f.maxFollowUp > 0 {
			if ev, _ := body["event"].(map[string]interface{}); ev != nil && len(promptText(ev["content"])) > f.maxFollowUp {
				f.sizeRejected++
				w.WriteHeader(500)
				return
			}
		}
		if f.rejectSend != 0 {
			w.WriteHeader(f.rejectSend)
			if f.rejectBody != "" {
				w.Write([]byte(f.rejectBody))
			}
			return
		}
		f.sends = append(f.sends, body)
		th := f.threads[body["threadId"].(string)]
		if th == nil {
			w.WriteHeader(404)
			return
		}
		event := body["event"].(map[string]interface{})
		f.startTurn(th, promptText(event["content"]))
		if f.dropAckSend {
			w.WriteHeader(500)
			return
		}
		w.Write([]byte(`{}`))
	case "getThreadTranscript":
		th := f.threads[body["threadId"].(string)]
		if th == nil {
			w.WriteHeader(404)
			return
		}
		patches := []interface{}{map[string]interface{}{"op": "committed", "sequence": th.seq}}
		for _, e := range th.entities {
			patches = append(patches, map[string]interface{}{"op": "put", "entity": e})
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"type": "agent_service_transcript_page", "patches": patches, "has_more_backward": f.hasMore,
			"session": map[string]interface{}{"status": th.status, "sequence": th.seq},
		})
	case "saveTransactionsFanout":
		txs, _ := body["transactions"].([]interface{})
		for _, rawTx := range txs {
			tx, _ := rawTx.(map[string]interface{})
			ops, _ := tx["operations"].([]interface{})
			for _, rawOp := range ops {
				op, _ := rawOp.(map[string]interface{})
				ptr, _ := op["pointer"].(map[string]interface{})
				args, _ := op["args"].(map[string]interface{})
				if ptr["table"] == "thread" && op["command"] == "update" {
					if id, _ := ptr["id"].(string); id != "" {
						if title, _ := args["title"].(string); title != "" {
							f.titles[id] = title
						}
						if us, ok := args["user_set_title"].(bool); ok {
							f.userSetTitle[id] = us
						}
					}
				}
			}
		}
		w.Write([]byte(`{}`))
	default:
		w.WriteHeader(404)
	}
}

func promptText(content interface{}) string {
	parts, _ := content.([]interface{})
	var b strings.Builder
	for _, p := range parts {
		m, _ := p.(map[string]interface{})
		if rows, ok := m["text"].([]interface{}); ok {
			for _, row := range rows {
				for _, cell := range row.([]interface{}) {
					b.WriteString(cell.(string))
				}
			}
		}
	}
	return b.String()
}

func (f *fakeAgentServer) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.creates), len(f.sends)
}

func runTurn(t *testing.T, f *fakeAgentServer, store *agentThreadStore, acc *Account, clientKey string, messages []ChatMessage) (string, error) {
	t.Helper()
	var out strings.Builder
	err := callAgentThreadResumable(acc, messages, "", "low", clientKey, false, func(text string, done bool, _ *UsageInfo) { out.WriteString(text) }, f.server.Client(), store)
	return out.String(), err
}

func u(text string) ChatMessage { return ChatMessage{Role: "user", Content: text} }
func a(text string) ChatMessage { return ChatMessage{Role: "assistant", Content: text} }

func TestAgentThreadResumesSameThreadAcrossTurnsAndRestart(t *testing.T) {
	f := newFakeAgentServer(t)
	path := filepath.Join(t.TempDir(), "bindings.state")
	store := newAgentThreadStore(path)
	acc := &Account{UserID: "user", SpaceID: "space"}
	sys := ChatMessage{Role: "system", Content: "project rules"}

	out, err := runTurn(t, f, store, acc, "", []ChatMessage{sys, u("remember MARKER-42")})
	if err != nil || out != "reply-1" {
		t.Fatalf("turn 1: %q %v", out, err)
	}
	out, err = runTurn(t, f, store, acc, "", []ChatMessage{sys, u("remember MARKER-42"), a("anything echoed"), u("what was the marker?")})
	if err != nil || out != "reply-2" {
		t.Fatalf("turn 2 must contain only the new answer: %q %v", out, err)
	}
	// Restart: new store loads the persisted binding.
	store = newAgentThreadStore(path)
	out, err = runTurn(t, f, store, acc, "", []ChatMessage{sys, u("remember MARKER-42"), a("echo"), u("what was the marker?"), a("echo2"), u("third question")})
	if err != nil || out != "reply-3" {
		t.Fatalf("turn 3 after restart: %q %v", out, err)
	}
	creates, sends := f.counts()
	if creates != 1 || sends != 2 || len(f.threads) != 1 {
		t.Fatalf("creates=%d sends=%d threads=%d", creates, sends, len(f.threads))
	}
	first := f.creates[0]["threadId"]
	for _, s := range f.sends {
		if s["threadId"] != first {
			t.Fatal("follow-up used a different thread")
		}
		event := s["event"].(map[string]interface{})
		if event["type"] != "user.message" || s["reasoningEffort"] != "low" || s["clientEventId"] == "" {
			t.Fatalf("unexpected follow-up shape: %v", s)
		}
	}
	prompt2 := promptText(f.sends[0]["event"].(map[string]interface{})["content"])
	if !strings.Contains(prompt2, "what was the marker?") || strings.Contains(prompt2, "MARKER-42") || strings.Contains(prompt2, "project rules") {
		t.Fatalf("follow-up must carry only new messages: %q", prompt2)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "MARKER") || strings.Contains(string(raw), "project rules") {
		t.Fatal("bindings file contains message text")
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Fatalf("bindings file mode %v", info.Mode().Perm())
	}
}

// Notion's getThreadTranscript reports reasoning as kind="thinking" entities
// whose text sits in the top-level content_text field. The bridge must surface
// the current turn's thinking through CallOptions.ThinkingBlocks and
// ThinkingCallback, without leaking earlier turns and without forking the thread.
func TestAgentThreadBridgesThinkingEntities(t *testing.T) {
	f := newFakeAgentServer(t)
	f.thinkingOn = map[int][]string{
		1: {"plan A step 1", "   ", "plan A step 2"},
		2: {"plan B"},
		3: {"plan C before empty continue"},
		4: {"plan C after continue"},
	}
	f.replyFn = func(n int) string {
		if n == 3 {
			return "" // triggers emptyContinues in-place continuation (reply 4)
		}
		return fmt.Sprintf("reply-%d", n)
	}
	useRecoveryStore(t, filepath.Join(t.TempDir(), "bindings.state"))
	oldClient, oldQuota := agentThreadHTTPClient, quotaFetcher
	agentThreadHTTPClient = func(time.Duration) *http.Client { return f.server.Client() }
	quotaFetcher = func(*Account) (*QuotaInfo, error) { return nil, errors.New("offline") }
	t.Cleanup(func() { agentThreadHTTPClient, quotaFetcher = oldClient, oldQuota })
	acc := &Account{UserID: "user", SpaceID: "space"}
	sys := ChatMessage{Role: "system", Content: "project rules"}

	run := func(msgs []ChatMessage) (string, []string, []string, bool) {
		var out strings.Builder
		var blocks []ThinkingBlock
		var deltas []string
		closed := false
		opt := CallOptions{
			ThinkingBlocks: &blocks,
			Session:        &Session{ClientKey: "ncb-thinking-bridge-test"},
			ThinkingCallback: func(delta string, done bool, signature string) {
				if done {
					closed = true
					return
				}
				deltas = append(deltas, delta)
			},
		}
		err := callAgentThreadInference(acc, msgs, "auto", func(text string, done bool, _ *UsageInfo) { out.WriteString(text) }, opt)
		if err != nil {
			t.Fatalf("callAgentThreadInference: %v", err)
		}
		var contents []string
		for _, b := range blocks {
			contents = append(contents, b.Content)
		}
		return out.String(), contents, deltas, closed
	}

	out, blocks, deltas, closed := run([]ChatMessage{sys, u("first question")})
	want1 := []string{"plan A step 1", "plan A step 2"}
	if out != "reply-1" || strings.Join(blocks, "|") != strings.Join(want1, "|") {
		t.Fatalf("turn 1: out=%q blocks=%q want %q", out, blocks, want1)
	}
	if strings.Join(deltas, "|") != strings.Join(want1, "|") || !closed {
		t.Fatalf("turn 1 callback: deltas=%q closed=%v", deltas, closed)
	}

	// Next round echoes the previous reply (with its thinking) back; it must
	// reuse the same thread and only surface the new turn's thinking.
	out, blocks, deltas, closed = run([]ChatMessage{sys, u("first question"), a("reply-1"), u("second question")})
	if out != "reply-2" || strings.Join(blocks, "|") != "plan B" {
		t.Fatalf("turn 2: out=%q blocks=%q (earlier-turn thinking must not leak)", out, blocks)
	}
	if strings.Join(deltas, "|") != "plan B" || !closed {
		t.Fatalf("turn 2 callback: deltas=%q closed=%v", deltas, closed)
	}

	// Turn 3 ends empty at first (with thinking "plan C before empty continue"),
	// triggers emptyContinues, and finishes on reply-4 ("plan C after continue").
	// Both thinking segments of this logical turn must be preserved in order.
	out, blocks, deltas, closed = run([]ChatMessage{sys, u("first question"), a("reply-1"), u("second question"), a("reply-2"), u("third question")})
	want3 := []string{"plan C before empty continue", "plan C after continue"}
	if out != "reply-4" || strings.Join(blocks, "|") != strings.Join(want3, "|") {
		t.Fatalf("turn 3 (empty continue): out=%q blocks=%q want %q", out, blocks, want3)
	}
	if strings.Join(deltas, "|") != strings.Join(want3, "|") || !closed {
		t.Fatalf("turn 3 callback: deltas=%q closed=%v", deltas, closed)
	}

	creates, sends := f.counts()
	if creates != 1 || sends != 3 || len(f.threads) != 1 {
		t.Fatalf("thread must be reused: creates=%d sends=%d threads=%d", creates, sends, len(f.threads))
	}
	if f.sends[0]["threadId"] != f.creates[0]["threadId"] {
		t.Fatal("follow-up used a different thread")
	}
}

func TestAgentThreadToolLoopContinues(t *testing.T) {
	f := newFakeAgentServer(t)
	store := newAgentThreadStore("")
	acc := &Account{UserID: "user", SpaceID: "space"}
	call := ToolCall{ID: "call_1", Type: "function", Function: ToolCallFunction{Name: "Read", Arguments: `{"file_path":"/tmp/x"}`}}
	runTurn(t, f, store, acc, "", []ChatMessage{u("read it")})
	out, err := runTurn(t, f, store, acc, "", []ChatMessage{u("read it"), {Role: "assistant", ToolCalls: []ToolCall{call}}, {Role: "tool", ToolCallID: "call_1", Name: "Read", Content: "file text"}})
	if err != nil || out != "reply-2" {
		t.Fatalf("%q %v", out, err)
	}
	creates, sends := f.counts()
	if creates != 1 || sends != 1 {
		t.Fatalf("creates=%d sends=%d", creates, sends)
	}
	if p := promptText(f.sends[0]["event"].(map[string]interface{})["content"]); !strings.Contains(p, "LOCAL TOOL RESULT (call_id=call_1, name=Read)") || !strings.Contains(p, "file text") {
		t.Fatalf("tool result missing: %q", p)
	}
}

func TestAgentThreadIsolationAndRollback(t *testing.T) {
	f := newFakeAgentServer(t)
	store := newAgentThreadStore("")
	acc := &Account{UserID: "user", SpaceID: "space"}
	base := []ChatMessage{u("hello")}
	runTurn(t, f, store, acc, "client-a", base)
	follow := []ChatMessage{u("hello"), a("x"), u("more")}
	if _, err := runTurn(t, f, store, acc, "client-a", follow); err != nil {
		t.Fatal(err)
	}
	if c, s := f.counts(); c != 1 || s != 1 {
		t.Fatalf("setup creates=%d sends=%d", c, s)
	}
	cases := map[string]struct {
		acc      *Account
		key      string
		messages []ChatMessage
	}{
		"other client session": {acc, "client-b", follow},
		"other account":        {&Account{UserID: "other", SpaceID: "space"}, "client-a", follow},
		"other workspace":      {&Account{UserID: "user", SpaceID: "space2"}, "client-a", follow},
	}
	for name, c := range cases {
		beforeC, beforeS := f.counts()
		if _, err := runTurn(t, f, store, c.acc, c.key, c.messages); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		afterC, afterS := f.counts()
		if afterC != beforeC+1 || afterS != beforeS {
			t.Fatalf("%s: must start a new thread (creates %d->%d sends %d->%d)", name, beforeC, afterC, beforeS, afterS)
		}
		body := promptText(f.creates[afterC-1]["content"])
		if !strings.Contains(body, c.messages[0].Content) || !strings.Contains(body, c.messages[len(c.messages)-1].Content) {
			t.Fatalf("%s: new thread must carry the full history", name)
		}
	}
	// Edits/rollbacks of a bound session are refused explicitly (issue #11);
	// see TestAgentBindingDivergedRequestsAreRefusedNotForked.
}

func TestAgentThreadRetryDoesNotDuplicate(t *testing.T) {
	f := newFakeAgentServer(t)
	AppConfig.Timeouts.InferenceTimeout = 1
	store := newAgentThreadStore("")
	acc := &Account{UserID: "user", SpaceID: "space"}
	msgs := []ChatMessage{u("slow job")}
	f.stall = true
	if _, err := runTurn(t, f, store, acc, "", msgs); err == nil {
		t.Fatal("expected timeout")
	}
	// Server finishes the turn later; the client retries the identical request.
	f.mu.Lock()
	for _, th := range f.threads {
		f.stall = false
		f.replyN++
		th.seq++
		th.entities = append(th.entities,
			map[string]interface{}{"id": "late-a", "kind": "assistant_message", "sequence": th.seq, "content": []map[string]string{{"type": "text", "text": "late answer"}}},
			map[string]interface{}{"id": "late-d", "kind": "turn_completed", "sequence": th.seq + 1, "stop_reason": "end_turn"})
		th.seq++
		th.status = "idle"
	}
	f.mu.Unlock()
	out, err := runTurn(t, f, store, acc, "", msgs)
	if err != nil || out != "late answer" {
		t.Fatalf("%q %v", out, err)
	}
	// Retrying again after completion replays the same reply without sending.
	out, err = runTurn(t, f, store, acc, "", msgs)
	if err != nil || out != "late answer" {
		t.Fatalf("replay %q %v", out, err)
	}
	if c, s := f.counts(); c != 1 || s != 0 {
		t.Fatalf("creates=%d sends=%d", c, s)
	}
}

func TestAgentThreadConcurrentIdenticalRequestsShareOneThread(t *testing.T) {
	f := newFakeAgentServer(t)
	store := newAgentThreadStore("")
	acc := &Account{UserID: "user", SpaceID: "space"}
	msgs := []ChatMessage{u("same")}
	var wg sync.WaitGroup
	outs := make([]string, 4)
	for i := range outs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			outs[i], _ = runTurn(t, f, store, acc, "", msgs)
		}(i)
	}
	wg.Wait()
	if c, s := f.counts(); c != 1 || s != 0 {
		t.Fatalf("creates=%d sends=%d", c, s)
	}
	for _, o := range outs {
		if o != "reply-1" {
			t.Fatalf("outputs %v", outs)
		}
	}
}

func TestAgentThreadRejectedFollowUpFallsBackToNewThread(t *testing.T) {
	f := newFakeAgentServer(t)
	store := newAgentThreadStore("")
	acc := &Account{UserID: "user", SpaceID: "space"}
	runTurn(t, f, store, acc, "", []ChatMessage{u("one")})
	f.rejectSend = 400
	out, err := runTurn(t, f, store, acc, "", []ChatMessage{u("one"), a("x"), u("two")})
	if err != nil || out == "" {
		t.Fatalf("%q %v", out, err)
	}
	if c, _ := f.counts(); c != 2 {
		t.Fatalf("fallback must create a new thread, creates=%d", c)
	}
	if p := promptText(f.creates[1]["content"]); !strings.Contains(p, "one") || !strings.Contains(p, "two") {
		t.Fatal("fallback thread lost history")
	}
	// A server-side outage (5xx) is not treated as rejection: error surfaces, no new thread.
	f.rejectSend = 503
	store2 := newAgentThreadStore("")
	runTurn(t, f, store2, acc, "", []ChatMessage{u("a1")})
	if _, err = runTurn(t, f, store2, acc, "", []ChatMessage{u("a1"), a("x"), u("a2")}); err == nil {
		t.Fatal("5xx must surface")
	}
	if c, _ := f.counts(); c != 3 {
		t.Fatalf("5xx created a thread: %d", c)
	}
}

func TestAgentThreadFailedTurnDiscardsBinding(t *testing.T) {
	f := newFakeAgentServer(t)
	store := newAgentThreadStore("")
	acc := &Account{UserID: "user", SpaceID: "space"}
	f.failTurn = true
	if _, err := runTurn(t, f, store, acc, "", []ChatMessage{u("boom")}); err == nil {
		t.Fatal("failure accepted")
	}
	f.failTurn = false
	out, err := runTurn(t, f, store, acc, "", []ChatMessage{u("boom")})
	if err != nil || out == "" {
		t.Fatalf("%q %v", out, err)
	}
	if c, _ := f.counts(); c != 2 {
		t.Fatalf("retry after failure must use a fresh thread, creates=%d", c)
	}
}

func TestAgentTranscriptSinceFiltersOldTurnsAndLongThreads(t *testing.T) {
	page := agentTranscriptPage{}
	raw := `{"type":"agent_service_transcript_page","has_more_backward":true,"session":{"status":"idle","sequence":9},"patches":[
	{"op":"put","entity":{"id":"old","kind":"assistant_message","sequence":3,"content":[{"type":"text","text":"old answer"}]}},
	{"op":"put","entity":{"id":"oldd","kind":"turn_completed","sequence":4,"stop_reason":"end_turn"}},
	{"op":"put","entity":{"id":"new","kind":"assistant_message","sequence":8,"content":[{"type":"text","text":"new answer"}]}},
	{"op":"put","entity":{"id":"newd","kind":"turn_completed","sequence":9,"stop_reason":"end_turn"}}]}`
	if err := json.Unmarshal([]byte(raw), &page); err != nil {
		t.Fatal(err)
	}
	text, done, seq, err := readAgentTranscriptSince(page, 4)
	if err != nil || !done || text != "new answer" || seq != 9 {
		t.Fatalf("%q %v %v %v", text, done, seq, err)
	}
	if _, _, _, err = readAgentTranscriptSince(page, 0); err == nil {
		t.Fatal("new thread must still refuse multi-page transcripts")
	}
	if _, _, _, err = readAgentTranscriptSince(page, 2); err != nil {
		// baseline below every entity: page does not reach the previous turn
		t.Log("refused as expected:", err)
	} else {
		t.Fatal("ambiguous page accepted")
	}
	// A completed old turn alone must not signal completion of the new turn.
	page.Patches = page.Patches[:2]
	page.Session.Sequence = 4
	if _, done, _, err = readAgentTranscriptSince(page, 4); err != nil || done {
		t.Fatalf("old completion leaked: %v %v", done, err)
	}
}

func TestAgentThreadPreferredAccountAfterRestart(t *testing.T) {
	f := newFakeAgentServer(t)
	path := filepath.Join(t.TempDir(), "b.state")
	store := newAgentThreadStore(path)
	acc := &Account{UserID: "user", SpaceID: "space"}
	sys := ChatMessage{Role: "system", Content: "tools"}
	runTurn(t, f, store, acc, "k", []ChatMessage{sys, u("hi")})
	store = newAgentThreadStore(path)
	// Non-system view of the next request (as seen by the router before tool injection).
	got := store.preferredAccount([]ChatMessage{u("hi"), a("x"), u("next")}, "k", "")
	if got != agentAccountKey(acc) {
		t.Fatalf("preferred %q", got)
	}
	if store.preferredAccount([]ChatMessage{u("hi"), a("x"), u("next")}, "other", "") != "" || store.preferredAccount([]ChatMessage{u("different")}, "k", "") != "" {
		t.Fatal("unrelated conversation got a preferred account")
	}
}

func TestAgentThreadResumeConfigDefaults(t *testing.T) {
	cfg := DefaultConfig()
	if !cfg.AgentThreadResumeEnabled() {
		t.Fatal("resume should default on")
	}
	t.Setenv("AGENT_THREAD_RESUME", "false")
	loaded, err := LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.AgentThreadResumeEnabled() {
		t.Fatal("env override ignored")
	}
}
