package proxy

import (
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Sanitized real capture of getCreditRateLimitStatus (main account).
const capturedMainCredit = `{"status":"within_limit","window":{"limit":100,"scope":"per_user","creditType":"basic_ai_credits","window":"6h","used":23.81},"resetsInSeconds":16748,"billingPeriodWindow":{"limit":100,"scope":"per_user","creditType":"basic_ai_credits","cadence":"billing_period","used":5.62,"periodEndMs":1794484800000},"allowances":[{"bucketId":"standard","cadence":"billing_period","state":"resolved","enforcement":"enforced","limit":100,"used":5.62,"periodStartMs":1791806400000,"periodEndMs":1794484800000},{"bucketId":"auto","cadence":"billing_period","state":"not_exposed"},{"bucketId":"flex","cadence":"billing_period","state":"not_exposed"},{"bucketId":"standard","cadence":"rolling","state":"resolved","enforcement":"enforced","window":"6h","limit":100,"used":23.81,"resetsInSeconds":16748}]}`

const capturedEduCredit = `{"status":"not_applicable"}`

var creditT0 = time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

func mustParseCredit(t *testing.T, body string, at time.Time) *CreditStatus {
	t.Helper()
	cs, err := parseCreditRateLimitStatus([]byte(body), at)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return cs
}

func fptr(v float64) *float64 { return &v }

func TestParseCreditStatusCapturedMainAccount(t *testing.T) {
	cs := mustParseCredit(t, capturedMainCredit, creditT0)
	if cs.Status != CreditStatusWithinLimit || cs.Rolling == nil || cs.Billing == nil {
		t.Fatalf("unexpected parse: %+v", cs)
	}
	if *cs.Rolling.Used != 23.81 || *cs.Rolling.Limit != 100 || cs.Rolling.Scope != "per_user" ||
		cs.Rolling.CreditType != "basic_ai_credits" || cs.Rolling.Window != "6h" {
		t.Errorf("rolling window = %+v", cs.Rolling)
	}
	if *cs.ResetsInSeconds != 16748 {
		t.Errorf("resets = %v", *cs.ResetsInSeconds)
	}
	if *cs.Billing.Used != 5.62 || cs.Billing.PeriodEndMs != 1794484800000 || cs.Billing.Cadence != "billing_period" {
		t.Errorf("billing = %+v", cs.Billing)
	}
	if len(cs.Allowances) != 4 {
		t.Fatalf("allowances = %d", len(cs.Allowances))
	}
	if cs.Allowances[1].State != "not_exposed" || cs.Allowances[1].Limit != nil || cs.Allowances[1].Used != nil {
		t.Errorf("not_exposed allowance must keep missing limit/used: %+v", cs.Allowances[1])
	}
	if cs.Allowances[3].Cadence != "rolling" || *cs.Allowances[3].Used != 23.81 {
		t.Errorf("rolling allowance = %+v", cs.Allowances[3])
	}
}

func TestCreditViewMatchesNativeRoundingAndCountdown(t *testing.T) {
	cs := mustParseCredit(t, capturedMainCredit, creditT0)
	v := creditView(cs, creditT0)
	if v.State != CreditStateWithinLimit || !v.CountsAsUse {
		t.Fatalf("state = %s counts=%v", v.State, v.CountsAsUse)
	}
	if *v.Rolling.PercentInt != 24 || *v.Billing.PercentInt != 6 {
		t.Errorf("rounded = %d/%d, want 24/6", *v.Rolling.PercentInt, *v.Billing.PercentInt)
	}
	if *v.Rolling.Percent != 23.81 || *v.Billing.Percent != 5.62 {
		t.Errorf("fractional = %v/%v", *v.Rolling.Percent, *v.Billing.Percent)
	}
	if *v.Rolling.ResetsIn != 16748 {
		t.Errorf("resets in = %d", *v.Rolling.ResetsIn)
	}
	// Countdown advances with wall time, it is not frozen at the observation.
	v2 := creditView(cs, creditT0.Add(1000*time.Second))
	if *v2.Rolling.ResetsIn != 15748 {
		t.Errorf("after 1000s resets in = %d, want 15748", *v2.Rolling.ResetsIn)
	}
	if v.Billing.PeriodEnd != time.UnixMilli(1794484800000).UTC().Format(time.RFC3339) {
		t.Errorf("period end = %s", v.Billing.PeriodEnd)
	}
	if v.RecoverAt != "" || v.Rolling.Full {
		t.Errorf("a 24%% window must not carry a recovery time: %+v", v)
	}
}

func TestCreditStatusExpiredWindowIsNotCurrent(t *testing.T) {
	cs := mustParseCredit(t, capturedMainCredit, creditT0)
	late := creditT0.Add(16749 * time.Second)
	if cs.confirmsUse(late) {
		t.Error("expired rolling window must not confirm use")
	}
	v := creditView(cs, late)
	if v.Rolling == nil || !v.Rolling.Expired || *v.Rolling.ResetsIn != 0 {
		t.Errorf("rolling after reset = %+v", v.Rolling)
	}
	if v.State != CreditStateExpired || v.CountsAsUse {
		t.Errorf("rolling reset passed: state=%s counts=%v", v.State, v.CountsAsUse)
	}
	pastBilling := time.UnixMilli(1794484800000 + 1000).UTC()
	v = creditView(cs, pastBilling)
	if v.State != CreditStateExpired || v.CountsAsUse {
		t.Errorf("state after everything passed = %s counts=%v", v.State, v.CountsAsUse)
	}
}

func TestCreditStatusMissingNullAndUnknown(t *testing.T) {
	// null / missing used+limit are unknown, not 0 percent.
	cs := mustParseCredit(t, `{"status":"within_limit","window":{"limit":null,"scope":"per_user","creditType":"basic_ai_credits","window":"6h"},"resetsInSeconds":60}`, creditT0)
	v := creditView(cs, creditT0)
	if v.Rolling == nil || v.Rolling.Percent != nil || v.Rolling.PercentInt != nil || v.Rolling.Used != nil {
		t.Errorf("missing used/limit must stay unknown: %+v", v.Rolling)
	}
	if _, ok := cs.creditRemaining(creditT0); ok {
		t.Error("no usable remaining may be reported for unknown numbers")
	}
	// zero limit does not divide.
	if _, ok := windowPercent(fptr(1), fptr(0)); ok {
		t.Error("zero limit must be unknown")
	}

	// within_limit without any window object.
	cs = mustParseCredit(t, `{"status":"within_limit"}`, creditT0)
	if got := cs.state(creditT0); got != CreditStateNoWindow || cs.confirmsUse(creditT0) {
		t.Errorf("no-window state = %s", got)
	}

	// an unobserved status is kept raw in the UI; a numerically full, non-expired
	// window confirms exhaustion even when legacy V1 still says eligible=true.
	cs = mustParseCredit(t, `{"status":"limit_reached","window":{"limit":100,"used":100,"scope":"per_user","creditType":"basic_ai_credits","window":"6h"},"resetsInSeconds":600}`, creditT0)
	v = creditView(cs, creditT0)
	if v.State != CreditStateUnknown || v.Status != "limit_reached" || v.CountsAsUse {
		t.Errorf("unknown status view = %+v", v)
	}
	if v.RecoverAt == "" || !v.Rolling.Full {
		t.Errorf("numerically full window should expose the observed reset time: %+v", v)
	}
	if effectiveEligible(true, cs, creditT0) {
		t.Error("numerically full non-expired window must override legacy eligible=true")
	}
	if !effectiveEligible(true, cs, creditT0.Add(601*time.Second)) {
		t.Error("expired full window must fall back to legacy eligible=true")
	}
	if effectiveEligible(false, cs, creditT0) {
		t.Error("unknown status must not make a legacy-ineligible account eligible")
	}
	// an unknown status whose window is NOT full still leaves legacy=true untouched.
	csUnknownNotFull := mustParseCredit(t, `{"status":"limit_reached","window":{"limit":100,"used":20,"scope":"per_user","creditType":"basic_ai_credits","window":"6h"},"resetsInSeconds":600}`, creditT0)
	if !effectiveEligible(true, csUnknownNotFull, creditT0) {
		t.Error("unknown status without a full window must not override legacy eligible=true")
	}

	// Real Notion rate_limited answer omits top-level resetsInSeconds and puts
	// resetsInSeconds only inside the rolling allowance entry.
	csRateLimited := mustParseCredit(t, `{"status":"rate_limited","window":{"limit":100,"used":115.24,"scope":"per_user","creditType":"basic_ai_credits","window":"6h"},"allowances":[{"bucketId":"standard","cadence":"rolling","state":"resolved","enforcement":"enforced","window":"6h","limit":100,"used":115.24,"resetsInSeconds":3914}]}`, creditT0)
	vRL := creditView(csRateLimited, creditT0)
	if vRL.RecoverAt == "" || vRL.Rolling == nil || !vRL.Rolling.Full || vRL.Rolling.ResetsIn == nil || *vRL.Rolling.ResetsIn != 3914 {
		t.Errorf("rate_limited with allowance reset should populate rolling reset & recover_at: %+v, rolling=%+v", vRL, vRL.Rolling)
	}
	if effectiveEligible(true, csRateLimited, creditT0) {
		t.Error("rate_limited with allowance reset must override legacy eligible=true")
	}

	// bodies that are not a status answer are errors, never "no limit".
	for _, body := range []string{``, `[]`, `{}`, `{"status":""}`, `{"status":null}`, `not json`} {
		if _, err := parseCreditRateLimitStatus([]byte(body), creditT0); err == nil {
			t.Errorf("body %q should fail to parse", body)
		}
	}
}

func TestCreditStatusNotApplicableIsNeverFullOrEmpty(t *testing.T) {
	cs := mustParseCredit(t, capturedEduCredit, creditT0)
	v := creditView(cs, creditT0)
	if v.State != CreditStateNotApplicable || v.Rolling != nil || v.Billing != nil || v.CountsAsUse {
		t.Errorf("not_applicable view = %+v", v)
	}
	if _, ok := cs.creditRemaining(creditT0); ok {
		t.Error("not_applicable must not report remaining (no 100% remaining, no unlimited)")
	}
	b, _ := json.Marshal(v)
	if strings.Contains(string(b), `"percent":0`) {
		t.Errorf("not_applicable must not render 0%%: %s", b)
	}
	if effectiveEligible(false, cs, creditT0) != false || effectiveEligible(true, cs, creditT0) != true {
		t.Error("not_applicable must leave the legacy decision unchanged")
	}
}

func TestEffectiveEligibleLegacyBasicZeroDoesNotOverrideWindow(t *testing.T) {
	cs := mustParseCredit(t, capturedMainCredit, creditT0)
	if !effectiveEligible(false, cs, creditT0) {
		t.Error("legacy not-eligible (Basic 0) with a 24% native window must be eligible")
	}
	// Window passed its reset -> stale, falls back to legacy.
	if effectiveEligible(false, cs, creditT0.Add(20000*time.Second)) {
		t.Error("expired window must not keep overriding legacy")
	}
	// Failed refresh -> no override.
	failed := cloneCreditStatus(cs)
	failed.FetchError = "http_500"
	if effectiveEligible(false, failed, creditT0) {
		t.Error("failed refresh must not override legacy")
	}
	// Numerically full window with a within_limit label is exhausted even if legacy=true.
	full := mustParseCredit(t, `{"status":"within_limit","window":{"limit":100,"used":100,"scope":"per_user","creditType":"basic_ai_credits","window":"6h"},"resetsInSeconds":60}`, creditT0)
	if effectiveEligible(false, full, creditT0) || effectiveEligible(true, full, creditT0) {
		t.Error("full rolling window must mark account ineligible even when legacy=true")
	}
	fullBilling := mustParseCredit(t, `{"status":"within_limit","window":{"limit":100,"used":20,"scope":"per_user","creditType":"basic_ai_credits","window":"6h"},"resetsInSeconds":600,"billingPeriodWindow":{"limit":100,"used":100,"scope":"per_user","creditType":"basic_ai_credits","cadence":"billing_period","periodEndMs":1794484800000}}`, creditT0)
	if effectiveEligible(true, fullBilling, creditT0) {
		t.Error("full billing window must mark account ineligible even when legacy=true")
	}
	// A nil snapshot is legacy-only.
	if effectiveEligible(false, nil, creditT0) || !effectiveEligible(true, nil, creditT0) {
		t.Error("nil credit must be legacy-only")
	}
}

// ---- CheckQuota / CheckCreditStatus against a fake Notion API ----

// useFakeNotion points the native endpoints at a plain-HTTP fake server.
func useFakeNotion(t *testing.T, base string) {
	t.Helper()
	oldCfg, oldBase, oldClient := AppConfig, NotionAPIBase, nativeHTTPClient
	AppConfig, NotionAPIBase = DefaultConfig(), base
	nativeHTTPClient = func(d time.Duration) *http.Client { return &http.Client{Timeout: d} }
	t.Cleanup(func() { AppConfig, NotionAPIBase, nativeHTTPClient = oldCfg, oldBase, oldClient })
}

type fakeNotion struct {
	srv         *httptest.Server
	creditBody  string
	creditCode  int
	v1Body      string
	creditCalls int
	creditReq   map[string]interface{}
}

func newFakeNotion(t *testing.T) *fakeNotion {
	t.Helper()
	f := &fakeNotion{creditCode: 200, v1Body: `{"isEligible":true,"spaceUsage":1,"spaceLimit":50,"userUsage":2,"userLimit":50}`}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case "/getAIUsageEligibility":
			io.WriteString(w, f.v1Body)
		case "/getAIUsageEligibilityV2":
			io.WriteString(w, `{"premiumCredits":{"totalCreditBalance":0}}`)
		case "/getCreditRateLimitStatus":
			f.creditCalls++
			_ = json.Unmarshal(data, &f.creditReq)
			w.WriteHeader(f.creditCode)
			io.WriteString(w, f.creditBody)
		default:
			w.WriteHeader(404)
		}
	}))
	useFakeNotion(t, f.srv.URL)
	t.Cleanup(f.srv.Close)
	return f
}

func TestCheckQuotaLegacyBasicZeroWithNativeWindowIsNotExhausted(t *testing.T) {
	f := newFakeNotion(t)
	f.v1Body = `{"isEligible":false,"spaceUsage":50,"spaceLimit":50,"userUsage":50,"userLimit":50}` // legacy "Basic 0"
	f.creditBody = capturedMainCredit
	acc := makeTestAccount("user-a", "a@example.com", "space-a", "A", 0, 0)

	info, err := CheckQuota(acc)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsEligible || info.LegacyEligible {
		t.Errorf("IsEligible=%v LegacyEligible=%v, want true/false", info.IsEligible, info.LegacyEligible)
	}
	if info.Credit == nil || info.Credit.Status != CreditStatusWithinLimit {
		t.Fatalf("credit = %+v", info.Credit)
	}
	if basicRemaining(info) != 0 {
		t.Error("legacy counters must stay legacy (no conversion)")
	}
	// Request is scoped to this account's own workspace and carries only spaceId.
	if len(f.creditReq) != 1 || f.creditReq["spaceId"] != "space-a" {
		t.Errorf("credit request body = %v", f.creditReq)
	}

	// The same legacy answer with the window absent keeps the old verdict.
	f.creditBody = capturedEduCredit
	info, err = CheckQuota(acc)
	if err != nil || info.IsEligible {
		t.Errorf("not_applicable + legacy ineligible: eligible=%v err=%v", info.IsEligible, err)
	}
}

func TestCheckQuotaCreditFailureDoesNotFailOrGuess(t *testing.T) {
	f := newFakeNotion(t)
	f.creditCode = 500
	f.creditBody = `<html>internal secret detail</html>`
	acc := makeTestAccount("user-a", "a@example.com", "space-a", "A", 0, 0)
	info, err := CheckQuota(acc)
	if err != nil {
		t.Fatalf("native window failure must not fail the whole check: %v", err)
	}
	if info.Credit == nil || info.Credit.FetchError != "http_500" || info.Credit.Rolling != nil {
		t.Errorf("credit = %+v", info.Credit)
	}
	if strings.Contains(info.Credit.FetchError, "secret") {
		t.Error("response body leaked into stored error")
	}
	if !info.IsEligible {
		t.Error("legacy decision must stand when the native window failed")
	}
	if got := creditView(info.Credit, time.Now()); got.State != CreditStateFailed || got.Rolling != nil {
		t.Errorf("view = %+v", got)
	}

	f.creditCode, f.creditBody = 200, `{"nope":1}`
	info, _ = CheckQuota(acc)
	if info.Credit.FetchError != "parse" {
		t.Errorf("garbage body error = %q", info.Credit.FetchError)
	}
}

func TestApplyQuotaInfoKeepsLastGoodWindowOnFailedRefresh(t *testing.T) {
	pool := NewAccountPool()
	acc := makeTestAccount("user-a", "a@example.com", "space-a", "A", 0, 50)
	pool.AddAccount(acc)
	good := mustParseCredit(t, capturedMainCredit, time.Now())
	pool.applyQuotaInfo(acc, &QuotaInfo{IsEligible: true, Credit: good})

	pool.applyQuotaInfo(acc, &QuotaInfo{IsEligible: true, LegacyEligible: true,
		Credit: &CreditStatus{FetchError: "network", FetchErrorAt: time.Now()}})
	got := acc.quotaSnapshot().Info.Credit
	if got.FetchError != "network" || got.Rolling == nil || *got.Rolling.Used != 23.81 {
		t.Errorf("previous snapshot must be kept next to the error: %+v", got)
	}
	v := creditView(got, time.Now())
	if v.State != CreditStateFailed || v.Rolling == nil || *v.Rolling.PercentInt != 24 || v.CountsAsUse {
		t.Errorf("failed view must show last data but not confirm use: %+v", v)
	}
	// A later success replaces it completely and clears the error.
	pool.applyQuotaInfo(acc, &QuotaInfo{IsEligible: true, Credit: mustParseCredit(t, capturedEduCredit, time.Now())})
	got = acc.quotaSnapshot().Info.Credit
	if got.FetchError != "" || got.Rolling != nil || got.Status != CreditStatusNotApplicable {
		t.Errorf("success must replace the snapshot: %+v", got)
	}
}

func TestApplyQuotaInfoWindowClearsStaleLegacyPermanentExhaustion(t *testing.T) {
	pool := NewAccountPool()
	acc := makeTestAccount("user-a", "a@example.com", "space-a", "A", 50, 50)
	acc.PlanType = "free"
	pool.AddAccount(acc)
	// Legacy says Basic 0 / not eligible -> free account is permanently disabled.
	pool.applyQuotaInfo(acc, &QuotaInfo{IsEligible: false, SpaceLimit: 50, SpaceUsage: 50})
	if !acc.quotaSnapshot().PermanentlyExhausted {
		t.Fatal("setup: expected legacy permanent exhaustion")
	}
	// An account never cross-checked against the native window must be re-queried.
	if creditSettled(acc.quotaSnapshot().Info) {
		t.Error("account without a native answer must not be skipped by the refresh loop")
	}
	// Window answers within_limit -> it is usable again.
	pool.applyQuotaInfo(acc, &QuotaInfo{IsEligible: true, LegacyEligible: false,
		SpaceLimit: 50, SpaceUsage: 50, Credit: mustParseCredit(t, capturedMainCredit, time.Now())})
	q := acc.quotaSnapshot()
	if q.PermanentlyExhausted || q.ExhaustedAt != nil || pool.AvailableCount() != 1 {
		t.Errorf("confirmed window must clear legacy exhaustion: %+v avail=%d", q, pool.AvailableCount())
	}
}

func TestApplyQuotaInfoNotApplicableKeepsLegacyAndIsSettled(t *testing.T) {
	pool := NewAccountPool()
	acc := makeTestAccount("user-e", "e@example.edu", "space-e", "E", 50, 50)
	acc.PlanType = "free"
	pool.AddAccount(acc)
	info := &QuotaInfo{IsEligible: false, SpaceLimit: 50, SpaceUsage: 50, Credit: mustParseCredit(t, capturedEduCredit, time.Now())}
	pool.applyQuotaInfo(acc, info)
	q := acc.quotaSnapshot()
	if !q.PermanentlyExhausted {
		t.Error("not_applicable must not change the legacy verdict either way")
	}
	if !creditSettled(q.Info) {
		t.Error("answered not_applicable account may be skipped by the refresh loop")
	}
	// Role is a user routing choice, never derived from the answer.
	if acc.roleSnapshot() != "" {
		t.Errorf("role changed automatically: %q", acc.roleSnapshot())
	}
}

func TestQuotaPriorityUsesNativeRemainingOnlyWhenConfirmed(t *testing.T) {
	acc := makeTestAccount("user-a", "a@example.com", "space-a", "A", 50, 50) // legacy remaining 0
	acc.QuotaInfo.Credit = mustParseCredit(t, capturedMainCredit, time.Now())
	if r := accountQuotaRank(acc); r.layer != rankNative || r.plan != 76 { // min(100-23.81, 100-5.62) floored
		t.Errorf("rank = %+v, want native/76", r)
	}
	acc.QuotaInfo.Credit = mustParseCredit(t, capturedEduCredit, time.Now())
	if r := accountQuotaRank(acc); r.layer != rankLegacy || r.plan != 0 {
		t.Errorf("not_applicable rank = %+v, want legacy 0", r)
	}
	acc.QuotaInfo.Credit = &CreditStatus{FetchError: "network"}
	if r := accountQuotaRank(acc); r.layer != rankLegacy || r.plan != 0 {
		t.Errorf("failed rank = %+v, want legacy 0", r)
	}
}

// ---- persistence ----

func TestCreditStatusPersistenceRoundTripAndLegacyFiles(t *testing.T) {
	dir := t.TempDir()
	acc := makeTestAccount("user-a", "a@example.com", "space-a", "A", 5, 50)
	observed := time.Date(2026, 7, 1, 12, 0, 0, 123, time.UTC)
	acc.QuotaInfo.Credit = mustParseCredit(t, capturedMainCredit, observed)
	acc.QuotaInfo.Credit.FetchError = "http_503"
	acc.QuotaInfo.Credit.FetchErrorAt = observed.Add(time.Minute)
	acc.QuotaInfo.LegacyEligible = true
	if _, err := SaveAccountToFile(acc, dir); err != nil {
		t.Fatal(err)
	}
	pool := NewAccountPool()
	pool.AddAccount(acc)
	pool.SaveAccounts(dir)

	loaded := NewAccountPool()
	if err := loaded.LoadFromDir(dir); err != nil {
		t.Fatal(err)
	}
	got := loaded.FindByAccountID(acc.AccountID)
	if got == nil || got.QuotaInfo == nil || got.QuotaInfo.Credit == nil {
		t.Fatalf("credit not restored: %+v", got)
	}
	c := got.QuotaInfo.Credit
	if c.Status != CreditStatusWithinLimit || *c.Rolling.Used != 23.81 || *c.ResetsInSeconds != 16748 ||
		c.Billing.PeriodEndMs != 1794484800000 || len(c.Allowances) != 4 || c.FetchError != "http_503" ||
		!c.ObservedAt.Equal(observed) {
		t.Errorf("round trip lost data: %+v", c)
	}
	// countdown continues from the persisted observation time.
	v := creditView(c, observed.Add(100*time.Second))
	if *v.Rolling.ResetsIn != 16648 {
		t.Errorf("resets in after reload = %d", *v.Rolling.ResetsIn)
	}

	// A file written before native windows existed still loads (credit nil).
	old := []byte(`{"quota_info":{"is_eligible":true,"space_usage":3,"space_limit":50}}`)
	qi := loadPersistedQuotaInfo(old)
	if qi == nil || qi.Credit != nil || !qi.IsEligible || !qi.LegacyEligible {
		t.Errorf("legacy file = %+v", qi)
	}
	if view := creditView(qi.Credit, time.Now()); view.State != CreditStateUnchecked || view.Rolling != nil {
		t.Errorf("unchecked view = %+v", view)
	}
}

// ---- API detail + summary ----

func TestAccountDetailsExposeCreditSourceAndDedupeKeys(t *testing.T) {
	pool := NewAccountPool()
	a := makeTestAccount("user-a", "a@example.com", "space-shared", "Shared", 1, 50)
	b := makeTestAccount("user-b", "b@example.com", "space-shared", "Shared", 1, 50)
	a.QuotaInfo.Credit = mustParseCredit(t, capturedMainCredit, time.Now())
	pool.AddAccount(a)
	pool.AddAccount(b)
	details := pool.GetAccountDetails()
	if len(details) != 2 {
		t.Fatalf("details = %d", len(details))
	}
	byEmail := map[string]map[string]interface{}{}
	for _, d := range details {
		byEmail[d["email"].(string)] = d
	}
	da, db := byEmail["a@example.com"], byEmail["b@example.com"]
	if da["workspace_key"] == "" || da["workspace_key"] != db["workspace_key"] || da["user_key"] == db["user_key"] {
		t.Errorf("dedupe keys wrong: %v %v / %v %v", da["workspace_key"], da["user_key"], db["workspace_key"], db["user_key"])
	}
	for _, k := range []string{"workspace_key", "user_key"} {
		if s, _ := da[k].(string); strings.Contains(s, "space") || strings.Contains(s, "user-a") {
			t.Errorf("%s leaks raw id: %s", k, s)
		}
	}
	if da["legacy_source"] != "getAIUsageEligibility" {
		t.Errorf("legacy fields must be source-labelled: %v", da["legacy_source"])
	}
	if cv := da["credit"].(*CreditView); cv.State != CreditStateWithinLimit {
		t.Errorf("a credit = %+v", cv)
	}
	if cv := db["credit"].(*CreditView); cv.State != CreditStateUnchecked || cv.Rolling != nil {
		t.Errorf("b credit must be 'unchecked', not 0%%: %+v", cv)
	}
	// /health summary carries only masked emails.
	for _, e := range pool.GetQuotaSummary() {
		if strings.Contains(e["email"].(string), "a@example.com") || strings.Contains(e["email"].(string), "b@example.com") {
			t.Errorf("health leaks email: %v", e["email"])
		}
		if _, ok := e["credit"].(*CreditView); !ok {
			t.Error("health summary missing credit")
		}
	}
}

func summaryEntry(ws, user string, cv *CreditView, extra map[string]interface{}) map[string]interface{} {
	m := map[string]interface{}{"workspace_key": ws, "user_key": user, "credit": cv}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func TestSummaryDedupesSharedWorkspaceAndNeverMixesScopes(t *testing.T) {
	main := creditView(mustParseCredit(t, capturedMainCredit, creditT0), creditT0)
	second := creditView(mustParseCredit(t, strings.ReplaceAll(strings.ReplaceAll(capturedMainCredit, "23.81", "50"), "5.62", "10"), creditT0), creditT0)
	edu := creditView(mustParseCredit(t, capturedEduCredit, creditT0), creditT0)
	unknownScope := creditView(mustParseCredit(t, strings.ReplaceAll(capturedMainCredit, "per_user", "per_seat"), creditT0), creditT0)
	failed := &CreditView{State: CreditStateFailed}
	expired := creditView(mustParseCredit(t, capturedMainCredit, creditT0), creditT0.Add(20000*time.Second))

	legacy := func(space, user, remaining, premium int) map[string]interface{} {
		return map[string]interface{}{"space_usage": space, "space_limit": 50, "space_remaining": 50 - space,
			"user_usage": user, "user_limit": 50, "user_remaining": 50 - user, "remaining": remaining, "premium_balance": premium, "premium_limit": 300}
	}
	accounts := []map[string]interface{}{
		// two accounts of different users in the SAME workspace
		summaryEntry("wsA", "u1", main, legacy(10, 5, 40, 300)),
		summaryEntry("wsA", "u2", second, legacy(10, 8, 40, 300)),
		// same user (u1) in another workspace: per_user window is not double counted
		summaryEntry("wsB", "u1", main, legacy(0, 5, 45, 0)),
		summaryEntry("wsE", "u3", edu, legacy(50, 50, 0, 0)),
		summaryEntry("wsF", "u4", unknownScope, nil),
		summaryEntry("wsG", "u5", failed, nil),
		summaryEntry("wsH", "u6", expired, nil),
		{"workspace_key": "wsI", "user_key": "u7"}, // no credit at all
	}
	s := summarizeAccounts(accounts)

	// legacy: shared workspace counted once
	if s.TotalSpaceUsage != 10+0+50 || s.TotalPremiumBalance != 300 {
		t.Errorf("legacy space usage=%d premium=%d (shared workspace double counted?)", s.TotalSpaceUsage, s.TotalPremiumBalance)
	}
	if s.TotalUserUsage != 5+8+50 { // u1 once
		t.Errorf("user usage = %d", s.TotalUserUsage)
	}
	wantStates := map[string]int{CreditStateWithinLimit: 4, CreditStateNotApplicable: 1, CreditStateFailed: 1, CreditStateExpired: 1, CreditStateUnchecked: 1}
	if s.Credit.ByState[CreditStateNotApplicable] != 1 || s.Credit.ByState[CreditStateFailed] != 1 || s.Credit.ByState[CreditStateUnchecked] != 1 ||
		s.Credit.ByState[CreditStateExpired] != 1 || s.Credit.ByState[CreditStateWithinLimit] != 4 {
		t.Errorf("by_state = %v want %v", s.Credit.ByState, wantStates)
	}
	var rolling, billing *CreditPool
	var seats []CreditPool
	for i := range s.Credit.Pools {
		p := &s.Credit.Pools[i]
		switch {
		case p.Scope == "per_seat":
			seats = append(seats, *p)
		case p.Kind == "rolling":
			rolling = p
		case p.Kind == "billing":
			billing = p
		}
	}
	if rolling == nil || billing == nil {
		t.Fatalf("pools = %+v", s.Credit.Pools)
	}
	// rolling windows reset on independent clocks: counted, never summed.
	if rolling.Accounts != 2 || rolling.Summed != 0 || rolling.Used != nil || rolling.Limit != nil || rolling.Percent != nil {
		t.Errorf("rolling pool must not be summed: %+v", rolling)
	}
	if rolling.MaxPercent == nil || math.Abs(*rolling.MaxPercent-50) > 1e-9 {
		t.Errorf("rolling max percent = %v", rolling.MaxPercent)
	}
	if billing.Summed != 2 || math.Abs(*billing.Used-15.62) > 1e-9 {
		t.Errorf("billing pool = %+v", billing)
	}
	if len(seats) != 2 {
		t.Errorf("unknown scope must form its own pools: %+v", seats)
	}
	for _, seat := range seats {
		if seat.Used != nil || seat.Limit != nil || seat.Percent != nil || seat.Accounts != 1 {
			t.Errorf("unknown scope must be counted but never summed: %+v", seat)
		}
	}
	// Pools never mix units: different kinds/windows are separate entries.
	for _, p := range s.Credit.Pools {
		if p.Kind == "rolling" && p.Window != "6h" && p.Scope == "per_user" {
			t.Errorf("rolling pool without window: %+v", p)
		}
	}
	if s.AvailableAccounts != len(accounts) || s.TotalAccounts != len(accounts) {
		t.Errorf("available=%d total=%d", s.AvailableAccounts, s.TotalAccounts)
	}
}

func TestSummaryWithoutDedupeKeysKeepsLegacyBehavior(t *testing.T) {
	s := summarizeAccounts([]map[string]interface{}{
		{"space_usage": 1, "premium_balance": 10},
		{"space_usage": 2, "premium_balance": 20},
	})
	if s.TotalSpaceUsage != 3 || s.TotalPremiumBalance != 30 {
		t.Errorf("unkeyed accounts cannot be proven duplicates: %+v", s)
	}
	if s.Credit.ByState[CreditStateUnchecked] != 2 || len(s.Credit.Pools) != 0 {
		t.Errorf("credit summary = %+v", s.Credit)
	}
}

// ---- email backfill ----

const userContentBody = `{"recordMap":{"notion_user":{
 "user-other":{"value":{"value":{"id":"user-other","email":"other@example.com"}}},
 "user-a":{"value":{"value":{"id":"user-a","email":"own@example.com"}}}},
 "user_root":{"user-a":{"value":{"value":{"space_views":["v"]}}}}}}`

func TestExtractUserEmailMatchesOnlyOwnUserRecord(t *testing.T) {
	if got := extractUserEmail([]byte(userContentBody), "user-a"); got != "own@example.com" {
		t.Errorf("own email = %q", got)
	}
	if got := extractUserEmail([]byte(userContentBody), "user-missing"); got != "" {
		t.Errorf("unknown user must not borrow another record: %q", got)
	}
	if got := extractUserEmail([]byte(userContentBody), ""); got != "" {
		t.Errorf("empty user id = %q", got)
	}
	flat := `{"recordMap":{"notion_user":{"user-a":{"value":{"email":"flat@example.com"}}}}}`
	if got := extractUserEmail([]byte(flat), "user-a"); got != "flat@example.com" {
		t.Errorf("flat shape = %q", got)
	}
	noEmail := `{"recordMap":{"notion_user":{"user-a":{"value":{"value":{"id":"user-a"}}}}}}`
	if got := extractUserEmail([]byte(noEmail), "user-a"); got != "" {
		t.Errorf("record without email = %q", got)
	}
	if got := extractUserEmail([]byte(`garbage`), "user-a"); got != "" {
		t.Errorf("garbage = %q", got)
	}
}

func TestFetchAccountEmailUsesOwnCredentialsAndRecord(t *testing.T) {
	var cookie, active string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, active = r.Header.Get("Cookie"), r.Header.Get("x-notion-active-user-header")
		io.WriteString(w, userContentBody)
	}))
	defer srv.Close()
	useFakeNotion(t, srv.URL)

	acc := makeTestAccount("user-a", "", "space-a", "A", 0, 0)
	email, err := FetchAccountEmail(acc)
	if err != nil || email != "own@example.com" {
		t.Fatalf("email=%q err=%v", email, err)
	}
	if active != "user-a" || !strings.Contains(cookie, acc.TokenV2) {
		t.Error("request must be signed as this account")
	}
	acc2 := makeTestAccount("user-zzz", "", "space-z", "Z", 0, 0)
	if email, err = FetchAccountEmail(acc2); err == nil || email != "" || err.Error() != "not_found" {
		t.Errorf("unknown identity must stay unknown: %q %v", email, err)
	}
}

func TestBackfillAccountEmailOnlyFillsEmptyAndPersists(t *testing.T) {
	old := emailFetcher
	defer func() { emailFetcher = old }()
	calls := 0
	emailFetcher = func(acc *Account) (string, error) { calls++; return "own@example.com", nil }

	dir := t.TempDir()
	pool := NewAccountPool()
	acc := makeTestAccount("user-a", "", "space-a", "A", 0, 0)
	pool.AddAccount(acc)
	if _, err := SaveAccountToFile(acc, dir); err != nil {
		t.Fatal(err)
	}
	if !pool.backfillAccountEmail(acc) || acc.UserEmail != "own@example.com" || calls != 1 {
		t.Fatalf("backfill: email=%q calls=%d", acc.UserEmail, calls)
	}
	idBefore := acc.AccountID
	// Existing email is never overwritten or even looked up.
	other := makeTestAccount("user-b", "keep@example.com", "space-b", "B", 0, 0)
	pool.AddAccount(other)
	if pool.backfillAccountEmail(other) || other.UserEmail != "keep@example.com" || calls != 1 {
		t.Error("existing email must not be touched")
	}
	pool.SaveAccounts(dir)
	var found bool
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		raw := readJSONFile(t, filepath.Join(dir, e.Name()))
		if raw["user_id"] == "user-a" {
			found = true
			if raw["user_email"] != "own@example.com" || raw["account_id"] != idBefore {
				t.Errorf("persisted identity wrong: email=%v id=%v", raw["user_email"], raw["account_id"])
			}
		}
	}
	if !found {
		t.Fatal("account file for user-a not found")
	}

	// Failure leaves it unknown; no account without a user id is probed.
	emailFetcher = func(acc *Account) (string, error) { return "", errors.New("http_500") }
	c := makeTestAccount("user-c", "", "space-c", "C", 0, 0)
	pool.AddAccount(c)
	if pool.backfillAccountEmail(c) || c.UserEmail != "" {
		t.Error("failed lookup must leave email unknown")
	}
	emailFetcher = func(acc *Account) (string, error) { t.Fatal("must not be called"); return "", nil }
	d := makeTestAccount("", "", "space-d", "D", 0, 0)
	if pool.backfillAccountEmail(d) {
		t.Error("account without user id has no authoritative record")
	}
}

// ---- failover stays quota-confirmed ----

func TestQuotaConfirmedFailoverIgnoresLegacyBasicZeroWhenWindowWithinLimit(t *testing.T) {
	old := quotaFetcher
	defer func() { quotaFetcher = old }()
	acc := makeTestAccount("user-a", "a@example.com", "space-a", "A", 50, 50)
	rateErr := &agentHTTPError{Endpoint: "sendEvent", Status: 429}
	cs := mustParseCredit(t, capturedMainCredit, time.Now())

	// legacy ineligible but native window within limit -> not confirmed exhausted
	quotaFetcher = func(*Account) (*QuotaInfo, error) {
		return &QuotaInfo{IsEligible: effectiveEligible(false, cs, time.Now()), Credit: cs}, nil
	}
	if err := classifyAgentQuotaFailure(acc, rateErr); errors.Is(err, ErrQuotaExhausted) {
		t.Error("window within limit must not be treated as quota exhaustion")
	}
	// API failure -> unchanged error (no auto failover)
	quotaFetcher = func(*Account) (*QuotaInfo, error) { return nil, errors.New("down") }
	if err := classifyAgentQuotaFailure(acc, rateErr); errors.Is(err, ErrQuotaExhausted) {
		t.Error("quota API failure must not confirm exhaustion")
	}
	// not_applicable + legacy ineligible -> still the authoritative legacy verdict
	na := mustParseCredit(t, capturedEduCredit, time.Now())
	quotaFetcher = func(*Account) (*QuotaInfo, error) {
		return &QuotaInfo{IsEligible: effectiveEligible(false, na, time.Now()), Credit: na}, nil
	}
	if err := classifyAgentQuotaFailure(acc, rateErr); !errors.Is(err, ErrQuotaExhausted) {
		t.Errorf("legacy verdict (not_applicable, ineligible) must still confirm exhaustion: %v", err)
	}
}

// ---- review round 2: counterexamples (written before the fixes) ----

func TestIncompleteWithinLimitWindowCannotConfirmUse(t *testing.T) {
	cases := map[string]string{
		"missing used":     `{"status":"within_limit","window":{"limit":100,"scope":"per_user","creditType":"basic_ai_credits","window":"6h"},"resetsInSeconds":600}`,
		"limit zero":       `{"status":"within_limit","window":{"used":24,"limit":0,"scope":"per_user","creditType":"basic_ai_credits","window":"6h"},"resetsInSeconds":600}`,
		"no reset time":    `{"status":"within_limit","window":{"used":24,"limit":100,"scope":"per_user","creditType":"basic_ai_credits","window":"6h"}}`,
		"negative reset":   `{"status":"within_limit","window":{"used":24,"limit":100,"scope":"per_user","creditType":"basic_ai_credits","window":"6h"},"resetsInSeconds":-5}`,
		"negative used":    `{"status":"within_limit","window":{"used":-1,"limit":100,"scope":"per_user","creditType":"basic_ai_credits","window":"6h"},"resetsInSeconds":600}`,
		"billing no used":  `{"status":"within_limit","window":{"used":24,"limit":100,"scope":"per_user","creditType":"basic_ai_credits","window":"6h"},"resetsInSeconds":600,"billingPeriodWindow":{"limit":100,"scope":"per_user","creditType":"basic_ai_credits","cadence":"billing_period","periodEndMs":1794484800000}}`,
		"billing no end":   `{"status":"within_limit","window":{"used":24,"limit":100,"scope":"per_user","creditType":"basic_ai_credits","window":"6h"},"resetsInSeconds":600,"billingPeriodWindow":{"used":5,"limit":100,"scope":"per_user","creditType":"basic_ai_credits","cadence":"billing_period"}}`,
		"billing zero lim": `{"status":"within_limit","window":{"used":24,"limit":100,"scope":"per_user","creditType":"basic_ai_credits","window":"6h"},"resetsInSeconds":600,"billingPeriodWindow":{"used":5,"limit":0,"scope":"per_user","creditType":"basic_ai_credits","cadence":"billing_period","periodEndMs":1794484800000}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			cs := mustParseCredit(t, body, creditT0)
			if cs.confirmsUse(creditT0) {
				t.Error("incomplete window must not confirm use")
			}
			if effectiveEligible(false, cs, creditT0) {
				t.Error("incomplete window must not override legacy eligible=false")
			}
			if _, ok := cs.creditRemaining(creditT0); ok {
				t.Error("incomplete window must not feed routing priority")
			}
			v := creditView(cs, creditT0)
			if v.CountsAsUse || v.State == CreditStateWithinLimit {
				t.Errorf("UI state must agree with routing: state=%s counts=%v", v.State, v.CountsAsUse)
			}
			if v.State != CreditStateIncomplete {
				t.Errorf("state = %s, want incomplete", v.State)
			}
		})
	}
	// control: the full captured sample still confirms and shows within_limit
	cs := mustParseCredit(t, capturedMainCredit, creditT0)
	if v := creditView(cs, creditT0); !v.CountsAsUse || v.State != CreditStateWithinLimit {
		t.Errorf("control: %+v", v)
	}
	// ObservedAt unknown => reset cannot be verified
	cs = mustParseCredit(t, capturedMainCredit, time.Time{})
	if cs.confirmsUse(creditT0) {
		t.Error("unknown observation time must not confirm use")
	}
	// legacy=true stays true for every incomplete shape
	cs = mustParseCredit(t, cases["limit zero"], creditT0)
	if !effectiveEligible(true, cs, creditT0) {
		t.Error("incomplete window must leave legacy=true untouched")
	}
}

func TestSummaryDifferentBillingPeriodsAreNotAggregated(t *testing.T) {
	mk := func(endMs string) *CreditView {
		body := strings.ReplaceAll(capturedMainCredit, "1794484800000", endMs)
		return creditView(mustParseCredit(t, body, creditT0), creditT0)
	}
	a := mk("1794484800000")
	b := mk("1795262400000") // 9 days later
	same := mk("1794484800000")
	s := summarizeAccounts([]map[string]interface{}{
		summaryEntry("w1", "u1", a, nil),
		summaryEntry("w2", "u2", b, nil),
		summaryEntry("w3", "u3", same, nil),
	})
	var billing []CreditPool
	for _, p := range s.Credit.Pools {
		if p.Kind == "billing" {
			billing = append(billing, p)
		}
		if p.Kind == "rolling" && (p.Used != nil || p.Limit != nil || p.Percent != nil) {
			t.Errorf("rolling windows reset on independent clocks and must never be summed: %+v", p)
		}
	}
	if len(billing) != 2 {
		t.Fatalf("billing pools = %+v", billing)
	}
	for _, p := range billing {
		if p.PeriodEnd == "" {
			t.Errorf("billing pool must carry its period: %+v", p)
		}
		switch p.Summed {
		case 2: // u1 + u3 share the same period end
			if *p.Used != 11.24 && math.Abs(*p.Used-11.24) > 1e-9 {
				t.Errorf("same-period used = %v", *p.Used)
			}
		case 1:
			if math.Abs(*p.Used-5.62) > 1e-9 {
				t.Errorf("single-period used = %v", *p.Used)
			}
		default:
			t.Errorf("unexpected Summed: %+v", p)
		}
	}
}

func TestRoutingRankIsLayeredNotSummedAcrossUnits(t *testing.T) {
	native := func(email string, usedPct float64, premium int) *Account {
		acc := makeTestAccount("u-"+email, email, "s-"+email, email, 50, 50)
		body := strings.ReplaceAll(capturedMainCredit, "23.81", strings.TrimRight(strings.TrimRight(fmtFloat(usedPct), "0"), "."))
		acc.QuotaInfo.Credit = mustParseCredit(t, body, time.Now())
		if premium > 0 {
			acc.QuotaInfo.HasPremium, acc.QuotaInfo.PremiumBalance, acc.QuotaInfo.PremiumLimit = true, premium, premium
		}
		return acc
	}
	lowPlanBigCredits := native("a@x.test", 90, 300) // 10 plan credits left, 300 Notion credits
	highPlanNoCredits := native("b@x.test", 20, 0)   // 80 plan credits left
	if !accountQuotaRank(highPlanNoCredits).better(accountQuotaRank(lowPlanBigCredits)) {
		t.Errorf("plan allowance must outrank Notion credits: %+v vs %+v", accountQuotaRank(highPlanNoCredits), accountQuotaRank(lowPlanBigCredits))
	}
	// credits only break an exact plan tie
	t1, t2 := native("c@x.test", 20, 300), native("d@x.test", 20, 0)
	if !accountQuotaRank(t1).better(accountQuotaRank(t2)) || accountQuotaRank(t2).better(accountQuotaRank(t1)) {
		t.Error("credits must break plan ties")
	}
	// a confirmed native window is a different layer from legacy counters
	legacy := makeTestAccount("u-l", "l@x.test", "s-l", "L", 0, 500) // legacy remaining 500
	if accountQuotaRank(highPlanNoCredits).better(accountQuotaRank(legacy)) != true {
		t.Error("confirmed native layer should rank above unverified legacy counters regardless of magnitude")
	}
	// the pool picks accordingly
	pool := NewAccountPool()
	pool.AddAccount(lowPlanBigCredits)
	pool.AddAccount(highPlanNoCredits)
	for i := 0; i < 4; i++ {
		if got := pool.NextBest(); got != highPlanNoCredits {
			t.Fatalf("NextBest picked %s", got.UserEmail)
		}
	}
}

func fmtFloat(f float64) string { return strconv.FormatFloat(f, 'f', 2, 64) }
