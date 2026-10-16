// Package spacepick ranks the workspaces (spaces) of one Notion user so that
// account import picks the same, deterministic candidate everywhere.
package spacepick

import (
	"sort"
	"strings"
)

// Candidate is one workspace the user can reach. Only the fields that decide
// the ranking are carried; Index is the position in the original order.
type Candidate struct {
	Index     int
	PlanType  string // space.plan_type ("team" for both free and paid team spaces)
	Tier      string // space.subscription_tier ("free", "plus", "business", "enterprise", ...)
	AIEnabled bool   // false when the space disables AI
}

// TierRank orders subscription tiers: enterprise > business > plus/team (paid)
// > unknown > free.
func TierRank(tier string) int {
	switch strings.ToLower(strings.TrimSpace(tier)) {
	case "enterprise":
		return 4
	case "business":
		return 3
	case "plus", "team", "personal", "personal_pro", "pro":
		return 2
	case "free":
		return 0
	}
	return 1 // empty or not recognized
}

func paid(planType string) bool {
	return !strings.EqualFold(strings.TrimSpace(planType), "free")
}

// better reports whether a strictly outranks b. Equal rank is not "better", so
// the earlier candidate in the original order stays first.
func better(a, b Candidate) bool {
	if a.AIEnabled != b.AIEnabled {
		return a.AIEnabled
	}
	if ra, rb := TierRank(a.Tier), TierRank(b.Tier); ra != rb {
		return ra > rb
	}
	if pa, pb := paid(a.PlanType), paid(b.PlanType); pa != pb {
		return pa
	}
	return false
}

// Order returns the candidates best first. The sort is stable: candidates of
// equal rank keep their original relative order (the first one wins).
func Order(cands []Candidate) []Candidate {
	out := append([]Candidate(nil), cands...)
	sort.SliceStable(out, func(i, j int) bool { return better(out[i], out[j]) })
	return out
}
