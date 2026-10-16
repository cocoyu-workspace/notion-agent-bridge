package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- merged from account_identity_test.go ---
// helper: create a temp dir with JSON account files and return the dir path.
func writeTempAccounts(t *testing.T, accounts ...map[string]interface{}) string {
	t.Helper()
	dir := t.TempDir()
	for i, acc := range accounts {
		data, err := json.MarshalIndent(acc, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		name := "acc" + string(rune('A'+i)) + ".json"
		if err := os.WriteFile(filepath.Join(dir, name), data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func baseAccount(userID, spaceID, email, name string) map[string]interface{} {
	return map[string]interface{}{
		"token_v2":       "tok_" + userID + "_" + spaceID,
		"user_id":        userID,
		"space_id":       spaceID,
		"user_email":     email,
		"user_name":      name,
		"space_name":     "Space-" + spaceID[:8],
		"client_version": DefaultClientVersion,
		"browser_id":     "00000000-0000-0000-0000-000000000000",
	}
}

// Test 1: same user_id + different space_id loads two profiles
func TestLoadFromDir_SameUserDifferentSpace(t *testing.T) {
	dir := writeTempAccounts(t,
		baseAccount("user-1", "space-aaa", "u@test.com", "User1"),
		baseAccount("user-1", "space-bbb", "u@test.com", "User1"),
	)
	p := NewAccountPool()
	if err := p.LoadFromDir(dir); err != nil {
		t.Fatal(err)
	}
	if p.Count() != 2 {
		t.Fatalf("expected 2 accounts, got %d", p.Count())
	}
}

// Test 2: same email + different space_id loads two profiles
func TestLoadFromDir_SameEmailDifferentSpace(t *testing.T) {
	dir := writeTempAccounts(t,
		baseAccount("user-A", "space-111", "shared@test.com", "Alice"),
		baseAccount("user-B", "space-222", "shared@test.com", "Bob"),
	)
	p := NewAccountPool()
	if err := p.LoadFromDir(dir); err != nil {
		t.Fatal(err)
	}
	if p.Count() != 2 {
		t.Fatalf("expected 2 accounts, got %d", p.Count())
	}
}

// Test 3: same user_id + same space_id is deduplicated
func TestLoadFromDir_SameUserSameSpace_Dedup(t *testing.T) {
	dir := writeTempAccounts(t,
		baseAccount("user-1", "space-aaa", "u@test.com", "User1"),
		baseAccount("user-1", "space-aaa", "u@test.com", "User1-dup"),
	)
	p := NewAccountPool()
	if err := p.LoadFromDir(dir); err != nil {
		t.Fatal(err)
	}
	if p.Count() != 1 {
		t.Fatalf("expected 1 account (deduped), got %d", p.Count())
	}
}

// Test 4: hot reload adds second workspace
func TestReloadFromDir_AddsSecondWorkspace(t *testing.T) {
	dir := writeTempAccounts(t,
		baseAccount("user-1", "space-aaa", "u@test.com", "User1"),
	)
	p := NewAccountPool()
	if err := p.LoadFromDir(dir); err != nil {
		t.Fatal(err)
	}
	if p.Count() != 1 {
		t.Fatalf("expected 1 account, got %d", p.Count())
	}
	// Add a second workspace file
	acc2 := baseAccount("user-1", "space-bbb", "u@test.com", "User1")
	data, _ := json.MarshalIndent(acc2, "", "  ")
	os.WriteFile(filepath.Join(dir, "accB.json"), data, 0644)
	p.ReloadFromDir(dir)
	if p.Count() != 2 {
		t.Fatalf("expected 2 accounts after reload, got %d", p.Count())
	}
}

// Test 5: SaveAccountToFile updates only matching workspace
func TestSaveAccountToFile_MatchingWorkspace(t *testing.T) {
	dir := t.TempDir()
	acc := &Account{
		TokenV2:       "tok_test",
		UserID:        "user-1",
		SpaceID:       "space-aaa",
		UserEmail:     "u@test.com",
		UserName:      "User1",
		SpaceName:     "SpaceA",
		ClientVersion: DefaultClientVersion,
		BrowserID:     "00000000-0000-0000-0000-000000000000",
	}
	acc.EnsureAccountID()
	_, err := SaveAccountToFile(acc, dir)
	if err != nil {
		t.Fatal(err)
	}
	// Verify the saved file contains account_id
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("expected 1 file, got %d", len(entries))
	}
	data, _ := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	var saved map[string]interface{}
	json.Unmarshal(data, &saved)
	if saved["account_id"] == nil || saved["account_id"] == "" {
		t.Fatal("saved file missing account_id")
	}
	if saved["account_id"] != acc.AccountID {
		t.Fatalf("account_id mismatch: got %v, want %s", saved["account_id"], acc.AccountID)
	}
}

// Test 6: deleting one workspace preserves another
func TestRemoveAccount_PreservesOtherWorkspace(t *testing.T) {
	dir := writeTempAccounts(t,
		baseAccount("user-1", "space-aaa", "u@test.com", "User1"),
		baseAccount("user-1", "space-bbb", "u@test.com", "User1"),
	)
	p := NewAccountPool()
	if err := p.LoadFromDir(dir); err != nil {
		t.Fatal(err)
	}
	if p.Count() != 2 {
		t.Fatalf("expected 2, got %d", p.Count())
	}
	// Find the space-aaa account and remove it by AccountID
	var target *Account
	for _, acc := range p.accounts {
		if acc.SpaceID == "space-aaa" {
			target = acc
			break
		}
	}
	if target == nil {
		t.Fatal("space-aaa account not found")
	}
	p.RemoveAccountByAccountID(target.AccountID)
	if p.Count() != 1 {
		t.Fatalf("expected 1 after removal, got %d", p.Count())
	}
	if p.accounts[0].SpaceID != "space-bbb" {
		t.Fatalf("wrong account preserved: got space_id=%s", p.accounts[0].SpaceID)
	}
}

// Test 7: legacy JSON without account_id remains loadable
func TestLoadFromDir_LegacyJSON(t *testing.T) {
	// Legacy JSON has no account_id field
	legacy := map[string]interface{}{
		"token_v2":       "tok_legacy",
		"user_id":        "user-legacy",
		"space_id":       "space-legacy",
		"user_email":     "legacy@test.com",
		"user_name":      "Legacy",
		"space_name":     "OldSpace",
		"client_version": DefaultClientVersion,
		"browser_id":     "00000000-0000-0000-0000-000000000000",
	}
	dir := writeTempAccounts(t, legacy)
	p := NewAccountPool()
	if err := p.LoadFromDir(dir); err != nil {
		t.Fatal(err)
	}
	if p.Count() != 1 {
		t.Fatalf("expected 1 account, got %d", p.Count())
	}
	acc := p.accounts[0]
	if acc.AccountID == "" {
		t.Fatal("account_id should be computed on load for legacy JSON")
	}
	expected := ComputeAccountID("user-legacy", "space-legacy")
	if acc.AccountID != expected {
		t.Fatalf("account_id mismatch: got %s, want %s", acc.AccountID, expected)
	}
}

// Test 8: legacy email lookup returns ambiguity error
func TestFindByEmail_AmbiguityError(t *testing.T) {
	dir := writeTempAccounts(t,
		baseAccount("user-1", "space-aaa", "shared@test.com", "User1"),
		baseAccount("user-1", "space-bbb", "shared@test.com", "User1"),
	)
	p := NewAccountPool()
	if err := p.LoadFromDir(dir); err != nil {
		t.Fatal(err)
	}
	_, err := p.FindByEmail("shared@test.com")
	if err == nil {
		t.Fatal("expected ambiguity error, got nil")
	}
	var ambErr *AmbiguousEmailError
	if !errors.As(err, &ambErr) {
		t.Fatalf("expected AmbiguousEmailError, got %T: %v", err, err)
	}
	if ambErr.Count != 2 {
		t.Fatalf("expected 2 matches, got %d", ambErr.Count)
	}
}

// Test 9: failover selects another workspace
func TestNextExcluding_FailoverSelectsOther(t *testing.T) {
	dir := writeTempAccounts(t,
		baseAccount("user-1", "space-aaa", "u@test.com", "User1"),
		baseAccount("user-1", "space-bbb", "u@test.com", "User1"),
	)
	p := NewAccountPool()
	if err := p.LoadFromDir(dir); err != nil {
		t.Fatal(err)
	}
	first := p.Next()
	if first == nil {
		t.Fatal("Next() returned nil")
	}
	exclude := map[*Account]bool{first: true}
	second := p.NextExcluding(exclude)
	if second == nil {
		t.Fatal("NextExcluding() returned nil — failover failed")
	}
	if second.AccountID == first.AccountID {
		t.Fatal("failover returned the same account")
	}
}

// Test 10: dashboard DTO contains account_id
func TestAccountDTO_ContainsAccountID(t *testing.T) {
	acc := &Account{
		TokenV2:   "tok_secret",
		UserID:    "user-1",
		SpaceID:   "space-aaa-1234-5678-9abc",
		UserEmail: "u@test.com",
		UserName:  "User1",
		SpaceName: "MySpace",
		PlanType:  "business",
	}
	acc.EnsureAccountID()
	dto := NewAccountDTO(acc)
	if dto.AccountID == "" {
		t.Fatal("DTO missing account_id")
	}
	if dto.AccountID != acc.AccountID {
		t.Fatalf("DTO account_id mismatch: got %s, want %s", dto.AccountID, acc.AccountID)
	}
	if dto.SpaceIDShort != "space-aa" {
		t.Fatalf("DTO space_id_short wrong: got %s", dto.SpaceIDShort)
	}
	if dto.UserEmail != "u@test.com" {
		t.Fatal("DTO missing user_email")
	}
	if dto.SpaceName != "MySpace" {
		t.Fatal("DTO missing space_name")
	}
}

// Test 11: dashboard DTO contains no token_v2 or cookie
func TestAccountDTO_NoSecrets(t *testing.T) {
	acc := &Account{
		TokenV2:    "super_secret_token",
		FullCookie: "session=abc123; token_v2=secret",
		UserID:     "user-1",
		SpaceID:    "space-aaa",
		UserEmail:  "u@test.com",
		UserName:   "User1",
		SpaceName:  "MySpace",
	}
	dto := NewAccountDTO(acc)
	// Marshal to JSON and check no secrets leak
	data, err := json.Marshal(dto)
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	if contains(s, "super_secret_token") {
		t.Fatal("DTO JSON contains token_v2")
	}
	if contains(s, "session=abc123") {
		t.Fatal("DTO JSON contains cookie")
	}
	if contains(s, "token_v2") {
		t.Fatal("DTO JSON contains token_v2 key")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// Test: NUL separator prevents ambiguity between ("ab","c") and ("a","bc")
func TestComputeAccountID_NULSeparator(t *testing.T) {
	id1 := ComputeAccountID("ab", "c")
	id2 := ComputeAccountID("a", "bc")
	if id1 == id2 {
		t.Fatal("(ab,c) and (a,bc) must produce different account_ids")
	}
}

// Test: EnsureAccountID is fail-closed on empty inputs
func TestEnsureAccountID_EmptyInputs(t *testing.T) {
	// Empty user_id
	acc1 := &Account{UserID: "", SpaceID: "space-aaa"}
	acc1.EnsureAccountID()
	if acc1.AccountID != "" {
		t.Fatal("empty user_id should not produce an account_id")
	}
	// Empty space_id
	acc2 := &Account{UserID: "user-1", SpaceID: ""}
	acc2.EnsureAccountID()
	if acc2.AccountID != "" {
		t.Fatal("empty space_id should not produce an account_id")
	}
	// Both empty
	acc3 := &Account{}
	acc3.EnsureAccountID()
	if acc3.AccountID != "" {
		t.Fatal("both empty should not produce an account_id")
	}
}

// Test: DTO does not contain full user_id or full space_id
func TestAccountDTO_NoFullIDs(t *testing.T) {
	acc := &Account{
		TokenV2:   "secret",
		UserID:    "user-1-full-uuid-value",
		SpaceID:   "space-aaa-full-uuid-value",
		UserEmail: "u@test.com",
		UserName:  "User1",
		SpaceName: "MySpace",
	}
	dto := NewAccountDTO(acc)
	data, _ := json.Marshal(dto)
	s := string(data)
	if contains(s, "user-1-full-uuid-value") {
		t.Fatal("DTO contains full user_id")
	}
	if contains(s, "space-aaa-full-uuid-value") {
		t.Fatal("DTO contains full space_id")
	}
	if !contains(s, "space-aa") {
		t.Fatal("DTO should contain shortened space_id")
	}
}

// Test: Two SaveAccountToFile calls with same email produce different files
func TestSaveAccountToFile_TwoWorkspacesDifferentFiles(t *testing.T) {
	dir := t.TempDir()
	accA := &Account{
		TokenV2: "tokA", UserID: "user-1", SpaceID: "space-aaa",
		UserEmail: "same@test.com", UserName: "User1", SpaceName: "SpaceA",
		ClientVersion: DefaultClientVersion, BrowserID: "00000000-0000-0000-0000-000000000000",
	}
	accB := &Account{
		TokenV2: "tokB", UserID: "user-1", SpaceID: "space-bbb",
		UserEmail: "same@test.com", UserName: "User1", SpaceName: "SpaceB",
		ClientVersion: DefaultClientVersion, BrowserID: "00000000-0000-0000-0000-000000000000",
	}
	fA, err := SaveAccountToFile(accA, dir)
	if err != nil {
		t.Fatal(err)
	}
	fB, err := SaveAccountToFile(accB, dir)
	if err != nil {
		t.Fatal(err)
	}
	if fA == fB {
		t.Fatalf("two workspaces with same email produced same filename: %s", fA)
	}
	// Verify both files exist
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Fatalf("expected 2 files, got %d", len(entries))
	}
}

// Test: DeleteAccountFileByEmail returns ambiguity error with two matches
func TestDeleteAccountFileByEmail_AmbiguityError(t *testing.T) {
	dir := t.TempDir()
	accA := &Account{
		TokenV2: "tokA", UserID: "user-1", SpaceID: "space-aaa",
		UserEmail: "same@test.com", UserName: "User1", SpaceName: "SpaceA",
		ClientVersion: DefaultClientVersion, BrowserID: "00000000-0000-0000-0000-000000000000",
	}
	accB := &Account{
		TokenV2: "tokB", UserID: "user-1", SpaceID: "space-bbb",
		UserEmail: "same@test.com", UserName: "User1", SpaceName: "SpaceB",
		ClientVersion: DefaultClientVersion, BrowserID: "00000000-0000-0000-0000-000000000000",
	}
	SaveAccountToFile(accA, dir)
	SaveAccountToFile(accB, dir)
	err := DeleteAccountFileByEmail("same@test.com", dir)
	if err == nil {
		t.Fatal("expected ambiguity error")
	}
	var ambErr *AmbiguousEmailError
	if !errors.As(err, &ambErr) {
		t.Fatalf("expected AmbiguousEmailError, got: %v", err)
	}
	// Verify nothing was deleted
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Fatalf("expected 2 files preserved, got %d", len(entries))
	}
}

// Test: DeleteAccountFile by account_id deletes only target
func TestDeleteAccountFile_ByAccountID_DeletesOnlyTarget(t *testing.T) {
	dir := t.TempDir()
	accA := &Account{
		TokenV2: "tokA", UserID: "user-1", SpaceID: "space-aaa",
		UserEmail: "same@test.com", UserName: "User1", SpaceName: "SpaceA",
		ClientVersion: DefaultClientVersion, BrowserID: "00000000-0000-0000-0000-000000000000",
	}
	accB := &Account{
		TokenV2: "tokB", UserID: "user-1", SpaceID: "space-bbb",
		UserEmail: "same@test.com", UserName: "User1", SpaceName: "SpaceB",
		ClientVersion: DefaultClientVersion, BrowserID: "00000000-0000-0000-0000-000000000000",
	}
	SaveAccountToFile(accA, dir)
	SaveAccountToFile(accB, dir)
	accA.EnsureAccountID()
	err := DeleteAccountFile(accA.AccountID, dir)
	if err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("expected 1 file remaining, got %d", len(entries))
	}
	// Verify the remaining file is accB
	data, _ := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	var saved map[string]interface{}
	json.Unmarshal(data, &saved)
	if saved["space_id"] != "space-bbb" {
		t.Fatalf("wrong file preserved: space_id=%v", saved["space_id"])
	}
}

// Test: Profile with usage > limit, remaining = 0, eligible = true should show as available
func TestAccountDTO_UsageExceedsLimit_StillAvailable(t *testing.T) {
	acc := &Account{
		UserID:    "user-overquota",
		SpaceID:   "space-overquota",
		UserEmail: "over@test.com",
		UserName:  "OverUser",
		SpaceName: "OverSpace",
		PlanType:  "business",
		QuotaInfo: &QuotaInfo{
			IsEligible: true,
			SpaceUsage: 604,
			SpaceLimit: 75,
			UserUsage:  604,
			UserLimit:  75,
		},
	}
	acc.EnsureAccountID()

	// The DTO should show the account as available (not exhausted)
	dto := NewAccountDTO(acc)
	if dto.IsExhausted {
		t.Fatal("account with eligible=true should NOT be marked exhausted even when usage > limit")
	}
	if dto.IsPermanent {
		t.Fatal("account with eligible=true should NOT be permanently exhausted")
	}

	// Verify the pool also treats it as usable
	pool := NewAccountPool()
	pool.accounts = []*Account{acc}
	if pool.isQuotaExhausted(acc) {
		t.Fatal("pool should NOT mark eligible=true account as exhausted")
	}
	next := pool.Next()
	if next == nil {
		t.Fatal("pool.Next() should return the account (it is eligible)")
	}

	// Verify quota details are exposed correctly for UI
	details := pool.GetAccountDetails()
	if len(details) != 1 {
		t.Fatalf("expected 1 detail entry, got %d", len(details))
	}
	d := details[0]
	// Basic usage fields
	if d["space_usage"] != 604 {
		t.Fatalf("space_usage wrong: got %v", d["space_usage"])
	}
	if d["space_limit"] != 75 {
		t.Fatalf("space_limit wrong: got %v", d["space_limit"])
	}
	// Remaining should be 0 (clamped, not negative)
	if d["remaining"] != 0 {
		t.Fatalf("remaining should be 0 when usage > limit, got %v", d["remaining"])
	}
	// Eligible should be true
	if d["eligible"] != true {
		t.Fatalf("eligible should be true, got %v", d["eligible"])
	}
	// Exhausted should be false
	if d["exhausted"] != false {
		t.Fatalf("exhausted should be false for eligible account, got %v", d["exhausted"])
	}
}

// Test: ComputeAccountID determinism and format
func TestComputeAccountID(t *testing.T) {
	id1 := ComputeAccountID("user-1", "space-aaa")
	id2 := ComputeAccountID("user-1", "space-aaa")
	if id1 != id2 {
		t.Fatal("ComputeAccountID is not deterministic")
	}
	if len(id1) != 64 {
		t.Fatalf("expected 64-char hex, got %d chars", len(id1))
	}
	// Different space_id must produce different account_id
	id3 := ComputeAccountID("user-1", "space-bbb")
	if id1 == id3 {
		t.Fatal("different space_id should produce different account_id")
	}
}

// --- merged from account_space_selection_test.go ---
// Workspace selection at import: a user with several spaces whose plan_type is
// "team" for both a free and a paid one (only subscription_tier differs) must get
// the paid one regardless of the order Notion lists them.

type testSpace struct {
	id, name, plan, tier string
	aiOff                bool
}

// spaceJSON renders a space record in the nested ({value:{value:{...}}}) or the
// flat ({value:{...}}) shape.
func spaceJSON(s testSpace, nested bool) string {
	fields := fmt.Sprintf(`"id":%q,"name":%q,"plan_type":%q`, s.id, s.name, s.plan)
	if s.tier != "" {
		fields += fmt.Sprintf(`,"subscription_tier":%q`, s.tier)
	}
	if s.aiOff {
		fields += `,"settings":{"disable_ai_feature":true}`
	}
	if nested {
		return `{"value":{"value":{` + fields + `}}}`
	}
	return `{"value":{` + fields + `}}`
}

func discoveryBody(spaces []testSpace, nested bool) string {
	var pointers, records []string
	for _, s := range spaces {
		pointers = append(pointers, fmt.Sprintf(`{"spaceId":%q,"id":"view-%s"}`, s.id, s.id))
		records = append(records, fmt.Sprintf(`%q:%s`, s.id, spaceJSON(s, nested)))
	}
	return `{"recordMap":{"notion_user":{"u1":{"value":{"value":{"name":"U","email":"u@example.com"}}}},` +
		`"user_root":{"u1":{"value":{"value":{"space_view_pointers":[` + strings.Join(pointers, ",") + `]}}}},` +
		`"space":{` + strings.Join(records, ",") + `},"user_settings":{}}}`
}

type quotaScript struct {
	mu       sync.Mutex
	calls    []string
	eligible map[string]bool // missing = nil result (unknown)
	failFor  map[string]bool
}

func stubSpaceQuota(t *testing.T, q *quotaScript) {
	t.Helper()
	discoveryCheckQuota = func(acc *Account) (*QuotaInfo, error) {
		q.mu.Lock()
		defer q.mu.Unlock()
		q.calls = append(q.calls, acc.SpaceID)
		if q.failFor[acc.SpaceID] {
			return nil, fmt.Errorf("quota api down")
		}
		if e, ok := q.eligible[acc.SpaceID]; ok {
			return &QuotaInfo{IsEligible: e}, nil
		}
		return nil, nil
	}
	// stubAccountDiscovery restores the original on cleanup.
}

func discoverSpaces(t *testing.T, spaces []testSpace, nested bool, opts AccountDiscoveryOptions) (*Account, error) {
	t.Helper()
	stubAccountDiscovery(t, discoveryBody(spaces, nested), nil)
	return DiscoverAccountFromTokenWithOptions("secret-token", opts)
}

var (
	freeTeam = testSpace{id: "sp-free", name: "Free Space", plan: "team", tier: "free"}
	bizTeam  = testSpace{id: "sp-biz", name: "Biz Space", plan: "team", tier: "business"}
)

func TestWorkspaceSelectionPrefersBusinessTierInEitherOrderAndShape(t *testing.T) {
	for _, nested := range []bool{true, false} {
		for name, order := range map[string][]testSpace{"free first": {freeTeam, bizTeam}, "business first": {bizTeam, freeTeam}} {
			t.Run(fmt.Sprintf("nested=%v/%s", nested, name), func(t *testing.T) {
				acc, err := discoverSpaces(t, order, nested, AccountDiscoveryOptions{})
				if err != nil {
					t.Fatal(err)
				}
				if acc.SpaceID != "sp-biz" {
					t.Fatalf("picked %s (%s), want the business space", acc.SpaceID, acc.SpaceName)
				}
			})
		}
	}
}

func TestWorkspaceSelectionTierLadderAndFallbacks(t *testing.T) {
	cases := []struct {
		name   string
		spaces []testSpace
		want   string
	}{
		{"enterprise beats business", []testSpace{bizTeam, {id: "sp-ent", name: "E", plan: "enterprise", tier: "enterprise"}}, "sp-ent"},
		{"business beats plus", []testSpace{{id: "sp-plus", name: "P", plan: "team", tier: "plus"}, bizTeam}, "sp-biz"},
		{"plus beats unknown tier", []testSpace{{id: "sp-unk", name: "U", plan: "team"}, {id: "sp-plus", name: "P", plan: "team", tier: "plus"}}, "sp-plus"},
		{"unknown tier beats free tier", []testSpace{freeTeam, {id: "sp-unk", name: "U", plan: "team"}}, "sp-unk"},
		{"AI disabled loses even to a free AI-enabled space", []testSpace{{id: "sp-off", name: "Off", plan: "team", tier: "enterprise", aiOff: true}, freeTeam}, "sp-free"},
		{"all free: the first one wins, not the last", []testSpace{{id: "sp-a", name: "A", plan: "free", tier: "free"}, {id: "sp-b", name: "B", plan: "free", tier: "free"}, {id: "sp-c", name: "C", plan: "free", tier: "free"}}, "sp-a"},
		{"equal rank keeps the first", []testSpace{{id: "sp-1", name: "1", plan: "team", tier: "business"}, {id: "sp-2", name: "2", plan: "team", tier: "business"}}, "sp-1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			acc, err := discoverSpaces(t, tc.spaces, true, AccountDiscoveryOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if acc.SpaceID != tc.want {
				t.Fatalf("picked %s, want %s", acc.SpaceID, tc.want)
			}
		})
	}
}

func TestWorkspaceSelectionRequestedSpaceIDIsAlwaysHonored(t *testing.T) {
	q := &quotaScript{eligible: map[string]bool{"sp-biz": true, "sp-free": false}}
	stubAccountDiscovery(t, discoveryBody([]testSpace{bizTeam, freeTeam}, true), nil)
	stubSpaceQuota(t, q)
	acc, err := DiscoverAccountFromTokenWithOptions("secret-token", AccountDiscoveryOptions{SpaceID: "sp-free"})
	if err != nil {
		t.Fatal(err)
	}
	if acc.SpaceID != "sp-free" {
		t.Fatalf("explicit NOTION_SPACE_ID must win, got %s", acc.SpaceID)
	}
	if len(q.calls) != 1 {
		t.Fatalf("a pinned space needs a single quota call, got %v", q.calls)
	}
	if _, err := DiscoverAccountFromTokenWithOptions("secret-token", AccountDiscoveryOptions{SpaceID: "nope"}); err == nil {
		t.Fatal("unknown requested space must still fail")
	}
}

func TestWorkspaceSelectionEligibilityBreaksTieWithinTopTwo(t *testing.T) {
	third := testSpace{id: "sp-third", name: "Third", plan: "team", tier: "free"}
	for name, order := range map[string][]testSpace{"biz first": {bizTeam, freeTeam, third}, "free first": {freeTeam, bizTeam, third}} {
		t.Run("top candidate eligible costs one call/"+name, func(t *testing.T) {
			q := &quotaScript{eligible: map[string]bool{"sp-biz": true, "sp-free": true}}
			stubAccountDiscovery(t, discoveryBody(order, true), nil)
			stubSpaceQuota(t, q)
			acc, err := DiscoverAccountFromTokenWithOptions("secret-token", AccountDiscoveryOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if acc.SpaceID != "sp-biz" || len(q.calls) != 1 {
				t.Fatalf("space=%s calls=%v (the single probe must be reused as the import quota check)", acc.SpaceID, q.calls)
			}
		})
		t.Run("ineligible first, eligible second wins/"+name, func(t *testing.T) {
			q := &quotaScript{eligible: map[string]bool{"sp-biz": false, "sp-free": true, "sp-third": true}}
			stubAccountDiscovery(t, discoveryBody(order, true), nil)
			stubSpaceQuota(t, q)
			acc, err := DiscoverAccountFromTokenWithOptions("secret-token", AccountDiscoveryOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if acc.SpaceID != "sp-free" || len(q.calls) != 2 {
				t.Fatalf("space=%s calls=%v: only the top two are probed", acc.SpaceID, q.calls)
			}
			if acc.QuotaInfo == nil || !acc.QuotaInfo.IsEligible {
				t.Fatalf("quota of the chosen space must be stored: %#v", acc.QuotaInfo)
			}
		})
	}
	t.Run("both ineligible keeps tier order", func(t *testing.T) {
		q := &quotaScript{eligible: map[string]bool{"sp-biz": false, "sp-free": false}}
		stubAccountDiscovery(t, discoveryBody([]testSpace{freeTeam, bizTeam}, true), nil)
		stubSpaceQuota(t, q)
		acc, err := DiscoverAccountFromTokenWithOptions("secret-token", AccountDiscoveryOptions{})
		if err != nil || acc.SpaceID != "sp-biz" {
			t.Fatalf("acc=%v err=%v", acc, err)
		}
	})
	t.Run("quota API failure never changes the tier choice", func(t *testing.T) {
		q := &quotaScript{failFor: map[string]bool{"sp-biz": true, "sp-free": true}}
		stubAccountDiscovery(t, discoveryBody([]testSpace{freeTeam, bizTeam}, true), nil)
		stubSpaceQuota(t, q)
		acc, err := DiscoverAccountFromTokenWithOptions("secret-token", AccountDiscoveryOptions{})
		if err != nil || acc.SpaceID != "sp-biz" || len(q.calls) != 1 {
			t.Fatalf("acc=%v err=%v calls=%v", acc, err, q.calls)
		}
	})
	t.Run("single space and AI-disabled second are not probed twice", func(t *testing.T) {
		q := &quotaScript{eligible: map[string]bool{"sp-biz": false}}
		off := testSpace{id: "sp-off", name: "Off", plan: "team", tier: "enterprise", aiOff: true}
		stubAccountDiscovery(t, discoveryBody([]testSpace{bizTeam, off}, true), nil)
		stubSpaceQuota(t, q)
		acc, err := DiscoverAccountFromTokenWithOptions("secret-token", AccountDiscoveryOptions{})
		if err != nil || acc.SpaceID != "sp-biz" || len(q.calls) != 1 {
			t.Fatalf("acc=%v err=%v calls=%v", acc, err, q.calls)
		}
	})
}

// --- merged from account_api_discovery_test.go ---
type accountDiscoveryRoundTripper func(*http.Request) (*http.Response, error)

func (f accountDiscoveryRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func stubAccountDiscovery(t *testing.T, responseBody string, inspect func(*http.Request)) {
	t.Helper()
	originalConfig := AppConfig
	originalHTTPClient := discoveryHTTPClient
	originalFetchModels := discoveryFetchModels
	originalCheckQuota := discoveryCheckQuota
	AppConfig = DefaultConfig()
	discoveryHTTPClient = func(time.Duration) *http.Client {
		return &http.Client{Transport: accountDiscoveryRoundTripper(func(req *http.Request) (*http.Response, error) {
			if inspect != nil {
				inspect(req)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(responseBody)),
				Request:    req,
			}, nil
		})}
	}
	discoveryFetchModels = func(*Account) ([]ModelEntry, error) { return nil, nil }
	discoveryCheckQuota = func(*Account) (*QuotaInfo, error) { return nil, nil }
	t.Cleanup(func() {
		AppConfig = originalConfig
		discoveryHTTPClient = originalHTTPClient
		discoveryFetchModels = originalFetchModels
		discoveryCheckQuota = originalCheckQuota
	})
}

func TestDiscoverAccountFromTokenWithOptionsSelectsExactTarget(t *testing.T) {
	responseBody := `{
  "recordMap": {
    "notion_user": {
      "decoy-user": {"value":{"value":{"name":"Decoy","email":"decoy@example.com"}}},
      "target-user": {"value":{"value":{"name":"Target","email":"target@example.com"}}}
    },
    "user_root": {
      "decoy-user": {"value":{"value":{"space_view_pointers":[{"spaceId":"decoy-space","id":"decoy-view"}]}}},
      "target-user": {"value":{"value":{"space_view_pointers":[{"spaceId":"target-space","id":"target-view"}]}}}
    },
    "space": {
      "decoy-space": {"value":{"value":{"id":"decoy-space","name":"Decoy Space","plan_type":"enterprise"}}},
      "target-space": {"value":{"value":{"id":"target-space","name":"Target Space","plan_type":"team"}}}
    },
    "user_settings": {}
  }
}`
	stubAccountDiscovery(t, responseBody, func(req *http.Request) {
		if got := req.Header.Get("x-notion-active-user-header"); got != "target-user" {
			t.Fatalf("active user header = %q, want target-user", got)
		}
		for name, want := range map[string]string{
			"token_v2":       "secret-token",
			"notion_user_id": "target-user",
			"notion_users":   `%5B%22target-user%22%5D`,
		} {
			cookie, err := req.Cookie(name)
			if err != nil {
				t.Fatalf("cookie %s: %v", name, err)
			}
			if cookie.Value != want {
				t.Fatalf("cookie %s = %q, want %q", name, cookie.Value, want)
			}
		}
	})

	account, err := DiscoverAccountFromTokenWithOptions("secret-token", AccountDiscoveryOptions{
		ActiveUserID:  "target-user",
		SpaceID:       "target-space",
		ExpectedEmail: "TARGET@example.com",
	})
	if err != nil {
		t.Fatalf("DiscoverAccountFromTokenWithOptions: %v", err)
	}
	if account.UserID != "target-user" || account.UserEmail != "target@example.com" || account.SpaceID != "target-space" {
		t.Fatalf("discovered wrong account: user=%q email=%q space=%q", account.UserID, account.UserEmail, account.SpaceID)
	}
}

func TestHandleAddAccountUsesOptionalNotionUserID(t *testing.T) {
	responseBody := `{
  "recordMap": {
    "notion_user": {
      "target-user": {"value":{"value":{"name":"Target","email":"target@example.com"}}}
    },
    "user_root": {
      "target-user": {"value":{"value":{"space_view_pointers":[{"spaceId":"target-space","id":"target-view"}]}}}
    },
    "space": {
      "target-space": {"value":{"value":{"id":"target-space","name":"Target Space","plan_type":"team"}}}
    },
    "user_settings": {}
  }
}`
	stubAccountDiscovery(t, responseBody, func(req *http.Request) {
		if got := req.Header.Get("x-notion-active-user-header"); got != "target-user" {
			t.Fatalf("active user header = %q, want target-user", got)
		}
	})

	pool := NewAccountPool()
	request := httptest.NewRequest(http.MethodPost, "/admin/accounts/add", strings.NewReader(
		`{"token_v2":"secret-token","notion_user_id":" target-user "}`,
	))
	recorder := httptest.NewRecorder()
	HandleAddAccount(pool, t.TempDir()).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if pool.Count() != 1 {
		t.Fatalf("pool count = %d, want 1", pool.Count())
	}
	if account := pool.GetByEmail("target@example.com"); account == nil || account.UserID != "target-user" {
		t.Fatalf("imported account = %#v, want target-user", account)
	}
}

func TestDiscoverAccountFromTokenWithOptionsRejectsMismatchedSelector(t *testing.T) {
	responseBody := `{
  "recordMap": {
    "notion_user": {
      "decoy-user": {"value":{"value":{"name":"Decoy","email":"decoy@example.com"}}}
    },
    "user_root": {
      "decoy-user": {"value":{"value":{"space_view_pointers":[{"spaceId":"decoy-space","id":"decoy-view"}]}}}
    },
    "space": {
      "decoy-space": {"value":{"value":{"id":"decoy-space","name":"Decoy Space","plan_type":"team"}}}
    }
  }
}`
	stubAccountDiscovery(t, responseBody, nil)

	account, err := DiscoverAccountFromTokenWithOptions("secret-token", AccountDiscoveryOptions{
		ExpectedEmail: "target@example.com",
		SpaceID:       "target-space",
	})
	if err == nil || !strings.Contains(err.Error(), "selectors") {
		t.Fatalf("discovery error = %v", err)
	}
	if account != nil {
		t.Fatalf("account = %#v, want nil", account)
	}
}
