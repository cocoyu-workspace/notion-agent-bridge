package proxy

import (
	"errors"
	"strings"
	"testing"
)

const diagSecret = "PRIVATE-PROMPT-TEXT-9d1f"

func TestAgentRequestShapeCountsRiskyCharactersWithoutText(t *testing.T) {
	prompt := "head\nLOCAL TOOL RESULT (call_id=c1, name=Read)\n" + diagSecret + "\x00\x01\u2023😀\xff\nLOCAL TOOL REQUESTS\n[]\n"
	body := agentThreadFollowUpBody(&Account{SpaceID: "space"}, prompt, "", "low", "thread", "event", nil)
	data := mustJSON(t, body)
	shape := describeAgentRequestShape("sendEventToAgentThread", data)
	if shape.ToolResults != 1 || shape.ToolRequests != 1 || shape.CtrlChars != 1 || shape.RichMarker != 1 || shape.NonBMP != 1 || !shape.FollowUp || shape.Effort != "low" {
		t.Fatalf("unexpected shape: %+v", shape)
	}
	// json.Marshal rewrites the invalid byte to U+FFFD and escapes NUL as \u0000;
	// both survive the round trip as characters the shape counts. NUL is counted
	// as nul (not ctrl) by design, so ctrl=1 above is the \x01 only.
	if shape.NulChars != 1 || shape.Replacement != 1 || shape.BadUTF8 != 0 {
		t.Fatalf("unexpected nul/bad utf8 counts: %+v", shape)
	}
	if strings.Contains(shape.String(), diagSecret) {
		t.Fatal("shape must never contain prompt text")
	}
}

func TestAgentRejectedFollowUpReportsShapeNotContent(t *testing.T) {
	f := newFakeAgentServer(t)
	store := newAgentThreadStore("")
	acc := &Account{UserID: "user", SpaceID: "space"}
	if _, err := runKeyed(t, f, store, acc, "k", "low", true, []ChatMessage{u("first")}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.rejectSend, f.rejectBody = 400, `{"name":"UserValidationError","message":"`+diagSecret+`","debugMessage":"x"}`
	f.mu.Unlock()
	msgs := []ChatMessage{u("first"), {Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Type: "function", Function: ToolCallFunction{Name: "Read", Arguments: `{"file_path":"/x"}`}}}},
		{Role: "tool", ToolCallID: "c1", Name: "Read", Content: diagSecret + "\x00 body"}}
	_, err := runKeyed(t, f, store, acc, "k", "low", true, msgs)
	if err == nil {
		t.Fatal("expected the rejection")
	}
	var bind *agentBindingError
	var httpErr *agentHTTPError
	if !errors.As(err, &bind) || !errors.As(err, &httpErr) {
		t.Fatalf("want binding rejection wrapping the HTTP error, got %v", err)
	}
	if httpErr.Code != "UserValidationError" || httpErr.BodyKeys != "debugMessage,message,name" {
		t.Fatalf("code=%q keys=%q", httpErr.Code, httpErr.BodyKeys)
	}
	msg := classifyAgentFailure(err).Message
	for _, want := range []string{"HTTP 400", "UserValidationError", "request_shape:", "tool_results=1", "nul=1", "follow_up=true", "endpoint=sendEventToAgentThread"} {
		if !strings.Contains(msg, want) && !strings.Contains(err.Error(), want) {
			t.Fatalf("diagnostic %q missing from %q", want, msg)
		}
	}
	for _, text := range []string{msg, err.Error(), httpErr.Shape, httpErr.BodyKeys} {
		if strings.Contains(text, diagSecret) {
			t.Fatalf("diagnostic leaked private content: %q", text)
		}
	}
	if c, s := f.counts(); c != 1 || s != 0 {
		t.Fatalf("a rejection must not fork or resend: creates=%d sends=%d", c, s)
	}
}

func TestAgentRepeated500KeepsBindingAndReportsShape(t *testing.T) {
	f := newFakeAgentServer(t)
	store := newAgentThreadStore("")
	acc := &Account{UserID: "user", SpaceID: "space"}
	if _, err := runKeyed(t, f, store, acc, "k", "low", true, []ChatMessage{u("first")}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.rejectSend = 500
	f.mu.Unlock()
	msgs := []ChatMessage{u("first"), a("reply-1"), u("again " + diagSecret)}
	for i := 0; i < 3; i++ {
		_, err := runKeyed(t, f, store, acc, "k", "low", true, msgs)
		var httpErr *agentHTTPError
		if err == nil || !errors.As(err, &httpErr) || httpErr.Status != 500 || !strings.Contains(httpErr.Shape, "follow_up=true") {
			t.Fatalf("attempt %d: want HTTP 500 with request shape, got %v", i, err)
		}
		if f2 := classifyAgentFailure(err); !f2.Pending || strings.Contains(err.Error(), diagSecret) {
			t.Fatalf("attempt %d: a 500 is an unconfirmed acceptance (pending), never content-bearing: %+v", i, f2)
		}
	}
	if c, s := f.counts(); c != 1 || s != 0 || threadCount(f) != 1 {
		t.Fatalf("500s must never create another thread: creates=%d sends=%d threads=%d", c, s, threadCount(f))
	}
}
