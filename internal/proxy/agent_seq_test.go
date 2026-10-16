package proxy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Deterministic, synthetic reproduction of the real failure: the live
// getThreadTranscript keeps a "status" entity with sequence 26.5, which an int
// field rejected (HTTP 502 on the second resumed turn).
const fractionalStatusFixture = `{"type":"agent_service_transcript_page","session":{"status":"idle","sequence":26},"patches":[{"op":"put","entity":{"id":"status-fixture","kind":"status","sequence":26.5}}]}`

func TestAgentTranscriptDecodesFractionalSequenceFixture(t *testing.T) {
	var page agentTranscriptPage
	if err := json.Unmarshal([]byte(fractionalStatusFixture), &page); err != nil {
		t.Fatalf("fractional sequence must decode: %v", err)
	}
	if got := page.Patches[0].Entity.Sequence; got != 26.5 {
		t.Fatalf("entity sequence = %v", got)
	}
	text, done, maxSeq, err := readAgentTranscriptSince(page, 0)
	if err != nil || text != "" || done || maxSeq != 26.5 {
		t.Fatalf("text=%q done=%v max=%v err=%v", text, done, maxSeq, err)
	}
}

func TestAgentSeqDecodeForms(t *testing.T) {
	ok := map[string]agentSeq{`26`: 26, `26.5`: 26.5, `0.25`: 0.25, `"26.5"`: 26.5, `"7"`: 7, `null`: 0, `1e2`: 100}
	for in, want := range ok {
		var got agentSeq
		if err := json.Unmarshal([]byte(in), &got); err != nil || got != want {
			t.Fatalf("%s -> %v, %v (want %v)", in, got, err, want)
		}
	}
	for _, bad := range []string{`"abc"`, `{}`, `[1]`, `true`, `"NaN"`} {
		var got agentSeq
		if err := json.Unmarshal([]byte(bad), &got); err == nil {
			t.Fatalf("%s must be rejected, got %v", bad, got)
		}
	}
	// A bad sequence inside a page fails loudly instead of being dropped.
	var page agentTranscriptPage
	raw := `{"type":"agent_service_transcript_page","patches":[{"op":"put","entity":{"id":"x","kind":"status","sequence":{}}}]}`
	if err := json.Unmarshal([]byte(raw), &page); err == nil {
		t.Fatal("object sequence must be rejected")
	}
}

func TestAgentTranscriptFractionalOrderingAndReplyFilter(t *testing.T) {
	raw := `{"type":"agent_service_transcript_page","has_more_backward":true,"session":{"status":"idle","sequence":28},"patches":[
	{"op":"put","entity":{"id":"old","kind":"assistant_message","sequence":26,"content":[{"type":"text","text":"OLD"}]}},
	{"op":"put","entity":{"id":"st","kind":"status","sequence":26.5}},
	{"op":"put","entity":{"id":"b","kind":"assistant_message","sequence":27.5,"content":[{"type":"text","text":"second"}]}},
	{"op":"put","entity":{"id":"a","kind":"assistant_message","sequence":27.25,"content":[{"type":"text","text":"first"}]}},
	{"op":"put","entity":{"id":"mid","kind":"assistant_message","sequence":27,"content":[{"type":"text","text":"zeroth"}]}},
	{"op":"put","entity":{"id":"done","kind":"turn_completed","sequence":28,"stop_reason":"end_turn"}}]}`
	var page agentTranscriptPage
	if err := json.Unmarshal([]byte(raw), &page); err != nil {
		t.Fatal(err)
	}
	// Baseline 26.5 (a prior status entity) drops 26 and 26.5 but keeps 27+.
	text, done, maxSeq, err := readAgentTranscriptSince(page, 26.5)
	if err != nil || !done || maxSeq != 28 || text != "zeroth\nfirst\nsecond" {
		t.Fatalf("text=%q done=%v max=%v err=%v", text, done, maxSeq, err)
	}
	// A fractional baseline inside the new turn is honoured exactly (<=).
	text, _, _, err = readAgentTranscriptSince(page, 27.25)
	if err != nil || text != "second" {
		t.Fatalf("baseline 27.25 -> %q %v", text, err)
	}
	// A baseline just below an entity must include it (no int truncation).
	text, _, _, err = readAgentTranscriptSince(page, 27.24)
	if err != nil || text != "first\nsecond" {
		t.Fatalf("baseline 27.24 -> %q %v", text, err)
	}
}

func TestAgentThreadFractionalBaselinePersistsAndLegacyIntStateLoads(t *testing.T) {
	f := newFakeAgentServer(t)
	path := filepath.Join(t.TempDir(), "bindings.state")
	store := newAgentThreadStore(path)
	acc := &Account{UserID: "user", SpaceID: "space"}
	if out, err := runTurn(t, f, store, acc, "", []ChatMessage{u("one")}); err != nil || out != "reply-1" {
		t.Fatalf("turn 1: %q %v", out, err)
	}
	if out, err := runTurn(t, f, store, acc, "", []ChatMessage{u("one"), a("x"), u("two")}); err != nil || out != "reply-2" {
		t.Fatalf("turn 2: %q %v", out, err)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), `"last_seq": 6.5`) && !strings.Contains(string(raw), `"last_seq":6.5`) {
		t.Fatalf("fractional last_seq not persisted: %s", raw)
	}
	reloaded := newAgentThreadStore(path)
	if len(reloaded.records) != 1 {
		t.Fatalf("records after restart: %d", len(reloaded.records))
	}
	for _, rec := range reloaded.records {
		if rec.LastSeq != 6.5 || rec.Baseline != 3.5 {
			t.Fatalf("restored baseline=%v last=%v", rec.Baseline, rec.LastSeq)
		}
	}
	if out, err := runTurn(t, f, reloaded, acc, "", []ChatMessage{u("one"), a("x"), u("two"), a("y"), u("three")}); err != nil || out != "reply-3" {
		t.Fatalf("turn 3 after restart: %q %v", out, err)
	}

	// State files written by the previous (integer) format still load.
	legacy := `[{"account_id":"a","space_id":"s","thread_id":"t1","prefix_len":1,"prefix_hash":"h","conv_len":1,"conv_hash":"c","baseline":4,"last_seq":9,"state":"completed","created_at":"2026-01-01T00:00:00Z","updated_at":"2099-01-01T00:00:00Z"}]`
	legacyPath := filepath.Join(t.TempDir(), "legacy.state")
	if err := os.WriteFile(legacyPath, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newAgentThreadStore(legacyPath)
	if rec := s.records["t1"]; rec == nil || rec.Baseline != 4 || rec.LastSeq != 9 {
		t.Fatalf("legacy integer state not loaded: %#v", rec)
	}
}

// A bookkeeping status entity newer than the baseline must not be mistaken for
// proof that an unacknowledged follow-up was already accepted.
func TestAgentThreadRetryNotSuppressedByStatusEntity(t *testing.T) {
	f := newFakeAgentServer(t)
	store := newAgentThreadStore("")
	acc := &Account{UserID: "user", SpaceID: "space"}
	if _, err := runTurn(t, f, store, acc, "", []ChatMessage{u("one")}); err != nil {
		t.Fatal(err)
	}
	// A late status entity (sequence above the stored baseline) shows up.
	f.mu.Lock()
	for _, th := range f.threads {
		th.entities = append(th.entities, map[string]interface{}{"id": "late-status", "kind": "status", "sequence": float64(th.seq) + 0.75})
	}
	f.mu.Unlock()
	follow := []ChatMessage{u("one"), a("x"), u("two")}
	f.rejectSend = 500 // ambiguous failure: acknowledgement unknown
	if _, err := runTurn(t, f, store, acc, "", follow); err == nil {
		t.Fatal("expected the 5xx to surface")
	}
	f.rejectSend = 0
	out, err := runTurn(t, f, store, acc, "", follow)
	if err != nil || out != "reply-2" {
		t.Fatalf("retry must submit and read the new reply: %q %v", out, err)
	}
	if _, s := f.counts(); s != 1 {
		t.Fatalf("exactly one follow-up must be submitted, got %d", s)
	}
}
