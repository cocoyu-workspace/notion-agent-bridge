package proxy

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// Auto-compact (Claude Code 2.1.296) on a bound Notion thread.
//
// testdata/claude-code-2.1.296-auto-compact-request.json is the structure of a
// real summarisation request the CLI sent when its context window filled up
// (CLAUDE_CODE_AUTO_COMPACT_WINDOW set small): five user turns, two of them with
// a tool round trip, role "system" entries in between, and - unlike /compact -
// NO assistant reply after the last user message: the compaction runs BEFORE the
// fifth turn is sent, so the summary instruction is appended as a second text
// block to the pending user message. Texts are de-identified; the instruction is
// the real text (testdata, byte-identical to /compact's).
//
// The continuation the client sends afterwards is derived from the 2.1.296 code
// (compactConversation with isAutoCompact: no messages kept, the summary
// message carries the "Continue the conversation ..." sentence, no /compact
// command, caveat or stdout blocks, no new user text), because a real one could
// not be captured while the bridge refused the summary request.

const autoPending = "Name one primary colour. One word.\n"

func autoFixture(t *testing.T) []map[string]interface{} {
	t.Helper()
	data, err := os.ReadFile("testdata/claude-code-2.1.296-auto-compact-request.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		Messages []map[string]interface{} `json:"messages"`
	}
	if err := json.Unmarshal(data, &fx); err != nil {
		t.Fatal(err)
	}
	for _, m := range fx.Messages {
		blocks, _ := m["content"].([]interface{})
		for _, b := range blocks {
			bm := b.(map[string]interface{})
			switch bm["text"] {
			case "$ATTRIBUTION":
				bm["text"] = wireAttribution
			case "$INSTRUCTION":
				bm["text"] = wireInstruction
			}
		}
	}
	if len(fx.Messages) != 20 {
		t.Fatalf("fixture has %d messages, want 20", len(fx.Messages))
	}
	return fx.Messages
}

// autoCuts are the ends of the ordinary requests that precede the compaction:
// one per user message / tool result with its trailing system entry.
var autoCuts = []int{2, 5, 8, 11, 14, 17}

// autoBind sends the five ordinary requests (the sixth reply number is the
// side request's) and returns the bound thread.
func (e *compactEnv) autoBind(fx []map[string]interface{}) string {
	e.t.Helper()
	for _, c := range autoCuts {
		if _, err := e.send(e.key, fx[:c]...); err != nil {
			e.t.Fatalf("request ending at message %d: %v", c, err)
		}
	}
	return e.boundThread(e.key)
}

const autoSummaryReplyNo = 7 // the fake numbers every reply; six ordinary requests came first

func (e *compactEnv) autoSummary(fx []map[string]interface{}) string {
	e.t.Helper()
	reply := "<analysis>scratch notes</analysis>\n\n<summary>\n" + strings.ReplaceAll(summaryInner, "\n\n", "\n\n\n") + "\n</summary>"
	e.replies[autoSummaryReplyNo] = reply
	out, err := e.send(e.key, fx...)
	if err != nil || out != reply {
		e.t.Fatalf("auto-compact summarisation request: %q %v", out, err)
	}
	return reply
}

// postAutoCompact is the first request after an auto-compact: the whole history
// is replaced by the one summary message (attribution block first, like any
// first user message); extra blocks follow the summary block.
func postAutoCompact(summary string, extra ...string) []map[string]interface{} {
	texts := append([]string{wireAttribution, wirePreface + summary + wirePointer}, extra...)
	return []map[string]interface{}{wireBlocks("user", texts...), contextMsg()}
}

func TestAutoCompactSummaryRequestIsServedOnSideThreadNotOnMainThread(t *testing.T) {
	e := newCompactEnv(t)
	fx := autoFixture(t)
	main := e.autoBind(fx)
	before := e.entities(main)
	createsBefore, sendsBefore := e.f.counts()
	e.autoSummary(fx)
	if got := e.entities(main); got != before {
		t.Fatalf("the summarisation request reached the bound thread: entities %d -> %d", before, got)
	}
	if c, s := e.f.counts(); c != createsBefore+1 || s != sendsBefore {
		t.Fatalf("creates %d -> %d, sends %d -> %d; want exactly one one-shot create and no send", createsBefore, c, sendsBefore, s)
	}
	side := promptText(e.f.creates[len(e.f.creates)-1]["content"])
	for _, want := range []string{instrHead, strings.TrimSpace(autoPending)} {
		if !strings.Contains(side, want) {
			t.Errorf("the side thread must see the summary instruction and the pending message, lacks %q", want)
		}
	}
	if e.f.creates[len(e.f.creates)-1]["threadId"] == main || e.bindings() != 1 || e.boundThread(e.key) != main {
		t.Fatal("the side request must neither use nor move the binding")
	}
	// The compaction may fail on the client side; the pending turn then goes out
	// as an ordinary follow-up on the bound thread, without the instruction.
	next := append(append([]map[string]interface{}{}, fx[:18]...), wireBlocks("user", autoPending), fx[19])
	if _, err := e.send(e.key, next...); err != nil {
		t.Fatalf("ordinary follow-up after the summarisation request: %v", err)
	}
	if c, s := e.f.counts(); c != createsBefore+1 || s != sendsBefore+1 || !strings.HasPrefix(e.sendPrompts()[sendsBefore], main+"\x00") {
		t.Fatalf("follow-up must go to the bound thread: creates=%d sends=%d", c, s)
	}
	if p := e.sendPrompts()[sendsBefore]; strings.Contains(p, instrHead) || !strings.Contains(p, strings.TrimSpace(autoPending)) {
		t.Fatalf("follow-up carries the instruction or lacks the pending message: %q", p)
	}
}

func TestAutoCompactContinuationRebasesBoundThread(t *testing.T) {
	e := newCompactEnv(t)
	fx := autoFixture(t)
	main := e.autoBind(fx)
	reply := e.autoSummary(fx)
	c0, s0 := e.f.counts()
	e.replies[autoSummaryReplyNo+1] = "Red"
	out, err := e.send(e.key, postAutoCompact(clientSummary(reply))...)
	if err != nil || out != "Red" {
		t.Fatalf("post auto-compact request: %q %v", out, err)
	}
	if c, s := e.f.counts(); c != c0 || s != s0+1 {
		t.Fatalf("creates %d -> %d, sends %d -> %d; the continuation must be one send on the bound thread", c0, c, s0, s)
	}
	sent := e.sendPrompts()[s0]
	if !strings.HasPrefix(sent, main+"\x00") {
		t.Fatalf("sent to %q, want the bound thread %q", strings.SplitN(sent, "\x00", 2)[0], main)
	}
	for _, want := range []string{testMarker, "Primary Request and Intent", "Continue the conversation from where it left off"} {
		if !strings.Contains(sent, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
	for _, leak := range []string{"Attribution for git commits", "read the full transcript at", instrHead, "NEW MESSAGE"} {
		if strings.Contains(sent, leak) {
			t.Errorf("%q must not be forwarded: %q", leak, sent)
		}
	}
	if e.bindings() != 1 || e.boundThread(e.key) != main {
		t.Fatal("binding moved")
	}
	// The next ordinary turn of the compacted conversation: only the new message.
	next := append(postAutoCompact(clientSummary(reply)), wireBlocks("assistant", "Red"), wireBlocks("user", "and one secondary colour?"))
	if _, err := e.send(e.key, next[0], next[1], next[2], next[3]); err != nil {
		t.Fatal(err)
	}
	second := e.sendPrompts()[s0+1]
	if c, s := e.f.counts(); c != c0 || s != s0+2 || !strings.Contains(second, "and one secondary colour?") || strings.Contains(second, "Primary Request and Intent") {
		t.Fatalf("follow-up after auto-compact must carry only the new message (creates=%d sends=%d): %q", c, s, second)
	}
}

func TestAutoCompactContinuationKeepsRestoredContextBlocksAndSurvivesRestart(t *testing.T) {
	e := newCompactEnv(t)
	fx := autoFixture(t)
	e.autoBind(fx)
	reply := e.autoSummary(fx)
	e.store = newAgentThreadStore(e.path) // bridge restart between summary and continuation
	restored := "<system-reminder>\nCalled the Read tool with the following input: {\"file_path\":\"/work/calc.go\"}\n</system-reminder>\n"
	e.replies[autoSummaryReplyNo+1] = "Red"
	if _, err := e.send(e.key, postAutoCompact(clientSummary(reply), restored)...); err != nil {
		t.Fatal(err)
	}
	// bound create + one-shot create; five ordinary follow-ups + the continuation
	if c, s := e.f.counts(); c != 2 || s != 6 {
		t.Fatalf("creates=%d sends=%d", c, s)
	}
	if p := e.sendPrompts()[5]; !strings.Contains(p, "Called the Read tool with the following input") {
		t.Fatalf("restored client context was dropped: %q", p)
	}
}

func TestAutoCompactVariantsStayRefused(t *testing.T) {
	bad := strings.Replace(wireInstruction, "TEXT ONLY", "TEXT ONLY!", 1)
	summaryCases := map[string][]map[string]interface{}{
		"instruction block without a pending user message": {wireBlocks("user", wireInstruction)},
		"altered instruction as appended block":            {wireBlocks("user", autoPending, bad)},
		"pending text after the instruction block":         {wireBlocks("user", wireInstruction, autoPending)},
		"blank pending text":                               {wireBlocks("user", "\n", wireInstruction)},
	}
	for name, last := range summaryCases {
		t.Run("summary request: "+name, func(t *testing.T) {
			e := newCompactEnv(t)
			fx := autoFixture(t)
			main := e.autoBind(fx)
			c0, s0 := e.f.counts()
			ent := e.entities(main)
			req := append(append([]map[string]interface{}{}, fx[:18]...), last...)
			_, err := e.send(e.key, req...)
			be, ok := err.(*agentBindingError)
			if !ok || be.Kind != "diverged" || !strings.Contains(be.Msg, "summarisation") {
				t.Fatalf("want a diverged summarisation refusal, got %v", err)
			}
			if c, s := e.f.counts(); c != c0 || s != s0 || e.entities(main) != ent || e.boundThread(e.key) != main {
				t.Fatal("a refused summarisation request must send nothing and keep the binding")
			}
		})
	}

	otherArm := strings.Replace(wirePointer, "Pick up the last task", "Take your time, then pick up the last task", 1)
	contCases := map[string]func(e *compactEnv, fx []map[string]interface{}, reply string) []map[string]interface{}{
		"no summarisation request was seen": nil,
		"summary differs from the produced one": func(e *compactEnv, fx []map[string]interface{}, reply string) []map[string]interface{} {
			return postAutoCompact(clientSummary(reply) + "\n   - an extra bullet")
		},
		"conversation moved on after the summarisation request": func(e *compactEnv, fx []map[string]interface{}, reply string) []map[string]interface{} {
			if _, err := e.send(e.key, append(append([]map[string]interface{}{}, fx[:18]...), wireBlocks("user", autoPending), fx[19])...); err != nil {
				e.t.Fatal(err)
			}
			return postAutoCompact(clientSummary(reply))
		},
		"typed text after the continuation sentence": func(e *compactEnv, fx []map[string]interface{}, reply string) []map[string]interface{} {
			return postAutoCompact(clientSummary(reply), "please also delete the repository")
		},
		"unknown continuation wording": func(e *compactEnv, fx []map[string]interface{}, reply string) []map[string]interface{} {
			return []map[string]interface{}{wireBlocks("user", wireAttribution, wirePreface+clientSummary(reply)+otherArm), contextMsg()}
		},
		"more than one message": func(e *compactEnv, fx []map[string]interface{}, reply string) []map[string]interface{} {
			return append(postAutoCompact(clientSummary(reply)), wireBlocks("assistant", "Red"), wireBlocks("user", "next"))
		},
	}
	for name, mutate := range contCases {
		t.Run("continuation: "+name, func(t *testing.T) {
			e := newCompactEnv(t)
			fx := autoFixture(t)
			main := e.autoBind(fx)
			reply := "<summary>\n" + summaryInner + "\n</summary>"
			if mutate != nil {
				reply = e.autoSummary(fx)
			}
			var msgs []map[string]interface{}
			if mutate == nil {
				msgs = postAutoCompact(clientSummary(reply))
			} else {
				msgs = mutate(e, fx, reply)
			}
			c0, s0 := e.f.counts()
			ent := e.entities(main)
			_, err := e.send(e.key, msgs...)
			be, ok := err.(*agentBindingError)
			if !ok || be.Kind != "diverged" || !strings.Contains(be.Msg, "/compact") || !strings.Contains(be.Msg, "binding is kept") {
				t.Fatalf("want a diverged error that names the compaction continuation, got %v", err)
			}
			if c, s := e.f.counts(); c != c0 || s != s0 || e.entities(main) != ent || e.boundThread(e.key) != main {
				t.Fatal("a refused continuation must send nothing and keep the binding")
			}
		})
	}
}

func TestAutoCompactSummaryRequestWithoutBindingRunsOnOneShotThread(t *testing.T) {
	e := newCompactEnv(t)
	fx := autoFixture(t)
	e.replies[1] = "<summary>x</summary>"
	if _, err := e.send(e.key, fx...); err != nil {
		t.Fatal(err)
	}
	if c, s := e.f.counts(); c != 1 || s != 0 || e.bindings() != 0 {
		t.Fatalf("creates=%d sends=%d bindings=%d; an unbound summarisation request must not create a binding", c, s, e.bindings())
	}
}

func TestConvertAnthropicMessagesRecordsTextBlockLayout(t *testing.T) {
	chat := toChat(t, wireBlocks("user", "a", "b"), wireString("user", "plain"), wireBlocks("user", "only"))
	got := chat[2:] // protocol message and client system prompt first
	if got[0].TextBlocks != 2 || got[0].LastTextBlock != "b" || got[0].Content != "ab" {
		t.Errorf("two blocks: %+v", got[0])
	}
	if got[1].TextBlocks != 0 || got[1].LastTextBlock != "" || !got[1].StringContent {
		t.Errorf("plain string: %+v", got[1])
	}
	if got[2].TextBlocks != 1 || got[2].LastTextBlock != "only" {
		t.Errorf("one block: %+v", got[2])
	}
}

// Real Claude Code 2.1.296 alignment (captured end to end): the auto-compact
// request drops the bound thread's LAST assistant reply and appends the summary
// instruction, as a second text block, to the user message that reply answered.
func TestAutoCompactRealAlignmentDropsLastReplyRunsOnSideThread(t *testing.T) {
	e := newCompactEnv(t)
	fx := autoFixture(t)
	for _, c := range []int{2, 5, 8, 11, 14, 17} { // last request: "What is 17*23?" (fx[15]) answered on the bound thread
		if _, err := e.send(e.key, fx[:c]...); err != nil {
			t.Fatalf("request ending at %d: %v", c, err)
		}
	}
	main := e.boundThread(e.key)
	before := e.entities(main)
	createsBefore, sendsBefore := e.f.counts()
	blocks := fx[18]["content"].([]interface{})
	instr := blocks[len(blocks)-1].(map[string]interface{})["text"].(string)
	pending := fx[15]["content"].(string) + "\n"
	req := append(append([]map[string]interface{}{}, fx[:15]...), wireBlocks("user", pending, instr), contextMsg())
	reply := "<analysis>x</analysis>\n\n<summary>\n" + summaryInner + "\n</summary>"
	e.replies[autoSummaryReplyNo] = reply
	out, err := e.send(e.key, req...)
	if err != nil || out != reply {
		t.Fatalf("real-shape auto-compact summarisation request: %q %v", out, err)
	}
	if got := e.entities(main); got != before {
		t.Fatalf("summarisation reached the bound thread: entities %d -> %d", before, got)
	}
	if c, s := e.f.counts(); c != createsBefore+1 || s != sendsBefore {
		t.Fatalf("creates %d -> %d, sends %d -> %d; want one one-shot create, no send", createsBefore, c, sendsBefore, s)
	}
	if e.bindings() != 1 || e.boundThread(e.key) != main {
		t.Fatal("the side request must not move the binding")
	}
	// The same alignment with an edited earlier user message is still refused.
	bad := append(append([]map[string]interface{}{}, fx[:15]...), wireBlocks("user", "What is 17*24? Reply with the number only.\n", instr), contextMsg())
	if _, err := e.send(e.key, bad...); err == nil {
		t.Fatal("an edited pending message with the instruction must be refused")
	}
}

// The continuation 2.1.296 really sends after that summary request: the summary
// message ends with the "kept verbatim" paragraph, then the reply the summary
// request dropped (assistant, unchanged) and the new user message. It re-bases
// the bound thread with one send that carries the summary and the new message.
func TestAutoCompactRealContinuationKeepsLastReplyAndNewMessage(t *testing.T) {
	e := newCompactEnv(t)
	fx := autoFixture(t)
	for _, c := range []int{2, 5, 8, 11, 14, 17} {
		if _, err := e.send(e.key, fx[:c]...); err != nil {
			t.Fatalf("request ending at %d: %v", c, err)
		}
	}
	main := e.boundThread(e.key)
	blocks := fx[18]["content"].([]interface{})
	instr := blocks[len(blocks)-1].(map[string]interface{})["text"].(string)
	req := append(append([]map[string]interface{}{}, fx[:15]...), wireBlocks("user", fx[15]["content"].(string)+"\n", instr), contextMsg())
	reply := "<analysis>x</analysis>\n\n<summary>\n" + summaryInner + "\n</summary>"
	e.replies[autoSummaryReplyNo] = reply
	if out, err := e.send(e.key, req...); err != nil || out != reply {
		t.Fatalf("summarisation: %q %v", out, err)
	}
	kept := "\n\nThe messages after this summary are the most recent messages from before compaction, kept verbatim. The summary was written without seeing them, so something it says has not happened yet may already have happened in them."
	first := wireBlocks("user", wireAttribution, wirePreface+clientSummary(reply)+strings.TrimSuffix(wirePointer, "\n")+kept)
	cont := []map[string]interface{}{first, contextMsg(), wireBlocks("assistant", "391"), wireBlocks("user", "<system-reminder>\nstate\n</system-reminder>", autoPending), contextMsg()}
	c0, s0 := e.f.counts()
	e.replies[autoSummaryReplyNo+1] = "Red"
	out, err := e.send(e.key, cont...)
	if err != nil || out != "Red" {
		t.Fatalf("real auto-compact continuation: %q %v", out, err)
	}
	if c, s := e.f.counts(); c != c0 || s != s0+1 {
		t.Fatalf("creates %d -> %d, sends %d -> %d; want one send on the bound thread", c0, c, s0, s)
	}
	sent := e.sendPrompts()[s0]
	if !strings.HasPrefix(sent, main+"\x00") {
		t.Fatal("continuation not sent to the bound thread")
	}
	for _, want := range []string{testMarker, "NEW MESSAGE", "Name one primary colour"} {
		if !strings.Contains(sent, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
	for _, leak := range []string{"read the full transcript at", "kept verbatim. The summary was written", instrHead} {
		if strings.Contains(sent, leak) {
			t.Errorf("%q must not be forwarded", leak)
		}
	}
	if e.bindings() != 1 || e.boundThread(e.key) != main {
		t.Fatal("binding moved")
	}
	// Next ordinary turn: only the new message.
	next := append(append([]map[string]interface{}{}, cont...), wireBlocks("assistant", "Red"), wireBlocks("user", "and one secondary colour?"))
	if _, err := e.send(e.key, next...); err != nil {
		t.Fatal(err)
	}
	if second := e.sendPrompts()[s0+1]; !strings.Contains(second, "and one secondary colour?") || strings.Contains(second, "Primary Request and Intent") {
		t.Fatalf("follow-up after auto-compact must carry only the new message: %q", second)
	}
	// Kept tails outside the verified layout are refused.
	for name, rest := range map[string][]ChatMessage{
		"no reply":        {{Role: "user", Content: "x"}},
		"user first":      {{Role: "user", Content: "x"}, {Role: "user", Content: "y"}},
		"reply with tool": {{Role: "assistant", Content: "ok", ToolCalls: []ToolCall{{ID: "t"}}}, {Role: "user", Content: "y"}},
		"no user text":    {{Role: "assistant", Content: "391"}, {Role: "user", Content: "  "}},
	} {
		if _, why := autoCompactKeptTail(rest); why == "" {
			t.Errorf("%s: kept tail accepted", name)
		}
	}
}

// Real Claude Code 2.1.280/2.1.296 post-tool /compact: when a turn used a tool
// (so >= 2 assistant messages exist), /compact routes through reactive_compact,
// drops the final assistant reply, and appends the summary instruction (with or
// without focus) as a text block to the preceding tool_result user message. The
// continuation then carries [summary user message, kept assistant reply, user
// message with caveat + /compact command + stdout + new user text].
func TestPostToolManualCompactDropsLastReplyAndRebasesWithKeptReply(t *testing.T) {
	e := newCompactEnv(t)
	fx := autoFixture(t)
	// fx[:8] ends after the first tool round-trip: fx[5] is assistant tool_use, fx[6] is user tool_result, fx[7] is system context.
	for _, c := range []int{2, 5, 8} {
		if _, err := e.send(e.key, fx[:c]...); err != nil {
			t.Fatalf("request ending at %d: %v", c, err)
		}
	}
	main := e.boundThread(e.key)
	before := e.entities(main)
	createsBefore, sendsBefore := e.f.counts()

	// Build the post-tool /compact summary request: fx[6] (tool_result) gets the focused instruction appended as a text block.
	toolResultBlocks := fx[6]["content"].([]interface{})
	focusedInstr := focusInstruction("keep the echo hi result")
	mergedToolUser := map[string]interface{}{
		"role": "user",
		"content": []interface{}{
			toolResultBlocks[0],
			map[string]interface{}{"type": "text", "text": focusedInstr},
		},
	}
	req := append(append([]map[string]interface{}{}, fx[:6]...), mergedToolUser, contextMsg())
	reply := "<analysis>ok</analysis>\n\n<summary>\n" + summaryInner + "\n</summary>"
	e.replies[4] = reply
	out, err := e.send(e.key, req...)
	if err != nil || out != reply {
		t.Fatalf("post-tool /compact summary request: %q %v", out, err)
	}
	if got := e.entities(main); got != before {
		t.Fatalf("summary request reached bound thread: %d -> %d", before, got)
	}
	if c, s := e.f.counts(); c != createsBefore+1 || s != sendsBefore {
		t.Fatalf("creates %d -> %d, sends %d -> %d; want one side-thread create and no send", createsBefore, c, sendsBefore, s)
	}

	// Continuation: summary message (plain string, no claudeCompactionKept in 2.1.280), kept assistant reply, then caveat + /compact + stdout + newUser.
	summaryMsg := wireString("user", wirePreface+clientSummary(reply)+strings.TrimSuffix(wirePointer, "\n"))
	keptAssistant := wireBlocks("assistant", "The command printed hi.")
	withArgs := strings.Replace(wireCommand, "<command-args></command-args>", "<command-args>keep the echo hi result</command-args>", 1)
	newUserMsg := wireBlocks("user", wireAttribution, wireCaveat, withArgs, wireStdout, "What did we run earlier?")
	cont := []map[string]interface{}{summaryMsg, keptAssistant, newUserMsg, contextMsg()}

	c0, s0 := e.f.counts()
	e.replies[5] = testMarker
	out, err = e.send(e.key, cont...)
	if err != nil || out != testMarker {
		t.Fatalf("post-tool /compact continuation: %q %v", out, err)
	}
	if c, s := e.f.counts(); c != c0 || s != s0+1 {
		t.Fatalf("creates %d -> %d, sends %d -> %d; want one send on bound thread", c0, c, s0, s)
	}
	sent := e.sendPrompts()[s0]
	if !strings.HasPrefix(sent, main+"\x00") {
		t.Fatalf("sent to wrong thread: %q", sent)
	}
	for _, want := range []string{testMarker, "Primary Request and Intent", "What did we run earlier?"} {
		if !strings.Contains(sent, want) {
			t.Errorf("prompt lacks %q: %q", want, sent)
		}
	}
	for _, leak := range []string{"<local-command-caveat>", "<command-name>", "<local-command-stdout>", "Attribution for git commits", "read the full transcript at", instrHead} {
		if !strings.Contains(sent, leak) {
			continue
		}
		t.Errorf("boilerplate %q leaked into prompt: %q", leak, sent)
	}

	// Follow-up turn after the post-tool /compact continuation.
	next := append(append([]map[string]interface{}{}, cont...), wireBlocks("assistant", testMarker), wireBlocks("user", "and one more question"))
	e.replies[6] = "done"
	if out, err := e.send(e.key, next...); err != nil || out != "done" {
		t.Fatalf("follow-up after post-tool /compact: %q %v", out, err)
	}
	if second := e.sendPrompts()[s0+1]; !strings.Contains(second, "and one more question") || strings.Contains(second, "Primary Request and Intent") {
		t.Fatalf("follow-up must carry only the new message: %q", second)
	}
}

func TestPostToolAutoCompactDropsLastReplyAndRebasesOn21280And21296(t *testing.T) {
	e := newCompactEnv(t)
	fx := autoFixture(t)
	for _, c := range []int{2, 5, 8} {
		if _, err := e.send(e.key, fx[:c]...); err != nil {
			t.Fatalf("request ending at %d: %v", c, err)
		}
	}
	main := e.boundThread(e.key)
	toolResultBlocks := fx[6]["content"].([]interface{})
	mergedToolUser := map[string]interface{}{
		"role": "user",
		"content": []interface{}{
			toolResultBlocks[0],
			map[string]interface{}{"type": "text", "text": wireInstruction},
		},
	}
	req := append(append([]map[string]interface{}{}, fx[:6]...), mergedToolUser, contextMsg())
	reply := "<analysis>ok</analysis>\n\n<summary>\n" + summaryInner + "\n</summary>"
	e.replies[4] = reply
	if out, err := e.send(e.key, req...); err != nil || out != reply {
		t.Fatalf("post-tool auto-compact summary: %q %v", out, err)
	}
	// 2.1.280 auto-compact continuation: no claudeCompactionKept suffix on ns[0], but AuxRewound is true.
	first := wireBlocks("user", wireAttribution, wirePreface+clientSummary(reply)+strings.TrimSuffix(wirePointer, "\n"))
	cont := []map[string]interface{}{first, contextMsg(), wireBlocks("assistant", "tool turn finished"), wireBlocks("user", autoPending), contextMsg()}
	e.replies[5] = "Red"
	if out, err := e.send(e.key, cont...); err != nil || out != "Red" {
		t.Fatalf("post-tool auto-compact continuation: %q %v", out, err)
	}
	if e.boundThread(e.key) != main || e.bindings() != 1 {
		t.Fatal("binding moved")
	}
}

// Real 2.1.296 mid-tool-loop auto-compact: the client compacted after a tool
// result came back, so the kept tail is the bound thread's last reply (a tool
// call) followed by that call's result and reminder text. The result is new to
// the bound thread and is forwarded after the summary.
func TestAutoCompactKeptTailMidToolLoop(t *testing.T) {
	reply := ChatMessage{Role: "assistant", ToolCalls: []ToolCall{{ID: "call_1"}}}
	result := ChatMessage{Role: "tool", ToolCallID: "call_1", Name: "Read", Content: "1\tline one"}
	reminder := ChatMessage{Role: "user", Content: "<system-reminder>\nstate\n</system-reminder>"}
	kept, why := autoCompactKeptTail([]ChatMessage{reply, result, reminder})
	if why != "" || len(kept) != 2 || kept[0].ToolCallID != "call_1" {
		t.Fatalf("mid-tool-loop kept tail refused: %q %v", why, kept)
	}
	prompt := agentThreadCompactionPrompt(nil, claudeCompaction{Auto: true, Kept: true, Summary: "S", KeptNew: kept})
	for _, want := range []string{"CLIENT SUMMARY", "LOCAL TOOL RESULT (call_id=call_1, name=Read)", "line one", "NEW MESSAGES"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt lacks %q: %q", want, prompt)
		}
	}
	for name, rest := range map[string][]ChatMessage{
		"result for another call": {reply, {Role: "tool", ToolCallID: "call_2", Content: "x"}},
		"calls without a result":  {reply, reminder},
		"assistant after reply":   {reply, result, {Role: "assistant", Content: "x"}},
	} {
		if _, why := autoCompactKeptTail(rest); why == "" {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestAutoCompactTruncatedSummaryRequestRunsOnSideThreadAndRebasesBoundThread(t *testing.T) {
	for _, tc := range []struct {
		name     string
		dropHead bool
		keepTail bool
	}{
		{name: "truncated_tail_keeps_root", dropHead: false, keepTail: true},
		{name: "truncated_head_changes_root", dropHead: true, keepTail: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newCompactEnv(t)
			fx := autoFixture(t)
			for _, c := range []int{2, 5, 8, 11, 14, 17} {
				if _, err := e.send(e.key, fx[:c]...); err != nil {
					t.Fatalf("request ending at %d: %v", c, err)
				}
			}
			main := e.boundThread(e.key)
			before := e.entities(main)
			createsBefore, sendsBefore := e.f.counts()

			blocks := fx[18]["content"].([]interface{})
			instr := blocks[len(blocks)-1].(map[string]interface{})["text"].(string)
			lastMsg := wireBlocks("user", autoPending, instr)

			var req []map[string]interface{}
			if tc.dropHead {
				// Oldest turns dropped from the head (conv[1] != rec.RootHash, n < rec.ConvLen).
				req = append(append([]map[string]interface{}{}, fx[8:14]...), lastMsg, contextMsg())
			} else {
				// Root kept, middle turns dropped (conv[1] == rec.RootHash, n < rec.ConvLen).
				req = append(append([]map[string]interface{}{}, fx[:8]...), lastMsg, contextMsg())
			}

			reply := "<analysis>ok</analysis>\n\n<summary>\n" + summaryInner + "\n</summary>"
			e.replies[autoSummaryReplyNo] = reply
			out, err := e.send(e.key, req...)
			if err != nil || out != reply {
				t.Fatalf("truncated summarisation request failed: %q %v", out, err)
			}
			if got := e.entities(main); got != before {
				t.Fatalf("truncated summarisation reached bound thread: %d -> %d", before, got)
			}
			if c, s := e.f.counts(); c != createsBefore+1 || s != sendsBefore {
				t.Fatalf("creates %d -> %d, sends %d -> %d; want one side-thread create and no send", createsBefore, c, sendsBefore, s)
			}

			var cont []map[string]interface{}
			if tc.keepTail {
				kept := "\n\nThe messages after this summary are the most recent messages from before compaction, kept verbatim. The summary was written without seeing them, so something it says has not happened yet may already have happened in them."
				first := wireBlocks("user", wireAttribution, wirePreface+clientSummary(reply)+strings.TrimSuffix(wirePointer, "\n")+kept)
				cont = []map[string]interface{}{first, contextMsg(), wireBlocks("assistant", "391"), wireBlocks("user", autoPending), contextMsg()}
			} else {
				cont = postAutoCompact(clientSummary(reply))
			}

			c0, s0 := e.f.counts()
			e.replies[autoSummaryReplyNo+1] = "Red"
			out, err = e.send(e.key, cont...)
			if err != nil || out != "Red" {
				t.Fatalf("continuation after truncated summarisation failed: %q %v", out, err)
			}
			if c, s := e.f.counts(); c != c0 || s != s0+1 || e.boundThread(e.key) != main || e.bindings() != 1 {
				t.Fatalf("continuation must reuse bound thread: creates %d -> %d, sends %d -> %d", c0, c, s0, s)
			}
		})
	}
}

func TestCompactSummarisationTextOnlyFramingAndNoToolConversion(t *testing.T) {
	e := newCompactEnv(t)
	fx := autoFixture(t)
	for _, c := range []int{2, 5} {
		if _, err := e.send(e.key, fx[:c]...); err != nil {
			t.Fatalf("request ending at %d: %v", c, err)
		}
	}

	blocks := fx[18]["content"].([]interface{})
	instr := blocks[len(blocks)-1].(map[string]interface{})["text"].(string)
	pendingWithReminder := "<system-reminder>\nSKILL_INDEX_TO_STRIP\n</system-reminder>你开一个子 Agent 继续"
	lastMsg := wireBlocks("user", pendingWithReminder, instr)
	req := append(append([]map[string]interface{}{}, fx[:5]...), lastMsg, contextMsg())

	// Even if the model outputs a JSON tool-call object on a summarisation turn,
	// the Anthropic handler must return it as text (never converting it into a
	// tool_use block with empty text, which causes Claude Code to fail with
	// "summarization produced empty response").
	reply := `<analysis>ok</analysis>` + "\n" + `<summary>Summary text</summary>` + "\n" + `{"name":"Bash","arguments":{"command":"ls","description":"list"}}`
	e.replies[3] = reply
	out, err := e.send(e.key, req...)
	if err != nil || out != reply {
		t.Fatalf("expected full text reply on summarisation request, got %q (err=%v)", out, err)
	}

	prompt := agentThreadSummaryPrompt([]ChatMessage{
		{
			Role:          "user",
			Content:       pendingWithReminder + instr,
			TextBlocks:    2,
			LastTextBlock: instr,
		},
	})
	if strings.Contains(prompt, agentThreadPromptHead) || strings.Contains(prompt, agentThreadPromptTail) {
		t.Fatal("agentThreadSummaryPrompt must not include local tool action framing")
	}
	if !strings.Contains(prompt, agentThreadSummaryHead) || !strings.Contains(prompt, agentThreadSummaryTail) {
		t.Fatal("agentThreadSummaryPrompt must include text-only summary framing")
	}
	if strings.Contains(prompt, "SKILL_INDEX_TO_STRIP") {
		t.Fatal("agentThreadSummaryPrompt must strip <system-reminder> before appended summary instruction")
	}
	if !strings.Contains(prompt, "你开一个子 Agent 继续\n\n"+summaryInstructionHead) {
		t.Fatal("agentThreadSummaryPrompt must separate pending user text from summary instruction")
	}
}


