package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

// --- merged from agent_size_test.go ---
func toolMsg(id, content string) ChatMessage {
	return ChatMessage{Role: "tool", ToolCallID: id, Name: "Read", Content: content}
}

func renderTools(msgs ...ChatMessage) string {
	var b strings.Builder
	b.WriteString("HEAD\n")
	writeAgentMessages(&b, msgs)
	b.WriteString("TAIL")
	return b.String()
}

func TestFitAgentPromptBoundaryAndMinimalCut(t *testing.T) {
	big := toolMsg("c1", bigResult(50000))
	p := renderTools(u("hi"), big)
	if out, rep, err := fitAgentPrompt(p, []ChatMessage{big}, len(p)); err != nil || out != p || rep.Shortened != 0 {
		t.Fatal("a prompt exactly at the budget must be untouched")
	}
	out, rep, err := fitAgentPrompt(p, []ChatMessage{big}, len(p)-1)
	if err != nil || rep.Shortened != 1 || len(out) > len(p)-1 {
		t.Fatalf("%v %+v len=%d", err, rep, len(out))
	}
	// Only as much as needed is removed (the marker costs a few bytes more than the 1 byte).
	if len(p)-len(out) > 400 {
		t.Fatalf("cut %d bytes for a 1-byte overshoot", len(p)-len(out))
	}
	if !strings.HasPrefix(out, "HEAD\nUSER\nhi\n\n") || !strings.HasSuffix(out, "TAIL") {
		t.Fatal("the surrounding prompt must be unchanged")
	}
}

func TestFitAgentPromptMarkerReportsExactOmittedBytes(t *testing.T) {
	body := bigResult(100000)
	m := toolMsg("c1", body)
	out, _, err := fitAgentPrompt(renderTools(m), []ChatMessage{m}, 60000)
	if err != nil || len(out) > 60000 {
		t.Fatalf("%v %d", err, len(out))
	}
	i := strings.Index(out, "[notion-bridge: tool output truncated, ")
	var omitted, total int
	if _, err := fmtSscanMarker(out[i:], &omitted, &total); err != nil || total != len(body) {
		t.Fatalf("marker: %v total=%d", err, total)
	}
	// head + omitted + tail == original
	head := out[strings.Index(out, "\n")+1:]
	head = head[strings.Index(head, "\n")+1:] // skip "HEAD\n" and the tool header line
	h := strings.Index(head, "\n[notion-bridge")
	tailStart := strings.Index(out, "]\n") + 2
	tail := out[tailStart : len(out)-len("\n\nTAIL")]
	if h+len(tail)+omitted != len(body) || !strings.HasPrefix(body, head[:h]) || !strings.HasSuffix(body, tail) {
		t.Fatalf("head %d + omitted %d + tail %d != %d", h, omitted, len(tail), len(body))
	}
}

func fmtSscanMarker(s string, omitted, total *int) (int, error) {
	return fmt.Sscanf(s, "[notion-bridge: tool output truncated, %d of %d bytes omitted", omitted, total)
}

func TestTruncateMiddleNeverBreaksUTF8(t *testing.T) {
	for _, unit := range []string{"a", "é", "汉", "🙂", "a🙂é汉"} {
		s := strings.Repeat(unit, 3000)
		for omit := 1; omit < len(s); omit += 997 {
			out, omitted := truncateMiddle(s, omit)
			if !utf8.ValidString(out) || omitted < 0 || omitted > len(s) {
				t.Fatalf("%q omit=%d valid=%v omitted=%d", unit, omit, utf8.ValidString(out), omitted)
			}
			if abs(omitted-omit) > 8 {
				t.Fatalf("%q: asked %d, omitted %d", unit, omit, omitted)
			}
		}
	}
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

func TestFitAgentPromptKeepsMinimumAndRefusesWhenTextCannotShrink(t *testing.T) {
	m := toolMsg("c1", bigResult(20000))
	paste := u(strings.Repeat("p", 50000))
	p := renderTools(paste, m)
	_, rep, err := fitAgentPrompt(p, []ChatMessage{paste, m}, 40000)
	be, ok := err.(*agentBindingError)
	if !ok || be.Kind != "too_large" || rep.Shortened != 1 || rep.After < 50000 {
		t.Fatalf("%v %+v", err, rep)
	}
	if !strings.Contains(be.Msg, "budget is 40000") || !strings.Contains(be.Msg, "50") {
		t.Fatalf("the error must name sizes and budget: %s", be.Msg)
	}
	// A tool result is never cut below the minimum it keeps.
	out, _, err := fitAgentPrompt(renderTools(m), []ChatMessage{m}, 100)
	if err == nil || out != "" {
		t.Fatal("want too_large and no text")
	}
}

func TestFitAgentPromptIgnoresToolResultsNotInThePrompt(t *testing.T) {
	inPrompt := toolMsg("c2", bigResult(30000))
	old := toolMsg("c1", bigResult(90000)) // history: not part of this message
	p := renderTools(inPrompt)
	out, rep, err := fitAgentPrompt(p, []ChatMessage{old, inPrompt}, 20000)
	if err != nil || rep.Shortened != 1 || len(out) > 20000 {
		t.Fatalf("%v %+v", err, rep)
	}
}

func TestFitAgentPromptIsDeterministic(t *testing.T) {
	a, b := toolMsg("c1", bigResult(70000)), toolMsg("c2", bigResult(90000))
	p := renderTools(a, b)
	o1, _, _ := fitAgentPrompt(p, []ChatMessage{a, b}, 100000)
	o2, _, _ := fitAgentPrompt(p, []ChatMessage{a, b}, 100000)
	if o1 != o2 || len(o1) > 100000 || strings.Contains(o1, "bytes omitted") == false {
		t.Fatal("not deterministic")
	}
	// The larger result goes first; the smaller one stays whole when that is enough.
	if !strings.Contains(o1, a.Content) {
		t.Fatal("the smaller result should be intact")
	}
}

func TestAgentSizeBudgetsAreConfigurable(t *testing.T) {
	f, store, acc := sizeSetup(t)
	AppConfig.Proxy.AgentFollowUpMaxBytes = 50000
	if agentFollowUpMaxBytes() != 50000 || agentCreateMaxBytes() != defaultAgentCreateMaxBytes {
		t.Fatal("accessors")
	}
	if _, err := runKeyed(t, f, store, acc, "s", "low", true, toolTurn("x")[:2]); err != nil {
		t.Fatal(err)
	}
	if _, err := runKeyed(t, f, store, acc, "s", "low", true, toolTurn(bigResult(100000))); err != nil {
		t.Fatal(err)
	}
	if p := lastSentPrompt(f); len(p) > 50000 || !strings.Contains(p, "tool output truncated") {
		t.Fatalf("prompt %dB with a 50000B budget", len(p))
	}
	AppConfig.Proxy.AgentFollowUpMaxBytes, AppConfig.Proxy.AgentCreateMaxBytes = 0, 0
	if agentFollowUpMaxBytes() != defaultAgentFollowUpMaxBytes {
		t.Fatal("zero means default")
	}
}

func TestTooLargeResponseIs413WithoutRetry(t *testing.T) {
	w := httptest.NewRecorder()
	writeAgentFailureJSON(w, "req-1", classifyAgentFailure(&agentBindingError{Kind: "too_large", Msg: "m"}))
	if w.Code != 413 || w.Header().Get("x-should-retry") != "false" || !strings.Contains(w.Body.String(), "request_too_large") || !strings.Contains(w.Body.String(), "[bridge:too_large]") {
		t.Fatalf("%d %v %s", w.Code, w.Header(), w.Body.String())
	}
}

func TestFitAgentPromptOffloadRawSystemAndUserReminders(t *testing.T) {
	sysProto := ChatMessage{Role: "system", Content: "LOCAL ACTION PROTOCOL\n" + strings.Repeat("P", 36000)}
	sysClient := ChatMessage{
		Role: "system",
		Content: "x-anthropic-billing-header: cc_version=2.1.296.0; cch=12345;\n" +
			strings.Repeat("S", 48000) +
			"\n<total_tokens>195000 tokens left</total_tokens>",
	}
	userRem := "<system-reminder>\n# claudeMd\n" + strings.Repeat("M", 65000) + "\n</system-reminder>\n" +
		"<system-reminder>\n# skills\n" + strings.Repeat("K", 60000) + "\n</system-reminder>\n" +
		"USER_PLAIN_MESSAGE_KEEP_ME"
	userMsg := ChatMessage{Role: "user", Content: userRem}
	msgs := []ChatMessage{sysProto, sysClient, userMsg}
	prompt := agentThreadRebindPrompt(msgs)
	if len(prompt) <= defaultAgentFollowUpMaxBytes {
		t.Fatalf("expected prompt > %d, got %d", defaultAgentFollowUpMaxBytes, len(prompt))
	}
	out, rep, files, err := fitAgentPromptOffload(prompt, msgs, defaultAgentFollowUpMaxBytes, 8)
	if err != nil {
		t.Fatalf("expected fitAgentPromptOffload to succeed, got err: %v", err)
	}
	if len(out) > defaultAgentFollowUpMaxBytes {
		t.Fatalf("expected fitted prompt <= %d, got %d", defaultAgentFollowUpMaxBytes, len(out))
	}
	if rep.Offloaded < 2 || len(files) < 2 {
		t.Fatalf("expected at least 2 offloaded files, got rep=%+v files=%d", rep, len(files))
	}
	if !strings.Contains(out, "USER_PLAIN_MESSAGE_KEEP_ME") {
		t.Fatalf("expected user plain message to remain intact")
	}

	// Verify full helper-first rebind flow through runKeyed (120 KB follow-up budget).
	f, store, acc := sizeSetup(t)
	if _, err := runKeyed(t, f, store, acc, "s-rebind", "low", false, []ChatMessage{u("Generate a concise title")}); err != nil {
		t.Fatalf("helper turn failed: %v", err)
	}
	if _, err := runKeyed(t, f, store, acc, "s-rebind", "low", true, msgs); err != nil {
		t.Fatalf("rebind main turn with 158KB+ context failed: %v", err)
	}
	if p := lastSentPrompt(f); len(p) > defaultAgentFollowUpMaxBytes || !strings.Contains(p, "USER_PLAIN_MESSAGE_KEEP_ME") {
		t.Fatalf("unexpected sent prompt len=%d", len(p))
	}
}

func TestFitAgentPromptOffloadShortensHistoricalAssistantToolCalls(t *testing.T) {
	writeArgs := fmt.Sprintf(`{"file_path":"big.go","content":"%s"}`, strings.Repeat("W", 180000))
	histAssistant := ChatMessage{
		Role:    "assistant",
		Content: "Writing big.go now.",
		ToolCalls: []ToolCall{{
			ID:       "call_write_1",
			Type:     "function",
			Function: ToolCallFunction{Name: "Write", Arguments: writeArgs},
		}},
	}
	msgs := []ChatMessage{
		u("please create big.go"),
		histAssistant,
		{Role: "tool", ToolCallID: "call_write_1", Name: "Write", Content: "File created successfully"},
		u("KEEP_FINAL_USER_INSTRUCTION"),
	}
	prompt := agentThreadPrompt(msgs)
	if len(prompt) <= defaultAgentCreateMaxBytes {
		t.Fatalf("expected prompt > %d, got %d", defaultAgentCreateMaxBytes, len(prompt))
	}
	out, rep, files, err := fitAgentPromptOffload(prompt, msgs, defaultAgentCreateMaxBytes, 8)
	if err != nil {
		t.Fatalf("expected Stage 2 historical tool call offload to succeed, got err: %v", err)
	}
	if len(out) > defaultAgentCreateMaxBytes {
		t.Fatalf("expected fitted prompt <= %d, got %d", defaultAgentCreateMaxBytes, len(out))
	}
	if rep.Offloaded < 1 || len(files) < 1 || !strings.Contains(files[0].FileName, "tool-req-") {
		t.Fatalf("expected offloaded tool-req attachment, got rep=%+v files=%+v", rep, files)
	}
	if !strings.Contains(out, "KEEP_FINAL_USER_INSTRUCTION") {
		t.Fatalf("expected final user instruction to stay intact")
	}
}

func TestAgentThreadPreservesClientWebSearchToolCall(t *testing.T) {
	prev := AppConfig
	AppConfig = &Config{Proxy: ProxyConfig{AgentThread: true}}
	defer func() { AppConfig = prev }()

	calls := []ToolCall{{
		ID:       "call_ws_1",
		Type:     "function",
		Function: ToolCallFunction{Name: "WebSearch", Arguments: `{"query":"golang context"}`},
	}}
	prep := finishPreparedCalls(calls, "")
	if !prep.HasCalls || len(prep.ToolCalls) != 1 || prep.WebSearchQuery != "" {
		t.Fatalf("expected AgentThread mode to preserve WebSearch tool_use instead of legacy interception, got %+v", prep)
	}
}

// --- merged from agent_size_context_test.go ---
// Issue #58: after a Notion server-side compaction the full project context
// (~25 KB) is re-sent inline; with a large pasted user message the follow-up
// used to be refused as too_large although the user text alone fits.
func contextResendCase() ([]ChatMessage, string, string) {
	sys := "CONTEXT-HEAD " + strings.Repeat("rule line of the project context\n", 800) + " CONTEXT-TAIL"
	user := "USER-HEAD " + strings.Repeat("u", 100000) + " USER-TAIL"
	msgs := []ChatMessage{{Role: "system", Content: sys}, {Role: "user", Content: "hi"}, {Role: "assistant", Content: "ok"}, {Role: "user", Content: user}}
	prompt := agentThreadFollowUpPromptWithContext(systemMessages(msgs), msgs[3:])
	return msgs, prompt, user
}

func TestAgentSizeContextOffloadedAfterCompaction(t *testing.T) {
	msgs, prompt, user := contextResendCase()
	if len(prompt) <= 120000 {
		t.Fatalf("fixture too small: %d", len(prompt))
	}
	out, rep, files, err := fitAgentPromptOffload(prompt, msgs, 120000, 8)
	if err != nil {
		t.Fatalf("refused: %v", err)
	}
	if len(out) > 120000 || !strings.Contains(out, user) {
		t.Fatalf("len=%d, user text kept=%v", len(out), strings.Contains(out, user))
	}
	if len(files) != 1 || string(files[0].Data) != msgs[0].Content || !strings.Contains(files[0].FileName, "context-1") {
		t.Fatalf("files=%d", len(files))
	}
	if !strings.Contains(out, "CONTEXT-HEAD") || !strings.Contains(out, "CONTEXT-TAIL") || !strings.Contains(out, "FULL context is attached") || !strings.Contains(out, files[0].FileName) {
		t.Fatalf("context marker missing")
	}
	if rep.Offloaded != 1 {
		t.Fatalf("report %+v", rep)
	}
	again, _, files2, _ := fitAgentPromptOffload(prompt, msgs, 120000, 8)
	if again != out || len(files2) != 1 {
		t.Fatal("not deterministic")
	}
}

func TestAgentSizeContextTruncatedWithoutAttachments(t *testing.T) {
	msgs, prompt, user := contextResendCase()
	out, _, files, err := fitAgentPromptOffload(prompt, msgs, 120000, 0)
	if err != nil || len(files) != 0 || len(out) > 120000 || !strings.Contains(out, user) || !strings.Contains(out, "earlier full version in this thread still applies") {
		t.Fatalf("err=%v files=%d len=%d", err, len(files), len(out))
	}
}

func TestAgentSizeContextNotTouchedWhenToolResultsSuffice(t *testing.T) {
	msgs, _, _ := contextResendCase()
	msgs[3] = ChatMessage{Role: "user", Content: "short"}
	msgs = append(msgs, ChatMessage{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Type: "function", Function: ToolCallFunction{Name: "Read", Arguments: "{}"}}}}, ChatMessage{Role: "tool", ToolCallID: "c1", Name: "Read", Content: strings.Repeat("r", 150000)})
	prompt := agentThreadFollowUpPromptWithContext(systemMessages(msgs), msgs[3:])
	out, _, files, err := fitAgentPromptOffload(prompt, msgs, 120000, 8)
	if err != nil || !strings.Contains(out, "PROJECT CONTEXT AND LOCAL CLIENT INSTRUCTIONS\n"+msgs[0].Content) || len(files) != 1 || !strings.Contains(files[0].FileName, "tool-c1") {
		t.Fatalf("err=%v files=%d", err, len(files))
	}
}

func TestAgentSizeUserTextAloneTooLargeStillRefused(t *testing.T) {
	msgs, _, _ := contextResendCase()
	msgs[3].Content = strings.Repeat("x", 130000)
	prompt := agentThreadFollowUpPromptWithContext(systemMessages(msgs), msgs[3:])
	if _, _, _, err := fitAgentPromptOffload(prompt, msgs, 120000, 8); err == nil {
		t.Fatal("expected too_large")
	}
}

// --- merged from agent_offload_test.go ---
// offloadSetup is sizeSetup plus fake Agent file endpoints and signed storage.
func offloadSetup(t *testing.T) (*fakeAgentServer, *agentThreadStore, *Account, *offloadFake) {
	f, store, acc := sizeSetup(t)
	o := &offloadFake{targets: map[string]map[string]interface{}{}, bodies: map[string]string{}}
	storage := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		o.mu.Lock()
		o.bodies[r.URL.Path] = string(data)
		o.mu.Unlock()
	}))
	t.Cleanup(storage.Close)
	old := http.DefaultTransport
	http.DefaultTransport = storage.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = old })
	inner := f.server.Config.Handler
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "AgentServiceFileUpload") {
			inner.ServeHTTP(w, r)
			return
		}
		o.mu.Lock()
		defer o.mu.Unlock()
		if o.fail {
			w.WriteHeader(503)
			return
		}
		var b map[string]interface{}
		json.NewDecoder(r.Body).Decode(&b)
		if strings.HasSuffix(r.URL.Path, "createAgentServiceFileUploadURL") {
			o.n++
			id := fmt.Sprintf("file-%d", o.n)
			o.targets[id] = b["target"].(map[string]interface{})
			o.names = append(o.names, fmt.Sprint(b["filename"]), fmt.Sprint(b["mediaType"]))
			json.NewEncoder(w).Encode(map[string]interface{}{"type": "file_upload_url", "file": map[string]string{"id": id}, "upload": map[string]interface{}{"type": "single_part", "method": "PUT", "url": storage.URL + "/" + id, "headers": map[string]string{}}})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"type": "file", "id": fmt.Sprint(b["fileId"])})
	})
	return f, store, acc, o
}

type offloadFake struct {
	mu      sync.Mutex
	fail    bool
	n       int
	targets map[string]map[string]interface{}
	bodies  map[string]string
	names   []string
}

func lastSentFiles(f *fakeAgentServer) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ids []string
	for _, v := range f.sends[len(f.sends)-1]["event"].(map[string]interface{})["content"].([]interface{}) {
		if c := v.(map[string]interface{}); c["type"] == "file" {
			ids = append(ids, fmt.Sprint(c["file_id"]))
		}
	}
	return ids
}

// An oversized tool result is no longer lost: head and tail stay inline, the
// FULL output is attached to the same follow-up as a text file on the bound
// thread, and the marker names that file.
func TestSizeGuardOffloadsFullOutputAsAttachment(t *testing.T) {
	f, store, acc, o := offloadSetup(t)
	h1 := []ChatMessage{sysMsg("tools-v1"), u("read big.txt")}
	if _, err := runKeyed(t, f, store, acc, "s", "low", true, h1); err != nil {
		t.Fatal(err)
	}
	full := bigResult(300000)
	if _, err := runKeyed(t, f, store, acc, "s", "low", true, toolTurn(full)); err != nil {
		t.Fatal(err)
	}
	p := lastSentPrompt(f)
	files := lastSentFiles(f)
	if len(p) > 120000 || !strings.Contains(p, headMark) || !strings.Contains(p, tailMark) {
		t.Fatalf("inline part must fit and keep head and tail (%dB)", len(p))
	}
	if !strings.Contains(p, "attached to this message as the file bridge-output-1-tool-call_1.txt") || strings.Contains(p, "re-run with a narrower range") {
		t.Fatalf("marker must name the attachment: %q", p[len(p)/2-200:len(p)/2+200])
	}
	if len(files) != 1 {
		t.Fatalf("want one attached file, got %v", files)
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.bodies["/"+files[0]] != full {
		t.Fatalf("attachment must hold the full output (%d of %d bytes)", len(o.bodies["/"+files[0]]), len(full))
	}
	tg := o.targets[files[0]]
	if tg["type"] != "thread" || tg["threadId"] == "" || o.names[0] != "bridge-output-1-tool-call_1.txt" || o.names[1] != "text/plain" {
		t.Fatalf("follow-up attachment must target the bound thread as text/plain: %v %v", tg, o.names)
	}
}

// When the upload fails nothing is lost compared to before: the same budget is
// met by plain truncation and the turn is still sent, without attachments.
func TestSizeGuardOffloadUploadFailureFallsBackToTruncation(t *testing.T) {
	f, store, acc, o := offloadSetup(t)
	if _, err := runKeyed(t, f, store, acc, "s", "low", true, []ChatMessage{sysMsg("tools-v1"), u("read big.txt")}); err != nil {
		t.Fatal(err)
	}
	o.fail = true
	if _, err := runKeyed(t, f, store, acc, "s", "low", true, toolTurn(bigResult(300000))); err != nil {
		t.Fatal(err)
	}
	p := lastSentPrompt(f)
	if len(lastSentFiles(f)) != 0 || !strings.Contains(p, "tool output truncated") || strings.Contains(p, "attached to this message") || len(p) > 120000 {
		t.Fatalf("fallback must be the plain truncation (%dB)", len(p))
	}
}

func TestFitAgentPromptOffloadCapsFilesAndIsDeterministic(t *testing.T) {
	var msgs []ChatMessage
	var b strings.Builder
	for i := 0; i < 4; i++ {
		m := ChatMessage{Role: "tool", ToolCallID: fmt.Sprintf("c%d", i), Name: "Read", Content: strings.Repeat(fmt.Sprint(i), 50000+i)}
		msgs = append(msgs, m)
		b.WriteString(toolResultBlockHeader(m) + m.Content + "\n")
	}
	out1, rep, files, err := fitAgentPromptOffload(b.String(), msgs, 60000, 2)
	if err != nil || len(out1) > 60000 {
		t.Fatalf("fit: %v %d", err, len(out1))
	}
	if len(files) != 2 || rep.Offloaded != 2 || rep.Shortened != 4 {
		t.Fatalf("want 2 attached of 4 shortened, got files=%d rep=%+v", len(files), rep)
	}
	if strings.Count(out1, "attached to this message") != 2 || strings.Count(out1, "tool output truncated") != 2 {
		t.Fatal("blocks beyond the cap must use the plain truncation marker")
	}
	out2, _, files2, _ := fitAgentPromptOffload(b.String(), msgs, 60000, 2)
	if out1 != out2 || files[0].FileName != files2[0].FileName || string(files[1].Data) != string(files2[1].Data) {
		t.Fatal("identical input must give identical prompt and files")
	}
	if _, _, none, _ := fitAgentPromptOffload(b.String(), msgs, 60000, 0); none != nil {
		t.Fatal("maxFiles 0 must not offload")
	}
}

// fitTurnPrompt can run once per attempt of the same turn: the second run must
// rebuild the same prompt and attachments, not re-fit the fitted text.
func TestFitTurnPromptIsStableAcrossAttempts(t *testing.T) {
	msgs := toolTurn(bigResult(300000))
	turn := &agentTurn{followUp: true, prompt: agentThreadFollowUpPrompt(msgs[2:])}
	if err := fitTurnPrompt(turn, msgs); err != nil || len(turn.offloads) != 1 {
		t.Fatalf("first fit: %v files=%d", err, len(turn.offloads))
	}
	first := turn.prompt
	if err := fitTurnPrompt(turn, msgs); err != nil || turn.prompt != first || len(turn.offloads) != 1 {
		t.Fatalf("second fit changed the turn: %v files=%d same=%v", err, len(turn.offloads), turn.prompt == first)
	}
	turn.prompt = turn.offloadFallback // upload failed on the first attempt
	if err := fitTurnPrompt(turn, msgs); err != nil || turn.prompt != first || len(turn.offloads) != 1 {
		t.Fatal("a retry after a failed upload must try the attachment again")
	}
}

// --- merged from agent_size_guard_test.go ---
// Notion rejects oversized messages: a follow-up of about 136KB answers
// sendEventToAgentThread with HTTP 500, a create of about 200KB with HTTP 400
// ValidationError (measured with the real service, ASCII, bytes = runes; 132KB
// and 185KB passed). The fake models those limits (maxFollowUp / maxCreate). The
// bridge must keep every message it sends under a configurable budget by
// shortening tool results, and must refuse before sending when that is not
// enough, without disturbing the binding.

const (
	fakeFollowUpLimit = 135000
	fakeCreateLimit   = 190000
	headMark          = "HEAD-MARK-0001"
	tailMark          = "TAIL-MARK-9999"
)

func bigResult(n int) string {
	line := strings.Repeat("a", 79) + "\n"
	var b strings.Builder
	b.WriteString(headMark + "\n")
	for b.Len() < n-len(tailMark)-1 {
		b.WriteString(line)
	}
	s := b.String()[:n-len(tailMark)-1]
	return s + "\n" + tailMark
}

var readCall = ToolCall{ID: "call_1", Type: "function", Function: ToolCallFunction{Name: "Read", Arguments: `{"file_path":"big.txt"}`}}

func toolTurn(result string) []ChatMessage {
	return []ChatMessage{sysMsg("tools-v1"), u("read big.txt"), {Role: "assistant", ToolCalls: []ToolCall{readCall}}, {Role: "tool", ToolCallID: "call_1", Name: "Read", Content: result}}
}

func sizeSetup(t *testing.T) (*fakeAgentServer, *agentThreadStore, *Account) {
	f, store, acc := bindingSetup(t)
	f.maxCreate, f.maxFollowUp = fakeCreateLimit, fakeFollowUpLimit
	return f, store, acc
}

func lastSentPrompt(f *fakeAgentServer) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.sends[len(f.sends)-1]
	return promptText(s["event"].(map[string]interface{})["content"])
}

func TestSizeGuardBigToolResultFollowUpIsTruncatedAndSent(t *testing.T) {
	f, store, acc := sizeSetup(t)
	h1 := []ChatMessage{sysMsg("tools-v1"), u("read big.txt")}
	if _, err := runKeyed(t, f, store, acc, "s", "low", true, h1); err != nil {
		t.Fatal(err)
	}
	huge := bigResult(230 * 1024)
	out, err := runKeyed(t, f, store, acc, "s", "low", true, toolTurn(huge))
	if err != nil || out == "" {
		t.Fatalf("230KB tool result: %q %v", out, err)
	}
	if c, s := f.counts(); c != 1 || s != 1 {
		t.Fatalf("creates=%d sends=%d", c, s)
	}
	p := lastSentPrompt(f)
	if len(p) > 120000 {
		t.Fatalf("follow-up prompt is %dB, over the default 120000B budget", len(p))
	}
	for _, want := range []string{headMark, tailMark, "[notion-bridge: tool output truncated, ", " of 235520 bytes omitted to fit the Notion message size limit; re-run with a narrower range"} {
		if !strings.Contains(p, want) {
			t.Errorf("sent prompt lacks %q", want)
		}
	}
	if strings.Contains(p, huge) {
		t.Fatal("the full tool result was sent")
	}
}

func TestSizeGuardNextTurnWithOriginalHistoryStillContinuesTheBoundThread(t *testing.T) {
	f, store, acc := sizeSetup(t)
	if _, err := runKeyed(t, f, store, acc, "s", "low", true, toolTurn("small")[:2]); err != nil {
		t.Fatal(err)
	}
	huge := bigResult(230 * 1024)
	h2 := toolTurn(huge)
	if _, err := runKeyed(t, f, store, acc, "s", "low", true, h2); err != nil {
		t.Fatal(err)
	}
	// The client keeps the ORIGINAL tool result in its history.
	h3 := append(append([]ChatMessage{}, h2...), a("reply-2"), u("thanks, next question"))
	out, err := runKeyed(t, f, store, acc, "s", "low", true, h3)
	if err != nil || out == "" {
		t.Fatalf("next turn: %q %v", out, err)
	}
	if c, s := f.counts(); c != 1 || s != 2 {
		t.Fatalf("creates=%d sends=%d, want one thread and two follow-ups", c, s)
	}
	if p := lastSentPrompt(f); !strings.Contains(p, "thanks, next question") || strings.Contains(p, "tool output truncated") || strings.Contains(p, headMark) {
		t.Fatalf("the next follow-up must carry only the new message: %.200q", p)
	}
}

func TestSizeGuardTruncationIsDeterministicAcrossIdenticalRequests(t *testing.T) {
	var prompts [2]string
	for i := range prompts {
		f, store, acc := sizeSetup(t)
		if _, err := runKeyed(t, f, store, acc, "s", "low", true, toolTurn("x")[:2]); err != nil {
			t.Fatal(err)
		}
		if _, err := runKeyed(t, f, store, acc, "s", "low", true, toolTurn(bigResult(300000))); err != nil {
			t.Fatal(err)
		}
		prompts[i] = lastSentPrompt(f)
	}
	if prompts[0] != prompts[1] || !strings.Contains(prompts[0], "tool output truncated") {
		t.Fatal("the same request must produce byte-identical truncated prompts")
	}
}

func TestSizeGuardLostAckRetryIsNotResent(t *testing.T) {
	f, store, acc := sizeSetup(t)
	if _, err := runKeyed(t, f, store, acc, "s", "low", true, toolTurn("x")[:2]); err != nil {
		t.Fatal(err)
	}
	h2 := toolTurn(bigResult(300000))
	f.dropAckSend = true
	if _, err := runKeyed(t, f, store, acc, "s", "low", true, h2); err == nil {
		t.Fatal("want the lost acknowledgement reported")
	}
	f.dropAckSend = false
	out, err := runKeyed(t, f, store, acc, "s", "low", true, h2)
	if err != nil || out == "" {
		t.Fatalf("retry: %q %v", out, err)
	}
	if c, s := f.counts(); c != 1 || s != 1 {
		t.Fatalf("creates=%d sends=%d: the identical retry must read the accepted turn", c, s)
	}
}

func TestSizeGuardLargestToolResultIsShortenedFirst(t *testing.T) {
	f, store, acc := sizeSetup(t)
	if _, err := runKeyed(t, f, store, acc, "s", "low", true, []ChatMessage{sysMsg("tools-v1"), u("read both")}); err != nil {
		t.Fatal(err)
	}
	call2 := ToolCall{ID: "call_2", Type: "function", Function: ToolCallFunction{Name: "Read", Arguments: `{"file_path":"b.txt"}`}}
	small := strings.Repeat("s", 30000) + "SMALL-TAIL"
	msgs := []ChatMessage{sysMsg("tools-v1"), u("read both"), {Role: "assistant", ToolCalls: []ToolCall{readCall, call2}},
		{Role: "tool", ToolCallID: "call_1", Name: "Read", Content: bigResult(150000)},
		{Role: "tool", ToolCallID: "call_2", Name: "Read", Content: small}}
	if _, err := runKeyed(t, f, store, acc, "s", "low", true, msgs); err != nil {
		t.Fatal(err)
	}
	p := lastSentPrompt(f)
	if !strings.Contains(p, small) || !strings.Contains(p, "tool output truncated") || len(p) > 120000 {
		t.Fatalf("the smaller result must stay intact while the larger one is shortened (prompt %dB)", len(p))
	}
}

func TestSizeGuardMultibyteResultIsCutOnRuneBoundaries(t *testing.T) {
	f, store, acc := sizeSetup(t)
	if _, err := runKeyed(t, f, store, acc, "s", "low", true, toolTurn("x")[:2]); err != nil {
		t.Fatal(err)
	}
	huge := strings.Repeat("汉字é🙂", 40000) // 3+3+2+4 bytes per repeat
	if _, err := runKeyed(t, f, store, acc, "s", "low", true, toolTurn(huge)); err != nil {
		t.Fatal(err)
	}
	p := lastSentPrompt(f)
	if !utf8.ValidString(p) || len(p) > 120000 || !strings.Contains(p, "tool output truncated") {
		t.Fatalf("valid=%v bytes=%d", utf8.ValidString(p), len(p))
	}
}

func TestSizeGuardOversizedUserTextIsRefusedBeforeSendingAndBindingStaysUsable(t *testing.T) {
	f, store, acc := sizeSetup(t)
	h1 := []ChatMessage{sysMsg("tools-v1"), u("hello")}
	if _, err := runKeyed(t, f, store, acc, "s", "low", true, h1); err != nil {
		t.Fatal(err)
	}
	paste := strings.Repeat("p", 300000)
	h2 := append(append([]ChatMessage{}, h1...), a("reply-1"), u(paste))
	_, err := runKeyed(t, f, store, acc, "s", "low", true, h2)
	be, ok := err.(*agentBindingError)
	if !ok || be.Kind != "too_large" || !strings.Contains(be.Msg, "300") {
		t.Fatalf("want a too_large error naming the size, got %v", err)
	}
	f.mu.Lock()
	tries := f.sendTries
	f.mu.Unlock()
	if tries != 0 {
		t.Fatalf("%d send attempt(s): nothing may be sent", tries)
	}
	// The binding is intact: the user shortens the message and the same thread continues.
	h3 := append(append([]ChatMessage{}, h1...), a("reply-1"), u("a short question"))
	if out, err := runKeyed(t, f, store, acc, "s", "low", true, h3); err != nil || out == "" {
		t.Fatalf("after the refusal: %q %v", out, err)
	}
	if c, s := f.counts(); c != 1 || s != 1 {
		t.Fatalf("creates=%d sends=%d, want the same thread and one follow-up", c, s)
	}
}

func TestSizeGuardOversizedFirstMessageCreatesNothingAndNoBinding(t *testing.T) {
	f, store, acc := sizeSetup(t)
	_, err := runKeyed(t, f, store, acc, "s", "low", true, []ChatMessage{sysMsg("tools-v1"), u(strings.Repeat("p", 250000))})
	if be, ok := err.(*agentBindingError); !ok || be.Kind != "too_large" {
		t.Fatalf("got %v", err)
	}
	f.mu.Lock()
	tries := f.createTries
	f.mu.Unlock()
	store.mu.Lock()
	n := len(store.records)
	store.mu.Unlock()
	if tries != 0 || n != 0 {
		t.Fatalf("create attempts=%d bindings=%d, want 0/0", tries, n)
	}
	// A smaller first message binds normally afterwards.
	if _, err := runKeyed(t, f, store, acc, "s", "low", true, []ChatMessage{sysMsg("tools-v1"), u("hello")}); err != nil {
		t.Fatal(err)
	}
}

func TestSizeGuardCreateWithHistoryShortensToolResults(t *testing.T) {
	f, store, acc := sizeSetup(t)
	// A conversation that arrives for the first time already carrying a large tool result.
	h := append(toolTurn(bigResult(260000)), a("noted"), u("what was in it?"))
	if out, err := runKeyed(t, f, store, acc, "s", "low", true, h); err != nil || out == "" {
		t.Fatalf("%q %v", out, err)
	}
	f.mu.Lock()
	created := promptText(f.creates[0]["content"])
	f.mu.Unlock()
	if len(created) > 170000 || !strings.Contains(created, "tool output truncated") || !strings.Contains(created, tailMark) {
		t.Fatalf("create prompt %dB", len(created))
	}
}

func TestSizeGuardLogsSizesNotContent(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(log.Writer())
	f, store, acc := sizeSetup(t)
	if _, err := runKeyed(t, f, store, acc, "s", "low", true, toolTurn("x")[:2]); err != nil {
		t.Fatal(err)
	}
	secret := "SECRET-FILE-CONTENT-" + bigResult(300000)
	if _, err := runKeyed(t, f, store, acc, "s", "low", true, toolTurn(secret)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "SECRET-FILE-CONTENT") || !strings.Contains(buf.String(), "truncat") {
		t.Fatal("the log must mention the truncation (sizes) and never the content")
	}
}

func TestSizeGuardErrorIsClassifiedAs413TooLarge(t *testing.T) {
	f := classifyAgentFailure(&agentBindingError{Kind: "too_large", Msg: "x"})
	if f.Status != 413 || f.ErrType != "request_too_large" || f.ShouldRetry || !strings.HasPrefix(f.Message, "[bridge:too_large]") {
		t.Fatalf("%+v", f)
	}
}

func TestSizeGuardOversizedTaskNotificationUserMessageIsShortened(t *testing.T) {
	f, store, acc := sizeSetup(t)
	h1 := []ChatMessage{sysMsg("tools-v1"), u("spawn agent")}
	if _, err := runKeyed(t, f, store, acc, "s", "low", true, h1); err != nil {
		t.Fatal(err)
	}
	subOut := "<task-notification>\n<task-id>a1</task-id>\n<result>" + headMark + strings.Repeat("s", 180000) + tailMark + "</result>\n</task-notification>"
	h2 := append(append([]ChatMessage{}, h1...), a("waiting"), u(subOut))
	if out, err := runKeyed(t, f, store, acc, "s", "low", true, h2); err != nil || out == "" {
		t.Fatalf("oversized task-notification must be shortened and succeed, got %q %v", out, err)
	}
	p := lastSentPrompt(f)
	if len(p) > 120000 || !strings.Contains(p, headMark) || !strings.Contains(p, tailMark) || !strings.Contains(p, "tool output truncated") {
		t.Fatalf("sent prompt len=%d missing head/tail/marker", len(p))
	}
}

func TestSizeGuardCreateWithOversizedHistoryTurnsIsShortened(t *testing.T) {
	f, store, acc := sizeSetup(t)
	// A multi-turn rebind / compaction request where historical assistant replies exceed 170KB.
	h := []ChatMessage{
		sysMsg("tools-v1"),
		u("summarize the findings"),
		a("reply-1 " + headMark + strings.Repeat("a", 210000) + tailMark),
		u("what did we conclude?"),
	}
	if out, err := runKeyed(t, f, store, acc, "s", "low", true, h); err != nil || out == "" {
		t.Fatalf("oversized historical assistant turns must be shortened on create, got %q %v", out, err)
	}
	f.mu.Lock()
	created := promptText(f.creates[0]["content"])
	f.mu.Unlock()
	if len(created) > 170000 || !strings.Contains(created, "what did we conclude?") || !strings.Contains(created, "tool output truncated") {
		t.Fatalf("create prompt %dB", len(created))
	}
}

func TestFitAgentPromptOffloadStage5MultiTurnDeepTruncation(t *testing.T) {
	// Build a 40-turn history where each block is ~3.5KB (below Stage 1-2's 4096B
	// minimum keep) so Stages 1-4 alone cannot bring the prompt under 170KB/250KB.
	msgs := []ChatMessage{
		sysMsg("SYSTEM_CONTEXT_" + strings.Repeat("C", 3500)),
	}
	for i := 0; i < 40; i++ {
		rem := fmt.Sprintf("<system-reminder>REM_%d_%s</system-reminder>\n", i, strings.Repeat("R", 900))
		msgs = append(msgs,
			u(rem+fmt.Sprintf("user_turn_%d_%s", i, strings.Repeat("U", 3000))),
			ChatMessage{
				Role:    "assistant",
				Content: fmt.Sprintf("assistant_turn_%d_%s", i, strings.Repeat("A", 3000)),
				ToolCalls: []ToolCall{{
					ID:       fmt.Sprintf("call_%d", i),
					Type:     "function",
					Function: ToolCallFunction{Name: "Read", Arguments: fmt.Sprintf(`{"path":"file_%d.go"}`, i)},
				}},
			},
			toolMsg(fmt.Sprintf("call_%d", i), fmt.Sprintf("tool_out_%d_%s", i, strings.Repeat("T", 3500))),
		)
	}
	finalInstruction := "FINAL_SUMMARY_INSTRUCTION_MUST_STAY_INTACT"
	msgs = append(msgs, u(finalInstruction))

	prompt := agentThreadPrompt(msgs)
	if len(prompt) <= 250000 {
		t.Fatalf("expected raw prompt > 250000 bytes, got %d", len(prompt))
	}
	out, rep, _, err := fitAgentPromptOffload(prompt, msgs, defaultAgentCreateMaxBytes, 8)
	if err != nil {
		t.Fatalf("Stage 5 deep truncation failed: %v", err)
	}
	if len(out) > defaultAgentCreateMaxBytes {
		t.Fatalf("fitted prompt %d bytes exceeds budget %d", len(out), defaultAgentCreateMaxBytes)
	}
	if !strings.Contains(out, finalInstruction) {
		t.Fatal("final instruction must remain intact after Stage 5 deep truncation")
	}
	if !strings.Contains(out, "[truncated ") || rep.Shortened == 0 {
		t.Fatalf("expected Stage 5 deep truncation markers, got rep=%+v", rep)
	}
}

func TestFitAgentPromptOffloadTrailingNewlineSystemAnd35TurnCompact(t *testing.T) {
	// Raw system message ends with '\n', so systemMessages(msgs)[0].Content is a
	// strict prefix of msgs[0].Content; Stage 3 must not phantom-deduct remaining
	// bytes or attach duplicate context files when the second candidate no longer
	// exists in out (#116).
	msgs := []ChatMessage{
		sysMsg("PROJECT_SYSTEM_HEADER_" + strings.Repeat("S", 42000) + "\n"),
	}
	for i := 0; i < 35; i++ {
		rem := fmt.Sprintf("<system-reminder>TURN_REMINDER_%d_%s</system-reminder>\n", i, strings.Repeat("R", 1200))
		msgs = append(msgs,
			u(rem+fmt.Sprintf("user_turn_%d_%s", i, strings.Repeat("U", 2400))),
			a(fmt.Sprintf("assistant_turn_%d_%s", i, strings.Repeat("A", 2400))),
		)
	}
	msgs = append(msgs, ChatMessage{Role: "user", Content: wireInstruction, StringContent: true})

	if len(systemMessages(msgs)) != 1 || systemMessages(msgs)[0].Content == msgs[0].Content {
		t.Fatal("expected normalized system message to differ from raw system message by trailing newline")
	}

	for name, prompt := range map[string]string{
		"full":    agentThreadPrompt(msgs),
		"summary": agentThreadSummaryPrompt(msgs),
	} {
		t.Run(name, func(t *testing.T) {
			if len(prompt) <= defaultAgentCreateMaxBytes {
				t.Fatalf("expected initial %s prompt > %d, got %d", name, defaultAgentCreateMaxBytes, len(prompt))
			}
			out, _, files, err := fitAgentPromptOffload(prompt, msgs, defaultAgentCreateMaxBytes, 8)
			if err != nil {
				t.Fatalf("fitAgentPromptOffload(%s) failed: %v", name, err)
			}
			if len(out) > defaultAgentCreateMaxBytes {
				t.Fatalf("fitted %s prompt %d exceeds %d", name, len(out), defaultAgentCreateMaxBytes)
			}
			if !strings.Contains(out, summaryInstructionHead) {
				t.Fatalf("fitted %s prompt lost final summary instruction", name)
			}
			ctxFiles := 0
			for _, f := range files {
				if strings.Contains(f.FileName, "context-") {
					ctxFiles++
				}
			}
			if ctxFiles > 1 {
				t.Fatalf("expected at most 1 offloaded context file, got %d (%+v)", ctxFiles, files)
			}
		})
	}
}

func TestFitAgentPromptOffloadStage6UltraLongSessionFitsBudget(t *testing.T) {
	// 320 tool-using turns (960+ messages) where each message body is ~200 bytes:
	// even after Stage 5 truncates every block > 208B to 128B+marker, the 960+
	// headers and snippets exceed 170KB. Stage 6 must elide the oldest intermediate
	// blocks while keeping the first user message and final instruction intact.
	msgs := []ChatMessage{
		sysMsg("PROJECT_CONTEXT_" + strings.Repeat("S", 8000)),
		u("FIRST_USER_ROOT_MESSAGE_KEEP_ME"),
	}
	for i := 0; i < 320; i++ {
		callID := fmt.Sprintf("call_u_%d", i)
		msgs = append(msgs,
			ChatMessage{
				Role:    "assistant",
				Content: fmt.Sprintf("assistant_step_%d_%s", i, strings.Repeat("a", 240)),
				ToolCalls: []ToolCall{{
					ID:       callID,
					Type:     "function",
					Function: ToolCallFunction{Name: "Bash", Arguments: fmt.Sprintf(`{"command":"echo %d","description":"step %d"}`, i, i)},
				}},
			},
			ChatMessage{
				Role:       "tool",
				ToolCallID: callID,
				Name:       "Bash",
				Content:    fmt.Sprintf("tool_out_%d_%s", i, strings.Repeat("t", 240)),
			},
			u(fmt.Sprintf("user_follow_%d_%s", i, strings.Repeat("u", 180))),
		)
	}
	msgs = append(msgs, ChatMessage{Role: "user", Content: wireInstruction, StringContent: true})

	prompt := agentThreadSummaryPrompt(msgs)
	if len(prompt) <= defaultAgentCreateMaxBytes {
		t.Fatalf("expected ultra-long prompt > %d, got %d", defaultAgentCreateMaxBytes, len(prompt))
	}
	out, rep, _, err := fitAgentPromptOffload(prompt, msgs, defaultAgentCreateMaxBytes, 8)
	if err != nil {
		t.Fatalf("expected Stage 6 ultra-long session fit to succeed, got: %v", err)
	}
	if len(out) > defaultAgentCreateMaxBytes {
		t.Fatalf("fitted prompt %d exceeds budget %d", len(out), defaultAgentCreateMaxBytes)
	}
	if !strings.Contains(out, "FIRST_USER_ROOT_MESSAGE_KEEP_ME") {
		t.Fatal("first user message must remain intact after Stage 6")
	}
	if !strings.Contains(out, summaryInstructionHead) {
		t.Fatal("final summary instruction must remain intact after Stage 6")
	}
	if rep.Shortened == 0 {
		t.Fatalf("expected shortened blocks > 0, got %+v", rep)
	}
}

func TestTransientUserValidationErrorRetryAndUncreatedFirstTurnDiscard(t *testing.T) {
	f, store, acc := sizeSetup(t)

	// 1. Persistent HTTP 400 (code=UserValidationError) on Turn 1 createAgentThread:
	//    must be classified as retryable (ShouldRetry=true) and must discard the
	//    uncreated local "submitting" binding so the next request on the session
	//    binds cleanly.
	f.mu.Lock()
	f.rejectCreate = 400
	f.rejectBody = `{"name":"UserValidationError","message":"transient"}`
	f.mu.Unlock()

	_, err := runKeyed(t, f, store, acc, "subagent-1", "low", true, []ChatMessage{u("task 1")})
	if err == nil {
		t.Fatal("expected createAgentThread 400 error")
	}
	failure := classifyAgentFailure(err)
	if !failure.ShouldRetry || failure.Status != 502 {
		t.Fatalf("expected ShouldRetry=true Status=502 for UserValidationError, got %+v", failure)
	}
	store.mu.Lock()
	recCount := len(store.records)
	store.mu.Unlock()
	if recCount != 0 {
		t.Fatalf("expected uncreated Turn 1 binding to be discarded on definitive 400, got %d record(s)", recCount)
	}

	// 2. Subsequent request on the same session succeeds cleanly once upstream recovers.
	f.mu.Lock()
	f.rejectCreate = 0
	f.rejectBody = ""
	f.mu.Unlock()
	out, err := runKeyed(t, f, store, acc, "subagent-1", "low", true, []ChatMessage{u("task 1 retry")})
	if err != nil || out == "" {
		t.Fatalf("expected retry after discarded Turn 1 to succeed, got %q %v", out, err)
	}
}

