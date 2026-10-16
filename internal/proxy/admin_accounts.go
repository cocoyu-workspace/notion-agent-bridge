package proxy

import (
	"sort"
	"strings"
)

// AccountSummary aggregates pool-wide counts and quota sums used by the
// dashboard. We compute it server-side so the dashboard does not need
// to download the full account list to render headline numbers — that's
// what `/admin/accounts?page=...` is for.
//
// JSON tags use snake_case to match the rest of the admin API.
type AccountSummary struct {
	TotalAccounts      int `json:"total_accounts"`
	AvailableAccounts  int `json:"available_accounts"` // not exhausted, not permanent, has a workspace
	ExhaustedOnly      int `json:"exhausted_only"`
	NoWorkspace        int `json:"no_workspace"`
	PremiumAccounts    int `json:"premium_accounts"`
	ResearchLimited    int `json:"research_limited"`
	TotalResearchUsage int `json:"total_research_usage"`

	// Legacy counters (getAIUsageEligibility V1/V2). They are NOT the Notion
	// native plan allowance, they are only summed over distinct workspaces
	// (space-level fields, premium credits) or distinct users (user-level
	// fields) so accounts that share a workspace are not counted twice, and
	// the dashboard must label them as legacy.
	TotalRemaining      int   `json:"total_remaining"`
	TotalSpaceUsage     int   `json:"total_space_usage"`
	TotalSpaceLimit     int   `json:"total_space_limit"`
	TotalSpaceRemaining int   `json:"total_space_remaining"`
	TotalUserUsage      int   `json:"total_user_usage"`
	TotalUserLimit      int   `json:"total_user_limit"`
	TotalUserRemaining  int   `json:"total_user_remaining"`
	TotalPremiumBalance int64 `json:"total_premium_balance"`
	TotalPremiumLimit   int64 `json:"total_premium_limit"`

	// Credit is the Notion-native allowance overview. Accounts are counted by
	// state; used/limit are only aggregated inside a pool with the same
	// explicit scope + credit type + window, never across units.
	Credit CreditSummary `json:"credit"`
}

// CreditPool groups native windows that share scope, credit type, window and
// (for billing windows) the same billing period. Only billing pools are
// summed; rolling pools carry counts and the highest per-account percentage.
type CreditPool struct {
	Kind       string   `json:"kind"` // rolling | billing
	Scope      string   `json:"scope"`
	CreditType string   `json:"credit_type"`
	Window     string   `json:"window,omitempty"`  // e.g. 6h
	Cadence    string   `json:"cadence,omitempty"` // e.g. billing_period
	Accounts   int      `json:"accounts"`          // accounts reporting this window
	Summed     int      `json:"summed"`            // distinct owners included in Used/Limit
	Used       *float64 `json:"used"`              // nil when nothing may be summed
	Limit      *float64 `json:"limit"`
	Percent    *float64 `json:"percent"`
	PeriodEnd  string   `json:"period_end,omitempty"` // billing pools: windows are only grouped inside one period
	MaxPercent *float64 `json:"max_percent"`          // highest per-account used % (never a sum)
}

type CreditSummary struct {
	ByState map[string]int `json:"by_state"`
	Pools   []CreditPool   `json:"pools"`
}

func uniqueKey(seen map[string]bool, key string) bool {
	if key == "" {
		return true // unknown owner: cannot prove it is a duplicate
	}
	if seen[key] {
		return false
	}
	seen[key] = true
	return true
}

// summarizeAccounts walks the full account-detail list and produces the
// summary used by the dashboard headline cards. It treats missing fields
// as zero for the legacy counters, but never for native windows.
func summarizeAccounts(accounts []map[string]interface{}) AccountSummary {
	var s AccountSummary
	s.Credit = CreditSummary{ByState: map[string]int{}, Pools: []CreditPool{}}
	spaceSeen := map[string]bool{}
	userSeen := map[string]bool{}
	premiumSeen := map[string]bool{}
	type poolKey struct{ kind, scope, ctype, window, cadence, periodEnd string }
	type poolAcc struct {
		pool        CreditPool
		used, limit float64
		maxPct      float64
		hasPct      bool
		owners      map[string]bool
	}
	pools := map[poolKey]*poolAcc{}
	var order []poolKey
	for _, a := range accounts {
		s.TotalAccounts++
		exh := mapBool(a, "exhausted")
		perm := mapBool(a, "permanent")
		nws := mapBool(a, "no_workspace")
		if nws {
			s.NoWorkspace++
		}
		if (exh || perm) && !nws {
			s.ExhaustedOnly++
		}
		if !exh && !perm && !nws {
			s.AvailableAccounts++
		}
		if hasPremiumMap(a) {
			s.PremiumAccounts++
		}
		if !exh && !perm && !nws && isResearchLimitedMap(a) {
			s.ResearchLimited++
		}
		s.TotalResearchUsage += mapInt(a, "research_usage")
		wsKey := mapString(a, "workspace_key")
		usrKey := mapString(a, "user_key")
		if uniqueKey(spaceSeen, wsKey) {
			s.TotalRemaining += mapInt(a, "remaining")
			s.TotalSpaceUsage += mapInt(a, "space_usage")
			s.TotalSpaceLimit += mapInt(a, "space_limit")
			s.TotalSpaceRemaining += mapInt(a, "space_remaining")
		}
		if uniqueKey(userSeen, usrKey) {
			s.TotalUserUsage += mapInt(a, "user_usage")
			s.TotalUserLimit += mapInt(a, "user_limit")
			s.TotalUserRemaining += mapInt(a, "user_remaining")
		}
		if uniqueKey(premiumSeen, wsKey) {
			s.TotalPremiumBalance += int64(mapInt(a, "premium_balance"))
			s.TotalPremiumLimit += int64(mapInt(a, "premium_limit"))
		}

		cv, _ := a["credit"].(*CreditView)
		if cv == nil {
			s.Credit.ByState[CreditStateUnchecked]++
			continue
		}
		s.Credit.ByState[cv.State]++
		if !cv.CountsAsUse {
			continue
		}
		for _, w := range []struct {
			kind string
			v    *CreditWindowView
		}{{"rolling", cv.Rolling}, {"billing", cv.Billing}} {
			if w.v == nil || w.v.Expired || w.v.Scope == "" || w.v.CreditType == "" {
				continue
			}
			// Billing windows are only grouped inside one billing period
			// (identical period end); rolling windows reset on each account's
			// own clock and are never summed, only counted.
			periodEnd := ""
			if w.kind == "billing" {
				periodEnd = w.v.PeriodEnd
				if periodEnd == "" {
					continue // no verifiable period boundary: cannot be grouped
				}
			}
			k := poolKey{w.kind, w.v.Scope, w.v.CreditType, w.v.Window, w.v.Cadence, periodEnd}
			pa := pools[k]
			if pa == nil {
				pa = &poolAcc{owners: map[string]bool{}}
				pa.pool = CreditPool{Kind: w.kind, Scope: w.v.Scope, CreditType: w.v.CreditType, Window: w.v.Window, Cadence: w.v.Cadence, PeriodEnd: periodEnd}
				pools[k] = pa
				order = append(order, k)
			}
			// A per-user window belongs to the user, not to the workspace row:
			// the same user seen through two workspaces is one entry.
			ownerKnown := w.v.Scope == "per_user" && usrKey != ""
			if ownerKnown {
				if pa.owners[usrKey] {
					continue
				}
				pa.owners[usrKey] = true
			}
			pa.pool.Accounts++
			if w.v.Percent != nil && (!pa.hasPct || *w.v.Percent > pa.maxPct) {
				pa.maxPct, pa.hasPct = *w.v.Percent, true
			}
			// Only billing windows of one period may be summed, and only when
			// the owner is provably distinct. Rolling and other scopes are
			// counted, never summed.
			if w.kind == "billing" && ownerKnown && w.v.Used != nil && w.v.Limit != nil {
				pa.used += *w.v.Used
				pa.limit += *w.v.Limit
				pa.pool.Summed++
			}
		}
	}
	for _, k := range order {
		pa := pools[k]
		if pa.hasPct {
			m := pa.maxPct
			pa.pool.MaxPercent = &m
		}
		if pa.pool.Summed > 0 {
			u, l := pa.used, pa.limit
			pa.pool.Used, pa.pool.Limit = &u, &l
			if l > 0 {
				pct := u / l * 100
				pa.pool.Percent = &pct
			}
		}
		s.Credit.Pools = append(s.Credit.Pools, pa.pool)
	}
	return s
}

// filterAccountDetails keeps only entries whose email/name/plan/space
// contains q (case-insensitive). An empty q is a no-op.
func filterAccountDetails(accounts []map[string]interface{}, q string) []map[string]interface{} {
	q = strings.TrimSpace(strings.ToLower(q))
	if q == "" {
		return accounts
	}
	out := make([]map[string]interface{}, 0, len(accounts))
	for _, a := range accounts {
		if matchAccountQuery(a, q) {
			out = append(out, a)
		}
	}
	return out
}

func matchAccountQuery(a map[string]interface{}, qLower string) bool {
	for _, k := range []string{"email", "name", "plan", "space"} {
		if s, ok := a[k].(string); ok && s != "" {
			if strings.Contains(strings.ToLower(s), qLower) {
				return true
			}
		}
	}
	return false
}

// sortAccountDetails sorts in-place using the same criteria the dashboard
// previously applied client-side:
//  1. Permanently exhausted accounts to the bottom.
//  2. Accounts with no accessible workspace to the bottom.
//  3. Quota-exhausted accounts to the bottom.
//  4. More remaining basic quota first.
//  5. Research-limited accounts (no premium, research >= 3) to the bottom.
//  6. Stable fallback by name (lower-cased).
func sortAccountDetails(accounts []map[string]interface{}) {
	sort.SliceStable(accounts, func(i, j int) bool {
		ai, aj := mapBool(accounts[i], "permanent"), mapBool(accounts[j], "permanent")
		if ai != aj {
			return !ai
		}
		ai, aj = mapBool(accounts[i], "no_workspace"), mapBool(accounts[j], "no_workspace")
		if ai != aj {
			return !ai
		}
		ai, aj = mapBool(accounts[i], "exhausted"), mapBool(accounts[j], "exhausted")
		if ai != aj {
			return !ai
		}
		ri, rj := mapInt(accounts[i], "remaining"), mapInt(accounts[j], "remaining")
		if ri != rj {
			return ri > rj
		}
		ri2, rj2 := isResearchLimitedMap(accounts[i]), isResearchLimitedMap(accounts[j])
		if ri2 != rj2 {
			return !ri2
		}
		return strings.ToLower(mapString(accounts[i], "name")) <
			strings.ToLower(mapString(accounts[j], "name"))
	})
}

// paginateAccounts returns the slice [page*pageSize : page*pageSize+pageSize].
// pageSize <= 0 returns the input unchanged. Out-of-range pages return an
// empty (non-nil) slice so JSON encodes as `[]` rather than `null`.
func paginateAccounts(accounts []map[string]interface{}, page, pageSize int) []map[string]interface{} {
	if pageSize <= 0 {
		return accounts
	}
	if page < 0 {
		page = 0
	}
	start := page * pageSize
	if start >= len(accounts) {
		return []map[string]interface{}{}
	}
	end := start + pageSize
	if end > len(accounts) {
		end = len(accounts)
	}
	return accounts[start:end]
}

// hasPremiumMap mirrors the frontend's hasPremiumAccess: any of has_premium,
// premium_limit > 0, or premium_balance > 0 marks the account as having
// premium credits.
func hasPremiumMap(a map[string]interface{}) bool {
	if v, _ := a["has_premium"].(bool); v {
		return true
	}
	if mapInt(a, "premium_limit") > 0 {
		return true
	}
	if mapInt(a, "premium_balance") > 0 {
		return true
	}
	return false
}

// isResearchLimitedMap mirrors the frontend rule: non-premium accounts
// that have used >= 3 research-mode requests this billing cycle.
func isResearchLimitedMap(a map[string]interface{}) bool {
	if hasPremiumMap(a) {
		return false
	}
	return mapInt(a, "research_usage") >= 3
}

// mapBool / mapInt / mapString are tiny accessors that paper over the
// fact that GetAccountDetails stores values as interface{} (we round-trip
// JSON numbers through float64 in tests, but Go-native ints in prod).
func mapBool(a map[string]interface{}, k string) bool {
	v, _ := a[k].(bool)
	return v
}

func mapString(a map[string]interface{}, k string) string {
	v, _ := a[k].(string)
	return v
}

func mapInt(a map[string]interface{}, k string) int {
	switch v := a[k].(type) {
	case int:
		return v
	case int32:
		return int(v)
	case int64:
		return int(v)
	case float64:
		return int(v)
	case float32:
		return int(v)
	}
	return 0
}
