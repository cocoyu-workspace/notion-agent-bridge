package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Regression tests for the interrupted-request (Ctrl+C) edit flow beyond the
// basic happy path: attachments, lost acknowledgements, repeated interrupts and
// interrupting the corrected turn itself.

const staleToolReply = `{"name":"Write","arguments":{"file_path":"old.txt","content":"STALE"}}`

// startInterruptible runs a request that can be cancelled like a dropped client.
func startInterruptible(t *testing.T, f *fakeAgentServer, store *agentThreadStore, acc *Account, key string, msgs []ChatMessage, images ...UploadedAttachment) (chan struct{}, chan error) {
	t.Helper()
	gone := make(chan struct{})
	ended := make(chan error, 1)
	go func() {
		ended <- callAgentThreadResumableCancel(gone, acc, msgs, "", "low", key, false, func(string, bool, *UsageInfo) {
			t.Error("interrupted request delivered a reply")
		}, f.server.Client(), store, images...)
	}()
	return gone, ended
}

func waitForCounts(t *testing.T, f *fakeAgentServer, creates, sends int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		c, s := f.counts()
		if c == creates && s == sends {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("counts creates=%d sends=%d, want %d/%d", c, s, creates, sends)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func endRequest(t *testing.T, gone chan struct{}, ended chan error) error {
	t.Helper()
	close(gone)
	select {
	case err := <-ended:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("disconnect did not release the request")
	}
	return nil
}

func setStall(f *fakeAgentServer, v bool) {
	f.mu.Lock()
	f.stall = v
	f.mu.Unlock()
}

// finishStale completes every running turn. The reply it produces is a tool
// request that must never reach the client of an edited request.
func finishStale(f *fakeAgentServer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stall = false
	for _, th := range f.threads {
		if th.status != "running" {
			continue
		}
		f.finishLocked(th)
		for i := len(th.entities) - 1; i >= 0; i-- {
			if th.entities[i]["kind"] == "assistant_message" {
				th.entities[i]["content"] = []map[string]string{{"type": "text", "text": staleToolReply}}
				break
			}
		}
	}
}

func reopenStore(store *agentThreadStore) *agentThreadStore { return newAgentThreadStore(store.path) }

// imageUploadFake models the service's file ownership check: a follow-up may
// only reference files uploaded to that exact thread.
type imageUploadFake struct {
	mu      sync.Mutex
	uploads int
	targets map[string]map[string]interface{}
}

func (m *imageUploadFake) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.uploads
}

func wireImageUploads(t *testing.T, f *fakeAgentServer) *imageUploadFake {
	t.Helper()
	storage := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
	}))
	t.Cleanup(storage.Close)
	oldTransport := http.DefaultTransport
	http.DefaultTransport = storage.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = oldTransport })
	m := &imageUploadFake{targets: map[string]map[string]interface{}{}}
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "AgentServiceFileUpload") {
			var b map[string]interface{}
			json.NewDecoder(r.Body).Decode(&b)
			m.mu.Lock()
			defer m.mu.Unlock()
			if strings.HasSuffix(r.URL.Path, "createAgentServiceFileUploadURL") {
				m.uploads++
				id := fmt.Sprintf("file-%d", m.uploads)
				m.targets[id], _ = b["target"].(map[string]interface{})
				json.NewEncoder(w).Encode(map[string]interface{}{"type": "file_upload_url", "file": map[string]string{"id": id}, "upload": map[string]interface{}{"type": "single_part", "method": "PUT", "url": storage.URL, "headers": map[string]string{}}})
			} else {
				id, _ := b["fileId"].(string)
				json.NewEncoder(w).Encode(map[string]string{"type": "file", "id": id})
			}
			return
		}
		if strings.HasSuffix(r.URL.Path, "sendEventToAgentThread") {
			data, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(strings.NewReader(string(data)))
			var b map[string]interface{}
			json.Unmarshal(data, &b)
			for _, v := range b["event"].(map[string]interface{})["content"].([]interface{}) {
				c := v.(map[string]interface{})
				if c["type"] != "file" {
					continue
				}
				m.mu.Lock()
				target := m.targets[fmt.Sprint(c["file_id"])]
				m.mu.Unlock()
				if target["type"] != "thread" || target["threadId"] != b["threadId"] {
					w.WriteHeader(400)
					io.WriteString(w, `{"name":"ValidationError","debugMessage":"file not found"}`)
					return
				}
			}
		}
		f.handle(w, r)
	})
	return m
}

func rawImage(name string) UploadedAttachment {
	return UploadedAttachment{agentImage: &FileAttachment{FileName: name, ContentType: "image/png", Data: []byte("fixture")}}
}

func fileBlocks(send map[string]interface{}) int {
	n := 0
	for _, v := range send["event"].(map[string]interface{})["content"].([]interface{}) {
		if v.(map[string]interface{})["type"] == "file" {
			n++
		}
	}
	return n
}

// Round 2 carries an image and is interrupted. The corrected request resends
// the same image (same attachment count) with only the last character of the
// text changed: same thread, the old tool request never reaches the client.
func TestAgentInterruptedEditSameImageSameCount(t *testing.T) {
	f := newFakeAgentServer(t)
	up := wireImageUploads(t, f)
	store := newAgentThreadStore(filepath.Join(t.TempDir(), "img-edit.state"))
	acc := &Account{AccountID: "account", SpaceID: "space", TokenV2: "test-cookie"}
	const key = "img-edit"
	run := func(msgs []ChatMessage, images ...UploadedAttachment) (string, error) {
		var out strings.Builder
		err := callAgentThreadResumable(acc, msgs, "", "low", key, false, func(s string, _ bool, _ *UsageInfo) { out.WriteString(s) }, f.server.Client(), store, images...)
		return out.String(), err
	}
	if out, err := run([]ChatMessage{u("root")}); err != nil || out != "reply-1" {
		t.Fatalf("round 1: %q %v", out, err)
	}
	setStall(f, true)
	gone, ended := startInterruptible(t, f, store, acc, key, []ChatMessage{u("root"), a("reply-1"), u("describe this imagE")}, rawImage("shot.png"))
	waitForCounts(t, f, 1, 1)
	endRequest(t, gone, ended)
	finishStale(f)

	store = reopenStore(store)
	edited := []ChatMessage{u("root"), a("reply-1"), u("describe this image")}
	out, err := run(edited, rawImage("shot.png"))
	if err != nil || out != "reply-3" {
		t.Fatalf("edited image request: %q %v", out, err)
	}
	if strings.Contains(out, "STALE") {
		t.Fatal("old tool request leaked")
	}
	if c, s := f.counts(); c != 1 || s != 2 {
		t.Fatalf("forked or duplicated: creates=%d sends=%d", c, s)
	}
	if n := fileBlocks(f.sends[1]); n != 1 {
		t.Fatalf("corrected send has %d image blocks, want 1", n)
	}
	replay, err := run(edited, rawImage("shot.png"))
	if err != nil || replay != out {
		t.Fatalf("replay: %q %v", replay, err)
	}
	if c, s := f.counts(); c != 1 || s != 2 || up.count() != 2 {
		t.Fatalf("replay resent: creates=%d sends=%d uploads=%d", c, s, up.count())
	}
}

// The corrected send is accepted upstream but its acknowledgement is lost; the
// proxy restarts and the client retries: the message must not be sent twice.
func TestAgentInterruptedEditAckLostThenRestartNoDuplicate(t *testing.T) {
	f, store, acc := bindingSetup(t)
	store = newAgentThreadStore(filepath.Join(t.TempDir(), "ack.state"))
	setStall(f, true)
	gone, ended := startInterruptible(t, f, store, acc, "ack-edit", []ChatMessage{u("typo")})
	waitForCounts(t, f, 1, 0)
	endRequest(t, gone, ended)
	finishStale(f)

	fixed := []ChatMessage{u("fixed")}
	f.dropAckSend = true
	if _, err := runKeyed(t, f, store, acc, "ack-edit", "low", false, fixed); err == nil {
		t.Fatal("lost acknowledgement not reported")
	}
	f.dropAckSend = false
	store = reopenStore(store)
	out, err := runKeyed(t, f, store, acc, "ack-edit", "low", false, fixed)
	if err != nil || out != "reply-2" {
		t.Fatalf("retry after restart: %q %v", out, err)
	}
	if c, s := f.counts(); c != 1 || s != 1 {
		t.Fatalf("duplicate send: creates=%d sends=%d", c, s)
	}
	prompt := promptText(f.sends[0]["event"].(map[string]interface{})["content"])
	if !strings.Contains(prompt, "fixed") || strings.Contains(prompt, "typo") && !strings.Contains(prompt, agentCorrectionHead) {
		t.Fatalf("unexpected correction prompt: %q", prompt)
	}
	out, err = runKeyed(t, f, store, acc, "ack-edit", "low", false, []ChatMessage{u("fixed"), a(out), u("next")})
	if err != nil || out != "reply-3" {
		t.Fatalf("continuation: %q %v", out, err)
	}
}

// A second Ctrl+C lands while the edited request is still waiting for the old
// turn. Nothing is sent by it, and a further edit still lands on the thread.
func TestAgentInterruptedEditSecondCancelWhileWaiting(t *testing.T) {
	f, _, acc := bindingSetup(t)
	store := newAgentThreadStore(filepath.Join(t.TempDir(), "second.state"))
	setStall(f, true)
	gone, ended := startInterruptible(t, f, store, acc, "second", []ChatMessage{u("typo")})
	waitForCounts(t, f, 1, 0)
	endRequest(t, gone, ended)

	gone2, ended2 := startInterruptible(t, f, store, acc, "second", []ChatMessage{u("edit one")})
	time.Sleep(250 * time.Millisecond) // several polls of the still-running old turn
	if _, s := f.counts(); s != 0 {
		t.Fatal("edit sent while the old turn was running")
	}
	if err := endRequest(t, gone2, ended2); err == nil {
		t.Fatal("cancelled waiting edit reported success")
	}
	if _, s := f.counts(); s != 0 {
		t.Fatal("cancelled waiting edit sent a message")
	}
	finishStale(f)

	store = reopenStore(store)
	out, err := runKeyed(t, f, store, acc, "second", "low", false, []ChatMessage{u("edit two")})
	if err != nil || out != "reply-2" {
		t.Fatalf("edit after second cancel: %q %v", out, err)
	}
	if c, s := f.counts(); c != 1 || s != 1 {
		t.Fatalf("forked or duplicated: creates=%d sends=%d", c, s)
	}
	replay, err := runKeyed(t, f, store, acc, "second", "low", false, []ChatMessage{u("edit two")})
	if err != nil || replay != out {
		t.Fatalf("replay: %q %v", replay, err)
	}
}

// The corrected turn itself is interrupted and edited again.
func TestAgentInterruptedEditCorrectedTurnCancelledThenEditedAgain(t *testing.T) {
	f, _, acc := bindingSetup(t)
	store := newAgentThreadStore(filepath.Join(t.TempDir(), "again.state"))
	setStall(f, true)
	gone, ended := startInterruptible(t, f, store, acc, "again", []ChatMessage{u("typo")})
	waitForCounts(t, f, 1, 0)
	endRequest(t, gone, ended)
	finishStale(f)

	setStall(f, true)
	gone2, ended2 := startInterruptible(t, f, store, acc, "again", []ChatMessage{u("fix one")})
	waitForCounts(t, f, 1, 1)
	endRequest(t, gone2, ended2)

	store = reopenStore(store)
	type result struct {
		out string
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := runKeyed(t, f, store, acc, "again", "low", false, []ChatMessage{u("fix two")})
		done <- result{out, err}
	}()
	time.Sleep(250 * time.Millisecond)
	if _, s := f.counts(); s != 1 {
		t.Fatalf("second edit sent while the corrected turn was running: sends=%d", s)
	}
	finishStale(f)
	select {
	case r := <-done:
		if r.err != nil || r.out != "reply-3" {
			t.Fatalf("second edit: %q %v", r.out, r.err)
		}
		if strings.Contains(r.out, "STALE") {
			t.Fatal("stale reply leaked")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("second edit did not finish")
	}
	if c, s := f.counts(); c != 1 || s != 2 {
		t.Fatalf("forked or duplicated: creates=%d sends=%d", c, s)
	}
	store = reopenStore(store)
	out, err := runKeyed(t, f, store, acc, "again", "low", false, []ChatMessage{u("fix two")})
	if err != nil || out != "reply-3" {
		t.Fatalf("replay: %q %v", out, err)
	}
	out, err = runKeyed(t, f, store, acc, "again", "low", false, []ChatMessage{u("fix two"), a(out), u("next")})
	if err != nil || out != "reply-4" {
		t.Fatalf("continuation: %q %v", out, err)
	}
}

// unconfirmedCorrection leaves a thread whose corrected send ("fix one") got a
// non-definitive 503 while the client disconnected during it: the outcome is
// unknown, the record is still submitting, and the store was restarted.
func unconfirmedCorrection(t *testing.T, key string) (*fakeAgentServer, *agentThreadStore, *Account) {
	t.Helper()
	f, _, acc := bindingSetup(t)
	store := newAgentThreadStore(filepath.Join(t.TempDir(), key+".state"))
	setStall(f, true)
	gone, ended := startInterruptible(t, f, store, acc, key, []ChatMessage{u("typo")})
	waitForCounts(t, f, 1, 0)
	endRequest(t, gone, ended)
	finishStale(f)

	f.mu.Lock()
	f.rejectSend = 503
	f.mu.Unlock()
	if _, err := runKeyed(t, f, store, acc, key, "low", false, []ChatMessage{u("fix one")}); err == nil {
		t.Fatal("unavailable service reported success")
	}
	f.mu.Lock()
	f.rejectSend = 0
	f.mu.Unlock()
	store.mu.Lock()
	for _, rec := range store.records {
		rec.ClientInterrupted = true
	}
	store.saveLocked()
	store.mu.Unlock()
	return f, reopenStore(store), acc
}

// The outcome of the corrected send is unknown. A further edit must neither
// wait out the deadline for a turn that may not exist nor skip the unknown
// request with a new event (it could still land and reorder behind it). It
// returns a recoverable pending error, sends nothing and keeps the binding.
// Retrying the original corrected request resolves it and Ctrl+C editing then
// works again on the same thread.
func TestAgentInterruptedEditUnconfirmedCorrectionReturnsPending(t *testing.T) {
	f, store, acc := unconfirmedCorrection(t, "lost")

	start := time.Now()
	out, err := runKeyed(t, f, store, acc, "lost", "low", false, []ChatMessage{u("fix two")})
	var rec *agentRecoverableError
	if err == nil || !errors.As(err, &rec) || rec.Class != "correction_pending" || out != "" {
		t.Fatalf("expected recoverable correction_pending, got %q %v", out, err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("pending took %v; it must not wait for the deadline", time.Since(start))
	}
	if c, s := f.counts(); c != 1 || s != 0 {
		t.Fatalf("pending edit sent or forked: creates=%d sends=%d", c, s)
	}
	store.mu.Lock()
	kept := len(store.records) == 1
	for _, r := range store.records {
		kept = kept && r.Correction && r.State == agentStateSubmitting
	}
	store.mu.Unlock()
	if !kept {
		t.Fatal("binding or unconfirmed correction was lost")
	}

	// The original corrected request is retried unchanged and resolves.
	setStall(f, true)
	gone, ended := startInterruptible(t, f, store, acc, "lost", []ChatMessage{u("fix one")})
	waitForCounts(t, f, 1, 1)
	endRequest(t, gone, ended)
	finishStale(f)
	store = reopenStore(store)
	out, err = runKeyed(t, f, store, acc, "lost", "low", false, []ChatMessage{u("fix three")})
	if err != nil || out != "reply-3" || strings.Contains(out, "STALE") {
		t.Fatalf("edit after resolved correction: %q %v", out, err)
	}
	if c, s := f.counts(); c != 1 || s != 2 {
		t.Fatalf("forked or duplicated: creates=%d sends=%d", c, s)
	}
}

// The unknown corrected send is later really accepted (its acknowledgement is
// lost again). The next edit waits for that turn, drains it and neither leaks
// its text nor its old tool_use into the new turn.
func TestAgentInterruptedEditUnconfirmedCorrectionLaterAccepted(t *testing.T) {
	f, store, acc := unconfirmedCorrection(t, "late")
	if _, err := runKeyed(t, f, store, acc, "late", "low", false, []ChatMessage{u("fix two")}); err == nil {
		t.Fatal("unknown correction was skipped")
	}

	setStall(f, true)
	f.dropAckSend = true
	if _, err := runKeyed(t, f, store, acc, "late", "low", false, []ChatMessage{u("fix one")}); err == nil {
		t.Fatal("lost acknowledgement not reported")
	}
	f.dropAckSend = false
	if c, s := f.counts(); c != 1 || s != 1 {
		t.Fatalf("late acceptance: creates=%d sends=%d", c, s)
	}
	store.mu.Lock()
	for _, rec := range store.records {
		rec.ClientInterrupted = true
	}
	store.saveLocked()
	store.mu.Unlock()
	store = reopenStore(store)

	type result struct {
		out string
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := runKeyed(t, f, store, acc, "late", "low", false, []ChatMessage{u("fix two")})
		done <- result{out, err}
	}()
	time.Sleep(250 * time.Millisecond)
	if _, s := f.counts(); s != 1 {
		t.Fatalf("edit sent while the late corrected turn was running: sends=%d", s)
	}
	finishStale(f)
	select {
	case r := <-done:
		if r.err != nil || r.out != "reply-3" || strings.Contains(r.out, "STALE") || strings.Contains(r.out, "fix one") {
			t.Fatalf("edit after late acceptance: %q %v", r.out, r.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("edit did not finish")
	}
	if c, s := f.counts(); c != 1 || s != 2 {
		t.Fatalf("forked or duplicated: creates=%d sends=%d", c, s)
	}
	f.mu.Lock()
	prompt := promptText(f.sends[1]["event"].(map[string]interface{})["content"])
	f.mu.Unlock()
	if !strings.Contains(prompt, "fix two") || strings.Contains(prompt, "STALE") {
		t.Fatalf("second send prompt carries stale content: %q", prompt)
	}
}

func TestAgentInterruptedEditDuringSubmittingFollowUpSendsImmediately(t *testing.T) {
	setUnconfirmedGrace(t, 50*time.Millisecond)
	f, store, acc := bindingSetup(t)
	const key = "submitting-followup-edit"

	var blockSend atomic.Bool
	enteredSend := make(chan struct{}, 1)
	releaseSend := make(chan struct{})
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if blockSend.Load() && strings.HasSuffix(r.URL.Path, "sendEventToAgentThread") {
			select {
			case enteredSend <- struct{}{}:
			default:
			}
			select {
			case <-releaseSend:
			case <-r.Context().Done():
			}
			return
		}
		f.handle(w, r)
	})

	out, err := runKeyed(t, f, store, acc, key, "low", false, []ChatMessage{u("root")})
	if err != nil || out != "reply-1" {
		t.Fatalf("round 1: %q %v", out, err)
	}

	blockSend.Store(true)
	gone, ended := startInterruptible(t, f, store, acc, key, []ChatMessage{u("root"), a("reply-1"), u("draft message")})
	select {
	case <-enteredSend:
	case <-time.After(2 * time.Second):
		t.Fatal("sendEventToAgentThread was never started")
	}
	endRequest(t, gone, ended)
	blockSend.Store(false)
	close(releaseSend)

	rec := lifecycleRecord(store, acc, key)
	if rec.State != agentStateSubmitting || !rec.ClientInterrupted {
		t.Fatalf("expected State=submitting and ClientInterrupted=true, got state=%q interrupted=%v", rec.State, rec.ClientInterrupted)
	}

	start := time.Now()
	edited := []ChatMessage{u("root"), a("reply-1"), u("edited message")}
	out, err = runKeyed(t, f, store, acc, key, "low", false, edited)
	if err != nil || out != "reply-2" {
		t.Fatalf("edited request after submitting Ctrl+C failed: out=%q err=%v", out, err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("edited request took %v; expected immediate send without polling timeout", elapsed)
	}
	if c, s := f.counts(); c != 1 || s != 1 {
		t.Fatalf("expected creates=1 sends=1, got creates=%d sends=%d", c, s)
	}
	if n := correctionSendCount(f); n != 0 {
		t.Fatalf("expected 0 correction headers for an unaccepted turn, got %d", n)
	}
	after := lifecycleRecord(store, acc, key)
	if after.Turns != 2 {
		t.Fatalf("turns=%d, want 2", after.Turns)
	}

	out, err = runKeyed(t, f, store, acc, key, "low", false, []ChatMessage{u("root"), a("reply-1"), u("edited message"), a("reply-2"), u("next")})
	if err != nil || out != "reply-3" {
		t.Fatalf("continuation: %q %v", out, err)
	}
}

func TestAgentInterruptedEditDuringSubmittingFirstTurnCreatesThread(t *testing.T) {
	setUnconfirmedGrace(t, 50*time.Millisecond)
	f, store, acc := bindingSetup(t)
	const key = "submitting-first-edit"

	var blockCreate atomic.Bool
	enteredCreate := make(chan struct{}, 1)
	releaseCreate := make(chan struct{})
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if blockCreate.Load() && strings.HasSuffix(r.URL.Path, "createAgentThread") {
			select {
			case enteredCreate <- struct{}{}:
			default:
			}
			select {
			case <-releaseCreate:
			case <-r.Context().Done():
			}
			return
		}
		f.handle(w, r)
	})

	blockCreate.Store(true)
	gone, ended := startInterruptible(t, f, store, acc, key, []ChatMessage{u("draft first")})
	select {
	case <-enteredCreate:
	case <-time.After(2 * time.Second):
		t.Fatal("createAgentThread was never started")
	}
	endRequest(t, gone, ended)
	blockCreate.Store(false)
	close(releaseCreate)

	out, err := runKeyed(t, f, store, acc, key, "low", false, []ChatMessage{u("edited first")})
	if err != nil || out != "reply-1" {
		t.Fatalf("edited first turn failed: out=%q err=%v", out, err)
	}
	if c, s := f.counts(); c != 1 || s != 0 {
		t.Fatalf("expected creates=1 sends=0, got creates=%d sends=%d", c, s)
	}

	out, err = runKeyed(t, f, store, acc, key, "low", false, []ChatMessage{u("edited first"), a("reply-1"), u("second")})
	if err != nil || out != "reply-2" {
		t.Fatalf("second turn failed: out=%q err=%v", out, err)
	}
}

func TestAgentInterruptedEditThenClaudeResumeSyntheticMessages(t *testing.T) {
	f, _, acc := bindingSetup(t)
	store := newAgentThreadStore(filepath.Join(t.TempDir(), "resume.state"))
	const key = "resume-synthetic"

	out, err := runKeyed(t, f, store, acc, key, "low", false, []ChatMessage{u("root")})
	if err != nil || out != "reply-1" {
		t.Fatalf("round 1: %q %v", out, err)
	}

	setStall(f, true)
	gone, ended := startInterruptible(t, f, store, acc, key, []ChatMessage{u("root"), a("reply-1"), u("你可以调整为百万上下文吗")})
	waitForCounts(t, f, 1, 1)
	endRequest(t, gone, ended)
	finishStale(f)

	store = reopenStore(store)
	resumed := []ChatMessage{
		u("root"),
		a("reply-1"),
		u("你可以调整为百万上下文吗\n\nContinue from where you left off."),
		a("No response requested."),
		u("你继续你可以调整上下文吗"),
	}
	out, err = runKeyed(t, f, store, acc, key, "low", false, resumed)
	if err != nil || out != "reply-3" {
		t.Fatalf("resumed turn failed: out=%q err=%v", out, err)
	}
	f.mu.Lock()
	sentPrompt := promptText(f.sends[1]["event"].(map[string]interface{})["content"])
	f.mu.Unlock()
	if strings.Contains(sentPrompt, "No response requested.") || strings.Contains(sentPrompt, "Continue from where you left off.") {
		t.Fatalf("synthetic resume placeholder leaked into Notion prompt: %q", sentPrompt)
	}
	if !strings.Contains(sentPrompt, "你可以调整为百万上下文吗") || !strings.Contains(sentPrompt, "你继续你可以调整上下文吗") {
		t.Fatalf("expected user prompts in Notion prompt: %q", sentPrompt)
	}

	next := append(append([]ChatMessage(nil), resumed...), a(out), u("follow up"))
	out2, err := runKeyed(t, f, store, acc, key, "low", false, next)
	if err != nil || out2 != "reply-4" {
		t.Fatalf("follow-up after resume failed: out=%q err=%v", out2, err)
	}
}

func setUnconfirmedGrace(t *testing.T, d time.Duration) {
	t.Helper()
	old := agentUnconfirmedGrace
	agentUnconfirmedGrace = d
	t.Cleanup(func() { agentUnconfirmedGrace = old })
}

// A Ctrl+C'd send that lands shortly after the client is gone must not be
// treated as "not received": the edit waits out the grace period, sees the
// late turn and corrects it instead of sending a second, unrelated turn.
func TestAgentInterruptedEditDuringSubmittingLateLandingIsNotResent(t *testing.T) {
	setUnconfirmedGrace(t, 2*time.Second)
	f, store, acc := bindingSetup(t)
	const key = "submitting-late-landing"

	var blockSend atomic.Bool
	enteredSend := make(chan struct{}, 1)
	releaseSend := make(chan struct{})
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if blockSend.Load() && strings.HasSuffix(r.URL.Path, "sendEventToAgentThread") {
			select {
			case enteredSend <- struct{}{}:
			default:
			}
			<-releaseSend
			// The send still lands on the server after the client went away.
			f.handle(httptest.NewRecorder(), r.Clone(context.Background()))
			return
		}
		f.handle(w, r)
	})

	out, err := runKeyed(t, f, store, acc, key, "low", false, []ChatMessage{u("root")})
	if err != nil || out != "reply-1" {
		t.Fatalf("round 1: %q %v", out, err)
	}

	blockSend.Store(true)
	gone, ended := startInterruptible(t, f, store, acc, key, []ChatMessage{u("root"), a("reply-1"), u("draft message")})
	select {
	case <-enteredSend:
	case <-time.After(2 * time.Second):
		t.Fatal("sendEventToAgentThread was never started")
	}
	endRequest(t, gone, ended)
	blockSend.Store(false)
	go func() {
		time.Sleep(300 * time.Millisecond)
		close(releaseSend)
	}()

	edited := []ChatMessage{u("root"), a("reply-1"), u("edited message")}
	out, err = runKeyed(t, f, store, acc, key, "low", false, edited)
	if err != nil {
		t.Fatalf("edited request after late landing failed: out=%q err=%v", out, err)
	}
	if n := correctionSendCount(f); n != 1 {
		t.Fatalf("late-landed turn must be corrected (1 correction send), got %d", n)
	}
}

func TestAgentInterruptedToolResultTurnContinuesOnSameThread(t *testing.T) {
	f, _, acc := bindingSetup(t)
	store := newAgentThreadStore(filepath.Join(t.TempDir(), "tool-interrupt.state"))
	const key = "tool-interrupt"

	m1 := []ChatMessage{u("check logs")}
	out1, err := runKeyed(t, f, store, acc, key, "low", true, m1)
	if err != nil || out1 != "reply-1" {
		t.Fatalf("round 1: %q %v", out1, err)
	}

	toolCallMsg := ChatMessage{
		Role:      "assistant",
		ToolCalls: []ToolCall{{ID: "call_0_grep", Type: "function", Function: ToolCallFunction{Name: "Grep", Arguments: `{"pattern":"502"}`}}},
	}
	toolResMsg := ChatMessage{
		Role:       "tool",
		ToolCallID: "call_0_grep",
		Name:       "Grep",
		Content:    "setting/setting.go:587",
	}
	m2 := []ChatMessage{u("check logs"), toolCallMsg, toolResMsg}

	setStall(f, true)
	gone, ended := startInterruptible(t, f, store, acc, key, m2)
	waitForCounts(t, f, 1, 1)
	endRequest(t, gone, ended)
	finishStale(f)

	store = reopenStore(store)
	m3 := []ChatMessage{
		u("check logs"),
		toolCallMsg,
		toolResMsg,
		u("[Request interrupted by user]"),
		u("继续"),
	}
	out2, err := runKeyed(t, f, store, acc, key, "low", true, m3)
	if err != nil || out2 != "reply-3" {
		t.Fatalf("continuation after interrupted tool_result failed: out=%q err=%v", out2, err)
	}
	if strings.Contains(out2, "STALE") {
		t.Fatal("stale reply leaked")
	}
	if c, s := f.counts(); c != 1 || s != 2 {
		t.Fatalf("expected creates=1 sends=2, got creates=%d sends=%d", c, s)
	}

	m4 := append(append([]ChatMessage(nil), m3...), a(out2), u("next question"))
	out3, err := runKeyed(t, f, store, acc, key, "low", true, m4)
	if err != nil || out3 != "reply-4" {
		t.Fatalf("follow-up after tool_result correction failed: out=%q err=%v", out3, err)
	}
}

func TestAgentInterruptedToolResultLegacyEmptyEditPrefixHash(t *testing.T) {
	f, _, acc := bindingSetup(t)
	store := newAgentThreadStore(filepath.Join(t.TempDir(), "tool-legacy.state"))
	const key = "tool-legacy"

	m1 := []ChatMessage{u("check logs")}
	if _, err := runKeyed(t, f, store, acc, key, "low", true, m1); err != nil {
		t.Fatalf("round 1: %v", err)
	}

	toolCallMsg := ChatMessage{
		Role:      "assistant",
		ToolCalls: []ToolCall{{ID: "call_0_grep", Type: "function", Function: ToolCallFunction{Name: "Grep", Arguments: `{"pattern":"502"}`}}},
	}
	toolResMsg := ChatMessage{
		Role:       "tool",
		ToolCallID: "call_0_grep",
		Name:       "Grep",
		Content:    "setting/setting.go:587",
	}
	m2 := []ChatMessage{u("check logs"), toolCallMsg, toolResMsg}

	setStall(f, true)
	gone, ended := startInterruptible(t, f, store, acc, key, m2)
	waitForCounts(t, f, 1, 1)
	endRequest(t, gone, ended)
	finishStale(f)

	store.mu.Lock()
	rec := store.primaryLocked(agentPlanInput{ClientKey: key, AccountID: agentAccountKey(acc), SpaceID: "space"})
	rec.EditPrefixHash = ""
	rec.EditPrefixLen = 1
	store.saveLocked()
	store.mu.Unlock()

	store = reopenStore(store)
	m3 := []ChatMessage{
		u("check logs"),
		toolCallMsg,
		toolResMsg,
		u("[Request interrupted by user]"),
		u("继续"),
	}
	out2, err := runKeyed(t, f, store, acc, key, "low", true, m3)
	if err != nil || out2 != "reply-3" {
		t.Fatalf("legacy empty EditPrefixHash continuation failed: out=%q err=%v", out2, err)
	}
	f.mu.Lock()
	sentPrompt := promptText(f.sends[1]["event"].(map[string]interface{})["content"])
	f.mu.Unlock()
	if strings.Contains(sentPrompt, "setting/setting.go:587") {
		t.Fatalf("already-submitted tool_result must not be duplicated in correction prompt: %q", sentPrompt)
	}
	if !strings.Contains(sentPrompt, "继续") {
		t.Fatalf("expected user follow-up in correction prompt: %q", sentPrompt)
	}
}

func TestAgentInterruptedToolResultDuringSubmittingResendsToolAndUserTail(t *testing.T) {
	setUnconfirmedGrace(t, 50*time.Millisecond)
	f, store, acc := bindingSetup(t)
	const key = "tool-submitting-interrupt"

	var blockSend atomic.Bool
	enteredSend := make(chan struct{}, 1)
	releaseSend := make(chan struct{})
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if blockSend.Load() && strings.HasSuffix(r.URL.Path, "sendEventToAgentThread") {
			select {
			case enteredSend <- struct{}{}:
			default:
			}
			select {
			case <-releaseSend:
			case <-r.Context().Done():
			}
			return
		}
		f.handle(w, r)
	})

	if _, err := runKeyed(t, f, store, acc, key, "low", true, []ChatMessage{u("check logs")}); err != nil {
		t.Fatalf("round 1: %v", err)
	}

	toolCallMsg := ChatMessage{
		Role:      "assistant",
		ToolCalls: []ToolCall{{ID: "call_0_grep", Type: "function", Function: ToolCallFunction{Name: "Grep", Arguments: `{"pattern":"502"}`}}},
	}
	toolResMsg := ChatMessage{
		Role:       "tool",
		ToolCallID: "call_0_grep",
		Name:       "Grep",
		Content:    "setting/setting.go:587",
	}
	m2 := []ChatMessage{u("check logs"), toolCallMsg, toolResMsg}

	blockSend.Store(true)
	gone, ended := startInterruptible(t, f, store, acc, key, m2)
	select {
	case <-enteredSend:
	case <-time.After(2 * time.Second):
		t.Fatal("sendEventToAgentThread was never started")
	}
	endRequest(t, gone, ended)
	blockSend.Store(false)
	close(releaseSend)

	m3 := []ChatMessage{
		u("check logs"),
		toolCallMsg,
		toolResMsg,
		u("[Request interrupted by user]"),
		u("继续"),
	}
	out2, err := runKeyed(t, f, store, acc, key, "low", true, m3)
	if err != nil || out2 != "reply-2" {
		t.Fatalf("continuation after unconfirmed tool_result interrupt failed: out=%q err=%v", out2, err)
	}
	f.mu.Lock()
	sentPrompt := promptText(f.sends[0]["event"].(map[string]interface{})["content"])
	f.mu.Unlock()
	if !strings.Contains(sentPrompt, "setting/setting.go:587") || !strings.Contains(sentPrompt, "继续") {
		t.Fatalf("unconfirmed tool_result must be included alongside user continuation: %q", sentPrompt)
	}
}

func TestAgentInterruptedEditUnconfirmedCorrectionRecoversAfterGrace(t *testing.T) {
	setUnconfirmedGrace(t, 200*time.Millisecond)
	f, store, acc := unconfirmedCorrection(t, "grace-expired")

	// Within the grace window, a new edit still returns correction_pending.
	_, err := runKeyed(t, f, store, acc, "grace-expired", "low", false, []ChatMessage{
		u("fix one"),
		u("[Request interrupted by user]"),
		u("fix two"),
	})
	var recErr *agentRecoverableError
	if err == nil || !errors.As(err, &recErr) || recErr.Class != "correction_pending" {
		t.Fatalf("expected correction_pending within grace window, got %v", err)
	}

	// Once the grace period elapses and the remote thread is still idle without
	// having accepted "fix one", retrying the new edit supersedes the unconfirmed
	// correction on the same thread and preserves the correction header.
	time.Sleep(250 * time.Millisecond)
	out, err := runKeyed(t, f, store, acc, "grace-expired", "low", false, []ChatMessage{
		u("fix one"),
		u("[Request interrupted by user]"),
		u("fix two"),
	})
	if err != nil || out != "reply-2" {
		t.Fatalf("expected recovery after grace elapsed, got out=%q err=%v", out, err)
	}
	if c, s := f.counts(); c != 1 || s != 1 {
		t.Fatalf("expected 1 create and 1 send on the same thread, got creates=%d sends=%d", c, s)
	}
	f.mu.Lock()
	sentPrompt := promptText(f.sends[0]["event"].(map[string]interface{})["content"])
	f.mu.Unlock()
	if !strings.Contains(sentPrompt, "Earlier messages in this thread") || !strings.Contains(sentPrompt, "fix one") || !strings.Contains(sentPrompt, "fix two") {
		t.Fatalf("superseded correction lost rebind header or unsent text: %q", sentPrompt)
	}
}

