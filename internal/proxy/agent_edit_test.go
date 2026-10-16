package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// --- merged from agent_edit_test.go ---
func TestAgentInterruptedEditKeepsThreadAndSkipsOldReply(t *testing.T) {
	f, _, acc := bindingSetup(t)
	store := newAgentThreadStore(filepath.Join(t.TempDir(), "binding.state"))
	f.stall = true
	gone := make(chan struct{})
	ended := make(chan error, 1)
	go func() {
		ended <- callAgentThreadResumableCancel(gone, acc, []ChatMessage{u("typo")}, "", "low", "edit-client", false, func(string, bool, *UsageInfo) { t.Error("old reply delivered") }, f.server.Client(), store)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		c, _ := f.counts()
		if c == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("not submitted")
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(gone)
	select {
	case <-ended:
	case <-time.After(2 * time.Second):
		t.Fatal("disconnect did not release thread")
	}
	f.mu.Lock()
	f.stall = false
	for _, th := range f.threads {
		f.finishLocked(th)
		for _, e := range th.entities {
			if e["kind"] == "assistant_message" {
				e["content"] = []map[string]string{{"type": "text", "text": `{"name":"Write","arguments":{"file_path":"old.txt","content":"STALE"}}`}}
			}
		}
	}
	f.mu.Unlock()
	store = newAgentThreadStore(store.path)
	out, err := runKeyed(t, f, store, acc, "edit-client", "low", false, []ChatMessage{u("corrected")})
	if err != nil || out != "reply-2" {
		t.Fatalf("corrected reply=%q err=%v", out, err)
	}

	store = newAgentThreadStore(store.path)
	replay, err := runKeyed(t, f, store, acc, "edit-client", "low", false, []ChatMessage{u("corrected")})
	if err != nil || replay != out {
		t.Fatalf("corrected replay=%q %v", replay, err)
	}
	if strings.Contains(out, "STALE") {
		t.Fatal("old tool reply leaked")
	}
	if c, s := f.counts(); c != 1 || s != 1 {
		t.Fatalf("forked or duplicated: create=%d send=%d", c, s)
	}
	out, err = runKeyed(t, f, store, acc, "edit-client", "low", false, []ChatMessage{u("corrected"), a(out), u("next")})
	if err != nil || out != "reply-3" {
		t.Fatalf("continuation=%q %v", out, err)
	}
}

func TestAgentInterruptedEditWaitsForOldTurn(t *testing.T) {
	f, store, acc := bindingSetup(t)
	f.stall = true
	gone := make(chan struct{})
	ended := make(chan error, 1)
	go func() {
		ended <- callAgentThreadResumableCancel(gone, acc, []ChatMessage{u("old")}, "", "low", "wait-edit", false, func(string, bool, *UsageInfo) { t.Error("old reply delivered") }, f.server.Client(), store)
	}()
	deadline := time.Now().Add(time.Second)
	for {
		c, _ := f.counts()
		if c == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("not submitted")
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(gone)
	<-ended
	result := make(chan string, 1)
	errs := make(chan error, 1)
	go func() {
		out, err := runKeyed(t, f, store, acc, "wait-edit", "low", false, []ChatMessage{u("new")})
		result <- out
		errs <- err
	}()
	time.Sleep(50 * time.Millisecond)
	if _, s := f.counts(); s != 0 {
		t.Fatal("submitted replacement while old turn running")
	}
	select {
	case <-result:
		t.Fatal("returned before old turn settled")
	default:
	}
	f.mu.Lock()
	f.stall = false
	for _, th := range f.threads {
		f.finishLocked(th)
	}
	f.mu.Unlock()
	select {
	case out := <-result:
		if err := <-errs; err != nil || out != "reply-2" {
			t.Fatalf("%q %v", out, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("replacement did not finish")
	}
	if c, s := f.counts(); c != 1 || s != 1 {
		t.Fatalf("counts %d %d", c, s)
	}
}

func TestAgentEditNeedsDisconnectAndUnchangedPrefix(t *testing.T) {
	f, store, acc := bindingSetup(t)
	first := []ChatMessage{u("root")}
	runKeyed(t, f, store, acc, "guard", "low", false, first)
	next := []ChatMessage{u("root"), a("reply-1"), u("old")}
	runKeyed(t, f, store, acc, "guard", "low", false, next)
	changed := []ChatMessage{u("root"), a("reply-1"), u("new")}
	if _, err := runKeyed(t, f, store, acc, "guard", "low", false, changed); err == nil {
		t.Fatal("edit without disconnect accepted")
	}
	for _, rec := range store.records {
		rec.ClientInterrupted = true
	}
	wrongPrefix := []ChatMessage{u("edited root"), a("reply-1"), u("new")}
	if _, err := runKeyed(t, f, store, acc, "guard", "low", false, wrongPrefix); err == nil {
		t.Fatal("changed earlier history accepted")
	}
	out, err := runKeyed(t, f, store, acc, "guard", "low", false, changed)
	if err != nil || out != "reply-3" {
		t.Fatalf("completed old turn correction: %q %v", out, err)
	}
}

func TestAgentEditHTTPDisconnectBeforeFirstPing(t *testing.T) {
	for _, tc := range []struct {
		name          string
		stream, tools bool
	}{{"stream-text", true, false}, {"stream-tools", true, true}, {"nonstream", false, false}} {
		t.Run(tc.name, func(t *testing.T) {
			env := newRecoveryEnv(t)
			env.f.stall = true
			store := useRecoveryStore(t, "")
			setKeepalive(t, 5*time.Second, 10*time.Second)
			finished := make(chan struct{})
			handler := recoveryHandler(recAcc(), []ChatMessage{u("old")}, "auto", tc.tools, tc.stream)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(finished)
				io.Copy(io.Discard, r.Body)
				handler.ServeHTTP(w, r)
			}))
			defer srv.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, "POST", srv.URL, strings.NewReader("{}"))
			clientDone := make(chan struct{})
			go func() {
				defer close(clientDone)
				resp, _ := srv.Client().Do(req)
				if resp != nil {
					resp.Body.Close()
				}
			}()
			deadline := time.Now().Add(time.Second)
			for {
				c, _ := env.f.counts()
				if c == 1 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("not submitted")
				}
				time.Sleep(5 * time.Millisecond)
			}
			cancel()
			select {
			case <-finished:
			case <-time.After(time.Second):
				t.Fatal("socket cancellation waited for first SSE ping")
			}
			<-clientDone
			store.mu.Lock()
			defer store.mu.Unlock()
			for _, rec := range store.records {
				if !rec.ClientInterrupted || rec.EditPrefixHash == "" {
					t.Fatal("interruption not recorded")
				}
			}
		})
	}
}

// --- merged from agent_edit_lifecycle_test.go ---
// Lifecycle regressions for the interrupted-edit permission and the turn
// counter: a successful unchanged recovery consumes the Ctrl+C permission, and
// replaying an already completed turn never counts it twice.

func lifecycleRecord(store *agentThreadStore, acc *Account, key string) agentThreadRecord {
	return *store.primaryLocked(agentPlanInput{ClientKey: key, AccountID: agentAccountKey(acc), SpaceID: "space"})
}

func correctionSendCount(f *fakeAgentServer) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, s := range f.sends {
		if ev, ok := s["event"].(map[string]interface{}); ok && strings.Contains(promptText(ev["content"]), agentCorrectionHead) {
			n++
		}
	}
	return n
}

// finishOriginal lets the stalled original turn complete with its plain reply.
func finishOriginal(f *fakeAgentServer) {
	f.mu.Lock()
	f.stall = false
	for _, th := range f.threads {
		f.finishLocked(th)
	}
	f.mu.Unlock()
}

// (1) Ctrl+C, unchanged retry succeeds, then the text changes without a new
// Ctrl+C: this must not be treated as an interrupted edit.
func TestAgentSuccessfulUnchangedRecoveryConsumesEditPermission(t *testing.T) {
	f, store, acc := bindingSetup(t)
	setStall(f, true)
	gone, ended := startInterruptible(t, f, store, acc, "life-consume", []ChatMessage{u("old")})
	waitForCounts(t, f, 1, 0)
	endRequest(t, gone, ended)
	finishOriginal(f)

	out, err := runKeyed(t, f, store, acc, "life-consume", "low", false, []ChatMessage{u("old")})
	if err != nil || out != "reply-1" {
		t.Fatalf("unchanged recovery: %q %v", out, err)
	}
	if rec := lifecycleRecord(store, acc, "life-consume"); rec.ClientInterrupted {
		t.Fatalf("ClientInterrupted still set after the original reply was delivered")
	}
	out, err = runKeyed(t, f, store, acc, "life-consume", "low", false, []ChatMessage{u("new")})
	if err == nil {
		t.Fatalf("edit without a new Ctrl+C was accepted as an interrupted edit: %q", out)
	}
	if n := correctionSendCount(f); n != 0 {
		t.Fatalf("%d correction sends issued without a new interrupt", n)
	}
	if _, s := f.counts(); s != 0 {
		t.Fatalf("sends=%d, want 0", s)
	}
}

// (2) Replaying a completed ordinary turn must not grow Turns.
func TestAgentReplayCompletedTurnDoesNotGrowTurns(t *testing.T) {
	f, store, acc := bindingSetup(t)
	msgs := []ChatMessage{u("hello")}
	out, err := runKeyed(t, f, store, acc, "life-replay", "low", false, msgs)
	if err != nil || out != "reply-1" {
		t.Fatalf("first: %q %v", out, err)
	}
	first := lifecycleRecord(store, acc, "life-replay")
	if first.Turns != 1 {
		t.Fatalf("turns after first completion = %d, want 1", first.Turns)
	}
	c0, s0 := f.counts()
	out, err = runKeyed(t, f, store, acc, "life-replay", "low", false, msgs)
	if err != nil || out != "reply-1" {
		t.Fatalf("replay: %q %v", out, err)
	}
	after := lifecycleRecord(store, acc, "life-replay")
	if after.Turns != first.Turns || after.ClientMessageID != first.ClientMessageID {
		t.Fatalf("replay changed record: turns %d->%d clientMessageID %q->%q", first.Turns, after.Turns, first.ClientMessageID, after.ClientMessageID)
	}
	if c, s := f.counts(); c != c0 || s != s0 {
		t.Fatalf("replay sent again: creates %d->%d sends %d->%d", c0, c, s0, s)
	}
}

// (3) Replaying a completed corrected turn keeps Turns and CorrectionCount.
func TestAgentReplayCompletedCorrectedTurnDoesNotGrowTurns(t *testing.T) {
	f, store, acc := bindingSetup(t)
	setStall(f, true)
	gone, ended := startInterruptible(t, f, store, acc, "life-corrected", []ChatMessage{u("typo")})
	waitForCounts(t, f, 1, 0)
	endRequest(t, gone, ended)
	finishStale(f)

	edit := []ChatMessage{u("fixed")}
	out, err := runKeyed(t, f, store, acc, "life-corrected", "low", false, edit)
	if err != nil || out != "reply-2" {
		t.Fatalf("corrected: %q %v", out, err)
	}
	first := lifecycleRecord(store, acc, "life-corrected")
	if first.CorrectionCount != 1 {
		t.Fatalf("correction count = %d, want 1", first.CorrectionCount)
	}
	c0, s0 := f.counts()
	out, err = runKeyed(t, f, store, acc, "life-corrected", "low", false, edit)
	if err != nil || out != "reply-2" {
		t.Fatalf("replay: %q %v", out, err)
	}
	after := lifecycleRecord(store, acc, "life-corrected")
	if after.Turns != first.Turns || after.CorrectionCount != first.CorrectionCount || after.ClientMessageID != first.ClientMessageID {
		t.Fatalf("replay changed record: turns %d->%d corrections %d->%d clientMessageID %q->%q", first.Turns, after.Turns, first.CorrectionCount, after.CorrectionCount, first.ClientMessageID, after.ClientMessageID)
	}
	if c, s := f.counts(); c != c0 || s != s0 {
		t.Fatalf("replay sent again: creates %d->%d sends %d->%d", c0, c, s0, s)
	}
}

// (4) After a successful recovery, a genuinely new Ctrl+C is still honoured.
func TestAgentEditAfterRecoveryStillWorksOnNewInterrupt(t *testing.T) {
	f, store, acc := bindingSetup(t)
	setStall(f, true)
	gone, ended := startInterruptible(t, f, store, acc, "life-again", []ChatMessage{u("old")})
	waitForCounts(t, f, 1, 0)
	endRequest(t, gone, ended)
	finishOriginal(f)
	out, err := runKeyed(t, f, store, acc, "life-again", "low", false, []ChatMessage{u("old")})
	if err != nil || out != "reply-1" {
		t.Fatalf("unchanged recovery: %q %v", out, err)
	}

	setStall(f, true)
	next := []ChatMessage{u("old"), a("reply-1"), u("typo2")}
	gone, ended = startInterruptible(t, f, store, acc, "life-again", next)
	waitForCounts(t, f, 1, 1)
	endRequest(t, gone, ended)
	finishStale(f)

	out, err = runKeyed(t, f, store, acc, "life-again", "low", false, []ChatMessage{u("old"), a("reply-1"), u("fixed2")})
	if err != nil {
		t.Fatalf("edit after a new Ctrl+C was rejected: %v", err)
	}
	if strings.Contains(out, "STALE") || strings.TrimSpace(out) == "" {
		t.Fatalf("unexpected reply %q", out)
	}
	if n := correctionSendCount(f); n != 1 {
		t.Fatalf("correction sends=%d, want 1", n)
	}
	if c, s := f.counts(); c != 1 || s != 2 {
		t.Fatalf("creates=%d sends=%d, want 1/2", c, s)
	}
}

// --- merged from agent_edit_upload_test.go ---
// Regressions for who may hold the Ctrl+C edit permission when an image upload
// is involved: a cancelled upload must never grant an edit to the turn that was
// already delivered, and a binding first built from a full history that carries
// images must not guess where those images belong.

// (1) u1 is delivered, u2 (with an image) is cancelled while its upload is in
// flight. The failed upload rolls the record back to u1. u1 was never cancelled,
// so it must not become editable: an edited u1 is rejected and nothing is sent.
func TestAgentCancelledUploadCannotGrantPriorTurnEdit(t *testing.T) {
	f, store, acc := bindingSetup(t)
	const key = "upload-cancel"
	var block atomic.Bool
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	defer close(release)
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if block.Load() && strings.Contains(r.URL.Path, "AgentServiceFileUpload") {
			select {
			case entered <- struct{}{}:
			default:
			}
			select {
			case <-release:
			case <-r.Context().Done():
			}
			return
		}
		f.handle(w, r)
	})

	out, err := runKeyed(t, f, store, acc, key, "low", false, []ChatMessage{u("u1")})
	if err != nil || out != "reply-1" {
		t.Fatalf("first turn: %q %v", out, err)
	}
	before := lifecycleRecord(store, acc, key)

	block.Store(true)
	gone, ended := startInterruptible(t, f, store, acc, key, []ChatMessage{u("u1"), a(out), u("u2")}, rawImage("u2.png"))
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the image upload was never started")
	}
	endRequest(t, gone, ended)

	after := lifecycleRecord(store, acc, key)
	if after.ClientMessageID != before.ClientMessageID {
		t.Fatalf("rollback did not restore the delivered turn: clientMessageID %q -> %q", before.ClientMessageID, after.ClientMessageID)
	}
	if after.ClientInterrupted {
		t.Fatalf("cancelled upload marked the already delivered u1 as interrupted")
	}

	block.Store(false)
	out, err = runKeyed(t, f, store, acc, key, "low", false, []ChatMessage{u("edited u1")})
	if err == nil {
		t.Fatalf("already delivered u1 was accepted as an interrupted edit: %q", out)
	}
	if n := correctionSendCount(f); n != 0 {
		t.Fatalf("%d correction sends issued for a turn that was never cancelled", n)
	}
	if c, s := f.counts(); c != 1 || s != 0 {
		t.Fatalf("creates=%d sends=%d, want 1/0 (no fork, nothing sent)", c, s)
	}
}

// interruptImageThread creates a thread whose first request is interrupted
// while the model is still running, then lets the stale turn finish.
func interruptImageThread(t *testing.T, key string, msgs []ChatMessage) (*fakeAgentServer, *agentThreadStore, *Account) {
	t.Helper()
	f := newFakeAgentServer(t)
	wireImageUploads(t, f)
	store := newAgentThreadStore(filepath.Join(t.TempDir(), key+".state"))
	acc := &Account{AccountID: "account", SpaceID: "space", TokenV2: "test-cookie"}
	setStall(f, true)
	gone, ended := startInterruptible(t, f, store, acc, key, msgs, rawImage("shot.png"))
	waitForCounts(t, f, 1, 0)
	endRequest(t, gone, ended)
	finishStale(f)
	return f, store, acc
}

func runImageEdit(f *fakeAgentServer, store *agentThreadStore, acc *Account, key string, msgs []ChatMessage) (string, error) {
	var out strings.Builder
	err := callAgentThreadResumable(acc, msgs, "", "low", key, false, func(s string, _ bool, _ *UsageInfo) { out.WriteString(s) }, f.server.Client(), store, rawImage("shot.png"))
	return out.String(), err
}

// (2) A binding first built from a history with more than one non-system
// message and an attachment cannot tell where the image belongs, so it records
// no editable prefix: the Ctrl+C edit is refused, no fork, nothing sent.
func TestAgentFirstBuildFromHistoryWithImageRefusesEdit(t *testing.T) {
	const key = "hist-image"
	f, store, acc := interruptImageThread(t, key, []ChatMessage{u("root"), a("earlier"), u("describe this imagE")})
	if rec := lifecycleRecord(store, acc, key); rec.EditPrefixHash != "" {
		t.Fatalf("editable prefix recorded for an unprovable history: len=%d attachStart=%d", rec.EditPrefixLen, rec.EditAttachStart)
	}
	out, err := runImageEdit(f, store, acc, key, []ChatMessage{u("root"), a("earlier"), u("describe this image")})
	if err == nil {
		t.Fatalf("edit of a history-built binding with images was accepted: %q", out)
	}
	if n := correctionSendCount(f); n != 0 {
		t.Fatalf("%d correction sends issued", n)
	}
	if c, s := f.counts(); c != 1 || s != 0 {
		t.Fatalf("creates=%d sends=%d, want 1/0 (no fork, nothing sent)", c, s)
	}
}

// (3) A first request with exactly one user message plus an image is still
// editable: the image provably belongs to that message (attachment start 0).
func TestAgentFirstSingleUserImageStillAllowsEdit(t *testing.T) {
	const key = "single-image"
	f, store, acc := interruptImageThread(t, key, []ChatMessage{u("describe this imagE")})
	out, err := runImageEdit(f, store, acc, key, []ChatMessage{u("describe this image")})
	if err != nil {
		t.Fatalf("edit of a single-message image request was rejected: %v", err)
	}
	if strings.Contains(out, "STALE") || strings.TrimSpace(out) == "" {
		t.Fatalf("unexpected reply %q", out)
	}
	if c, s := f.counts(); c != 1 || s != 1 {
		t.Fatalf("creates=%d sends=%d, want 1/1", c, s)
	}
	if n := fileBlocks(f.sends[0]); n != 1 {
		t.Fatalf("corrected send has %d image blocks, want 1", n)
	}
}
