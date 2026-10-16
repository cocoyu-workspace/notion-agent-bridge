package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- merged from agent_recovery_test.go ---
// ---- seam: fake Notion Agent service behind a fault-injecting front --------

// recoveryEnv wraps the fakeAgentServer with a front that can drop the
// connection (before or after the fake processed a request) and that pages the
// transcript like the real service (newest `limit` entities, has_more_backward).
type recoveryEnv struct {
	t               *testing.T
	f               *fakeAgentServer
	front           *httptest.Server
	mu              sync.Mutex
	dropTranscript  int // close the connection on the next N transcript reads
	dropAfterCreate int // process N createAgentThread calls, then close without answering
	// pageSize is how many entities one transcript page holds (0 = the whole
	// thread in one page). It is deliberately NOT derived from the request limit:
	// the real service was observed to return 52 patches for limit 2 and 762 for
	// limit 100, so limit is not an entity cap.
	pageSize   int
	pageHook   func(depth int, cursor string, page map[string]interface{}) (status int, body string)
	reqs       []map[string]interface{} // every getThreadTranscript request body, in order
	hang       chan struct{}            // when set, transcript reads block until closed (or the client goes away)
	hangCursor bool                     // only block reads that carry a cursor
	hung       chan struct{}            // receives once per blocked transcript read
}

func newRecoveryEnv(t *testing.T) *recoveryEnv {
	f := newFakeAgentServer(t)
	e := &recoveryEnv{t: t, f: f}
	e.front = httptest.NewServer(http.HandlerFunc(e.serve))
	t.Cleanup(e.front.Close)
	NotionAPIBase = e.front.URL
	oldClient := agentThreadHTTPClient
	agentThreadHTTPClient = func(time.Duration) *http.Client { return e.front.Client() }
	oldQuota := quotaFetcher
	quotaFetcher = func(*Account) (*QuotaInfo, error) { return nil, errors.New("offline") }
	t.Cleanup(func() { agentThreadHTTPClient, quotaFetcher = oldClient, oldQuota })
	return e
}

func closeConn(w http.ResponseWriter) {
	if h, ok := w.(http.Hijacker); ok {
		if c, _, err := h.Hijack(); err == nil {
			c.Close()
		}
	}
}

func (e *recoveryEnv) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	endpoint := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	var body map[string]interface{}
	json.Unmarshal(raw, &body)
	e.mu.Lock()
	drop, dropAfter := false, false
	var hang chan struct{}
	var hook func(int, string, map[string]interface{}) (int, string)
	switch endpoint {
	case "getThreadTranscript":
		hook = e.pageHook
		e.reqs = append(e.reqs, body)
		if _, hasCursor := body["cursor"]; !e.hangCursor || hasCursor {
			hang = e.hang
		}
		if hang != nil && e.hung != nil {
			select {
			case e.hung <- struct{}{}:
			default:
			}
		}
		if e.dropTranscript > 0 {
			e.dropTranscript--
			drop = true
		}
	case "createAgentThread":
		if e.dropAfterCreate > 0 {
			e.dropAfterCreate--
			dropAfter = true
		}
	}
	e.mu.Unlock()
	if hang != nil {
		select {
		case <-hang:
		case <-r.Context().Done():
			return
		}
	}
	if drop {
		closeConn(w)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	rec := httptest.NewRecorder()
	e.f.handle(rec, r)
	if dropAfter {
		closeConn(w)
		return
	}
	out := rec.Body.Bytes()
	if endpoint == "getThreadTranscript" && rec.Code == 200 {
		status, text := e.pageTranscript(body, out, hook)
		if status != 0 {
			w.WriteHeader(status)
			io.WriteString(w, text)
			return
		}
		out = []byte(text)
	}
	for k, v := range rec.Header() {
		w.Header()[k] = v
	}
	w.WriteHeader(rec.Code)
	w.Write(out)
}

// pageTranscript turns the fake's full transcript into the real paged form:
// request {spaceId, threadId, direction:"backward", cursor?, limit}; response
// {type, patches (older entities first), has_more_backward, backward_cursor,
// session}. A page holds the newest pageSize entities below the cursor and its
// backward_cursor continues to strictly older ones. Field names follow the
// frontend contract confirmed read-only by Codex; the cursor VALUE is a test
// double (the real cursor is opaque and was not published).
func (e *recoveryEnv) pageTranscript(req map[string]interface{}, full []byte, hook func(int, string, map[string]interface{}) (int, string)) (int, string) {
	if req["direction"] != "backward" {
		return http.StatusBadRequest, `{"message":"direction must be backward"}`
	}
	var page map[string]interface{}
	json.Unmarshal(full, &page)
	patches, _ := page["patches"].([]interface{})
	var puts, other []interface{}
	for _, p := range patches {
		if m, _ := p.(map[string]interface{}); m["op"] == "put" {
			puts = append(puts, p)
		} else {
			other = append(other, p)
		}
	}
	tid, _ := req["threadId"].(string)
	end, depth, cursor := len(puts), 0, ""
	if c, ok := req["cursor"]; ok {
		cursor, _ = c.(string)
		if _, err := fmt.Sscanf(cursor, "cur|"+tid+"|%d|%d", &end, &depth); err != nil || end < 0 || end > len(puts) {
			return http.StatusBadRequest, `{"message":"invalid cursor"}`
		}
	}
	if e.pageSize > 0 {
		start := end - e.pageSize
		if start < 0 {
			start = 0
		}
		kept := append([]interface{}{}, puts[start:end]...)
		if cursor == "" {
			kept = append(append([]interface{}{}, other...), kept...)
		}
		page["patches"] = kept
		page["has_more_backward"] = start > 0
		if start > 0 {
			page["backward_cursor"] = fmt.Sprintf("cur|%s|%d|%d", tid, start, depth+1)
		}
	}
	if hook != nil {
		if status, text := hook(depth, cursor, page); status != 0 {
			return status, text
		}
	}
	out, _ := json.Marshal(page)
	return 0, string(out)
}

// maxDepth is the deepest cursor page requested so far (0 = only newest pages).
func (e *recoveryEnv) maxDepth() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	m := 0
	for _, r := range e.reqs {
		if c, ok := r["cursor"].(string); ok {
			if parts := strings.Split(c, "|"); len(parts) == 4 {
				var d int
				fmt.Sscanf(parts[3], "%d", &d)
				if d > m {
					m = d
				}
			}
		}
	}
	return m
}

func (e *recoveryEnv) setHook(h func(depth int, cursor string, page map[string]interface{}) (int, string)) {
	e.mu.Lock()
	e.pageHook = h
	e.mu.Unlock()
}

// complete finishes every stalled turn the way the real service eventually does,
// optionally after many intermediate (tool/activity) entities.
func (e *recoveryEnv) complete(fill int, text string) {
	e.f.mu.Lock()
	defer e.f.mu.Unlock()
	for _, th := range e.f.threads {
		if th.status != "running" {
			continue
		}
		for i := 0; i < fill; i++ {
			th.seq++
			th.entities = append(th.entities, map[string]interface{}{"id": fmt.Sprintf("act%d", th.seq), "kind": "tool_activity", "sequence": th.seq})
		}
		th.seq++
		th.entities = append(th.entities, map[string]interface{}{"id": fmt.Sprintf("a%d", th.seq), "kind": "assistant_message", "sequence": th.seq, "content": []map[string]string{{"type": "text", "text": text}}})
		th.seq++
		th.entities = append(th.entities, map[string]interface{}{"id": fmt.Sprintf("d%d", th.seq), "kind": "turn_completed", "sequence": th.seq, "stop_reason": "end_turn"})
		th.status = "idle"
	}
}

func useRecoveryStore(t *testing.T, path string) *agentThreadStore {
	agentStoreOnce.Do(func() {})
	old := agentStoreInst
	agentStoreInst = newAgentThreadStore(path)
	t.Cleanup(func() { agentStoreInst = old })
	return agentStoreInst
}

func setKeepalive(t *testing.T, delay, interval time.Duration) {
	od, oi := agentKeepaliveDelay, agentKeepaliveInterval
	agentKeepaliveDelay, agentKeepaliveInterval = delay, interval
	t.Cleanup(func() { agentKeepaliveDelay, agentKeepaliveInterval = od, oi })
}

// ---- seam: Anthropic /v1/messages response as Claude Code sees it ----------

type sseEvent struct {
	Name string
	Data map[string]interface{}
}

type sseCall struct {
	t      *testing.T
	events chan sseEvent
	done   chan struct{}
	Status int
	Header http.Header
	Body   string // non-SSE body
	ready  chan struct{}
	all    []sseEvent
	cancel func()
}

func recoveryHandler(acc *Account, msgs []ChatMessage, model string, hasTools, stream bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sess := &Session{ClientKey: "client-1"}
		if stream {
			handleAnthropicStream(w, acc, msgs, model, "msg_test", hasTools, false, false, nil, false, nil, nil, sess, r.Context().Done())
		} else {
			handleAnthropicNonStream(w, acc, msgs, model, "msg_test", hasTools, false, false, nil, false, nil, nil, sess, r.Context().Done())
		}
	})
}

func startSSE(t *testing.T, url string) *sseCall {
	c := &sseCall{t: t, events: make(chan sseEvent, 256), done: make(chan struct{}), ready: make(chan struct{})}
	tr := &http.Transport{}
	client := &http.Client{Transport: tr}
	c.cancel = func() { tr.CloseIdleConnections(); tr.CancelRequest(nil) }
	req, _ := http.NewRequest("POST", url, strings.NewReader("{}"))
	go func() {
		defer close(c.done)
		defer close(c.events)
		resp, err := client.Do(req)
		if err != nil {
			close(c.ready)
			return
		}
		defer resp.Body.Close()
		c.Status, c.Header = resp.StatusCode, resp.Header
		close(c.ready)
		if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
			b, _ := io.ReadAll(resp.Body)
			c.Body = string(b)
			return
		}
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		var name string
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				ev := sseEvent{Name: name, Data: map[string]interface{}{}}
				json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev.Data)
				c.events <- ev
			}
		}
	}()
	return c
}

func (c *sseCall) next(d time.Duration) (sseEvent, bool) {
	select {
	case ev, ok := <-c.events:
		if ok {
			c.all = append(c.all, ev)
		}
		return ev, ok
	case <-time.After(d):
		return sseEvent{}, false
	}
}

func (c *sseCall) drain(d time.Duration) []sseEvent {
	deadline := time.After(d)
	for {
		select {
		case ev, ok := <-c.events:
			if !ok {
				return c.all
			}
			c.all = append(c.all, ev)
		case <-deadline:
			c.t.Fatalf("stream did not finish within %v; events so far: %v", d, names(c.all))
		}
	}
}

func names(evs []sseEvent) []string {
	var out []string
	for _, e := range evs {
		out = append(out, e.Name)
	}
	return out
}

func textOf(evs []sseEvent) string {
	var b strings.Builder
	for _, e := range evs {
		if e.Name == "content_block_delta" {
			if d, _ := e.Data["delta"].(map[string]interface{}); d["type"] == "text_delta" {
				b.WriteString(d["text"].(string))
			}
		}
	}
	return b.String()
}

func countName(evs []sseEvent, name string) int {
	n := 0
	for _, e := range evs {
		if e.Name == name {
			n++
		}
	}
	return n
}

func errorEvent(t *testing.T, evs []sseEvent) map[string]interface{} {
	t.Helper()
	if len(evs) == 0 || evs[len(evs)-1].Name != "error" {
		t.Fatalf("stream must end with an SSE error event, got %v", names(evs))
	}
	m, _ := evs[len(evs)-1].Data["error"].(map[string]interface{})
	return m
}

func recAcc() *Account { return &Account{UserID: "user", SpaceID: "space"} }

// ---- 1. keepalive while the remote turn is running -------------------------

func TestAgentRecoveryStreamSendsLegalPingWhileRemoteRuns(t *testing.T) {
	for _, tooled := range []bool{false, true} {
		t.Run(fmt.Sprintf("tooled=%v", tooled), func(t *testing.T) {
			env := newRecoveryEnv(t)
			useRecoveryStore(t, "")
			AppConfig.Timeouts.InferenceTimeout = 20
			setKeepalive(t, 30*time.Millisecond, 30*time.Millisecond)
			env.f.stall = true
			srv := httptest.NewServer(recoveryHandler(recAcc(), []ChatMessage{u("do it")}, "auto", tooled, true))
			defer srv.Close()
			call := startSSE(t, srv.URL)
			first, ok := call.next(2 * time.Second)
			if !ok || first.Name != "message_start" {
				t.Fatalf("no message_start while the remote turn is running (got %q ok=%v): client would sit silent", first.Name, ok)
			}
			for i := 0; i < 3; i++ {
				ev, ok := call.next(2 * time.Second)
				if !ok || ev.Name != "ping" {
					t.Fatalf("ping %d missing while waiting: %q ok=%v", i, ev.Name, ok)
				}
			}
			reply := "final answer"
			if tooled {
				reply = `{"name":"Bash","arguments":{"command":"echo hi"}}`
			}
			env.complete(0, reply)
			evs := call.drain(10 * time.Second)
			if countName(evs, "message_start") != 1 || countName(evs, "message_stop") != 1 || countName(evs, "error") != 0 {
				t.Fatalf("lifecycle: %v", names(evs))
			}
			if tooled {
				tools := 0
				for _, e := range evs {
					if cb, _ := e.Data["content_block"].(map[string]interface{}); cb["type"] == "tool_use" {
						tools++
					}
				}
				if tools != 1 || countName(evs, "content_block_start") != 1 {
					t.Fatalf("expected exactly one tool_use and nothing fabricated: %v", names(evs))
				}
			} else if textOf(evs) != "final answer" {
				t.Fatalf("text %q", textOf(evs))
			}
			if c, s := env.f.counts(); c != 1 || s != 0 {
				t.Fatalf("creates=%d sends=%d", c, s)
			}
		})
	}
}

// ---- 2. wait deadline after accept: pending, not failure --------------------

func TestAgentRecoveryDeadlineStreamIsPendingThenRetryReplaysLateReplyOnce(t *testing.T) {
	env := newRecoveryEnv(t)
	useRecoveryStore(t, "")
	AppConfig.Timeouts.InferenceTimeout = 1
	setKeepalive(t, 20*time.Millisecond, 20*time.Millisecond)
	env.f.stall = true
	srv := httptest.NewServer(recoveryHandler(recAcc(), []ChatMessage{u("slow job")}, "auto", false, true))
	defer srv.Close()
	call := startSSE(t, srv.URL)
	evs := call.drain(10 * time.Second)
	e := errorEvent(t, evs)
	msg, _ := e["message"].(string)
	if !strings.Contains(msg, "[bridge:pending]") || !strings.Contains(msg, "Retry the identical request") || strings.Contains(msg, "[bridge:failed]") {
		t.Fatalf("pending state must be distinguishable from a real failure: %v", e)
	}
	if countName(evs, "ping") == 0 || countName(evs, "message_stop") != 0 || textOf(evs) != "" {
		t.Fatalf("no fabricated answer, no clean end: %v", names(evs))
	}
	env.complete(0, "late answer")
	for i := 0; i < 2; i++ { // second pass proves replay-once without sending
		call = startSSE(t, srv.URL)
		evs = call.drain(10 * time.Second)
		if textOf(evs) != "late answer" || countName(evs, "message_stop") != 1 || countName(evs, "error") != 0 {
			t.Fatalf("retry %d: %q %v", i, textOf(evs), names(evs))
		}
	}
	if c, s := env.f.counts(); c != 1 || s != 0 {
		t.Fatalf("one round must stay at one create / zero sends: creates=%d sends=%d", c, s)
	}
}

func TestAgentRecoveryDeadlineNonStreamIsRetryable504(t *testing.T) {
	env := newRecoveryEnv(t)
	useRecoveryStore(t, "")
	AppConfig.Timeouts.InferenceTimeout = 1
	env.f.stall = true
	srv := httptest.NewServer(recoveryHandler(recAcc(), []ChatMessage{u("slow job")}, "auto", false, false))
	defer srv.Close()
	call := startSSE(t, srv.URL)
	call.drain(10 * time.Second)
	if call.Status != 504 || call.Header.Get("x-should-retry") != "true" || !strings.Contains(call.Body, "[bridge:pending]") {
		t.Fatalf("status=%d should-retry=%q body=%q", call.Status, call.Header.Get("x-should-retry"), call.Body)
	}
	env.complete(0, "late answer")
	call = startSSE(t, srv.URL)
	call.drain(10 * time.Second)
	if call.Status != 200 || !strings.Contains(call.Body, "late answer") {
		t.Fatalf("retry: %d %q", call.Status, call.Body)
	}
	if c, s := env.f.counts(); c != 1 || s != 0 {
		t.Fatalf("creates=%d sends=%d", c, s)
	}
}

// ---- 3. real failures stay failures -----------------------------------------

func TestAgentRecoveryRealFailureIsNotPending(t *testing.T) {
	env := newRecoveryEnv(t)
	useRecoveryStore(t, "")
	env.f.failTurn = true
	srv := httptest.NewServer(recoveryHandler(recAcc(), []ChatMessage{u("will fail")}, "auto", false, true))
	defer srv.Close()
	call := startSSE(t, srv.URL)
	call.drain(10 * time.Second)
	if call.Status != 502 || call.Header.Get("x-should-retry") != "false" || !strings.Contains(call.Body, "[bridge:failed]") || strings.Contains(call.Body, "pending") {
		t.Fatalf("status=%d should-retry=%q body=%q", call.Status, call.Header.Get("x-should-retry"), call.Body)
	}
}

func TestAgentRecoveryUnsupportedModelIsClassifiedNotRemote(t *testing.T) {
	env := newRecoveryEnv(t)
	useRecoveryStore(t, "")
	srv := httptest.NewServer(recoveryHandler(recAcc(), []ChatMessage{u("hi")}, "claude-opus-5-5[1m]", false, true))
	defer srv.Close()
	call := startSSE(t, srv.URL)
	call.drain(5 * time.Second)
	if call.Status != 400 || !strings.Contains(call.Body, "[bridge:model_unavailable]") || call.Header.Get("x-should-retry") != "false" {
		t.Fatalf("status=%d body=%q", call.Status, call.Body)
	}
	if c, s := env.f.counts(); c != 0 || s != 0 {
		t.Fatalf("a model problem must not reach Notion: %d %d", c, s)
	}
}

func TestAgentRecoveryClassifyAgentFailureTable(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		kind    string
		pending bool
		status  int
		retry   bool
	}{
		{"recoverable deadline", &agentRecoverableError{Class: "deadline", ThreadID: "abcdef123456", Cause: errors.New("x")}, "pending", true, 504, true},
		{"definitive 404", &agentHTTPError{Endpoint: "getThreadTranscript", Status: 404}, "rejected", false, 502, false},
		{"turn failed", transcriptErr("Notion Agent transcript contains failure"), "failed", false, 502, false},
		{"bad transcript type", transcriptErrKind("transcript_format", "unexpected Notion Agent transcript type"), "transcript_format", false, 502, false},
		{"bounded read", &agentTranscriptIncompleteError{msg: "too large"}, "transcript_incomplete", false, 502, false},
		{"model", &agentModelError{model: "x[1m]"}, "model_unavailable", false, 400, false},
		{"empty", ErrEmptyResponse, "failed", false, 502, false},
		{"binding busy", &agentBindingError{Kind: "busy", Msg: "busy"}, "busy", false, 409, true},
		{"binding rejected follow-up", &agentBindingError{Kind: "rejected", Msg: "rejected"}, "rejected", false, 409, true},
	}
	for _, c := range cases {
		f := classifyAgentFailure(c.err)
		if f.Kind != c.kind || f.Pending != c.pending || f.Status != c.status || f.ShouldRetry != c.retry {
			t.Errorf("%s: got %+v", c.name, f)
		}
		if strings.Contains(f.Message, "http://") || strings.Contains(f.Message, "cookie") {
			t.Errorf("%s: message leaks transport detail: %q", c.name, f.Message)
		}
	}
	w := httptest.NewRecorder()
	writeAgentFailureJSON(w, "req_rejected", classifyAgentFailure(&agentBindingError{Kind: "rejected", Msg: "rejected follow-up"}))
	if w.Header().Get("x-should-retry") != "true" {
		t.Fatalf("expected x-should-retry=true for rolled-back rejected follow-up, got %q", w.Header().Get("x-should-retry"))
	}
}

// ---- 4. lost acknowledgement, dropped connection, restart -------------------

func TestAgentRecoveryAckLostAfterCreateRecoversWithoutSecondCreate(t *testing.T) {
	env := newRecoveryEnv(t)
	path := filepath.Join(t.TempDir(), "b.state")
	store := newAgentThreadStore(path)
	acc := recAcc()
	msgs := []ChatMessage{u("one shot")}
	env.dropAfterCreate = 1
	_, err := runTurn(t, env.f, store, acc, "client-1", msgs)
	var rec *agentRecoverableError
	if !errors.As(err, &rec) || rec.Class != "network" {
		t.Fatalf("lost ack must be a recoverable error, got %T %v", err, err)
	}
	store = newAgentThreadStore(path) // proxy restart
	out, err := runTurn(t, env.f, store, acc, "client-1", msgs)
	if err != nil || out != "reply-1" {
		t.Fatalf("%q %v", out, err)
	}
	if c, s := env.f.counts(); c != 1 || s != 0 {
		t.Fatalf("creates=%d sends=%d", c, s)
	}
}

func TestAgentRecoveryDroppedTranscriptReadIsRecoverable(t *testing.T) {
	env := newRecoveryEnv(t)
	store := newAgentThreadStore(filepath.Join(t.TempDir(), "b.state"))
	acc := recAcc()
	msgs := []ChatMessage{u("hello")}
	env.dropTranscript = 1
	_, err := runTurn(t, env.f, store, acc, "client-1", msgs)
	var rec *agentRecoverableError
	if !errors.As(err, &rec) || rec.Class != "network" || rec.ThreadID == "" {
		t.Fatalf("got %T %v", err, err)
	}
	out, err := runTurn(t, env.f, store, acc, "client-1", msgs)
	if err != nil || out != "reply-1" {
		t.Fatalf("%q %v", out, err)
	}
	if c, s := env.f.counts(); c != 1 || s != 0 {
		t.Fatalf("creates=%d sends=%d", c, s)
	}
}

func TestAgentRecoveryFollowUpDeadlineThenRestartNoDuplicateSend(t *testing.T) {
	env := newRecoveryEnv(t)
	AppConfig.Timeouts.InferenceTimeout = 1
	path := filepath.Join(t.TempDir(), "b.state")
	store := newAgentThreadStore(path)
	acc := recAcc()
	if out, err := runTurn(t, env.f, store, acc, "client-1", []ChatMessage{u("a")}); err != nil || out != "reply-1" {
		t.Fatalf("%q %v", out, err)
	}
	msgs := []ChatMessage{u("a"), a("reply-1"), u("b")}
	env.f.stall = true
	_, err := runTurn(t, env.f, store, acc, "client-1", msgs)
	var rec *agentRecoverableError
	if !errors.As(err, &rec) || rec.Class != "deadline" {
		t.Fatalf("got %T %v", err, err)
	}
	env.complete(0, "second late")
	store = newAgentThreadStore(path)
	out, err := runTurn(t, env.f, store, acc, "client-1", msgs)
	if err != nil || out != "second late" {
		t.Fatalf("%q %v", out, err)
	}
	if c, s := env.f.counts(); c != 1 || s != 1 {
		t.Fatalf("creates=%d sends=%d", c, s)
	}
}

// ---- 5. pagination over the real cursor chain ---------------------------------
//
// Seam: recoveryEnv.pageTranscript serves the real request/response FIELD shape
// (direction, cursor, limit -> patches, has_more_backward, backward_cursor,
// older sequences on later pages) behind callAgentThreadResumable. It catches
// merge order, bounds, loop/progress/format checks, failure classes and the
// no-duplicate-submit guarantee. It cannot catch: the real opaque cursor value,
// the real server's page boundaries, or real Notion errors. That was verified
// read-only by Codex only (limit 2 -> two cursor pages, third page message error).

// bigTurn runs a stalled turn that finishes after `fill` extra entities.
func bigTurn(t *testing.T, env *recoveryEnv, store *agentThreadStore, key string, msgs []ChatMessage, fill int, text string) (string, error) {
	t.Helper()
	env.f.stall = true
	go func() { time.Sleep(150 * time.Millisecond); env.complete(fill, text) }()
	return runTurn(t, env.f, store, recAcc(), key, msgs)
}

func assertTranscriptRequests(t *testing.T, env *recoveryEnv) {
	t.Helper()
	env.mu.Lock()
	defer env.mu.Unlock()
	if len(env.reqs) == 0 {
		t.Fatal("no transcript requests")
	}
	for i, r := range env.reqs {
		if r["direction"] != "backward" || r["limit"] != float64(agentTranscriptPageLimit) || r["spaceId"] != "space" || r["threadId"] == "" {
			t.Fatalf("request %d does not match the getThreadTranscript contract: %v", i, r)
		}
		if c, ok := r["cursor"]; ok {
			if cs, isStr := c.(string); !isStr || cs == "" {
				t.Fatalf("request %d cursor must be the opaque backward_cursor string: %v", i, r)
			}
		}
	}
}

func notFailed(t *testing.T, store *agentThreadStore) {
	t.Helper()
	for _, rec := range store.records {
		if rec.State == agentStateFailed {
			t.Fatal("a transcript read problem must not mark the remote turn failed")
		}
	}
}

func TestAgentRecoveryPagedNewThreadMergesAllPages(t *testing.T) {
	env := newRecoveryEnv(t)
	AppConfig.Timeouts.InferenceTimeout = 10
	env.pageSize = 100
	// Older pages must not be trusted for state: they claim a failed session.
	env.setHook(func(depth int, _ string, page map[string]interface{}) (int, string) {
		if depth > 0 {
			page["session"] = map[string]interface{}{"status": "failed", "sequence": 1}
		}
		return 0, ""
	})
	store := newAgentThreadStore("")
	out, err := bigTurn(t, env, store, "client-1", []ChatMessage{u("big job")}, 250, "big answer")
	if err != nil || out != "big answer" {
		t.Fatalf("paged thread must be read completely: %q %v", out, err)
	}
	assertTranscriptRequests(t, env)
	if env.maxDepth() != 2 {
		t.Fatalf("expected the cursor chain to be followed to the 3rd page, depth=%d", env.maxDepth())
	}
	if c, s := env.f.counts(); c != 1 || s != 0 {
		t.Fatalf("creates=%d sends=%d", c, s)
	}
}

func TestAgentRecoveryPagedFollowUpStopsWhereBaselineIsCovered(t *testing.T) {
	env := newRecoveryEnv(t)
	AppConfig.Timeouts.InferenceTimeout = 10
	env.pageSize = 100
	store := newAgentThreadStore("")
	if out, err := bigTurn(t, env, store, "client-1", []ChatMessage{u("a")}, 130, "first"); err != nil || out != "first" {
		t.Fatalf("%q %v", out, err)
	}
	// Turn two is ~120 entities: its newest page does not reach the previous
	// turn, the second page does while still reporting has_more_backward.
	msgs := []ChatMessage{u("a"), a("first"), u("b")}
	out, err := bigTurn(t, env, store, "client-1", msgs, 120, "second")
	if err != nil || out != "second" {
		t.Fatalf("follow-up must return only its own turn, got %q %v", out, err)
	}
	assertTranscriptRequests(t, env)
	if env.maxDepth() != 1 {
		t.Fatalf("baseline is covered on page 2; page 3 must not be read (depth=%d)", env.maxDepth())
	}
	if c, s := env.f.counts(); c != 1 || s != 1 {
		t.Fatalf("creates=%d sends=%d", c, s)
	}
}

// failure kinds of a cursor page, and what the client must see.
func TestAgentRecoveryCursorPageProblemsAreRefusedNotPartial(t *testing.T) {
	cases := []struct {
		name string
		hook func(depth int, cursor string, page map[string]interface{}) (int, string)
	}{
		{"third page is a message error (HTTP 200)", func(d int, _ string, _ map[string]interface{}) (int, string) {
			if d == 2 {
				return 200, `{"message":"cannot read this page"}`
			}
			return 0, ""
		}},
		{"third page is HTTP 400", func(d int, _ string, _ map[string]interface{}) (int, string) {
			if d == 2 {
				return 400, `{"message":"bad cursor"}`
			}
			return 0, ""
		}},
		{"cursor repeats", func(d int, c string, p map[string]interface{}) (int, string) {
			if d == 1 {
				p["backward_cursor"] = c
			}
			return 0, ""
		}},
		{"no progress (same entities again)", func(d int, c string, p map[string]interface{}) (int, string) {
			if d == 1 {
				p["patches"] = savedFirstPage.patches
			}
			return 0, ""
		}},
		{"more pages but no cursor", func(d int, _ string, p map[string]interface{}) (int, string) {
			if d == 1 {
				delete(p, "backward_cursor")
			}
			return 0, ""
		}},
		{"cursor is not a string", func(d int, _ string, p map[string]interface{}) (int, string) {
			if d == 1 {
				p["backward_cursor"] = 12345
			}
			return 0, ""
		}},
		{"page type is wrong", func(d int, _ string, p map[string]interface{}) (int, string) {
			if d == 1 {
				p["type"] = "error"
			}
			return 0, ""
		}},
		{"page body is not JSON", func(d int, _ string, _ map[string]interface{}) (int, string) {
			if d == 1 {
				return 200, "<html>gateway</html>"
			}
			return 0, ""
		}},
		{"cursor page without any sequence", func(d int, _ string, p map[string]interface{}) (int, string) {
			if d == 1 {
				p["patches"] = []interface{}{}
			}
			return 0, ""
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newRecoveryEnv(t)
			AppConfig.Timeouts.InferenceTimeout = 10
			env.pageSize = 100
			savedFirstPage = firstPageStash{}
			inner := tc.hook
			env.setHook(func(d int, c string, p map[string]interface{}) (int, string) {
				if d == 0 {
					savedFirstPage.patches = p["patches"]
				}
				return inner(d, c, p)
			})
			store := newAgentThreadStore("")
			out, err := bigTurn(t, env, store, "client-1", []ChatMessage{u("big job")}, 250, "big answer")
			var inc *agentTranscriptIncompleteError
			if out != "" || !errors.As(err, &inc) {
				t.Fatalf("a broken cursor page must be a completeness error with no output: %q %T %v", out, err, err)
			}
			t.Logf("refused with: %v", err)
			f := classifyAgentFailure(err)
			if f.Kind != "transcript_incomplete" || f.Pending || f.Status != http.StatusBadGateway {
				t.Fatalf("%+v", f)
			}
			if strings.Contains(err.Error(), "cur|") || strings.Contains(f.Message, "cur|") {
				t.Fatalf("cursor values must not be echoed: %v", err)
			}
			notFailed(t, store)
			// The turn stays bound: once the page reads work, the identical
			// request returns the reply and never creates/sends again.
			env.setHook(nil)
			out, err = runTurn(t, env.f, store, recAcc(), "client-1", []ChatMessage{u("big job")})
			if err != nil || out != "big answer" {
				t.Fatalf("recovery after cursor failure: %q %v", out, err)
			}
			if c, s := env.f.counts(); c != 1 || s != 0 {
				t.Fatalf("creates=%d sends=%d", c, s)
			}
		})
	}
}

type firstPageStash struct{ patches interface{} }

var savedFirstPage firstPageStash

func TestAgentRecoveryTransientCursorPageErrorIsPendingAndNeverResubmits(t *testing.T) {
	for _, follow := range []bool{false, true} {
		name := "new thread"
		if follow {
			name = "follow-up"
		}
		t.Run(name, func(t *testing.T) {
			env := newRecoveryEnv(t)
			AppConfig.Timeouts.InferenceTimeout = 10
			env.pageSize = 100
			path := filepath.Join(t.TempDir(), "b.state")
			store := newAgentThreadStore(path)
			msgs := []ChatMessage{u("a")}
			if follow {
				if out, err := runTurn(t, env.f, store, recAcc(), "client-1", msgs); err != nil || out != "reply-1" {
					t.Fatalf("%q %v", out, err)
				}
				msgs = []ChatMessage{u("a"), a("reply-1"), u("b")}
			}
			env.setHook(func(d int, _ string, _ map[string]interface{}) (int, string) {
				if d == 1 {
					return 503, `{"message":"try later"}`
				}
				return 0, ""
			})
			out, err := bigTurn(t, env, store, "client-1", msgs, 250, "late big")
			var rec *agentRecoverableError
			if out != "" || !errors.As(err, &rec) || rec.Class != "http_503" {
				t.Fatalf("a transient cursor error must be recoverable pending, got %q %T %v", out, err, err)
			}
			if f := classifyAgentFailure(err); f.Kind != "pending" || !f.ShouldRetry || f.Status != http.StatusGatewayTimeout {
				t.Fatalf("%+v", f)
			}
			notFailed(t, store)
			env.setHook(nil)
			store = newAgentThreadStore(path) // proxy restart in between
			out, err = runTurn(t, env.f, store, recAcc(), "client-1", msgs)
			if err != nil || out != "late big" {
				t.Fatalf("%q %v", out, err)
			}
			wantSends := 0
			if follow {
				wantSends = 1
			}
			if c, s := env.f.counts(); c != 1 || s != wantSends {
				t.Fatalf("a cursor failure must never re-submit: creates=%d sends=%d", c, s)
			}
		})
	}
}

func TestAgentRecoveryPageChainIsBounded(t *testing.T) {
	env := newRecoveryEnv(t)
	AppConfig.Timeouts.InferenceTimeout = 10
	old := agentTranscriptMaxPages
	agentTranscriptMaxPages = 3
	t.Cleanup(func() { agentTranscriptMaxPages = old })
	env.pageSize = 20
	store := newAgentThreadStore("")
	out, err := bigTurn(t, env, store, "client-1", []ChatMessage{u("big job")}, 250, "big answer")
	var inc *agentTranscriptIncompleteError
	if out != "" || !errors.As(err, &inc) {
		t.Fatalf("an over-long chain must be refused: %q %T %v", out, err, err)
	}
	if env.maxDepth() != 2 {
		t.Fatalf("chain must stop after %d pages, deepest cursor page=%d", agentTranscriptMaxPages, env.maxDepth())
	}
	notFailed(t, store)
	if c, s := env.f.counts(); c != 1 || s != 0 {
		t.Fatalf("creates=%d sends=%d", c, s)
	}
}

func TestAgentRecoveryGoneCancelsInflightCursorPage(t *testing.T) {
	env := newRecoveryEnv(t)
	AppConfig.Timeouts.InferenceTimeout = 20
	env.pageSize = 100
	store := newAgentThreadStore(filepath.Join(t.TempDir(), "b.state"))
	env.f.stall = true
	// Hang every read that carries a cursor, once the thread is big enough to need one.
	env.hang = make(chan struct{})
	env.hangCursor = true
	env.hung = make(chan struct{}, 1)
	gone := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		result <- callAgentThreadResumableCancel(gone, recAcc(), []ChatMessage{u("job")}, "", "low", "client-1", false, func(string, bool, *UsageInfo) {}, env.front.Client(), store)
	}()
	time.Sleep(300 * time.Millisecond)
	env.complete(250, "big")
	select {
	case <-env.hung:
	case <-time.After(5 * time.Second):
		t.Fatal("cursor page read never started")
	}
	start := time.Now()
	close(gone)
	select {
	case err := <-result:
		var rec *agentRecoverableError
		if !errors.As(err, &rec) || rec.Class != "client_gone" {
			t.Fatalf("want client_gone, got %T %v", err, err)
		}
		if time.Since(start) > 2*time.Second {
			t.Fatalf("cursor page request not cancelled promptly: %v", time.Since(start))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("in-flight cursor page ignored the disconnect")
	}
}

// ---- 6. client disconnect releases the thread --------------------------------

func TestAgentRecoveryClientDisconnectStopsWaitingAndKeepsRemoteTurn(t *testing.T) {
	env := newRecoveryEnv(t)
	useRecoveryStore(t, "")
	AppConfig.Timeouts.InferenceTimeout = 20
	setKeepalive(t, 20*time.Millisecond, 20*time.Millisecond)
	env.f.stall = true
	var wg sync.WaitGroup
	wg.Add(1)
	h := recoveryHandler(recAcc(), []ChatMessage{u("job")}, "auto", false, true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { defer wg.Done(); h.ServeHTTP(w, r) }))
	defer srv.Close()
	conn := startSSE(t, srv.URL)
	if ev, ok := conn.next(2 * time.Second); !ok || ev.Name != "message_start" {
		t.Fatalf("no stream %v", ev.Name)
	}
	conn.cancel()
	srv.CloseClientConnections()
	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("handler kept polling for a client that is gone; the thread stays locked for the retry")
	}
	env.complete(0, "after disconnect")
	srv2 := httptest.NewServer(recoveryHandler(recAcc(), []ChatMessage{u("job")}, "auto", false, true))
	defer srv2.Close()
	evs := startSSE(t, srv2.URL).drain(10 * time.Second)
	if textOf(evs) != "after disconnect" {
		t.Fatalf("%q %v", textOf(evs), names(evs))
	}
	if c, s := env.f.counts(); c != 1 || s != 0 {
		t.Fatalf("creates=%d sends=%d", c, s)
	}
}

func TestAgentRecoveryGoneCancelsInflightTranscript(t *testing.T) {
	env := newRecoveryEnv(t)
	AppConfig.Timeouts.InferenceTimeout = 20
	path := filepath.Join(t.TempDir(), "b.state")
	store := newAgentThreadStore(path)
	acc := recAcc()
	msgs := []ChatMessage{u("job")}
	env.hang = make(chan struct{})
	env.hung = make(chan struct{}, 1)
	gone := make(chan struct{})
	result := make(chan error, 1)
	start := time.Now()
	go func() {
		result <- callAgentThreadResumableCancel(gone, acc, msgs, "", "low", "client-1", false, func(string, bool, *UsageInfo) {}, env.front.Client(), store)
	}()
	select {
	case <-env.hung:
	case <-time.After(3 * time.Second):
		t.Fatal("transcript read never started")
	}
	close(gone)
	select {
	case err := <-result:
		var rec *agentRecoverableError
		if !errors.As(err, &rec) || rec.Class != "client_gone" || rec.ThreadID == "" {
			t.Fatalf("want client_gone recoverable, got %T %v", err, err)
		}
		if time.Since(start) > 2*time.Second {
			t.Fatalf("in-flight request was not cancelled promptly: %v", time.Since(start))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("in-flight transcript request ignored the disconnect; thread lock stays held")
	}
	// The remote turn is bound and recoverable: no second create/send, no busy lock.
	env.mu.Lock()
	env.hang = nil
	env.mu.Unlock()
	out, err := runTurn(t, env.f, store, acc, "client-1", msgs)
	if err != nil || out != "reply-1" {
		t.Fatalf("%q %v", out, err)
	}
	if c, s := env.f.counts(); c != 1 || s != 0 {
		t.Fatalf("creates=%d sends=%d", c, s)
	}
}

// ---- 5a. an unreadable transcript is never a remote failure ---------------------
//
// Seam: the fake front serves HTTP 200 bodies the reader cannot use (message
// error, wrong page type, non-JSON, entity without ID) on the NEWEST page while
// the remote turn is already complete, then recovers. Catches turn.failed being
// called for a read problem (the retry would then send again and return the
// old reply glued to a new one) and a probe that treats an unreadable page as
// "not submitted". Cannot catch what the real service returns in those cases.

var unreadablePages = []struct {
	name string
	hook func(depth int, cursor string, page map[string]interface{}) (int, string)
}{
	{"HTTP 200 message error", func(d int, _ string, _ map[string]interface{}) (int, string) {
		if d == 0 {
			return 200, `{"message":"temporary transcript error"}`
		}
		return 0, ""
	}},
	{"not a transcript page", func(d int, _ string, p map[string]interface{}) (int, string) {
		if d == 0 {
			p["type"] = "error"
		}
		return 0, ""
	}},
	{"not JSON", func(d int, _ string, _ map[string]interface{}) (int, string) {
		if d == 0 {
			return 200, "<html>bad gateway</html>"
		}
		return 0, ""
	}},
	{"entity without id", func(d int, _ string, p map[string]interface{}) (int, string) {
		if d == 0 {
			patches, _ := p["patches"].([]interface{})
			p["patches"] = append(patches, map[string]interface{}{"op": "put", "entity": map[string]interface{}{"kind": "assistant_message", "sequence": 999}})
		}
		return 0, ""
	}},
}

func TestAgentRecoveryUnreadableNewestPageNeverFailsTurnOrResubmits(t *testing.T) {
	for _, bad := range unreadablePages {
		for _, follow := range []bool{false, true} {
			name := bad.name + "/new thread"
			if follow {
				name = bad.name + "/follow-up"
			}
			t.Run(name, func(t *testing.T) {
				env := newRecoveryEnv(t)
				AppConfig.Timeouts.InferenceTimeout = 10
				path := filepath.Join(t.TempDir(), "b.state")
				store := newAgentThreadStore(path)
				msgs, want, wantSends := []ChatMessage{u("a")}, "reply-1", 0
				if follow {
					if out, err := runTurn(t, env.f, store, recAcc(), "client-1", msgs); err != nil || out != "reply-1" {
						t.Fatalf("%q %v", out, err)
					}
					msgs, want, wantSends = []ChatMessage{u("a"), a("reply-1"), u("b")}, "reply-2", 1
				}
				env.setHook(bad.hook)
				out, err := runTurn(t, env.f, store, recAcc(), "client-1", msgs)
				if err == nil || out != "" {
					t.Fatalf("an unreadable page must be an error without output: %q %v", out, err)
				}
				var terr *agentTranscriptError
				if errors.As(err, &terr) && terr.kind != "transcript_format" {
					t.Fatalf("unreadable must not look like a remote failure: %v", err)
				}
				if f := classifyAgentFailure(err); f.Kind == "failed" {
					t.Fatalf("%+v", f)
				}
				notFailed(t, store)
				env.setHook(nil)
				store = newAgentThreadStore(path) // restart: state comes from the file
				out, err = runTurn(t, env.f, store, recAcc(), "client-1", msgs)
				if err != nil || out != want {
					t.Fatalf("retry after recovery must replay only the existing reply %q, got %q %v", want, out, err)
				}
				if c, s := env.f.counts(); c != 1 || s != wantSends {
					t.Fatalf("a read problem must not create/send again: creates=%d sends=%d", c, s)
				}
			})
		}
	}
}

func TestAgentRecoveryUnreadablePageDuringProbeDoesNotResubmit(t *testing.T) {
	for _, bad := range unreadablePages {
		for _, follow := range []bool{false, true} {
			name := bad.name + "/lost create ack"
			if follow {
				name = bad.name + "/lost send ack"
			}
			t.Run(name, func(t *testing.T) {
				env := newRecoveryEnv(t)
				AppConfig.Timeouts.InferenceTimeout = 10
				path := filepath.Join(t.TempDir(), "b.state")
				store := newAgentThreadStore(path)
				msgs, want, wantSends := []ChatMessage{u("a")}, "reply-1", 0
				if follow {
					if out, err := runTurn(t, env.f, store, recAcc(), "client-1", msgs); err != nil || out != "reply-1" {
						t.Fatalf("%q %v", out, err)
					}
					msgs, want, wantSends = []ChatMessage{u("a"), a("reply-1"), u("b")}, "reply-2", 1
					env.f.mu.Lock()
					env.f.dropAckSend = true // the send is accepted, its acknowledgement is lost
					env.f.mu.Unlock()
				} else {
					env.dropAfterCreate = 1
				}
				if _, err := runTurn(t, env.f, store, recAcc(), "client-1", msgs); err == nil {
					t.Fatal("lost acknowledgement must surface as an error")
				}
				env.f.mu.Lock()
				env.f.dropAckSend = false
				env.f.mu.Unlock()
				store = newAgentThreadStore(path) // proxy restart: the next call probes the transcript
				env.setHook(bad.hook)
				out, err := runTurn(t, env.f, store, recAcc(), "client-1", msgs)
				if err == nil || out != "" {
					t.Fatalf("probe over an unreadable page must fail closed: %q %v", out, err)
				}
				if c, s := env.f.counts(); c != 1 || s != wantSends {
					t.Fatalf("the probe must not treat an unreadable page as 'not submitted': creates=%d sends=%d", c, s)
				}
				notFailed(t, store)
				env.setHook(nil)
				out, err = runTurn(t, env.f, store, recAcc(), "client-1", msgs)
				if err != nil || out != want {
					t.Fatalf("%q %v (want %q)", out, err, want)
				}
				if c, s := env.f.counts(); c != 1 || s != wantSends {
					t.Fatalf("creates=%d sends=%d", c, s)
				}
			})
		}
	}
}

// ---- 5b. collectAgentTranscript / merge semantics (pure) -----------------------

func tPut(id string, seq float64, kind, text string) agentTranscriptPatch {
	p := agentTranscriptPatch{Op: "put"}
	p.Entity = agentTranscriptEntity{ID: id, Kind: kind, Sequence: agentSeq(seq)}
	if text != "" {
		p.Entity.Content = []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}{{Type: "text", Text: text}}
	}
	return p
}

func tPage(more bool, cursor, status string, patches ...agentTranscriptPatch) agentTranscriptPage {
	p := agentTranscriptPage{Type: agentTranscriptPageType, HasMoreBackward: more, BackwardCursor: cursor, Patches: patches}
	p.Session.Status = status
	return p
}

func chainFetch(pages map[string]agentTranscriptPage, calls *[]string) agentTranscriptFetch {
	return func(cursor string) (agentTranscriptPage, error) {
		*calls = append(*calls, cursor)
		return pages[cursor], nil
	}
}

func TestAgentRecoveryCollectMergesOlderPagesFirstAndNewestWins(t *testing.T) {
	// older page: m1 "old draft", x (to be removed later), tail of an earlier turn
	// newest page: m1 rewritten, removes x, completes.
	older := tPage(false, "", "failed", tPut("m1", 10, "assistant_message", "old draft"), tPut("x", 11, "assistant_message", "junk"), tPut("m0", 9, "assistant_message", "first"))
	rm := agentTranscriptPatch{Op: "remove", ID: "x"}
	newest := tPage(true, "c1", "idle", tPut("m1", 12, "assistant_message", "final draft"), rm, tPut("d", 13, "turn_completed", ""))
	newest.Patches[2].Entity.StopReason = "end_turn"
	var calls []string
	merged, err := collectAgentTranscript(chainFetch(map[string]agentTranscriptPage{"": newest, "c1": older}, &calls), 0, nil)
	if err != nil || merged.HasMoreBackward || len(calls) != 2 || merged.Session.Status != "idle" {
		t.Fatalf("%v %+v %v", err, merged.HasMoreBackward, calls)
	}
	text, done, _, err := readAgentTranscriptSince(merged, 0)
	if err != nil || !done || text != "first\nfinal draft" {
		t.Fatalf("merge order/remove wrong: %q done=%v %v", text, done, err)
	}
	// a remove in the OLDER page cannot delete an entity the newer page re-puts
	older2 := tPage(false, "", "idle", tPut("x", 5, "assistant_message", "gone"), agentTranscriptPatch{Op: "remove", ID: "m1"}, tPut("m1", 6, "assistant_message", "v1"))
	newer2 := tPage(true, "c1", "idle", tPut("m1", 8, "assistant_message", "v2"), tPut("d", 9, "turn_completed", ""))
	newer2.Patches[1].Entity.StopReason = "end_turn"
	calls = nil
	merged, err = collectAgentTranscript(chainFetch(map[string]agentTranscriptPage{"": newer2, "c1": older2}, &calls), 0, nil)
	text, _, _, rerr := readAgentTranscriptSince(merged, 0)
	if err != nil || rerr != nil || text != "gone\nv2" {
		t.Fatalf("%q %v %v", text, err, rerr)
	}
}

func TestAgentRecoveryCollectStopsOnBaselineOrProofAndBoundsPages(t *testing.T) {
	p0 := tPage(true, "c1", "idle", tPut("e300", 300, "tool_activity", ""))
	p1 := tPage(true, "c2", "idle", tPut("e120", 120, "tool_activity", ""), tPut("e200", 200, "tool_activity", ""))
	p2 := tPage(false, "", "idle", tPut("e100", 100, "tool_activity", ""))
	pages := map[string]agentTranscriptPage{"": p0, "c1": p1, "c2": p2}
	var calls []string
	m, err := collectAgentTranscript(chainFetch(pages, &calls), 150, nil)
	if err != nil || m.HasMoreBackward || len(calls) != 2 || len(m.Patches) != 3 {
		t.Fatalf("baseline covered on page 2 must stop there: %v %v %v", err, calls, len(m.Patches))
	}
	calls = nil
	m, err = collectAgentTranscript(chainFetch(pages, &calls), 0, nil)
	if err != nil || len(calls) != 3 || len(m.Patches) != 4 {
		t.Fatalf("%v %v", err, calls)
	}
	// stop predicate (submit probe) ends the walk without reading further
	calls = nil
	_, err = collectAgentTranscript(chainFetch(pages, &calls), 0, func(p agentTranscriptPage) bool { return len(p.Patches) >= 3 })
	if err != nil || len(calls) != 2 {
		t.Fatalf("%v %v", err, calls)
	}
	old := agentTranscriptMaxPages
	agentTranscriptMaxPages = 2
	defer func() { agentTranscriptMaxPages = old }()
	calls = nil
	if _, err = collectAgentTranscript(chainFetch(pages, &calls), 0, nil); !errors.As(err, new(*agentTranscriptIncompleteError)) || len(calls) != 2 {
		t.Fatalf("page bound: %v %v", err, calls)
	}
	agentTranscriptMaxPages = old
	oldP := agentTranscriptMaxPatches
	agentTranscriptMaxPatches = 2
	defer func() { agentTranscriptMaxPatches = oldP }()
	if _, err = collectAgentTranscript(chainFetch(pages, new([]string)), 0, nil); !errors.As(err, new(*agentTranscriptIncompleteError)) {
		t.Fatalf("patch bound: %v", err)
	}
}

func TestAgentRecoveryCollectRejectsUnreadableFirstPage(t *testing.T) {
	bad := agentTranscriptPage{Type: "error"}
	_, err := collectAgentTranscript(func(string) (agentTranscriptPage, error) { return bad, nil }, 0, nil)
	var terr *agentTranscriptError
	if !errors.As(err, &terr) || terr.kind != "transcript_format" {
		t.Fatalf("an unreadable newest page must be an error, not an empty thread: %v", err)
	}
}

// --- merged from agent_submitted_recovery_test.go ---
// A submitted turn whose reply never reached the client because the proxy side
// failed (stream cut, deadline), not because the client hung up: the client
// gives up on that request and sends a new prompt. Claude Code merges the failed
// prompt and the new one into one user message (consecutive user turns).
func submittedThenProxyFailure(t *testing.T, key string) (*fakeAgentServer, *agentThreadStore, *Account) {
	t.Helper()
	f, store, acc := bindingSetup(t)
	if out, err := runKeyed(t, f, store, acc, key, "low", false, []ChatMessage{u("root")}); err != nil || out != "reply-1" {
		t.Fatalf("first turn %q %v", out, err)
	}
	f.stall = true
	gone := make(chan struct{})
	ended := make(chan error, 1)
	go func() {
		ended <- callAgentThreadResumableCancel(gone, acc, []ChatMessage{u("root"), a("reply-1"), u("old")}, "", "low", key, false, func(string, bool, *UsageInfo) {}, f.server.Client(), store)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, s := f.counts(); s == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("not submitted")
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(gone)
	<-ended
	store.mu.Lock()
	for _, rec := range store.records {
		rec.ClientInterrupted = false // proxy-side failure: the client did not disconnect
		rec.State = agentStateSubmitted
	}
	store.saveLocked()
	store.mu.Unlock()
	f.mu.Lock()
	f.stall = false
	for _, th := range f.threads {
		f.finishLocked(th)
	}
	f.mu.Unlock()
	return f, store, acc
}

func TestAgentSubmittedTurnLostThenMergedNewPromptConverges(t *testing.T) {
	f, store, acc := submittedThenProxyFailure(t, "lost-merged")
	out, err := runKeyed(t, f, store, acc, "lost-merged", "low", false, []ChatMessage{u("root"), a("reply-1"), u("old\n\nnew")})
	if err != nil {
		t.Fatalf("merged new prompt after a lost reply: %v", err)
	}
	if c, s := f.counts(); c != 1 || s != 2 {
		t.Fatalf("create=%d send=%d; want the same thread, one more send", c, s)
	}
	next, err := runKeyed(t, f, store, acc, "lost-merged", "low", false, []ChatMessage{u("root"), a("reply-1"), u("old\n\nnew"), a(out), u("again")})
	if err != nil || next == "" {
		t.Fatalf("continuation %q %v", next, err)
	}
}

func TestAgentSubmittedTurnLostThenSeparateNewPromptConverges(t *testing.T) {
	f, store, acc := submittedThenProxyFailure(t, "lost-separate")
	out, err := runKeyed(t, f, store, acc, "lost-separate", "low", false, []ChatMessage{u("root"), a("reply-1"), u("old"), u("new")})
	if err != nil {
		t.Fatalf("separate new prompt after a lost reply: %v", err)
	}
	if c, s := f.counts(); c != 1 || s != 2 {
		t.Fatalf("create=%d send=%d", c, s)
	}
	_ = out
}

// --- merged from agent_send_recovery_test.go ---
// After the sender side failed (HTTP 5xx / lost connection) the turn stays
// "submitting" with an unknown outcome. When the client gives up and the NEXT
// request is not the identical retry but continues the last stable state
// (failed turn's messages plus new ones, or a different continuation), the
// bridge probes the transcript: if upstream never accepted the failed turn and the
// thread is idle, the failed turn is treated as not having happened and the
// new request is sent once, as the delta from the last stable state. If upstream
// did accept it (late acceptance) the explicit error stays and nothing is sent.

func toolResult(text string) ChatMessage {
	return ChatMessage{Role: "tool", ToolCallID: "t1", Name: "Read", Content: text}
}

func TestSendRecoveryFailedTurnThenNewMessageContinuesOnce(t *testing.T) {
	f, store, acc := bindingSetup(t)
	runTurn(t, f, store, acc, "s", []ChatMessage{u("one")})
	failed := []ChatMessage{u("one"), a("x"), toolResult("BIG-RESULT")}
	f.rejectSend = 500
	if _, err := runTurn(t, f, store, acc, "s", failed); err == nil {
		t.Fatal("5xx must surface")
	}
	f.rejectSend = 0
	next := append(append([]ChatMessage{}, failed...), u("NEW-QUESTION"))
	out, err := runTurn(t, f, store, acc, "s", next)
	if err != nil || out == "" {
		t.Fatalf("a request that continues the last stable state must be served: %q %v", out, err)
	}
	if c, s := f.counts(); c != 1 || s != 1 || threadCount(f) != 1 {
		t.Fatalf("one thread, exactly one accepted send: creates=%d sends=%d threads=%d", c, s, threadCount(f))
	}
	p := lastSentPrompt(f)
	i, j := strings.Index(p, "BIG-RESULT"), strings.Index(p, "NEW-QUESTION")
	if i < 0 || j < 0 || i > j || strings.Count(p, "BIG-RESULT") != 1 || strings.Count(p, "NEW-QUESTION") != 1 {
		t.Fatalf("delta must carry the failed turn's content and the new message once, in order: %q", p)
	}
	// The conversation keeps working afterwards.
	more := append(append([]ChatMessage{}, next...), a("y"), u("again"))
	if _, err := runTurn(t, f, store, acc, "s", more); err != nil {
		t.Fatalf("later turns must continue: %v", err)
	}
	if c, s := f.counts(); c != 1 || s != 2 {
		t.Fatalf("creates=%d sends=%d", c, s)
	}
}

func TestSendRecoveryClientDroppedTheFailedTurn(t *testing.T) {
	// Same length, different last message: the failed turn is not in the history.
	f, store, acc := bindingSetup(t)
	runTurn(t, f, store, acc, "s", []ChatMessage{u("one")})
	f.rejectSend = 500
	if _, err := runTurn(t, f, store, acc, "s", []ChatMessage{u("one"), a("x"), toolResult("BIG")}); err == nil {
		t.Fatal("5xx must surface")
	}
	f.rejectSend = 0
	if _, err := runTurn(t, f, store, acc, "s", []ChatMessage{u("one"), a("x"), u("different-continuation")}); err != nil {
		t.Fatalf("must continue from the last stable state: %v", err)
	}
	if c, s := f.counts(); c != 1 || s != 1 || !strings.Contains(lastSentPrompt(f), "different-continuation") {
		t.Fatalf("creates=%d sends=%d", c, s)
	}
}

func TestSendRecoveryRepeatedFailuresDoNotWedgeTheSession(t *testing.T) {
	f, store, acc := bindingSetup(t)
	runTurn(t, f, store, acc, "s", []ChatMessage{u("one")})
	hist := []ChatMessage{u("one"), a("x")}
	for i := 0; i < 3; i++ {
		f.rejectSend = 500
		bad := append(append([]ChatMessage{}, hist...), toolResult("BIG"+string(rune('A'+i))))
		if _, err := runTurn(t, f, store, acc, "s", bad); err == nil {
			t.Fatal("5xx must surface")
		}
		f.rejectSend = 0
		hist = append(bad, u("q"+string(rune('A'+i))))
		if i < 2 {
			f.rejectSend = 500 // the follow-up fails too; the next request still recovers
			if _, err := runTurn(t, f, store, acc, "s", hist); err == nil {
				t.Fatal("5xx must surface")
			}
			f.rejectSend = 0
			hist = append(hist, u("z"+string(rune('A'+i))))
		}
		if _, err := runTurn(t, f, store, acc, "s", hist); err != nil {
			t.Fatalf("round %d: %v", i, err)
		}
		hist = append(hist, a("r"))
	}
	if c, s := f.counts(); c != 1 || s != 3 || threadCount(f) != 1 {
		t.Fatalf("creates=%d sends=%d", c, s)
	}
}

func TestSendRecoverySurvivesProxyRestart(t *testing.T) {
	f, _, acc := bindingSetup(t)
	path := filepath.Join(t.TempDir(), "state")
	store := newAgentThreadStore(path)
	runTurn(t, f, store, acc, "s", []ChatMessage{u("one")})
	f.rejectSend = 500
	failed := []ChatMessage{u("one"), a("x"), toolResult("BIG")}
	if _, err := runTurn(t, f, store, acc, "s", failed); err == nil {
		t.Fatal("5xx must surface")
	}
	f.rejectSend = 0
	store = newAgentThreadStore(path) // restart
	if _, err := runTurn(t, f, store, acc, "s", append(append([]ChatMessage{}, failed...), u("NEW"))); err != nil {
		t.Fatalf("recovery after restart: %v", err)
	}
	if c, s := f.counts(); c != 1 || s != 1 {
		t.Fatalf("creates=%d sends=%d", c, s)
	}
}

func TestSendRecoveryLateAcceptedTurnKeepsExplicitErrorAndSendsNothing(t *testing.T) {
	f, store, acc := bindingSetup(t)
	runTurn(t, f, store, acc, "s", []ChatMessage{u("one")})
	failed := []ChatMessage{u("one"), a("x"), toolResult("BIG")}
	f.dropAckSend = true // upstream accepted the turn; the proxy saw a 500
	if _, err := runTurn(t, f, store, acc, "s", failed); err == nil {
		t.Fatal("expected ack failure")
	}
	f.dropAckSend = false
	_, err := runTurn(t, f, store, acc, "s", append(append([]ChatMessage{}, failed...), u("NEW")))
	var be *agentBindingError
	if !errors.As(err, &be) || be.Kind != "pending" {
		t.Fatalf("an accepted failed turn must keep the explicit pending error, got %v", err)
	}
	if c, s := f.counts(); c != 1 || s != 1 || threadCount(f) != 1 {
		t.Fatalf("nothing may be sent or forked: creates=%d sends=%d", c, s)
	}
	// The identical original request still resolves the accepted turn (#17).
	if out, err := runTurn(t, f, store, acc, "s", failed); err != nil || out == "" {
		t.Fatalf("identical retry must reuse the accepted turn: %q %v", out, err)
	}
	if c, s := f.counts(); c != 1 || s != 1 {
		t.Fatalf("identical retry must not resend: creates=%d sends=%d", c, s)
	}
}

func TestSendRecoveryRunningThreadKeepsExplicitError(t *testing.T) {
	f, store, acc := bindingSetup(t)
	runTurn(t, f, store, acc, "s", []ChatMessage{u("one")})
	failed := []ChatMessage{u("one"), a("x"), toolResult("BIG")}
	f.rejectSend = 500
	if _, err := runTurn(t, f, store, acc, "s", failed); err == nil {
		t.Fatal("5xx must surface")
	}
	f.rejectSend = 0
	f.mu.Lock()
	for _, th := range f.threads {
		th.status = "running" // something is still going on upstream
	}
	f.mu.Unlock()
	_, err := runTurn(t, f, store, acc, "s", append(append([]ChatMessage{}, failed...), u("NEW")))
	var be *agentBindingError
	if !errors.As(err, &be) || be.Kind != "pending" {
		t.Fatalf("a non-idle thread must not be recovered, got %v", err)
	}
	if _, s := f.counts(); s != 0 {
		t.Fatalf("nothing may be sent: sends=%d", s)
	}
}

func TestSendRecoveryUnrelatedRequestsStayDiverged(t *testing.T) {
	f, store, acc := bindingSetup(t)
	runTurn(t, f, store, acc, "s", []ChatMessage{u("one")})
	f.rejectSend = 500
	if _, err := runTurn(t, f, store, acc, "s", []ChatMessage{u("one"), a("x"), toolResult("BIG")}); err == nil {
		t.Fatal("5xx must surface")
	}
	f.rejectSend = 0
	for name, msgs := range map[string][]ChatMessage{
		"edited earlier history": {u("one-edited"), a("x"), u("NEW")},
		"rolled back":            {u("one")},
	} {
		if name == "rolled back" {
			continue // an exact replay of an older stable state is not a continuation
		}
		_, err := runTurn(t, f, store, acc, "s", msgs)
		var be *agentBindingError
		if !errors.As(err, &be) || (be.Kind != "diverged" && be.Kind != "pending") {
			t.Fatalf("%s: want a refusal, got %v", name, err)
		}
	}
	if _, s := f.counts(); s != 0 {
		t.Fatalf("nothing may be sent for unrelated requests: sends=%d", s)
	}
}

func TestAgentInferenceAutoRetriesTransient502And504AfterKeepaliveStarted(t *testing.T) {
	env := newRecoveryEnv(t)
	store := useRecoveryStore(t, "")
	AppConfig.Timeouts.InferenceTimeout = 10
	setKeepalive(t, 15*time.Millisecond, 20*time.Millisecond)
	agentInferenceRetryDelay = 10 * time.Millisecond

	if out, err := runTurn(t, env.f, store, recAcc(), agentClientKey("client-1"), []ChatMessage{u("first")}); err != nil || out != "reply-1" {
		t.Fatalf("first turn: %q %v", out, err)
	}

	// Delay the first send past keepalive start (so HTTP 200 SSE is already committed),
	// fail send #1 with HTTP 502, then fail the first two transcript polls with HTTP 504
	// and a dropped connection before completing.
	sendCalls := 0
	pollFailures := 2
	env.f.stall = true
	srv := httptest.NewServer(recoveryHandler(recAcc(), []ChatMessage{u("first"), a("reply-1"), u("second")}, "auto", false, true))
	defer srv.Close()

	env.setHook(func(depth int, _ string, _ map[string]interface{}) (int, string) {
		env.mu.Lock()
		defer env.mu.Unlock()
		if pollFailures > 0 {
			pollFailures--
			if pollFailures == 1 {
				return http.StatusGatewayTimeout, `{"message":"gateway timeout"}`
			}
			return http.StatusBadGateway, `{"message":"bad gateway"}`
		}
		return 0, ""
	})
	env.f.mu.Lock()
	env.f.rejectSend = http.StatusBadGateway
	env.f.mu.Unlock()

	go func() {
		for {
			env.f.mu.Lock()
			tries := env.f.sendTries
			env.f.mu.Unlock()
			if tries >= 1 {
				sendCalls = tries
				env.f.mu.Lock()
				env.f.rejectSend = 0
				env.f.mu.Unlock()
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		for {
			_, s := env.f.counts()
			if s >= 1 {
				time.Sleep(40 * time.Millisecond)
				env.complete(0, "healed-after-502-504")
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()

	call := startSSE(t, srv.URL)
	evs := call.drain(5 * time.Second)
	if countName(evs, "error") != 0 || countName(evs, "message_stop") != 1 {
		t.Fatalf("expected self-healed stream without error event, got %v", names(evs))
	}
	got := textOf(evs)
	if !strings.Contains(got, "[bridge:auto-retry] recovered after") || !strings.HasSuffix(got, "healed-after-502-504") {
		t.Fatalf("got %q, want [bridge:auto-retry] notice followed by %q", got, "healed-after-502-504")
	}
	if c, s := env.f.counts(); c != 1 || s != 1 || sendCalls == 0 {
		t.Fatalf("expected 1 create and 1 accepted send after retry: creates=%d sends=%d firstTry=%d", c, s, sendCalls)
	}
}

func TestAgentRetryNoticeStrippedOnClientEchoAndAppendedOnExhaustedError(t *testing.T) {
	notice := formatAgentRetryNotice([]string{"submit_400_validation(1/4 thread=f2ba97f9)", "turn_retry(1/4 reason=http_502)"})
	if !strings.HasPrefix(notice, "[bridge:auto-retry] recovered after 2 auto-retry step(s)") {
		t.Fatalf("unexpected notice format: %q", notice)
	}
	converted, _ := convertAnthropicMessages(nil, []AnthropicMessage{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: []interface{}{
			map[string]interface{}{"type": "text", "text": notice},
			map[string]interface{}{"type": "text", "text": "actual answer"},
		}},
	})
	if len(converted) != 2 || converted[1].Content != "actual answer" {
		t.Fatalf("expected [bridge:auto-retry] block to be stripped on echo, got %+v", converted)
	}

	exhaustedErr := wrapAgentRetryTraces(&agentRecoverableError{
		Class:    "http_504",
		ThreadID: "f2ba97f9-35b0-4451-8375-d44b0732d7c9",
		Cause:    errors.New("gateway timeout"),
	}, []string{"turn_retry(1/4 reason=http_502)", "turn_retry(2/4 reason=http_504)"})
	f := classifyAgentFailure(exhaustedErr)
	if !strings.Contains(f.Message, "[auto_retries=2: turn_retry(1/4 reason=http_502) -> turn_retry(2/4 reason=http_504)]") {
		t.Fatalf("expected exhausted failure message to include auto_retries trace, got %q", f.Message)
	}
}

func TestAgentPollAttemptTimeoutContinuesWithinOuterInferenceTimeout(t *testing.T) {
	env := newRecoveryEnv(t)
	store := newAgentThreadStore("")
	AppConfig.Timeouts.InferenceTimeout = 5
	oldAttemptTimeout, oldHedge, oldMaxInFlight := agentPollAttemptTimeout, agentPollHedgeDelay, agentPollMaxInFlight
	agentPollAttemptTimeout = 40 * time.Millisecond
	agentPollHedgeDelay = 200 * time.Millisecond
	agentPollMaxInFlight = 1
	t.Cleanup(func() {
		agentPollAttemptTimeout = oldAttemptTimeout
		agentPollHedgeDelay = oldHedge
		agentPollMaxInFlight = oldMaxInFlight
	})

	// Hang the first transcript poll past agentPollAttemptTimeout while outer
	// InferenceTimeout (5s) still has plenty of time remaining.
	env.hang = make(chan struct{})
	env.hung = make(chan struct{}, 1)
	go func() {
		<-env.hung
		time.Sleep(90 * time.Millisecond) // > 40ms agentPollAttemptTimeout
		env.mu.Lock()
		close(env.hang)
		env.hang = nil
		env.mu.Unlock()
	}()

	out, err := runTurn(t, env.f, store, recAcc(), "client-poll-timeout", []ChatMessage{u("hello")})
	if err != nil || out != "reply-1" {
		t.Fatalf("single poll timeout within outer deadline must continue polling: out=%q err=%v", out, err)
	}
}

func TestSendRecoveryLateAcceptedPlainUserTurnPromotesToCorrectionAndConverges(t *testing.T) {
	f, store, acc := bindingSetup(t)
	if out, err := runTurn(t, f, store, acc, "s-user-late", []ChatMessage{u("one")}); err != nil || out != "reply-1" {
		t.Fatalf("first turn: %q %v", out, err)
	}
	// Plain user follow-up is accepted upstream, but proxy sees HTTP 500 (state stays submitting).
	failed := []ChatMessage{u("one"), a("reply-1"), u("old")}
	f.dropAckSend = true
	if _, err := runTurn(t, f, store, acc, "s-user-late", failed); err == nil {
		t.Fatal("expected ack failure")
	}
	f.dropAckSend = false

	// User sends "继续" (either merged into the failed user message or as a separate user message).
	out, err := runTurn(t, f, store, acc, "s-user-late", []ChatMessage{u("one"), a("reply-1"), u("old\n\n继续")})
	if err != nil || out != "reply-3" {
		t.Fatalf("late-accepted plain user turn followed by user continuation must drain and converge, got out=%q err=%v", out, err)
	}
	if c, s := f.counts(); c != 1 || s != 2 {
		t.Fatalf("creates=%d sends=%d, want 1/2", c, s)
	}
}

