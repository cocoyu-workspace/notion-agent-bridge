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

// Claude Code decides on auto-compaction from usage.input_tokens. The Agent
// transport has no upstream token counts, so the proxy reports an ESTIMATE of the
// whole client request (system + tools + messages incl. tool results) instead of 0.

func TestEstimateTextTokensRule(t *testing.T) {
	if got := estimateTextTokens(""); got != 0 {
		t.Fatalf("empty = %d", got)
	}
	if got := estimateTextTokens(strings.Repeat("a", 400)); got != 100 {
		t.Fatalf("400 ASCII chars = %d, want 100 (4 chars/token)", got)
	}
	if got := estimateTextTokens(strings.Repeat("a", 401)); got != 101 {
		t.Fatalf("rounds up: %d", got)
	}
	if got := estimateTextTokens(strings.Repeat("你", 100)); got != 150 {
		t.Fatalf("100 CJK chars = %d, want 150 (1.5 tokens/char, conservative)", got)
	}
	if got := estimateTextTokens(strings.Repeat("😀", 10)); got != 20 {
		t.Fatalf("10 emoji = %d, want 20 (2 tokens each)", got)
	}
	if estimateTextTokens("abc") < 1 {
		t.Fatalf("non-empty text must count at least 1")
	}
}

func decodeReq(t *testing.T, raw string) *AnthropicRequest {
	t.Helper()
	var req AnthropicRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatal(err)
	}
	return &req
}

func TestEstimateRequestCountsSystemToolsMessagesAndToolResults(t *testing.T) {
	big := strings.Repeat("x", 4000) // 1000 tokens
	base := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	baseTok := estimateAnthropicRequestTokens(decodeReq(t, base))
	if baseTok <= 0 {
		t.Fatalf("a request is never 0 tokens: %d", baseTok)
	}
	cases := map[string]string{
		"system string":      `{"model":"m","system":"` + big + `","messages":[{"role":"user","content":"hi"}]}`,
		"system blocks":      `{"model":"m","system":[{"type":"text","text":"` + big + `"}],"messages":[{"role":"user","content":"hi"}]}`,
		"tools":              `{"model":"m","tools":[{"name":"Bash","description":"` + big + `","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"hi"}]}`,
		"tool result":        `{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"` + big + `"}]}]}`,
		"tool result blocks": `{"model":"m","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"` + big + `"}]}]}]}`,
		"thinking":           `{"model":"m","messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"` + big + `","signature":"s"}]}]}`,
	}
	for name, raw := range cases {
		got := estimateAnthropicRequestTokens(decodeReq(t, raw))
		if got < baseTok+1000 || got > baseTok+1400 {
			t.Errorf("%s: %d tokens, want about base(%d)+1000", name, got, baseTok)
		}
	}
}

func TestEstimateRequestDoesNotCountBase64ImagePayloadByLength(t *testing.T) {
	blob := strings.Repeat("A", 2_000_000)
	raw := `{"model":"m","messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + blob + `"}},{"type":"text","text":"what is this"}]}]}`
	got := estimateAnthropicRequestTokens(decodeReq(t, raw))
	if got < 500 || got > 5000 {
		t.Fatalf("an image must count a bounded constant, not its base64 length: %d", got)
	}
}

func TestEstimateChatMessagesTokens(t *testing.T) {
	msgs := []ChatMessage{{Role: "system", Content: strings.Repeat("s", 400)}, {Role: "user", Content: strings.Repeat("u", 800)},
		{Role: "assistant", ToolCalls: []ToolCall{{Function: ToolCallFunction{Name: "Bash", Arguments: strings.Repeat("a", 400)}}}}}
	got := estimateChatMessagesTokens(msgs)
	if got < 400 || got > 450 {
		t.Fatalf("got %d, want about 100+200+100 plus small framing", got)
	}
}

func TestResolveUsagePrefersRealUpstreamNumbers(t *testing.T) {
	in, out := resolveAnthropicUsage(&UsageInfo{PromptTokens: 7, CompletionTokens: 3}, &Session{InputTokenEstimate: 999}, nil, "hello", "r")
	if in != 7 || out != 3 {
		t.Fatalf("real usage must win: %d/%d", in, out)
	}
	in, out = resolveAnthropicUsage(&UsageInfo{}, &Session{InputTokenEstimate: 999}, nil, strings.Repeat("a", 40), "r")
	if in != 999 || out != 10 {
		t.Fatalf("all-zero upstream usage is not a reading: %d/%d", in, out)
	}
}

func TestResolveUsageFallsBackToMessagesWithoutSessionEstimate(t *testing.T) {
	in, out := resolveAnthropicUsage(nil, nil, []ChatMessage{u(strings.Repeat("u", 400))}, "ok", "r")
	if in < 100 || out < 1 {
		t.Fatalf("fallback: %d/%d", in, out)
	}
}

type usageRun struct {
	startIn, startOut, deltaIn, deltaOut float64
	haveStart, haveDelta                 bool
	nsIn, nsOut                          float64
}

func runUsage(t *testing.T, reply string, hasTools, stream bool, sessionEstimate int, msgs []ChatMessage) usageRun {
	t.Helper()
	env := newRecoveryEnv(t)
	useRecoveryStore(t, "")
	env.f.replyFn = func(int) string { return reply }
	if msgs == nil {
		msgs = []ChatMessage{u("hello")}
	}
	srv := httptest.NewServer(httptestHandler(func(sess *Session) { sess.InputTokenEstimate = sessionEstimate },
		hasTools, stream, msgs))
	defer srv.Close()
	var res usageRun
	if stream {
		call := startSSE(t, srv.URL)
		for _, e := range call.drain(10 * time.Second) {
			switch e.Name {
			case "message_start":
				m, _ := e.Data["message"].(map[string]interface{})
				u, _ := m["usage"].(map[string]interface{})
				res.startIn, _ = u["input_tokens"].(float64)
				res.startOut, _ = u["output_tokens"].(float64)
				res.haveStart = true
			case "message_delta":
				u, _ := e.Data["usage"].(map[string]interface{})
				res.deltaIn, _ = u["input_tokens"].(float64)
				res.deltaOut, _ = u["output_tokens"].(float64)
				res.haveDelta = true
			}
		}
		return res
	}
	body := postJSON(t, srv.URL)
	var resp struct {
		Usage struct {
			In  float64 `json:"input_tokens"`
			Out float64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}
	res.nsIn, res.nsOut = resp.Usage.In, resp.Usage.Out
	return res
}

func TestUsageIsEstimatedNotZeroOnEveryAgentPath(t *testing.T) {
	reply := strings.Repeat("answer ", 40) // 280 chars -> 70 tokens
	for _, hasTools := range []bool{true, false} {
		name := map[bool]string{true: "tools", false: "text"}[hasTools]
		t.Run(name+"/stream", func(t *testing.T) {
			r := runUsage(t, reply, hasTools, true, 12345, nil)
			if !r.haveStart || !r.haveDelta {
				t.Fatalf("missing events: %+v", r)
			}
			if r.startIn != 12345 {
				t.Errorf("message_start input_tokens = %v, want the request estimate 12345", r.startIn)
			}
			if r.deltaIn != 12345 || r.deltaOut < 60 || r.deltaOut > 80 {
				t.Errorf("message_delta usage = in %v out %v, want 12345 / ~70", r.deltaIn, r.deltaOut)
			}
		})
		t.Run(name+"/non-stream", func(t *testing.T) {
			r := runUsage(t, reply, hasTools, false, 12345, nil)
			if r.nsIn != 12345 || r.nsOut < 60 || r.nsOut > 80 {
				t.Errorf("usage = in %v out %v, want 12345 / ~70", r.nsIn, r.nsOut)
			}
		})
	}
}

func TestUsageFallsBackToMessagesWhenSessionHasNoEstimate(t *testing.T) {
	msgs := []ChatMessage{u(strings.Repeat("u", 4000))}
	r := runUsage(t, "ok", true, false, 0, msgs)
	if r.nsIn < 1000 {
		t.Fatalf("input_tokens = %v, want >= 1000 from the message history", r.nsIn)
	}
}

func TestKeepaliveMessageStartCarriesTheEstimate(t *testing.T) {
	newRecoveryEnv(t)
	setKeepalive(t, 5*time.Millisecond, time.Hour)
	rec := httptest.NewRecorder()
	ka := newAnthropicKeepalive(rec, "msg_x", "auto")
	ka.SetInputTokens(4321)
	deadline := time.Now().Add(5 * time.Second)
	for !ka.Started() && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	ka.Stop()
	if !ka.Started() {
		t.Fatal("keepalive never started")
	}
	if !strings.Contains(rec.Body.String(), `"input_tokens":4321`) {
		t.Fatalf("message_start did not carry the estimate: %s", rec.Body.String())
	}
}

func httptestHandler(prep func(*Session), hasTools, stream bool, msgs []ChatMessage) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sess := &Session{ClientKey: "client-1"}
		setSessionTools(sess, declaredTools{"Bash": {Required: []string{"command"}}})
		prep(sess)
		if stream {
			handleAnthropicStream(w, recAcc(), msgs, "auto", "msg_test", hasTools, false, false, nil, false, nil, nil, sess, r.Context().Done())
		} else {
			handleAnthropicNonStream(w, recAcc(), msgs, "auto", "msg_test", hasTools, false, false, nil, false, nil, nil, sess, r.Context().Done())
		}
	})
}

func postJSON(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return string(raw)
}
