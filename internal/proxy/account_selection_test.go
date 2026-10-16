package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- merged from account_selection_test.go ---
func TestNextSkipsIneligibleAccounts(t *testing.T) {
	pool := &AccountPool{
		accounts: []*Account{
			{
				PlanType:  "personal",
				UserEmail: "blocked@example.com",
				QuotaInfo: &QuotaInfo{
					IsEligible:     false,
					HasPremium:     true,
					PremiumBalance: 1300000,
					PremiumLimit:   1300000,
				},
			},
			{
				UserEmail: "eligible@example.com",
				QuotaInfo: &QuotaInfo{
					IsEligible: true,
				},
			},
		},
	}

	got := pool.Next()
	if got == nil || got.UserEmail != "eligible@example.com" {
		t.Fatalf("expected eligible account, got %#v", got)
	}
}

func TestGetBestAccountPrefersEligibleAccount(t *testing.T) {
	pool := &AccountPool{
		accounts: []*Account{
			{
				PlanType:  "personal",
				UserEmail: "blocked@example.com",
				QuotaInfo: &QuotaInfo{
					IsEligible:     false,
					HasPremium:     true,
					PremiumBalance: 1300000,
					PremiumLimit:   1300000,
					SpaceLimit:     200,
					SpaceUsage:     180,
				},
			},
			{
				UserEmail: "eligible@example.com",
				QuotaInfo: &QuotaInfo{
					IsEligible: true,
					SpaceLimit: 200,
					SpaceUsage: 20,
				},
			},
		},
	}

	got := pool.GetBestAccount()
	if got == nil || got.UserEmail != "eligible@example.com" {
		t.Fatalf("expected eligible account, got %#v", got)
	}
}

func TestGetBestAccountReturnsNilWhenOnlyIneligibleAccounts(t *testing.T) {
	pool := &AccountPool{
		accounts: []*Account{
			{
				PlanType:  "business",
				UserEmail: "blocked@example.com",
				QuotaInfo: &QuotaInfo{
					IsEligible:     false,
					HasPremium:     true,
					PremiumBalance: 1300000,
					PremiumLimit:   1300000,
				},
			},
		},
	}

	if got := pool.GetBestAccount(); got != nil {
		t.Fatalf("expected nil, got %#v", got)
	}
}

func TestIsQuotaExhaustedUsesEligibilityFlag(t *testing.T) {
	pool := &AccountPool{
		accounts: []*Account{
			{
				PlanType:  "personal",
				UserEmail: "personal@example.com",
				QuotaInfo: &QuotaInfo{
					IsEligible:     false,
					HasPremium:     true,
					PremiumBalance: 1300000,
					PremiumLimit:   1300000,
				},
			},
		},
	}

	if got := pool.isQuotaExhausted(pool.accounts[0]); !got {
		t.Fatalf("expected account with is_eligible=false to be exhausted")
	}
}

func TestGetBestAccountUsesEffectiveRemaining(t *testing.T) {
	pool := &AccountPool{
		accounts: []*Account{
			{
				UserEmail: "space-rich-user-poor@example.com",
				QuotaInfo: &QuotaInfo{
					IsEligible: true,
					SpaceLimit: 200,
					SpaceUsage: 20,
					UserLimit:  200,
					UserUsage:  190,
				},
			},
			{
				UserEmail: "balanced@example.com",
				QuotaInfo: &QuotaInfo{
					IsEligible: true,
					SpaceLimit: 200,
					SpaceUsage: 60,
					UserLimit:  200,
					UserUsage:  60,
				},
			},
		},
	}

	got := pool.GetBestAccount()
	if got == nil || got.UserEmail != "balanced@example.com" {
		t.Fatalf("expected account with higher effective remaining, got %#v", got)
	}
}

func TestNextRoundRobinsRegardlessOfQuota(t *testing.T) {
	// Next() should rotate through accounts regardless of remaining quota.
	pool := &AccountPool{
		accounts: []*Account{
			{
				UserEmail: "low@example.com",
				QuotaInfo: &QuotaInfo{
					IsEligible: true,
					SpaceLimit: 200,
					SpaceUsage: 160,
					UserLimit:  200,
					UserUsage:  160,
				},
			},
			{
				UserEmail: "high@example.com",
				QuotaInfo: &QuotaInfo{
					IsEligible: true,
					SpaceLimit: 200,
					SpaceUsage: 40,
					UserLimit:  200,
					UserUsage:  40,
				},
			},
		},
	}

	first := pool.Next()
	second := pool.Next()
	if first == nil || second == nil {
		t.Fatal("expected non-nil accounts")
	}
	if first.UserEmail == second.UserEmail {
		t.Fatalf("expected different accounts on consecutive calls, both got %s", first.UserEmail)
	}
}

func TestNextExcludingRoundRobinsSkippingExcluded(t *testing.T) {
	low := &Account{
		UserEmail: "low@example.com",
		QuotaInfo: &QuotaInfo{
			IsEligible: true,
			SpaceLimit: 200,
			SpaceUsage: 170,
			UserLimit:  200,
			UserUsage:  170,
		},
	}
	mid := &Account{
		UserEmail: "mid@example.com",
		QuotaInfo: &QuotaInfo{
			IsEligible: true,
			SpaceLimit: 200,
			SpaceUsage: 80,
			UserLimit:  200,
			UserUsage:  80,
		},
	}
	high := &Account{
		UserEmail: "high@example.com",
		QuotaInfo: &QuotaInfo{
			IsEligible: true,
			SpaceLimit: 200,
			SpaceUsage: 20,
			UserLimit:  200,
			UserUsage:  20,
		},
	}
	pool := &AccountPool{
		accounts: []*Account{low, mid, high},
	}

	// With high excluded, NextExcluding should rotate between low and mid
	exclude := map[*Account]bool{high: true}
	seen := map[string]int{}
	for i := 0; i < 4; i++ {
		got := pool.NextExcluding(exclude)
		if got == nil {
			t.Fatal("expected non-nil account")
		}
		if got.UserEmail == "high@example.com" {
			t.Fatal("excluded account should not be returned")
		}
		seen[got.UserEmail]++
	}
	if seen["low@example.com"] == 0 || seen["mid@example.com"] == 0 {
		t.Fatalf("expected both low and mid to appear, got: %v", seen)
	}
}

func TestNextForResearchAllowsPremiumAtLimit(t *testing.T) {
	pool := &AccountPool{
		accounts: []*Account{
			{
				UserEmail: "premium@example.com",
				QuotaInfo: &QuotaInfo{
					IsEligible:        true,
					HasPremium:        true,
					ResearchModeUsage: 3,
				},
			},
		},
	}

	got := pool.NextForResearch()
	if got == nil || got.UserEmail != "premium@example.com" {
		t.Fatalf("expected premium account to remain research-capable, got %#v", got)
	}
}

// ── Round-Robin distribution tests ──

func TestNextDistributesEvenly(t *testing.T) {
	// Three accounts with identical quota — Next() must rotate across all three.
	pool := &AccountPool{
		accounts: []*Account{
			{UserEmail: "a@example.com", QuotaInfo: &QuotaInfo{IsEligible: true, SpaceLimit: 200, SpaceUsage: 100, UserLimit: 200, UserUsage: 100}},
			{UserEmail: "b@example.com", QuotaInfo: &QuotaInfo{IsEligible: true, SpaceLimit: 200, SpaceUsage: 100, UserLimit: 200, UserUsage: 100}},
			{UserEmail: "c@example.com", QuotaInfo: &QuotaInfo{IsEligible: true, SpaceLimit: 200, SpaceUsage: 100, UserLimit: 200, UserUsage: 100}},
		},
	}
	seen := map[string]int{}
	for i := 0; i < 6; i++ {
		acc := pool.Next()
		if acc == nil {
			t.Fatal("expected non-nil account")
		}
		seen[acc.UserEmail]++
	}
	for _, email := range []string{"a@example.com", "b@example.com", "c@example.com"} {
		if seen[email] != 2 {
			t.Fatalf("expected each account called 2 times in 6 calls, got distribution: %v", seen)
		}
	}
}

func TestNextDistributesWithUnequalQuota(t *testing.T) {
	// Even with different remaining quotas, Next() should still round-robin.
	pool := &AccountPool{
		accounts: []*Account{
			{UserEmail: "low@example.com", QuotaInfo: &QuotaInfo{IsEligible: true, SpaceLimit: 200, SpaceUsage: 160, UserLimit: 200, UserUsage: 160}},
			{UserEmail: "high@example.com", QuotaInfo: &QuotaInfo{IsEligible: true, SpaceLimit: 200, SpaceUsage: 40, UserLimit: 200, UserUsage: 40}},
		},
	}
	seen := map[string]int{}
	for i := 0; i < 4; i++ {
		acc := pool.Next()
		if acc == nil {
			t.Fatal("expected non-nil account")
		}
		seen[acc.UserEmail]++
	}
	for _, email := range []string{"low@example.com", "high@example.com"} {
		if seen[email] != 2 {
			t.Fatalf("expected each account called 2 times in 4 calls, got distribution: %v", seen)
		}
	}
}

func TestNextExcludingRoundRobinsAmongUntried(t *testing.T) {
	a := &Account{UserEmail: "a@example.com", QuotaInfo: &QuotaInfo{IsEligible: true, SpaceLimit: 200, SpaceUsage: 20, UserLimit: 200, UserUsage: 20}}
	b := &Account{UserEmail: "b@example.com", QuotaInfo: &QuotaInfo{IsEligible: true, SpaceLimit: 200, SpaceUsage: 80, UserLimit: 200, UserUsage: 80}}
	c := &Account{UserEmail: "c@example.com", QuotaInfo: &QuotaInfo{IsEligible: true, SpaceLimit: 200, SpaceUsage: 170, UserLimit: 200, UserUsage: 170}}
	pool := &AccountPool{
		accounts: []*Account{a, b, c},
	}

	// Exclude 'a': both b and c must appear, 'a' must never appear
	exclude := map[*Account]bool{a: true}
	seen := map[string]int{}
	for i := 0; i < 6; i++ {
		acc := pool.NextExcluding(exclude)
		if acc == nil {
			t.Fatal("expected non-nil account")
		}
		if acc.UserEmail == "a@example.com" {
			t.Fatal("excluded account should not be returned")
		}
		seen[acc.UserEmail]++
	}
	if seen["b@example.com"] == 0 || seen["c@example.com"] == 0 {
		t.Fatalf("expected both b and c to appear, got: %v", seen)
	}
}

func TestBasicRemainingUsesMostConstrainedQuota(t *testing.T) {
	info := &QuotaInfo{
		SpaceLimit: 200,
		SpaceUsage: 20,
		UserLimit:  200,
		UserUsage:  190,
	}

	if got := basicRemaining(info); got != 10 {
		t.Fatalf("expected effective remaining 10, got %d", got)
	}
}

func TestIsFreePlanTreatsPersonalWithPremiumAsPaid(t *testing.T) {
	acc := &Account{
		PlanType: "personal",
		QuotaInfo: &QuotaInfo{
			HasPremium:     true,
			PremiumBalance: 1300000,
			PremiumLimit:   1300000,
		},
	}

	if isFreePlan(acc) {
		t.Fatal("expected personal account with premium credits to be treated as paid")
	}
}

// --- merged from account_concurrency_test.go ---
func TestAccountRuntimeStateConcurrentAccess(t *testing.T) {
	pool := NewAccountPool()
	acc := &Account{
		UserName:  "user",
		UserEmail: "user@example.com",
		PlanType:  "plus",
	}
	pool.accounts = []*Account{acc}

	initialCheckedAt := time.Unix(0, 0)
	acc.setModels([]ModelEntry{{Name: "GPT 5.4", ID: "model-0"}})
	acc.setQuotaInfo(&QuotaInfo{
		IsEligible:        true,
		SpaceUsage:        1,
		SpaceLimit:        100,
		UserUsage:         1,
		UserLimit:         50,
		ResearchModeUsage: 0,
		HasPremium:        true,
		PremiumBalance:    9,
		PremiumUsage:      1,
		PremiumLimit:      10,
	}, &initialCheckedAt)

	const iterations = 1000
	start := make(chan struct{})
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < iterations; i++ {
			checkedAt := time.Unix(int64(i+1), 0)
			acc.setModels([]ModelEntry{
				{Name: fmt.Sprintf("GPT 5.%d", i%5), ID: fmt.Sprintf("model-%d", i%7)},
				{Name: fmt.Sprintf("Opus 4.%d", i%3), ID: fmt.Sprintf("opus-%d", i%5)},
			})
			acc.setQuotaInfo(&QuotaInfo{
				IsEligible:        i%4 != 0,
				SpaceUsage:        i % 100,
				SpaceLimit:        100,
				UserUsage:         i % 50,
				UserLimit:         50,
				ResearchModeUsage: i % 6,
				HasPremium:        i%2 == 0,
				PremiumBalance:    10 - (i % 10),
				PremiumUsage:      i % 10,
				PremiumLimit:      10,
			}, &checkedAt)
			if i%3 == 0 {
				acc.markQuotaExhausted(checkedAt, false)
			} else {
				acc.clearQuotaExhausted()
			}
		}
	}()

	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < iterations; i++ {
				_ = pool.AllModels()
				_ = buildPublicModels(pool.AllModels())
				_ = pool.GetAccountDetails()
				_ = pool.GetQuotaSummary()
				_ = pool.AvailableCount()
				_ = pool.GetByEmail(acc.UserEmail)
				_ = isFreePlan(acc)
				_ = accountQuotaPriority(acc)
			}
		}()
	}

	close(start)
	wg.Wait()
}

// --- merged from account_role_test.go ---
func roleAcc(email, role string, usage, limit int) *Account {
	acc := makeTestAccount("u-"+email, email, "space-"+email, "WS", usage, limit)
	acc.Role = role
	return acc
}

func poolOf(accs ...*Account) *AccountPool {
	pool := NewAccountPool()
	for _, acc := range accs {
		pool.AddAccount(acc)
	}
	return pool
}

func TestPrimaryPreferredOverEducationEvenWithLessQuota(t *testing.T) {
	primary := roleAcc("primary@example.com", AccountRolePrimary, 190, 200) // little left
	edu := roleAcc("edu@example.com", AccountRoleEducation, 0, 5000)        // lots left
	pool := poolOf(edu, primary)
	for i := 0; i < 5; i++ {
		if got := pool.NextBest(); got != primary {
			t.Fatalf("NextBest = %s, want primary", got.UserEmail)
		}
		if got := pool.Next(); got != primary {
			t.Fatalf("Next = %s, want primary", got.UserEmail)
		}
	}
}

func TestSwitchesToEducationOnlyWhenPrimaryExhaustedAndBack(t *testing.T) {
	primary := roleAcc("primary@example.com", "", 0, 200) // unspecified == primary
	edu := roleAcc("edu@example.com", AccountRoleEducation, 0, 5000)
	pool := poolOf(primary, edu)

	pool.MarkInferenceUnavailable(primary)
	if got := pool.NextBest(); got != edu {
		t.Fatalf("exhausted primary must fall back to education, got %v", got)
	}
	if got := pool.NextBestExcluding(map[*Account]bool{edu: true}); got != nil {
		t.Fatalf("no other usable account expected, got %s", got.UserEmail)
	}
	// API-confirmed recovery returns traffic to the primary account.
	pool.ClearQuotaExhausted(primary)
	if got := pool.NextBest(); got != primary {
		t.Fatalf("recovered primary must be preferred again, got %s", got.UserEmail)
	}
}

func TestPrimaryWithUnknownQuotaStillBeatsScoredEducation(t *testing.T) {
	primary := roleAcc("primary@example.com", AccountRolePrimary, 0, 200)
	primary.QuotaInfo = nil // not refreshed yet
	edu := roleAcc("edu@example.com", AccountRoleEducation, 0, 5000)
	if got := poolOf(edu, primary).NextBest(); got != primary {
		t.Fatalf("got %s, want primary", got.UserEmail)
	}
}

func TestNoRolesKeepsHighestQuotaBehavior(t *testing.T) {
	low := roleAcc("low@example.com", "", 190, 200)
	high := roleAcc("high@example.com", "", 0, 5000)
	if got := poolOf(low, high).NextBest(); got != high {
		t.Fatalf("got %s, want highest-quota account", got.UserEmail)
	}
}

func TestSetAccountRoleValidatesAndPersists(t *testing.T) {
	dir := t.TempDir()
	acc := roleAcc("edu@example.com", "", 0, 100)
	SaveAccountToFile(acc, dir)
	pool := poolOf(acc)
	if err := pool.SetAccountRole(acc.AccountID, "edu"); err != nil {
		t.Fatal(err)
	}
	if acc.Role != AccountRoleEducation {
		t.Fatalf("role = %q", acc.Role)
	}
	if err := pool.SetAccountRole(acc.AccountID, "admin"); err == nil {
		t.Fatal("unknown role must be rejected")
	}
	if err := pool.SetAccountRole("missing", "primary"); err == nil {
		t.Fatal("unknown account must be rejected")
	}
	pool.SaveAccounts(dir)
	reloaded := NewAccountPool()
	if err := reloaded.LoadFromDir(dir); err != nil {
		t.Fatal(err)
	}
	got := reloaded.FindByAccountID(acc.AccountID)
	if got == nil || got.Role != AccountRoleEducation {
		t.Fatalf("role not persisted: %#v", got)
	}
	pool.SetAccountRole(acc.AccountID, "")
	pool.SaveAccounts(dir)
	reloaded = NewAccountPool()
	reloaded.LoadFromDir(dir)
	if got := reloaded.FindByAccountID(acc.AccountID); got == nil || got.Role != "" {
		t.Fatalf("cleared role not persisted: %#v", got)
	}
}

func stubQuota(t *testing.T, eligible bool, fetchErr error) *int {
	t.Helper()
	calls := 0
	old := quotaFetcher
	quotaFetcher = func(*Account) (*QuotaInfo, error) {
		calls++
		if fetchErr != nil {
			return nil, fetchErr
		}
		return &QuotaInfo{IsEligible: eligible}, nil
	}
	t.Cleanup(func() { quotaFetcher = old })
	return &calls
}

func TestClassifyAgentQuotaFailureRequiresLiveConfirmation(t *testing.T) {
	acc := roleAcc("p@example.com", "", 0, 100)
	for _, status := range []int{402, 403, 429} {
		calls := stubQuota(t, false, nil)
		err := classifyAgentQuotaFailure(acc, &agentHTTPError{Endpoint: "createAgentThread", Status: status})
		if !errors.Is(err, ErrQuotaExhausted) || *calls != 1 {
			t.Fatalf("status %d confirmed ineligible must map to ErrQuotaExhausted (calls=%d, err=%v)", status, *calls, err)
		}
		var httpErr *agentHTTPError
		if !strings.Contains(err.Error(), "HTTP") || errors.As(err, &httpErr) {
			t.Fatalf("original detail should be kept as text only: %v", err)
		}
	}
	// Eligible per the quota API: a 429 is a transient limit, not exhaustion.
	calls := stubQuota(t, true, nil)
	orig := &agentHTTPError{Endpoint: "createAgentThread", Status: 429}
	if err := classifyAgentQuotaFailure(acc, orig); err != error(orig) || *calls != 1 {
		t.Fatalf("eligible account must keep the original error, got %v", err)
	}
	// Quota API unavailable: do not guess.
	calls = stubQuota(t, false, errors.New("boom"))
	if err := classifyAgentQuotaFailure(acc, orig); errors.Is(err, ErrQuotaExhausted) {
		t.Fatal("quota API failure must not be treated as exhaustion")
	}
	// Non-quota statuses and unrelated errors never query or convert.
	calls = stubQuota(t, false, nil)
	for _, e := range []error{&agentHTTPError{Status: 500}, &agentHTTPError{Status: 404}, errors.New("other"), nil} {
		if err := classifyAgentQuotaFailure(acc, e); errors.Is(err, ErrQuotaExhausted) {
			t.Fatalf("%v must not be converted", e)
		}
	}
	if *calls != 0 {
		t.Fatalf("quota API called %d times for non-suspect errors", *calls)
	}
	// A failed turn transcript on an ineligible account is also confirmed.
	calls = stubQuota(t, false, nil)
	if err := classifyAgentQuotaFailure(acc, transcriptErr("turn failed")); !errors.Is(err, ErrQuotaExhausted) {
		t.Fatalf("transcript failure on ineligible account: %v", err)
	}
}

// End to end at the transport level: the primary is rejected and confirmed
// exhausted, the request moves to the education account in a brand-new thread
// that carries the full history, and repeating it neither resubmits nor
// touches the primary again.
func TestAgentQuotaFailoverStartsIsolatedThreadWithoutDuplicates(t *testing.T) {
	f := newFakeAgentServer(t)
	store := newAgentThreadStore("")
	primary := roleAcc("primary@example.com", AccountRolePrimary, 200, 200)
	edu := roleAcc("edu@example.com", AccountRoleEducation, 0, 5000)
	history := []ChatMessage{u("first"), a("answer"), u("second")}

	// Establish a thread on the primary account.
	if _, err := runTurn(t, f, store, primary, "client", history[:1]); err != nil {
		t.Fatal(err)
	}
	if c, _ := f.counts(); c != 1 {
		t.Fatalf("setup creates=%d", c)
	}

	// Primary now rejects with a rate/quota status and the quota API confirms exhaustion.
	f.rejectCreate, f.rejectSend = 429, 429
	stubQuota(t, false, nil)
	_, err := runTurn(t, f, store, primary, "client", history)
	if err = classifyAgentQuotaFailure(primary, err); !errors.Is(err, ErrQuotaExhausted) {
		t.Fatalf("expected confirmed exhaustion, got %v", err)
	}
	pool := poolOf(primary, edu)
	pool.MarkInferenceUnavailable(primary)
	next := pool.NextBestExcluding(map[*Account]bool{primary: true})
	if next != edu {
		t.Fatalf("failover target = %v", next)
	}

	// Education account serves the same request on a new thread with full history.
	f.rejectCreate, f.rejectSend = 0, 0
	beforeC, beforeS := f.counts()
	if _, err := runTurn(t, f, store, edu, "client", history); err != nil {
		t.Fatal(err)
	}
	afterC, afterS := f.counts()
	if afterC != beforeC+1 || afterS != beforeS {
		t.Fatalf("failover must create exactly one new thread (creates %d->%d sends %d->%d)", beforeC, afterC, beforeS, afterS)
	}
	prompt := promptText(f.creates[afterC-1]["content"])
	for _, m := range history {
		if !strings.Contains(prompt, m.Content) {
			t.Fatalf("new thread prompt missing %q", m.Content)
		}
	}
	// Retry of the identical request is deduplicated on the education thread.
	if _, err := runTurn(t, f, store, edu, "client", history); err != nil {
		t.Fatal(err)
	}
	if c, s := f.counts(); c != afterC || s != afterS {
		t.Fatalf("retry must not resubmit (creates=%d sends=%d)", c, s)
	}
}

// --- merged from account_status_test.go ---
func detailsByEmail(t *testing.T, pool *AccountPool) map[string]map[string]interface{} {
	t.Helper()
	out := map[string]map[string]interface{}{}
	for _, d := range pool.GetAccountDetails() {
		out[d["email_display"].(string)] = d
	}
	return out
}

func twoAccountPool() (*AccountPool, *Account, *Account) {
	primary := roleAcc("primary@example.com", AccountRolePrimary, 10, 1000)
	edu := roleAcc("edu@example.com", AccountRoleEducation, 0, 5000)
	// Paid-style quota (premium credits) so exhaustion is recoverable, not the
	// permanent free-plan cap.
	primary.QuotaInfo.HasPremium, primary.QuotaInfo.PremiumLimit = true, 1000
	edu.QuotaInfo.HasPremium, edu.QuotaInfo.PremiumLimit = true, 1000
	return poolOf(primary, edu), primary, edu
}

func TestAccountDetailsShowRealEmailAndRoles(t *testing.T) {
	pool, _, _ := twoAccountPool()
	d := detailsByEmail(t, pool)
	p, e := d["primary@example.com"], d["edu@example.com"]
	if p == nil || e == nil {
		t.Fatalf("real emails must be shown: %v", d)
	}
	if p["email"] != "primary@example.com" || p["email_known"] != true || p["role"] != "primary" {
		t.Fatalf("primary entry: %v", p)
	}
	if e["role"] != "education" || e["role_set"] != true {
		t.Fatalf("education entry: %v", e)
	}
}

func TestAccountDetailsMissingEmailIsUnknownAndRoleDefaultsToPrimary(t *testing.T) {
	acc := roleAcc("x@example.com", "", 0, 100)
	acc.UserEmail = "  "
	d := poolOf(acc).GetAccountDetails()[0]
	if d["email_known"] != false || d["email_display"] != "unknown" || d["email"] != "" {
		t.Fatalf("missing email: %v", d)
	}
	if d["role"] != "primary" || d["role_set"] != false {
		t.Fatalf("role default: %v", d)
	}
}

func TestCurrentAccountAndStatusFollowSwitching(t *testing.T) {
	pool, primary, edu := twoAccountPool()
	for _, d := range pool.GetAccountDetails() {
		if d["current"] != false || d["status"] != AccountStatusOK {
			t.Fatalf("nothing used yet: %v", d)
		}
	}
	pool.RecordAccountUse(primary)
	d := detailsByEmail(t, pool)
	if d["primary@example.com"]["current"] != true || d["edu@example.com"]["current"] != false {
		t.Fatalf("primary should be current: %v", d)
	}
	if _, ok := d["primary@example.com"]["last_used_at"]; !ok {
		t.Fatal("last_used_at missing")
	}

	// Primary runs out; router records the error and uses the education account.
	pool.MarkInferenceUnavailable(primary)
	pool.RecordAccountError(primary, AccountErrQuotaExhausted)
	pool.RecordAccountUse(edu)
	d = detailsByEmail(t, pool)
	if d["primary@example.com"]["current"] != false || d["edu@example.com"]["current"] != true {
		t.Fatalf("current must move to education: %v", d)
	}
	if d["primary@example.com"]["status"] != AccountStatusExhausted || d["primary@example.com"]["last_error"] != AccountErrQuotaExhausted {
		t.Fatalf("primary status: %v", d["primary@example.com"])
	}
	if d["edu@example.com"]["status"] != AccountStatusOK {
		t.Fatalf("education status: %v", d["edu@example.com"])
	}
	if pool.CurrentAccountID() != edu.AccountID {
		t.Fatal("CurrentAccountID mismatch")
	}

	// A transient request error is surfaced, then cleared by the next success.
	pool.RecordAccountError(edu, AccountErrRequestFailed)
	if got := detailsByEmail(t, pool)["edu@example.com"]["status"]; got != AccountStatusError {
		t.Fatalf("status after error = %v", got)
	}
	pool.RecordAccountUse(edu)
	d = detailsByEmail(t, pool)
	if d["edu@example.com"]["status"] != AccountStatusOK {
		t.Fatalf("status must clear after success: %v", d["edu@example.com"])
	}
	if _, ok := d["edu@example.com"]["last_error"]; ok {
		t.Fatal("last_error must be cleared")
	}

	// API-confirmed recovery returns the primary to ok without losing its identity.
	pool.ClearQuotaExhausted(primary)
	pool.RecordAccountUse(primary)
	d = detailsByEmail(t, pool)
	if d["primary@example.com"]["status"] != AccountStatusOK || d["primary@example.com"]["current"] != true {
		t.Fatalf("primary after recovery: %v", d["primary@example.com"])
	}
}

func TestStatusPrecedenceAndRefreshKeepsIdentity(t *testing.T) {
	pool, primary, _ := twoAccountPool()
	// A simulated refresh (new quota snapshot) must not change email/role.
	pool.applyQuotaInfo(primary, &QuotaInfo{IsEligible: false, HasPremium: true, PremiumLimit: 1000, SpaceLimit: 1000, SpaceUsage: 1000})
	d := detailsByEmail(t, pool)["primary@example.com"]
	if d["status"] != AccountStatusExhausted || d["email"] != "primary@example.com" || d["role"] != "primary" {
		t.Fatalf("after refresh: %v", d)
	}
	pool.MarkPermanentlyExhausted(primary)
	if got := detailsByEmail(t, pool)["primary@example.com"]["status"]; got != AccountStatusPermanent {
		t.Fatalf("permanent status = %v", got)
	}
	// Import (add) of a new account shows up with its own real email.
	pool.AddAccount(roleAcc("new@example.com", "", 0, 100))
	if detailsByEmail(t, pool)["new@example.com"] == nil {
		t.Fatal("imported account missing")
	}
}

func TestAccountDetailsNeverContainSecrets(t *testing.T) {
	pool, primary, _ := twoAccountPool()
	primary.FullCookie = "cookie-secret-value"
	pool.RecordAccountError(primary, AccountErrRequestFailed)
	raw, err := json.Marshal(pool.GetAccountDetails())
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{primary.TokenV2, "cookie-secret-value"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("details leak %q", secret)
		}
	}
}

func TestHealthMasksEmails(t *testing.T) {
	pool, _, _ := twoAccountPool()
	rec := httptest.NewRecorder()
	HandleHealth(pool)(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	body := rec.Body.String()
	if strings.Contains(body, "primary@example.com") || strings.Contains(body, "edu@example.com") {
		t.Fatalf("/health must not expose full emails: %s", body)
	}
	if !strings.Contains(body, "p***@example.com") {
		t.Fatalf("masked email expected: %s", body)
	}
	for in, want := range map[string]string{"": "", "alice@example.com": "a***@example.com", "noat": "***", "@x.com": "***"} {
		if got := maskEmail(in); got != want {
			t.Fatalf("maskEmail(%q)=%q want %q", in, got, want)
		}
	}
}

func TestAdminAccountRoleEndpoint(t *testing.T) {
	pool, _, edu := twoAccountPool()
	dir := t.TempDir()
	SaveAccountToFile(edu, dir)
	handler := HandleAdminAccountRole(pool, dir)
	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/admin/accounts/role", strings.NewReader(body))
		rec := httptest.NewRecorder()
		handler(rec, req)
		return rec
	}
	if rec := post(`{"account_id":"` + edu.AccountID + `","role":"boss"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad role = %d", rec.Code)
	}
	if rec := post(`{"account_id":"nope","role":"primary"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown account = %d", rec.Code)
	}
	if rec := post(`{"account_id":"` + edu.AccountID + `","role":"primary"}`); rec.Code != http.StatusOK {
		t.Fatalf("set primary = %d %s", rec.Code, rec.Body.String())
	}
	if edu.Role != AccountRolePrimary {
		t.Fatalf("role not applied: %q", edu.Role)
	}
	reloaded := NewAccountPool()
	if err := reloaded.LoadFromDir(dir); err != nil {
		t.Fatal(err)
	}
	if got := reloaded.FindByAccountID(edu.AccountID); got == nil || got.Role != AccountRolePrimary {
		t.Fatalf("role not persisted: %#v", got)
	}
}

func TestAdminAccountsReportsCurrentAccountAcrossPages(t *testing.T) {
	pool, primary, edu := twoAccountPool()
	call := func() map[string]interface{} {
		rec := httptest.NewRecorder()
		HandleAdminAccounts(pool)(rec, httptest.NewRequest(http.MethodGet, "/admin/accounts?page=1&page_size=1", nil))
		var resp map[string]interface{}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		return resp
	}
	if _, ok := call()["current_account"]; ok {
		t.Fatal("no current account before first successful request")
	}
	pool.RecordAccountUse(primary)
	cur, _ := call()["current_account"].(map[string]interface{})
	if cur == nil || cur["email"] != "primary@example.com" || cur["role"] != "primary" {
		t.Fatalf("current after primary: %v", cur)
	}
	pool.RecordAccountUse(edu)
	cur, _ = call()["current_account"].(map[string]interface{})
	if cur == nil || cur["email"] != "edu@example.com" || cur["role"] != "education" {
		t.Fatalf("current after switch: %v", cur)
	}
	for _, k := range []string{"token_v2", "full_cookie", "models"} {
		if _, ok := cur[k]; ok {
			t.Fatalf("current_account must not include %s", k)
		}
	}
}

// --- merged from account_workspace_test.go ---
// TestHasNoWorkspaceOnlyAfterProbe verifies that an unprobed account is
// treated as usable until a probe records a real result. Otherwise a
// fresh-from-disk pool would refuse to serve traffic on the first boot
// before RefreshAll ever runs.
func TestHasNoWorkspaceOnlyAfterProbe(t *testing.T) {
	pool := NewAccountPool()
	acc := &Account{UserEmail: "u@example.com"}

	if pool.hasNoWorkspace(acc) {
		t.Fatalf("unprobed account should NOT be flagged as no_workspace")
	}

	now := time.Now()
	acc.WorkspaceCheckedAt = &now
	acc.SpaceCount = 0
	if !pool.hasNoWorkspace(acc) {
		t.Fatalf("probed account with SpaceCount=0 should be flagged")
	}

	acc.SpaceCount = 2
	if pool.hasNoWorkspace(acc) {
		t.Fatalf("account with SpaceCount>0 must not be flagged")
	}
}

// TestPickBestSkipsNoWorkspace ensures the dashboard "best" selector
// never returns an account that has been confirmed to lack a workspace,
// even when its quota is healthy.
func TestPickBestSkipsNoWorkspace(t *testing.T) {
	pool := NewAccountPool()
	now := time.Now()
	bad := &Account{
		UserEmail:          "bad@example.com",
		QuotaInfo:          &QuotaInfo{IsEligible: true, SpaceLimit: 100, SpaceUsage: 0, UserLimit: 100, UserUsage: 0},
		WorkspaceCheckedAt: &now,
		SpaceCount:         0,
	}
	good := &Account{
		UserEmail:          "good@example.com",
		QuotaInfo:          &QuotaInfo{IsEligible: true, SpaceLimit: 100, SpaceUsage: 50, UserLimit: 100, UserUsage: 50},
		WorkspaceCheckedAt: &now,
		SpaceCount:         3,
	}
	pool.accounts = []*Account{bad, good}

	for i := 0; i < 5; i++ {
		got := pool.GetBestAccount()
		if got == nil {
			t.Fatalf("iteration %d: GetBestAccount returned nil", i)
		}
		if got.UserEmail == bad.UserEmail {
			t.Fatalf("iteration %d: GetBestAccount returned no-workspace account", i)
		}
	}
}

// TestPickBestAllNoWorkspaceReturnsNil double-checks the edge case where
// every account is bad — we must return nil rather than fall back to a
// known-broken account just to have *something* to redirect to.
func TestPickBestAllNoWorkspaceReturnsNil(t *testing.T) {
	pool := NewAccountPool()
	now := time.Now()
	pool.accounts = []*Account{
		{UserEmail: "a@example.com", QuotaInfo: &QuotaInfo{IsEligible: true}, WorkspaceCheckedAt: &now, SpaceCount: 0},
		{UserEmail: "b@example.com", QuotaInfo: &QuotaInfo{IsEligible: true}, WorkspaceCheckedAt: &now, SpaceCount: 0},
	}
	if got := pool.GetBestAccount(); got != nil {
		t.Fatalf("expected nil when every account is workspace-less, got %s", got.UserEmail)
	}
}

// TestAvailableCountExcludesNoWorkspace ensures the dashboard summary
// number reflects accounts that can actually serve traffic.
func TestAvailableCountExcludesNoWorkspace(t *testing.T) {
	pool := NewAccountPool()
	now := time.Now()
	pool.accounts = []*Account{
		{UserEmail: "a@example.com", QuotaInfo: &QuotaInfo{IsEligible: true}, WorkspaceCheckedAt: &now, SpaceCount: 1},
		{UserEmail: "b@example.com", QuotaInfo: &QuotaInfo{IsEligible: true}, WorkspaceCheckedAt: &now, SpaceCount: 0},
		{UserEmail: "c@example.com", QuotaInfo: &QuotaInfo{IsEligible: false}, WorkspaceCheckedAt: &now, SpaceCount: 1},
	}
	if got := pool.AvailableCount(); got != 1 {
		t.Fatalf("AvailableCount = %d, want 1", got)
	}
}

// TestApplyWorkspaceCountStampsAndSignalsChange covers the first-probe
// vs. transition vs. no-op cases of applyWorkspaceCount so callers know
// when to log.
func TestApplyWorkspaceCountStampsAndSignalsChange(t *testing.T) {
	pool := NewAccountPool()
	acc := &Account{UserEmail: "u@example.com"}

	prev, changed := pool.applyWorkspaceCount(acc, 0)
	if !changed || prev != 0 || acc.SpaceCount != 0 || acc.WorkspaceCheckedAt == nil {
		t.Fatalf("first probe should set checked_at and report changed=true; got prev=%d changed=%v acc=%+v", prev, changed, acc)
	}

	prev, changed = pool.applyWorkspaceCount(acc, 0)
	if changed {
		t.Fatalf("re-probing the same value must not report changed=true (would spam logs)")
	}
	if prev != 0 {
		t.Fatalf("prev = %d, want 0", prev)
	}

	prev, changed = pool.applyWorkspaceCount(acc, 3)
	if !changed || prev != 0 || acc.SpaceCount != 3 {
		t.Fatalf("0→3 transition not recorded: prev=%d changed=%v space_count=%d", prev, changed, acc.SpaceCount)
	}
}

// TestRefreshAndPersistAccountSavesWorkspace ensures the post-register
// flow persists `space_count` / `workspace_checked_at` so a server
// restart immediately remembers which fresh accounts are bad.
func TestRefreshAndPersistAccountSavesWorkspace(t *testing.T) {
	dir := t.TempDir()
	email := "fresh@example.com"
	path := seedAccountFile(t, dir, email)

	pool := NewAccountPool()
	pool.accounts = []*Account{{UserEmail: email, SpaceID: "s-" + email, UserID: "u-" + email}}

	restoreFetch := withFetchers(t,
		func(*Account) (*QuotaInfo, error) {
			return &QuotaInfo{IsEligible: true, SpaceUsage: 1, SpaceLimit: 100}, nil
		},
		func(*Account) ([]ModelEntry, error) {
			return []ModelEntry{{ID: "m"}}, nil
		},
	)
	defer restoreFetch()
	restoreProbe := withWorkspaceProbe(t, func(*Account) (int, error) { return 0, nil })
	defer restoreProbe()

	if err := pool.RefreshAndPersistAccount(context.Background(), dir, email); err != nil {
		t.Fatalf("RefreshAndPersistAccount: %v", err)
	}

	raw, _ := os.ReadFile(path)
	var got map[string]interface{}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	sc, ok := got["space_count"].(float64)
	if !ok || int(sc) != 0 {
		t.Fatalf("space_count missing or wrong: %v (raw=%s)", got["space_count"], string(raw))
	}
	if got["workspace_checked_at"].(string) == "" {
		t.Fatalf("workspace_checked_at not persisted")
	}

	// In-memory state must agree.
	if !pool.hasNoWorkspace(pool.accounts[0]) {
		t.Fatalf("hasNoWorkspace should be true after probe")
	}
}

// TestRefreshAndPersistAccountSurvivesProbeError ensures a transient
// loadUserContent failure doesn't poison the persisted snapshot — we
// still want to commit the quota/models we successfully fetched.
func TestRefreshAndPersistAccountSurvivesProbeError(t *testing.T) {
	dir := t.TempDir()
	email := "probefail@example.com"
	path := seedAccountFile(t, dir, email)

	pool := NewAccountPool()
	pool.accounts = []*Account{{UserEmail: email, UserID: "u-" + email, SpaceID: "s-" + email}}

	restoreFetch := withFetchers(t,
		func(*Account) (*QuotaInfo, error) {
			return &QuotaInfo{IsEligible: true, SpaceUsage: 0, SpaceLimit: 100}, nil
		},
		func(*Account) ([]ModelEntry, error) { return nil, nil },
	)
	defer restoreFetch()
	restoreProbe := withWorkspaceProbe(t, func(*Account) (int, error) {
		return 0, errors.New("notion 503")
	})
	defer restoreProbe()

	if err := pool.RefreshAndPersistAccount(context.Background(), dir, email); err != nil {
		t.Fatalf("RefreshAndPersistAccount: %v", err)
	}

	raw, _ := os.ReadFile(path)
	var got map[string]interface{}
	_ = json.Unmarshal(raw, &got)
	if _, ok := got["quota_info"]; !ok {
		t.Fatalf("quota_info should still be persisted on probe failure: %s", string(raw))
	}
	if _, ok := got["space_count"]; ok {
		t.Fatalf("space_count must NOT be persisted on probe failure (would falsely flag account)")
	}
}

// TestLoadFromDirRehydratesWorkspaceProbe verifies the disk → runtime
// round-trip so a server restart remembers which accounts were bad.
func TestLoadFromDirRehydratesWorkspaceProbe(t *testing.T) {
	dir := t.TempDir()
	email := "persist@example.com"
	path := seedAccountFile(t, dir, email)

	// Rewrite the seed with persisted workspace fields.
	raw, _ := os.ReadFile(path)
	var body map[string]interface{}
	_ = json.Unmarshal(raw, &body)
	body["space_count"] = 0
	body["workspace_checked_at"] = time.Now().UTC().Format(time.RFC3339)
	out, _ := json.MarshalIndent(body, "", "  ")
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatalf("rewrite seed: %v", err)
	}

	pool := NewAccountPool()
	if err := pool.LoadFromDir(dir); err != nil {
		t.Fatalf("LoadFromDir: %v", err)
	}
	if len(pool.accounts) != 1 {
		t.Fatalf("expected 1 account, got %d", len(pool.accounts))
	}
	acc := pool.accounts[0]
	if acc.WorkspaceCheckedAt == nil || acc.SpaceCount != 0 {
		t.Fatalf("workspace probe not rehydrated: checked_at=%v count=%d", acc.WorkspaceCheckedAt, acc.SpaceCount)
	}
	if !pool.hasNoWorkspace(acc) {
		t.Fatalf("hasNoWorkspace should be true after rehydration")
	}
}

func TestClassifyAgentQuotaFailureImmediateOnTooManyRequestsError(t *testing.T) {
	f, store, acc := bindingSetup(t)
	acc.AccountID = "acc-biz"
	acc.UserEmail = "biz@example.com"
	if _, err := runTurn(t, f, store, acc, "client-429", []ChatMessage{u("turn-1")}); err != nil {
		t.Fatal(err)
	}

	// Simulate Business account where legacy V1 isEligible is still true (300 bonus credits),
	// while Notion upstream rejects sendEventToAgentThread with HTTP 429 TooManyRequestsError.
	stubQuota(t, true, nil)
	f.rejectSend = http.StatusTooManyRequests
	f.rejectBody = `{"name":"TooManyRequestsError","message":"Rate limit exceeded"}`
	_, err := runTurn(t, f, store, acc, "client-429", []ChatMessage{u("turn-1"), a("reply-1"), u("turn-2")})
	if err == nil {
		t.Fatal("expected HTTP 429 error")
	}
	if classified := classifyAgentQuotaFailure(acc, err); !errors.Is(classified, ErrQuotaExhausted) {
		t.Fatalf("expected HTTP 429 TooManyRequestsError to classify as ErrQuotaExhausted even when legacy isEligible=true, got %v", classified)
	}
	// Verify turn.rollbackRejected() restored the bound record to completed so it is not wedged in submitting.
	store.mu.Lock()
	for _, rec := range store.records {
		if rec.State != agentStateCompleted {
			t.Fatalf("expected bound thread state to roll back to completed after 429, got %q", rec.State)
		}
	}
	store.mu.Unlock()
}

func TestAnthropicStreamQuotaFailoverAcrossKeepaliveAndMarksUnavailable(t *testing.T) {
	f := newFakeAgentServer(t)
	useRecoveryStore(t, "")
	setKeepalive(t, 15*time.Millisecond, 20*time.Millisecond)
	stubQuota(t, true, nil)
	oldClient := agentThreadHTTPClient
	t.Cleanup(func() { agentThreadHTTPClient = oldClient })

	primary := roleAcc("primary@example.com", AccountRolePrimary, 10, 1000)
	primary.UserID, primary.SpaceID, primary.TokenV2 = "u-primary", "s-primary", "tok-primary"
	edu := roleAcc("edu@example.com", AccountRoleEducation, 0, 5000)
	edu.UserID, edu.SpaceID, edu.TokenV2 = "u-edu", "s-edu", "tok-edu"
	pool := poolOf(primary, edu)

	failPrimary := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failPrimary && r.Header.Get("x-notion-active-user-header") == "u-primary" &&
			(strings.HasSuffix(r.URL.Path, "sendEventToAgentThread") || strings.HasSuffix(r.URL.Path, "createAgentThread")) {
			// Sleep past agentKeepaliveDelay (15ms) so HTTP 200 SSE + message_start + ping are flushed first.
			time.Sleep(35 * time.Millisecond)
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"name":"TooManyRequestsError","message":"6h rolling window exhausted"}`))
			return
		}
		f.handle(w, r)
	}))
	defer upstream.Close()
	NotionAPIBase = upstream.URL
	agentThreadHTTPClient = func(time.Duration) *http.Client { return upstream.Client() }

	proxySrv := httptest.NewServer(HandleAnthropicMessages(pool))
	defer proxySrv.Close()

	postStream := func(body string) string {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, proxySrv.URL+"/v1/messages", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := proxySrv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var buf strings.Builder
		sc := make([]byte, 4096)
		for {
			n, rerr := resp.Body.Read(sc)
			if n > 0 {
				buf.Write(sc[:n])
			}
			if rerr != nil {
				break
			}
		}
		return buf.String()
	}

	// Turn 1: establishes thread on primary.
	sse1 := postStream(`{"model":"auto","max_tokens":256,"stream":true,"metadata":{"session_id":"stream_session_1"},"messages":[{"role":"user","content":"hello turn 1"}]}`)
	if !strings.Contains(sse1, "reply-1") || pool.CurrentAccountID() != primary.AccountID {
		t.Fatalf("turn 1 should succeed on primary: current=%s sse=%s", pool.CurrentAccountID(), sse1)
	}

	// Turn 2: primary stalls >15ms (keepalive starts) then returns 429 TooManyRequestsError.
	// HandleAnthropicMessages must mark primary unavailable, reuse the open keepalive stream,
	// and fail over to edu with full conversation history.
	failPrimary = true
	sse2 := postStream(`{"model":"auto","max_tokens":256,"stream":true,"metadata":{"session_id":"stream_session_1"},"messages":[{"role":"user","content":"hello turn 1"},{"role":"assistant","content":"reply-1"},{"role":"user","content":"hello turn 2"}]}`)
	if strings.Contains(sse2, "event: error") || !strings.Contains(sse2, "reply-2") || !strings.Contains(sse2, "event: message_stop") {
		t.Fatalf("turn 2 should fail over cleanly across keepalive to edu without error event, got:\n%s", sse2)
	}
	if !pool.isUnusable(primary) {
		t.Fatal("primary account must be marked unavailable after streaming ErrQuotaExhausted")
	}
	if pool.CurrentAccountID() != edu.AccountID {
		t.Fatalf("CurrentAccountID = %s, want edu (%s)", pool.CurrentAccountID(), edu.AccountID)
	}
	if got := agentThreadPreferredAccountIDForPool(pool, []ChatMessage{u("hello turn 1"), a("reply-1"), u("hello turn 2"), a("reply-2"), u("hello turn 3")}, "stream_session_1", "auto"); got != edu.AccountID {
		t.Fatalf("preferred account for stream_session_1 = %q, want edu (%q)", got, edu.AccountID)
	}
}

func TestAdminAccountSwitchHotMigratesActiveSessionAndDeletedAccountSkipped(t *testing.T) {
	f := newFakeAgentServer(t)
	useRecoveryStore(t, "")
	stubQuota(t, true, nil)
	oldClient := agentThreadHTTPClient
	t.Cleanup(func() { agentThreadHTTPClient = oldClient })
	agentThreadHTTPClient = func(time.Duration) *http.Client { return f.server.Client() }

	primary := roleAcc("primary@example.com", AccountRolePrimary, 10, 1000)
	primary.UserID, primary.SpaceID, primary.TokenV2 = "u-primary", "s-primary", "tok-primary"
	edu := roleAcc("edu@example.com", AccountRoleEducation, 0, 5000)
	edu.UserID, edu.SpaceID, edu.TokenV2 = "u-edu", "s-edu", "tok-edu"
	pool := poolOf(primary, edu)

	// Turn 1 on primary.
	req1 := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"auto","max_tokens":256,"stream":false,"metadata":{"session_id":"hot_switch_1"},"messages":[{"role":"user","content":"turn one on primary"}]}`))
	rec1 := httptest.NewRecorder()
	HandleAnthropicMessages(pool)(rec1, req1)
	if rec1.Code != http.StatusOK || pool.CurrentAccountID() != primary.AccountID {
		t.Fatalf("turn 1 failed: code=%d body=%s", rec1.Code, rec1.Body.String())
	}

	// Hot-switch to edu via POST /admin/accounts/switch without deleting primary or restarting.
	swReq := httptest.NewRequest(http.MethodPost, "/admin/accounts/switch", strings.NewReader(`{"account_id":"`+edu.AccountID+`"}`))
	swRec := httptest.NewRecorder()
	HandleAdminAccountSwitch(pool)(swRec, swReq)
	if swRec.Code != http.StatusOK {
		t.Fatalf("switch failed: code=%d body=%s", swRec.Code, swRec.Body.String())
	}
	if pool.CurrentAccountID() != edu.AccountID || pool.PreferredAccountID() != edu.AccountID {
		t.Fatalf("switch did not update current/preferred: current=%s preferred=%s", pool.CurrentAccountID(), pool.PreferredAccountID())
	}

	// Turn 2 of the SAME clientSessionID must now execute on edu (creating a new thread on edu with full history).
	req2 := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"auto","max_tokens":256,"stream":false,"metadata":{"session_id":"hot_switch_1"},"messages":[{"role":"user","content":"turn one on primary"},{"role":"assistant","content":"reply-1"},{"role":"user","content":"turn two on edu"}]}`))
	rec2 := httptest.NewRecorder()
	HandleAnthropicMessages(pool)(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("turn 2 failed: code=%d body=%s", rec2.Code, rec2.Body.String())
	}
	c, s := f.counts()
	if c != 2 || s != 0 {
		t.Fatalf("expected 2 creates (1 on primary, 1 on edu with full history) and 0 sends, got creates=%d sends=%d", c, s)
	}
	secondPrompt := promptText(f.creates[1]["content"])
	if !strings.Contains(secondPrompt, "turn one on primary") || !strings.Contains(secondPrompt, "turn two on edu") {
		t.Fatalf("edu thread create prompt missing full history: %q", secondPrompt)
	}
	if got := agentThreadPreferredAccountIDForPool(pool, []ChatMessage{u("turn one on primary"), a("reply-1"), u("turn two on edu"), a("reply-2"), u("turn three")}, "hot_switch_1", "auto"); got != edu.AccountID {
		t.Fatalf("preferred account for hot_switch_1 = %q, want edu (%q)", got, edu.AccountID)
	}

	// Deleting edu marks it removed and clears its preferred binding.
	if !pool.RemoveAccountByEmail(edu.UserEmail) {
		t.Fatal("RemoveAccountByEmail(edu) returned false")
	}
	if !edu.isRemoved() {
		t.Fatal("deleted account must have removed=true")
	}
	if got := agentThreadPreferredAccountIDForPool(pool, []ChatMessage{u("turn one on primary"), a("reply-1"), u("turn two on edu"), a("reply-2"), u("turn three")}, "hot_switch_1", "auto"); got != "" {
		t.Fatalf("deleted account must not be returned by agentThreadPreferredAccountIDForPool, got %q", got)
	}
}

