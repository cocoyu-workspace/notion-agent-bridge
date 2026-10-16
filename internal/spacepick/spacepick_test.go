package spacepick

import "testing"

func idx(c []Candidate) []int {
	out := make([]int, len(c))
	for i := range c {
		out[i] = c[i].Index
	}
	return out
}

func TestOrderPrefersBusinessOverFreeWhenPlanTypeIsTeamForBoth(t *testing.T) {
	free := Candidate{Index: 0, PlanType: "team", Tier: "free", AIEnabled: true}
	biz := Candidate{Index: 1, PlanType: "team", Tier: "business", AIEnabled: true}
	for name, in := range map[string][]Candidate{"free first": {free, biz}, "business first": {biz, free}} {
		got := Order(in)
		if got[0].Tier != "business" {
			t.Fatalf("%s: picked %q", name, got[0].Tier)
		}
	}
}

func TestOrderRules(t *testing.T) {
	c := func(i int, plan, tier string, ai bool) Candidate { return Candidate{i, plan, tier, ai} }
	cases := []struct {
		name string
		in   []Candidate
		want []int
	}{
		{"tier ladder", []Candidate{c(0, "team", "free", true), c(1, "team", "", true), c(2, "team", "plus", true), c(3, "team", "business", true), c(4, "enterprise", "enterprise", true)}, []int{4, 3, 2, 1, 0}},
		{"ai disabled loses to any ai enabled", []Candidate{c(0, "enterprise", "enterprise", false), c(1, "team", "free", true)}, []int{1, 0}},
		{"all free keeps original order", []Candidate{c(0, "free", "free", true), c(1, "free", "free", true), c(2, "free", "free", true)}, []int{0, 1, 2}},
		{"equal rank keeps first", []Candidate{c(0, "team", "business", true), c(1, "team", "business", true)}, []int{0, 1}},
		{"unknown tier falls back to plan_type", []Candidate{c(0, "free", "", true), c(1, "team", "", true)}, []int{1, 0}},
		{"paid tier beats unknown tier", []Candidate{c(0, "team", "", true), c(1, "team", "plus", true)}, []int{1, 0}},
		{"tier is case-insensitive", []Candidate{c(0, "team", "FREE", true), c(1, "team", "Business", true)}, []int{1, 0}},
	}
	for _, tc := range cases {
		got := idx(Order(tc.in))
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("%s: got %v want %v", tc.name, got, tc.want)
			}
		}
	}
}
