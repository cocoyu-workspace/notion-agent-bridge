package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Regression tests for upstream error classification:
//   - quota exhaustion must reach the Anthropic client as a non-retryable 402
//     billing_error, never as a retryable 5xx/overloaded error;
//   - Notion's MemcachedCrossCellError must become an actionable configuration
//     error, without changing the pending / no-blind-resend semantics.

const (
	quotaBody     = `{"code":"PaymentRequiredError","message":"SECRET-UPSTREAM-TEXT"}`
	crossCellBody = `{"name":"MemcachedCrossCellError","message":"Cross-cell memcached access is not allowed SECRET-UPSTREAM-TEXT"}`
)

type errclassEnv struct {
	f        *fakeAgentServer
	eligible atomic.Bool // what the (stubbed) live quota API answers
}

func newErrclassEnv(t *testing.T) *errclassEnv {
	t.Helper()
	f := newFakeAgentServer(t)
	e := &errclassEnv{f: f}
	e.eligible.Store(true)
	useRecoveryStore(t, "")
	oldClient, oldQuota := agentThreadHTTPClient, quotaFetcher
	agentThreadHTTPClient = func(time.Duration) *http.Client { return f.server.Client() }
	quotaFetcher = func(*Account) (*QuotaInfo, error) { return &QuotaInfo{IsEligible: e.eligible.Load()}, nil }
	t.Cleanup(func() { agentThreadHTTPClient, quotaFetcher = oldClient, oldQuota })
	return e
}

type errclassResp struct {
	Status  int
	Header  http.Header
	Type    string
	Message string
	Raw     string
}

func postMessages(t *testing.T, pool *AccountPool, stream bool, session string, msgs ...ChatMessage) errclassResp {
	t.Helper()
	var wire []map[string]string
	for _, m := range msgs {
		wire = append(wire, map[string]string{"role": m.Role, "content": m.Content})
	}
	body := map[string]interface{}{"model": "auto", "max_tokens": 64, "stream": stream, "messages": wire}
	if session != "" {
		body["metadata"] = map[string]interface{}{"user_id": `{"session_id":"` + session + `"}`}
	}
	raw, _ := json.Marshal(body)
	rec := httptest.NewRecorder()
	HandleAnthropicMessages(pool).ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(string(raw))))
	waitQuotaIdle(pool) // the handler refreshes quota in the background after every call
	out := errclassResp{Status: rec.Code, Header: rec.Header(), Raw: rec.Body.String()}
	var payload struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	json.Unmarshal(rec.Body.Bytes(), &payload)
	out.Type, out.Message = payload.Error.Type, payload.Error.Message
	return out
}

func waitQuotaIdle(pool *AccountPool) {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		pool.liveQuotaMu.Lock()
		n := len(pool.liveQuotaInflight)
		pool.liveQuotaMu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func assertNoIdentity(t *testing.T, r errclassResp, accs ...*Account) {
	t.Helper()
	for _, acc := range accs {
		for _, secret := range []string{acc.TokenV2, acc.UserEmail, acc.SpaceID, acc.UserID, "SECRET-UPSTREAM-TEXT"} {
			if secret != "" && strings.Contains(r.Raw, secret) {
				t.Fatalf("client error leaks %q: %s", secret, r.Raw)
			}
		}
	}
}

func assertQuotaExhausted(t *testing.T, r errclassResp) {
	t.Helper()
	if r.Status != http.StatusPaymentRequired || r.Type != "billing_error" {
		t.Fatalf("quota exhaustion must be HTTP 402 billing_error, got %d %q: %s", r.Status, r.Type, r.Raw)
	}
	if got := r.Header.Get("x-should-retry"); got != "false" {
		t.Fatalf("x-should-retry = %q, want false", got)
	}
	if !strings.HasPrefix(r.Message, "[bridge:quota_exhausted]") {
		t.Fatalf("message must start with [bridge:quota_exhausted]: %q", r.Message)
	}
}

func markIneligible(acc *Account) {
	now := time.Now()
	acc.mu.Lock()
	acc.QuotaInfo = &QuotaInfo{IsEligible: false}
	acc.QuotaCheckedAt = &now
	acc.mu.Unlock()
}

// ---- A. quota exhaustion -> 402 billing_error ------------------------------

func TestQuotaExhaustedIsNonRetryable402OnBothPaths(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, scenario := range []string{"upstream_402_confirmed", "live_precheck_ineligible"} {
			t.Run(fmt.Sprintf("stream=%v/%s", stream, scenario), func(t *testing.T) {
				env := newErrclassEnv(t)
				acc := roleAcc("quota-owner@example.com", "", 112, 100)
				pool := poolOf(acc)
				if scenario == "upstream_402_confirmed" {
					env.f.rejectCreate, env.f.rejectBody = 402, quotaBody
					env.eligible.Store(false) // the account looks fine until Notion answers 402
				} else {
					markIneligible(acc)
					env.eligible.Store(false)
				}
				r := postMessages(t, pool, stream, "", u("hello"))
				assertQuotaExhausted(t, r)
				assertNoIdentity(t, r, acc)
				// Second request: the account is now marked exhausted, nothing is selectable.
				before := env.f.createTries
				r = postMessages(t, pool, stream, "", u("hello again"))
				assertQuotaExhausted(t, r)
				if env.f.createTries != before {
					t.Fatalf("an exhausted account must not be contacted again")
				}
				if scenario == "live_precheck_ineligible" && env.f.createTries != 0 {
					t.Fatalf("ineligible account must not reach upstream (creates=%d)", env.f.createTries)
				}
			})
		}
	}
}

func TestTrueOverloadKeepsOriginal5xx(t *testing.T) {
	env := newErrclassEnv(t)
	// No account at all: nothing is exhausted, this stays the generic overload answer.
	r := postMessages(t, NewAccountPool(), false, "", u("hi"))
	if r.Status != http.StatusServiceUnavailable || r.Type != "overloaded_error" {
		t.Fatalf("empty pool = %d %q", r.Status, r.Type)
	}
	// Upstream 503 on an eligible account is a transient failure, not billing.
	acc := roleAcc("ok@example.com", "", 0, 100)
	env.f.rejectCreate = 503
	r = postMessages(t, poolOf(acc), false, "", u("hi"))
	if r.Status == http.StatusPaymentRequired || r.Type == "billing_error" || !strings.HasPrefix(r.Message, "[bridge:pending]") {
		t.Fatalf("transient 503 must stay pending/5xx, got %d %q %q", r.Status, r.Type, r.Message)
	}
}

func TestClassifyQuotaFailureMapping(t *testing.T) {
	agentOn := func(on bool) {
		old := AppConfig
		AppConfig = DefaultConfig()
		AppConfig.Proxy.AgentThread = on
		t.Cleanup(func() { AppConfig = old })
	}
	h402 := &agentHTTPError{Endpoint: "sendEventToAgentThread", Status: 402, Code: "PaymentRequiredError"}
	cases := map[string]error{
		"confirmed":     fmt.Errorf("%w: %v", ErrQuotaExhausted, h402),
		"raw 402":       h402,
		"bound 402":     &agentBindingError{Kind: "rejected", Cause: h402, Msg: "rejected"},
		"recoverable":   &agentRecoverableError{Class: "http_402", ThreadID: "t", Cause: h402},
		"legacy sentin": ErrQuotaExhausted,
	}
	for _, on := range []bool{true, false} {
		agentOn(on)
		for name, err := range cases {
			if !on && name != "confirmed" && name != "legacy sentin" {
				continue // legacy transport only ever sees the sentinel
			}
			f := agentFailureFor(err)
			if f.Status != http.StatusPaymentRequired || f.ErrType != "billing_error" || f.ShouldRetry || !strings.HasPrefix(f.Message, "[bridge:quota_exhausted]") {
				t.Fatalf("agent=%v %s: %+v", on, name, f)
			}
		}
	}
	// Other 4xx stay "rejected", 5xx stays pending: only quota changes class.
	agentOn(true)
	if f := classifyAgentFailure(&agentHTTPError{Endpoint: "sendEventToAgentThread", Status: 400, Code: "ValidationError"}); f.Status == 402 {
		t.Fatalf("400 must not be billing: %+v", f)
	}
}

// Follow-up of a bound thread whose send is answered 402 while the live quota
// API does not confirm exhaustion: the binding is kept, no second thread (and no
// account switch) happens, and the same event is never sent blindly again.
func TestAgentFollowUp402UnconfirmedKeepsBindingNoForkNoBlindResend(t *testing.T) {
	env := newErrclassEnv(t)
	one := roleAcc("one@example.com", "", 0, 100)
	two := roleAcc("two@example.com", "", 0, 100)
	pool := poolOf(one, two)
	const sess = "sess-402"
	if r := postMessages(t, pool, false, sess, u("first")); r.Status != 200 {
		t.Fatalf("setup: %d %s", r.Status, r.Raw)
	}
	if env.f.createTries != 1 {
		t.Fatalf("setup creates=%d", env.f.createTries)
	}
	history := []ChatMessage{u("first"), a("reply-1"), u("second")}
	env.f.rejectSend, env.f.rejectBody = 402, quotaBody
	r := postMessages(t, pool, false, sess, history...)
	assertQuotaExhausted(t, r) // upstream said 402; the quota API disagrees, so no failover
	if env.f.sendTries != 1 || env.f.createTries != 1 {
		t.Fatalf("one send attempt, no new thread: sends=%d creates=%d", env.f.sendTries, env.f.createTries)
	}
	// Recovery: the identical request continues the same thread.
	env.f.rejectSend, env.f.rejectBody = 0, ""
	if r := postMessages(t, pool, false, sess, history...); r.Status != 200 {
		t.Fatalf("retry after recovery: %d %s", r.Status, r.Raw)
	}
	if c, s := env.f.counts(); c != 1 || s != 1 {
		t.Fatalf("binding must continue the same thread: creates=%d sends=%d", c, s)
	}
}

func TestAgentFollowUpConfirmedExhaustedSingleAccountIs402AndKeepsBinding(t *testing.T) {
	env := newErrclassEnv(t)
	acc := roleAcc("solo@example.com", "", 0, 100)
	pool := poolOf(acc)
	const sess = "sess-solo"
	if r := postMessages(t, pool, true, sess, u("first")); r.Status != 200 {
		t.Fatalf("setup: %d %s", r.Status, r.Raw)
	}
	history := []ChatMessage{u("first"), a("reply-1"), u("second")}
	env.f.rejectSend, env.f.rejectBody = 402, quotaBody
	env.eligible.Store(false)
	r := postMessages(t, pool, true, sess, history...)
	assertQuotaExhausted(t, r)
	assertNoIdentity(t, r, acc)
	if c, s := env.f.counts(); c != 1 || s != 0 || env.f.sendTries != 1 {
		t.Fatalf("no new thread, no accepted send, one attempt: creates=%d sends=%d tries=%d", c, s, env.f.sendTries)
	}
	// Quota recovers: the same conversation resumes on the same thread.
	env.f.rejectSend, env.f.rejectBody = 0, ""
	env.eligible.Store(true)
	pool.ClearQuotaExhausted(acc)
	acc.mu.Lock()
	acc.QuotaInfo = &QuotaInfo{IsEligible: true}
	now := time.Now()
	acc.QuotaCheckedAt = &now
	acc.mu.Unlock()
	if r := postMessages(t, pool, true, sess, history...); r.Status != 200 {
		t.Fatalf("resume: %d %s", r.Status, r.Raw)
	}
	if c, s := env.f.counts(); c != 1 || s != 1 {
		t.Fatalf("resume must reuse the thread: creates=%d sends=%d", c, s)
	}
}

// 402 is a definitive upstream rejection (not accepted), never pending.
func TestAgent402IsDefinitiveRejectionNotPending(t *testing.T) {
	h := &agentHTTPError{Endpoint: "createAgentThread", Status: 402}
	if !h.definitiveRejection() {
		t.Fatal("402 must be a definitive rejection")
	}
	if got := transientClass(h); got != "" {
		t.Fatalf("402 must not be transient/pending, got %q", got)
	}
}

// ---- B. MemcachedCrossCellError -------------------------------------------

func TestCrossCellErrorNameDetectedFromBody(t *testing.T) {
	for _, body := range []string{
		`{"name":"MemcachedCrossCellError","message":"x"}`,
		`{"code":"internal_error","name":"MemcachedCrossCellError"}`,
		`{"error":{"name":"MemcachedCrossCellError"}}`,
	} {
		if got := agentErrorCodeFromBody([]byte(body)); got != "MemcachedCrossCellError" {
			t.Fatalf("%s -> %q", body, got)
		}
	}
	if got := agentErrorCodeFromBody([]byte(`{"name":"SomethingElse"}`)); got == "MemcachedCrossCellError" {
		t.Fatal("unrelated name matched")
	}
}

func TestCrossCellCreateIsActionableAndKeepsPendingSemantics(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			env := newErrclassEnv(t)
			acc := roleAcc("cell@example.com", "", 0, 100)
			pool := poolOf(acc)
			base := NotionAPIBase
			env.f.rejectCreate, env.f.rejectBody = 500, crossCellBody
			r := postMessages(t, pool, stream, "sess-cell", u("hello"))
			if !strings.HasPrefix(r.Message, "[bridge:notion_cross_cell]") || !strings.Contains(r.Message, "proxy.notion_api_base") || !strings.Contains(r.Message, "https://www.notion.so/api/v3") {
				t.Fatalf("cross-cell message not actionable: %d %q", r.Status, r.Message)
			}
			if r.Status < 500 || r.Status == http.StatusPaymentRequired || r.Header.Get("x-should-retry") != "false" {
				t.Fatalf("cross-cell is a config error, not a retryable fault: %d retry=%q", r.Status, r.Header.Get("x-should-retry"))
			}
			assertNoIdentity(t, r, acc)
			if env.f.createTries != 1 || NotionAPIBase != base {
				t.Fatalf("no automatic retry and no domain switch: tries=%d base=%s", env.f.createTries, NotionAPIBase)
			}
			// Unchanged semantics: the identical request after the operator fixes the
			// route is accepted exactly once (same binding, one thread).
			env.f.rejectCreate, env.f.rejectBody = 0, ""
			if r := postMessages(t, pool, stream, "sess-cell", u("hello")); r.Status != 200 {
				t.Fatalf("retry after fix: %d %s", r.Status, r.Raw)
			}
			if c, _ := env.f.counts(); c != 1 {
				t.Fatalf("accepted creates=%d", c)
			}
		})
	}
}

func TestCrossCellClassification(t *testing.T) {
	cc := &agentHTTPError{Endpoint: "createAgentThread", Status: 500, Code: "MemcachedCrossCellError"}
	old := AppConfig
	AppConfig = DefaultConfig()
	AppConfig.Proxy.AgentThread = true
	t.Cleanup(func() { AppConfig = old })
	for name, err := range map[string]error{
		"raw":         cc,
		"recoverable": &agentRecoverableError{Class: "http_500", ThreadID: "t", Cause: cc},
		"bound":       fmt.Errorf("follow-up failed: %w", cc),
	} {
		f := classifyAgentFailure(err)
		if !strings.HasPrefix(f.Message, "[bridge:notion_cross_cell]") || f.ShouldRetry {
			t.Fatalf("%s: %+v", name, f)
		}
	}
	// A plain 500 keeps the old pending classification.
	plain := classifyAgentFailure(&agentRecoverableError{Class: "http_500", ThreadID: "t", Cause: &agentHTTPError{Endpoint: "createAgentThread", Status: 500}})
	if !strings.HasPrefix(plain.Message, "[bridge:pending]") || !plain.ShouldRetry {
		t.Fatalf("plain 500: %+v", plain)
	}
}

func TestCrossCellDuringQuotaCheckGivesSameHint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(crossCellBody))
	}))
	defer srv.Close()
	oldCfg, oldBase, oldClient := AppConfig, NotionAPIBase, nativeHTTPClient
	AppConfig, NotionAPIBase = DefaultConfig(), srv.URL
	nativeHTTPClient = func(d time.Duration) *http.Client { return &http.Client{Timeout: d} }
	t.Cleanup(func() { AppConfig, NotionAPIBase, nativeHTTPClient = oldCfg, oldBase, oldClient })
	_, err := CheckQuota(roleAcc("q@example.com", "", 0, 100))
	if err == nil || !strings.Contains(err.Error(), "[bridge:notion_cross_cell]") || !strings.Contains(err.Error(), "proxy.notion_api_base") {
		t.Fatalf("quota check must carry the cross-cell hint: %v", err)
	}
	if strings.Contains(err.Error(), "SECRET-UPSTREAM-TEXT") {
		t.Fatalf("upstream message text leaked: %v", err)
	}
	if errors.Is(err, ErrQuotaExhausted) {
		t.Fatal("a routing error is not quota exhaustion")
	}
}
