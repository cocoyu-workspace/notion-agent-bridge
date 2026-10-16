package proxy

import (
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func newTestStats(t *testing.T) (*UsageStats, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "stats.json")
	s := &UsageStats{
		ByDay:     map[string]*usageBucket{},
		ByModel:   map[string]*usageBucket{},
		ByAccount: map[string]*usageBucket{},
		path:      path,
	}
	return s, path
}

func TestUsageStatsRecord_AggregatesAcrossDimensions(t *testing.T) {
	s, _ := newTestStats(t)

	s.Record("alice@example.com", "claude-sonnet-4", 100, 50)
	s.Record("alice@example.com", "claude-sonnet-4", 200, 75)
	s.Record("bob@example.com", "claude-haiku", 10, 5)

	snap := s.Snapshot(5)
	if got, want := snap.Total.Input, int64(310); got != want {
		t.Fatalf("total input = %d, want %d", got, want)
	}
	if got, want := snap.Total.Output, int64(130); got != want {
		t.Fatalf("total output = %d, want %d", got, want)
	}
	if got, want := snap.Total.Total, int64(440); got != want {
		t.Fatalf("total total = %d, want %d", got, want)
	}
	if got, want := snap.Total.Requests, int64(3); got != want {
		t.Fatalf("requests = %d, want %d", got, want)
	}
	if snap.Today.Total != snap.Total.Total {
		t.Fatalf("today should equal total when only today has data, got today=%d total=%d",
			snap.Today.Total, snap.Total.Total)
	}

	if len(snap.TopModels) != 2 {
		t.Fatalf("top models = %d, want 2", len(snap.TopModels))
	}
	if snap.TopModels[0].Model != "claude-sonnet-4" {
		t.Fatalf("top model[0] = %q, want claude-sonnet-4", snap.TopModels[0].Model)
	}
	if snap.TopModels[0].Count != 2 {
		t.Fatalf("sonnet count = %d, want 2", snap.TopModels[0].Count)
	}

	if len(snap.TopAccounts) != 2 {
		t.Fatalf("top accounts = %d, want 2", len(snap.TopAccounts))
	}
	if snap.TopAccounts[0].Email != "alice@example.com" {
		t.Fatalf("top account[0] = %q, want alice@example.com", snap.TopAccounts[0].Email)
	}
}

func TestUsageStatsRecord_ConcurrentSafe(t *testing.T) {
	s, _ := newTestStats(t)

	const goroutines = 8
	const perGoroutine = 200
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < perGoroutine; j++ {
				s.Record("acct", "model", 1, 1)
			}
		}()
	}
	wg.Wait()

	snap := s.Snapshot(5)
	want := int64(goroutines * perGoroutine)
	if snap.Total.Requests != want {
		t.Fatalf("requests = %d, want %d", snap.Total.Requests, want)
	}
	if snap.Total.Input != want || snap.Total.Output != want {
		t.Fatalf("input=%d output=%d, both want %d", snap.Total.Input, snap.Total.Output, want)
	}
}

func TestUsageStatsSaveLoadRoundTrip(t *testing.T) {
	s, path := newTestStats(t)
	s.Record("a@example.com", "m1", 7, 3)
	s.Record("b@example.com", "m2", 5, 5)

	if err := s.Save(path); err != nil {
		t.Fatalf("save: %v", err)
	}

	loaded := &UsageStats{
		ByDay:     map[string]*usageBucket{},
		ByModel:   map[string]*usageBucket{},
		ByAccount: map[string]*usageBucket{},
	}
	if err := loaded.Load(path); err != nil {
		t.Fatalf("load: %v", err)
	}

	got := loaded.Snapshot(5)
	if got.Total.Input != 12 || got.Total.Output != 8 {
		t.Fatalf("round-trip totals = %d/%d, want 12/8", got.Total.Input, got.Total.Output)
	}
	if got.Total.Requests != 2 {
		t.Fatalf("round-trip requests = %d, want 2", got.Total.Requests)
	}
	if len(got.TopModels) != 2 || len(got.TopAccounts) != 2 {
		t.Fatalf("round-trip top rows = %d/%d, want 2/2", len(got.TopModels), len(got.TopAccounts))
	}
}

func TestUsageStatsTrimsOldDays(t *testing.T) {
	s, _ := newTestStats(t)

	now := time.Now()
	// Seed 40 historical days; only the most recent 30 should survive a Record.
	for i := 0; i < 40; i++ {
		day := now.AddDate(0, 0, -i).Format("2006-01-02")
		s.ByDay[day] = &usageBucket{Input: 1, Output: 1}
	}
	if got := len(s.ByDay); got != 40 {
		t.Fatalf("seeded ByDay len = %d, want 40", got)
	}

	s.Record("acct", "model", 1, 1)

	if got := len(s.ByDay); got > usageStatsRetainDays+1 {
		// +1 because today's bucket may be added on top of the 30 retained.
		t.Fatalf("ByDay len after trim = %d, want <= %d", got, usageStatsRetainDays+1)
	}
}
