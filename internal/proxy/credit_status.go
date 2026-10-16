package proxy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"
)

// Notion-native AI allowance (getCreditRateLimitStatus).
//
// Observed from the real Settings -> Notion AI -> Usage page (request body is
// only {"spaceId": ...}, signed with the selected account's own credentials):
//
//	{"status":"within_limit",
//	 "window":{"limit":100,"scope":"per_user","creditType":"basic_ai_credits","window":"6h","used":23.81},
//	 "resetsInSeconds":16748,
//	 "billingPeriodWindow":{"limit":100,"scope":"per_user","creditType":"basic_ai_credits",
//	                        "cadence":"billing_period","used":5.62,"periodEndMs":1794484800000},
//	 "allowances":[{"bucketId":"standard","cadence":"billing_period","state":"resolved","enforcement":"enforced",
//	                "limit":100,"used":5.62,"periodStartMs":...,"periodEndMs":...},
//	               {"bucketId":"auto","cadence":"billing_period","state":"not_exposed"}, ...
//	               {"bucketId":"standard","cadence":"rolling","state":"resolved","enforcement":"enforced",
//	                "window":"6h","limit":100,"used":23.81,"resetsInSeconds":16748}]}
//
// and for an education workspace: {"status":"not_applicable"} (no windows).
//
// Only these two statuses were observed. Any other status string is kept as
// raw text and shown as "unknown"; it is never mapped to available/exhausted.
// These windows are a different quota system from the legacy V1/V2 counters
// (getAIUsageEligibility*): the two must not be converted into each other.

const (
	CreditStatusWithinLimit   = "within_limit"
	CreditStatusNotApplicable = "not_applicable"

	// Display states derived from a CreditStatus (see creditView).
	CreditStateUnchecked     = "unchecked"      // never queried
	CreditStateWithinLimit   = "within_limit"   // status confirmed, windows present
	CreditStateNoWindow      = "no_window"      // within_limit but the window object is missing/null
	CreditStateNotApplicable = "not_applicable" // API answered: no plan window for this account
	CreditStateUnknown       = "unknown_status" // API answered with a status we have not observed
	CreditStateFailed        = "failed"         // last refresh failed (previous data kept, if any)
	CreditStateIncomplete    = "incomplete"     // within_limit label, but numbers/times missing or contradictory
	CreditStateExpired       = "expired"        // every known window has passed its reset time
)

// CreditWindow is one usage window. Used/Limit are pointers so a missing or
// null value is never read as 0.
type CreditWindow struct {
	Scope           string   `json:"scope,omitempty"`
	CreditType      string   `json:"credit_type,omitempty"`
	Window          string   `json:"window,omitempty"`  // rolling size, e.g. "6h"
	Cadence         string   `json:"cadence,omitempty"` // e.g. "billing_period"
	Limit           *float64 `json:"limit,omitempty"`
	Used            *float64 `json:"used,omitempty"`
	ResetsInSeconds *float64 `json:"resets_in_seconds,omitempty"` // as of CreditStatus.ObservedAt
	PeriodEndMs     int64    `json:"period_end_ms,omitempty"`
}

type CreditAllowance struct {
	BucketID        string   `json:"bucket_id,omitempty"`
	Cadence         string   `json:"cadence,omitempty"`
	State           string   `json:"state,omitempty"` // resolved | not_exposed | ... (raw)
	Enforcement     string   `json:"enforcement,omitempty"`
	Window          string   `json:"window,omitempty"`
	Limit           *float64 `json:"limit,omitempty"`
	Used            *float64 `json:"used,omitempty"`
	ResetsInSeconds *float64 `json:"resets_in_seconds,omitempty"`
	PeriodStartMs   int64    `json:"period_start_ms,omitempty"`
	PeriodEndMs     int64    `json:"period_end_ms,omitempty"`
}

// CreditStatus is the stored snapshot of the last getCreditRateLimitStatus
// answer for an account/workspace.
type CreditStatus struct {
	Status          string            `json:"status,omitempty"` // raw API status
	ObservedAt      time.Time         `json:"observed_at"`
	Rolling         *CreditWindow     `json:"rolling,omitempty"`
	ResetsInSeconds *float64          `json:"resets_in_seconds,omitempty"` // top-level, rolling window reset
	Billing         *CreditWindow     `json:"billing,omitempty"`
	Allowances      []CreditAllowance `json:"allowances,omitempty"`
	// FetchError is a short machine code set when the latest refresh failed;
	// the previous snapshot (if any) is kept next to it.
	FetchError   string    `json:"fetch_error,omitempty"`
	FetchErrorAt time.Time `json:"fetch_error_at,omitempty"`
}

func cloneCreditStatus(src *CreditStatus) *CreditStatus {
	if src == nil {
		return nil
	}
	dst := *src
	if src.Rolling != nil {
		w := *src.Rolling
		dst.Rolling = &w
	}
	if src.Billing != nil {
		w := *src.Billing
		dst.Billing = &w
	}
	if src.ResetsInSeconds != nil {
		v := *src.ResetsInSeconds
		dst.ResetsInSeconds = &v
	}
	if src.Allowances != nil {
		dst.Allowances = append([]CreditAllowance(nil), src.Allowances...)
	}
	return &dst
}

// rawCreditResponse mirrors the observed response. Every field is optional.
type rawCreditResponse struct {
	Status          *string  `json:"status"`
	Window          *rawWin  `json:"window"`
	ResetsInSeconds *float64 `json:"resetsInSeconds"`
	Billing         *rawWin  `json:"billingPeriodWindow"`
	Allowances      []struct {
		BucketID        string   `json:"bucketId"`
		Cadence         string   `json:"cadence"`
		State           string   `json:"state"`
		Enforcement     string   `json:"enforcement"`
		Window          string   `json:"window"`
		Limit           *float64 `json:"limit"`
		Used            *float64 `json:"used"`
		ResetsInSeconds *float64 `json:"resetsInSeconds"`
		PeriodStartMs   *float64 `json:"periodStartMs"`
		PeriodEndMs     *float64 `json:"periodEndMs"`
	} `json:"allowances"`
}

type rawWin struct {
	Limit       *float64 `json:"limit"`
	Used        *float64 `json:"used"`
	Scope       string   `json:"scope"`
	CreditType  string   `json:"creditType"`
	Window      string   `json:"window"`
	Cadence     string   `json:"cadence"`
	PeriodEndMs *float64 `json:"periodEndMs"`
}

func (w *rawWin) toWindow() *CreditWindow {
	if w == nil {
		return nil
	}
	out := &CreditWindow{Scope: w.Scope, CreditType: w.CreditType, Window: w.Window, Cadence: w.Cadence, Limit: w.Limit, Used: w.Used}
	if w.PeriodEndMs != nil {
		out.PeriodEndMs = int64(*w.PeriodEndMs)
	}
	return out
}

// parseCreditRateLimitStatus decodes an API answer. A body that is not a JSON
// object, or that carries no status string, is an error: callers must treat
// that as a failed refresh, never as "no limit".
func parseCreditRateLimitStatus(body []byte, observedAt time.Time) (*CreditStatus, error) {
	var raw rawCreditResponse
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("parse credit status: %w", err)
	}
	if raw.Status == nil || strings.TrimSpace(*raw.Status) == "" {
		return nil, fmt.Errorf("credit status response has no status")
	}
	cs := &CreditStatus{
		Status:          strings.TrimSpace(*raw.Status),
		ObservedAt:      observedAt,
		Rolling:         raw.Window.toWindow(),
		ResetsInSeconds: raw.ResetsInSeconds,
		Billing:         raw.Billing.toWindow(),
	}
	for _, a := range raw.Allowances {
		al := CreditAllowance{BucketID: a.BucketID, Cadence: a.Cadence, State: a.State, Enforcement: a.Enforcement,
			Window: a.Window, Limit: a.Limit, Used: a.Used, ResetsInSeconds: a.ResetsInSeconds}
		if a.PeriodStartMs != nil {
			al.PeriodStartMs = int64(*a.PeriodStartMs)
		}
		if a.PeriodEndMs != nil {
			al.PeriodEndMs = int64(*a.PeriodEndMs)
		}
		cs.Allowances = append(cs.Allowances, al)
	}
	return cs, nil
}

// CheckCreditStatus calls the native allowance endpoint for the account's own
// workspace. The error is a short machine code string safe to store and show.
var creditStatusFetcher = CheckCreditStatus

// nativeHTTPClient builds the client used for the quota/identity endpoints; tests
// replace it to talk to a plain-HTTP fake server.
var nativeHTTPClient = getChromeHTTPClient

func CheckCreditStatus(acc *Account) (*CreditStatus, error) {
	body, _ := json.Marshal(map[string]string{"spaceId": acc.SpaceID})
	req, err := http.NewRequest("POST", NotionAPIBase+"/getCreditRateLimitStatus", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("request")
	}
	setNotionHeadersJSON(req, acc)
	resp, err := nativeHTTPClient(AppConfig.APITimeoutDuration()).Do(req)
	if err != nil {
		return nil, fmt.Errorf("network")
	}
	defer resp.Body.Close()
	reader, cleanup, derr := decompressBody(resp)
	if derr != nil {
		return nil, fmt.Errorf("decode")
	}
	if cleanup != nil {
		defer cleanup()
	}
	data, _ := io.ReadAll(io.LimitReader(reader, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http_%d", resp.StatusCode)
	}
	cs, perr := parseCreditRateLimitStatus(data, time.Now())
	if perr != nil {
		return nil, fmt.Errorf("parse")
	}
	return cs, nil
}

// ---- derived values (computed at read time so countdowns advance) ----

func floatPtrVal(p *float64) (float64, bool) {
	if p == nil || math.IsNaN(*p) || math.IsInf(*p, 0) {
		return 0, false
	}
	return *p, true
}

// windowPercent returns used/limit*100. Unknown when either value is missing
// or the limit is not positive; never 0 by default.
func windowPercent(used, limit *float64) (float64, bool) {
	u, okU := floatPtrVal(used)
	l, okL := floatPtrVal(limit)
	if !okU || !okL || l <= 0 || u < 0 {
		return 0, false
	}
	return u / l * 100, true
}

// rollingResetAt is when the rolling window resets, from the observation time.
func (cs *CreditStatus) rollingResetAt() (time.Time, bool) {
	if cs == nil {
		return time.Time{}, false
	}
	secs := cs.ResetsInSeconds
	if secs == nil && cs.Rolling != nil {
		secs = cs.Rolling.ResetsInSeconds
	}
	if secs == nil {
		for _, a := range cs.Allowances {
			if a.Cadence == "rolling" && a.ResetsInSeconds != nil {
				secs = a.ResetsInSeconds
				break
			}
		}
	}
	s, ok := floatPtrVal(secs)
	if !ok || s < 0 || cs.ObservedAt.IsZero() {
		return time.Time{}, false
	}
	return cs.ObservedAt.Add(time.Duration(s * float64(time.Second))), true
}

func (cs *CreditStatus) rollingExpired(now time.Time) bool {
	if cs == nil || cs.Rolling == nil {
		return false
	}
	at, ok := cs.rollingResetAt()
	return ok && now.After(at)
}

func (cs *CreditStatus) billingExpired(now time.Time) bool {
	if cs == nil || cs.Billing == nil || cs.Billing.PeriodEndMs <= 0 {
		return false
	}
	return now.After(time.UnixMilli(cs.Billing.PeriodEndMs))
}

// confirmsUse is true only for a fresh, successful, "within_limit" answer whose
// windows have not passed their reset times. It is the sole new signal that
// may override the legacy isEligible flag (a legacy Basic=0 must not mark an
// account exhausted while its native window is within limit).
func (cs *CreditStatus) confirmsUse(now time.Time) bool {
	if cs == nil || cs.FetchError != "" || cs.Status != CreditStatusWithinLimit || cs.Rolling == nil {
		return false
	}
	// Every boundary must be verifiable: an observation time, a finite
	// non-negative reset offset, and finite used/limit with 0 <= used < limit.
	// A "within_limit" label next to missing / zero / contradictory numbers is
	// unconfirmed (shown as "incomplete"), never a green light.
	if cs.ObservedAt.IsZero() || !windowUsable(cs.Rolling) {
		return false
	}
	if _, ok := cs.rollingResetAt(); !ok {
		return false
	}
	if cs.Billing != nil && (!windowUsable(cs.Billing) || cs.Billing.PeriodEndMs <= 0) {
		return false
	}
	return !cs.rollingExpired(now) && !cs.billingExpired(now)
}

// windowUsable is true when used and limit are both known, finite, limit is
// positive and the window is not numerically full.
func windowUsable(w *CreditWindow) bool {
	if w == nil {
		return false
	}
	u, okU := floatPtrVal(w.Used)
	l, okL := floatPtrVal(w.Limit)
	return okU && okL && l > 0 && u >= 0 && u < l
}

// windowFull is true when used and limit are both known, finite, limit is
// positive and the window is numerically full (used >= limit).
func windowFull(w *CreditWindow) bool {
	if w == nil {
		return false
	}
	u, okU := floatPtrVal(w.Used)
	l, okL := floatPtrVal(w.Limit)
	return okU && okL && l > 0 && u >= l
}

// confirmsExhausted is true when a fresh native credit snapshot has a
// non-expired rolling or billing window whose numeric usage is at or above its
// positive limit. Business accounts keep legacy V1 isEligible=true (from bonus
// credits) even when the enforced 6h rolling window is 100% used and Notion
// Agent rejects turns with 429 TooManyRequestsError.
func (cs *CreditStatus) confirmsExhausted(now time.Time) bool {
	if cs == nil || cs.FetchError != "" || cs.ObservedAt.IsZero() || cs.Status == CreditStatusNotApplicable {
		return false
	}
	if cs.Rolling != nil && windowFull(cs.Rolling) {
		if _, ok := cs.rollingResetAt(); ok && !cs.rollingExpired(now) {
			return true
		}
	}
	if cs.Billing != nil && cs.Billing.PeriodEndMs > 0 && !cs.billingExpired(now) && windowFull(cs.Billing) {
		return true
	}
	return false
}

// effectiveEligible combines the legacy V1 isEligible flag with the native
// window. A confirmed numerically full, non-expired native window marks the
// account not eligible even when legacy V1 still reports eligible=true; a
// confirmed "within_limit" window turns a legacy "not eligible" (e.g. Basic 0)
// into eligible; every other native state (not_applicable, unknown status
// without a full window, failed, expired, missing) leaves the legacy decision
// untouched.
func effectiveEligible(legacy bool, cs *CreditStatus, now time.Time) bool {
	if cs.confirmsExhausted(now) {
		return false
	}
	return legacy || cs.confirmsUse(now)
}

// state returns the display state of the snapshot at now.
func (cs *CreditStatus) state(now time.Time) string {
	switch {
	case cs == nil:
		return CreditStateUnchecked
	case cs.FetchError != "":
		return CreditStateFailed
	case cs.Status == CreditStatusNotApplicable:
		return CreditStateNotApplicable
	case cs.Status != CreditStatusWithinLimit:
		return CreditStateUnknown
	case cs.Rolling == nil && cs.Billing == nil:
		return CreditStateNoWindow
	case cs.rollingExpired(now) || cs.billingExpired(now):
		// a passed reset time means the stored numbers describe a window that
		// no longer exists; show "expired, refresh pending" instead of stale %.
		return CreditStateExpired
	case cs.confirmsUse(now):
		return CreditStateWithinLimit
	default:
		// within_limit label without complete, consistent numbers/times.
		return CreditStateIncomplete
	}
}

// CreditWindowView / CreditView are the JSON the dashboard receives.
type CreditWindowView struct {
	Scope      string   `json:"scope,omitempty"`
	CreditType string   `json:"credit_type,omitempty"`
	Window     string   `json:"window,omitempty"`
	Cadence    string   `json:"cadence,omitempty"`
	Used       *float64 `json:"used"`
	Limit      *float64 `json:"limit"`
	Percent    *float64 `json:"percent"`         // null when unknown
	PercentInt *int     `json:"percent_rounded"` // as the Notion UI rounds it (23.81 -> 24); null when unknown
	ResetAt    string   `json:"reset_at,omitempty"`
	ResetsIn   *int64   `json:"resets_in_seconds,omitempty"` // remaining now, never negative
	PeriodEnd  string   `json:"period_end,omitempty"`
	Expired    bool     `json:"expired"`
	Full       bool     `json:"full"` // used >= limit (numeric fact, not a routing decision)
}

type CreditView struct {
	State       string            `json:"state"`
	Status      string            `json:"status,omitempty"` // raw API status
	ObservedAt  string            `json:"observed_at,omitempty"`
	FetchError  string            `json:"fetch_error,omitempty"`
	Rolling     *CreditWindowView `json:"rolling,omitempty"`
	Billing     *CreditWindowView `json:"billing,omitempty"`
	Allowances  []CreditAllowance `json:"allowances,omitempty"`
	RecoverAt   string            `json:"recover_at,omitempty"` // only when a window is numerically full
	CountsAsUse bool              `json:"confirms_use"`
}

func windowView(w *CreditWindow, expired bool, resetAt time.Time, hasReset bool, now time.Time) *CreditWindowView {
	if w == nil {
		return nil
	}
	v := &CreditWindowView{Scope: w.Scope, CreditType: w.CreditType, Window: w.Window, Cadence: w.Cadence, Used: w.Used, Limit: w.Limit, Expired: expired}
	if p, ok := windowPercent(w.Used, w.Limit); ok {
		r := int(math.Round(p))
		v.PercentInt = &r
		p = math.Round(p*100) / 100
		v.Percent = &p
	}
	if u, ok := floatPtrVal(w.Used); ok {
		if l, ok := floatPtrVal(w.Limit); ok && l > 0 && u >= l {
			v.Full = true
		}
	}
	if hasReset {
		v.ResetAt = resetAt.UTC().Format(time.RFC3339)
		left := int64(math.Ceil(resetAt.Sub(now).Seconds()))
		if left < 0 {
			left = 0
		}
		v.ResetsIn = &left
	}
	if w.PeriodEndMs > 0 {
		v.PeriodEnd = time.UnixMilli(w.PeriodEndMs).UTC().Format(time.RFC3339)
	}
	return v
}

func creditView(cs *CreditStatus, now time.Time) *CreditView {
	v := &CreditView{State: cs.state(now)}
	if cs == nil {
		return v
	}
	v.Status = cs.Status
	if !cs.ObservedAt.IsZero() {
		v.ObservedAt = cs.ObservedAt.UTC().Format(time.RFC3339)
	}
	v.FetchError = cs.FetchError
	at, hasReset := cs.rollingResetAt()
	v.Rolling = windowView(cs.Rolling, cs.rollingExpired(now), at, hasReset, now)
	var endAt time.Time
	hasEnd := cs.Billing != nil && cs.Billing.PeriodEndMs > 0
	if hasEnd {
		endAt = time.UnixMilli(cs.Billing.PeriodEndMs)
	}
	v.Billing = windowView(cs.Billing, cs.billingExpired(now), endAt, hasEnd, now)
	v.Allowances = cs.Allowances
	v.CountsAsUse = cs.confirmsUse(now)
	var recover time.Time
	if v.Rolling != nil && v.Rolling.Full && !v.Rolling.Expired && hasReset {
		recover = at
	}
	if v.Billing != nil && v.Billing.Full && !v.Billing.Expired && hasEnd && endAt.After(recover) {
		recover = endAt
	}
	if !recover.IsZero() {
		v.RecoverAt = recover.UTC().Format(time.RFC3339)
	}
	return v
}

// creditRemaining returns the smallest remaining amount over the windows that
// are known and not expired, for routing priority among accounts that report
// the same native unit. ok=false when nothing usable is known.
func (cs *CreditStatus) creditRemaining(now time.Time) (float64, bool) {
	if !cs.confirmsUse(now) {
		return 0, false
	}
	best, found := 0.0, false
	consider := func(w *CreditWindow, expired bool) {
		if w == nil || expired {
			return
		}
		u, okU := floatPtrVal(w.Used)
		l, okL := floatPtrVal(w.Limit)
		if !okU || !okL || l <= 0 {
			return
		}
		rem := l - u
		if rem < 0 {
			rem = 0
		}
		if !found || rem < best {
			best, found = rem, true
		}
	}
	consider(cs.Rolling, cs.rollingExpired(now))
	consider(cs.Billing, cs.billingExpired(now))
	return best, found
}

// creditSettled reports whether the native window has been answered at least
// once and did not confirm use, i.e. the legacy "permanently exhausted" verdict
// has been cross-checked and a re-query every cycle would add nothing.
func creditSettled(info *QuotaInfo) bool {
	if info == nil || info.Credit == nil || info.Credit.FetchError != "" {
		return false
	}
	return info.Credit.Status != CreditStatusWithinLimit
}

// opaqueKey is a short, non-reversible identifier used by the dashboard only
// to tell whether two accounts share a workspace / user (dedupe), without
// exposing the raw ids.
func opaqueKey(kind, id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(kind + ":" + id))
	return hex.EncodeToString(sum[:6])
}

// addQuotaSourceFields adds the native credit view, dedupe keys and the
// legacy-source marker to an API entry. The flat legacy fields stay for
// compatibility; the dashboard must label them as legacy counters.
func addQuotaSourceFields(entry map[string]interface{}, acc *Account, info *QuotaInfo, now time.Time) {
	entry["workspace_key"] = opaqueKey("space", acc.SpaceID)
	entry["user_key"] = opaqueKey("user", acc.UserID)
	var cs *CreditStatus
	if info != nil {
		cs = info.Credit
		entry["legacy_eligible"] = info.LegacyEligible
		entry["legacy_source"] = "getAIUsageEligibility"
	}
	entry["credit"] = creditView(cs, now)
}
