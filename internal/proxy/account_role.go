package proxy

import (
	"errors"
	"fmt"
	"strings"
)

// Account roles control routing priority only. They never merge identities:
// each request is signed with exactly the selected account's own credentials,
// workspace and quota, so using an "education" account as a fallback neither
// shares nor bypasses any entitlement of the primary account.
const (
	AccountRolePrimary   = "primary"
	AccountRoleEducation = "education"
)

// normalizeAccountRole returns the canonical role. "" means unspecified and
// behaves like primary. An unknown value is rejected instead of guessed.
func normalizeAccountRole(role string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "":
		return "", nil
	case AccountRolePrimary:
		return AccountRolePrimary, nil
	case AccountRoleEducation, "edu", "fallback":
		return AccountRoleEducation, nil
	default:
		return "", fmt.Errorf("unsupported account role %q (use primary or education)", role)
	}
}

func (acc *Account) roleSnapshot() string {
	acc.mu.RLock()
	defer acc.mu.RUnlock()
	return acc.Role
}

// roleTier orders accounts for routing: 0 = primary/unspecified, 1 = education.
func (acc *Account) roleTier() int {
	if acc == nil {
		return 0
	}
	if r, err := normalizeAccountRole(acc.roleSnapshot()); err == nil && r == AccountRoleEducation {
		return 1
	}
	return 0
}

// SetAccountRole updates the in-memory role of the account with the given
// account ID. Callers persist it with SaveAccounts.
func (p *AccountPool) SetAccountRole(accountID, role string) error {
	normalized, err := normalizeAccountRole(role)
	if err != nil {
		return err
	}
	acc := p.FindByAccountID(accountID)
	if acc == nil {
		return fmt.Errorf("account not found")
	}
	acc.mu.Lock()
	acc.Role = normalized
	acc.mu.Unlock()
	return nil
}

// agentQuotaSuspect reports whether an Agent transport failure could be caused
// by account quota. It deliberately does not decide: the caller must confirm
// with the authoritative live quota check before switching accounts.
func agentQuotaSuspect(err error) bool {
	var httpErr *agentHTTPError
	if errors.As(err, &httpErr) {
		switch httpErr.Status {
		case 402, 403, 429:
			return true
		}
		return false
	}
	var transcriptErr *agentTranscriptError
	return errors.As(err, &transcriptErr)
}

// classifyAgentQuotaFailure converts a suspected quota failure into
// ErrQuotaExhausted when the live quota API reports the account as not
// eligible, or when Notion Agent explicitly rejects the request with HTTP 429
// TooManyRequestsError. Unconfirmed generic failures are returned unchanged, so
// a transient 429/403 or a quota-API outage never disables an account or moves
// a conversation to another one.
func classifyAgentQuotaFailure(acc *Account, err error) error {
	if err == nil || acc == nil || !agentQuotaSuspect(err) {
		return err
	}
	var httpErr *agentHTTPError
	if errors.As(err, &httpErr) && httpErr.Status == 429 && strings.EqualFold(httpErr.Code, "TooManyRequestsError") {
		return fmt.Errorf("%w: %v", ErrQuotaExhausted, err)
	}
	info, qerr := quotaFetcher(acc)
	if qerr == nil && info != nil && !info.IsEligible {
		return fmt.Errorf("%w: %v", ErrQuotaExhausted, err)
	}
	return err
}

