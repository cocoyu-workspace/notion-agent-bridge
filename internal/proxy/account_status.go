package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// Short machine codes recorded for the last failed request of an account.
// They never contain upstream messages, tokens or emails.
const (
	AccountErrQuotaExhausted   = "quota_exhausted"
	AccountErrPremiumGone      = "premium_unavailable"
	AccountErrEmptyResponse    = "empty_response"
	AccountErrRequestFailed    = "request_failed"
	accountEmailUnknownDisplay = "unknown"
)

// Status values reported to the dashboard, most specific first.
const (
	AccountStatusOK          = "ok"
	AccountStatusExhausted   = "exhausted"
	AccountStatusPermanent   = "permanent"
	AccountStatusNoWorkspace = "no_workspace"
	AccountStatusError       = "error"
)

// RecordAccountUse marks acc as the account currently serving inference and
// clears its last error. Called by the router after a successful request.
func (p *AccountPool) RecordAccountUse(acc *Account) {
	if acc == nil {
		return
	}
	acc.EnsureAccountID()
	now := time.Now()
	acc.mu.Lock()
	acc.LastUsedAt = &now
	acc.LastError = ""
	acc.LastErrorAt = nil
	acc.mu.Unlock()
	p.currentMu.Lock()
	if p.currentAccountID != acc.AccountID {
		p.currentSince = now
	}
	p.currentAccountID = acc.AccountID
	p.currentMu.Unlock()
}

// RecordAccountError remembers why the last request on acc failed (a short
// code, see AccountErr*). It does not change routing state by itself.
func (p *AccountPool) RecordAccountError(acc *Account, code string) {
	if acc == nil || code == "" {
		return
	}
	now := time.Now()
	acc.mu.Lock()
	acc.LastError = code
	acc.LastErrorAt = &now
	acc.mu.Unlock()
}

func (p *AccountPool) currentAccount() (string, time.Time) {
	p.currentMu.Lock()
	defer p.currentMu.Unlock()
	return p.currentAccountID, p.currentSince
}

// CurrentAccountID returns the account most recently used successfully ("" if none yet).
func (p *AccountPool) CurrentAccountID() string {
	id, _ := p.currentAccount()
	return id
}

// PreferredAccountID returns the manually selected active account ID ("" if none).
func (p *AccountPool) PreferredAccountID() string {
	if p == nil {
		return ""
	}
	p.currentMu.Lock()
	defer p.currentMu.Unlock()
	return p.preferredAccountID
}

func (p *AccountPool) clearPreferredAccountIf(accountID string) {
	if p == nil || accountID == "" {
		return
	}
	p.currentMu.Lock()
	if p.preferredAccountID == accountID {
		p.preferredAccountID = ""
	}
	if p.currentAccountID == accountID {
		p.currentAccountID = ""
		p.currentSince = time.Time{}
	}
	p.currentMu.Unlock()
}

// SwitchToAccount manually switches the active account in memory without
// deleting accounts or restarting client sessions. When target is empty, it
// picks the next best usable account excluding the current account.
func (p *AccountPool) SwitchToAccount(target string) (*Account, error) {
	if p == nil {
		return nil, fmt.Errorf("account pool not initialized")
	}
	target = strings.TrimSpace(target)
	var acc *Account
	if target != "" {
		acc = p.FindByAccountID(target)
		if acc == nil {
			acc = p.GetAccountByEmail(target)
		}
		if acc == nil {
			return nil, fmt.Errorf("account not found in pool")
		}
		if p.hasNoWorkspace(acc) {
			return nil, fmt.Errorf("account has no accessible workspace")
		}
		if p.isQuotaExhausted(acc) {
			return nil, fmt.Errorf("account quota is exhausted")
		}
	} else {
		curID := p.CurrentAccountID()
		p.currentMu.Lock()
		p.preferredAccountID = ""
		p.currentMu.Unlock()
		if curID != "" {
			if cur := p.FindByAccountID(curID); cur != nil {
				acc = p.NextBestExcluding(map[*Account]bool{cur: true})
			}
		}
		if acc == nil {
			acc = p.NextBest()
		}
		if acc == nil {
			return nil, fmt.Errorf("no usable account available in pool")
		}
	}
	acc.EnsureAccountID()
	p.currentMu.Lock()
	p.preferredAccountID = acc.AccountID
	p.currentMu.Unlock()
	p.RecordAccountUse(acc)
	if globalSessionManager != nil {
		globalSessionManager.ClearAll()
	}
	log.Printf("[account] switched active account to %s (%s) aid=%s", acc.UserName, acc.UserEmail, acc.ShortSpaceID())
	return acc, nil
}

// addAccountStatusFields adds identity/role/health fields for the dashboard.
// Caller holds p.mu (read). It never includes tokens or cookies.
func (p *AccountPool) addAccountStatusFields(entry map[string]interface{}, acc *Account, currentID string) {
	email := strings.TrimSpace(acc.UserEmail)
	entry["email_known"] = email != ""
	if email == "" {
		entry["email"] = ""
		entry["email_display"] = accountEmailUnknownDisplay
	} else {
		entry["email_display"] = email
	}

	role, _ := normalizeAccountRole(acc.roleSnapshot())
	entry["role_set"] = role != ""
	if role == "" {
		role = AccountRolePrimary
	}
	entry["role"] = role

	acc.mu.RLock()
	lastUsed, lastErr, lastErrAt := acc.LastUsedAt, acc.LastError, acc.LastErrorAt
	acc.mu.RUnlock()

	entry["current"] = currentID != "" && acc.AccountID == currentID
	if lastUsed != nil {
		entry["last_used_at"] = lastUsed.Format(time.RFC3339)
	}
	if lastErr != "" {
		entry["last_error"] = lastErr
		if lastErrAt != nil {
			entry["last_error_at"] = lastErrAt.Format(time.RFC3339)
		}
	}

	switch {
	case p.hasNoWorkspace(acc):
		entry["status"] = AccountStatusNoWorkspace
	case acc.quotaSnapshot().PermanentlyExhausted:
		entry["status"] = AccountStatusPermanent
	case p.isQuotaExhausted(acc):
		entry["status"] = AccountStatusExhausted
	case lastErr != "":
		entry["status"] = AccountStatusError
	default:
		entry["status"] = AccountStatusOK
	}
}

// maskEmail redacts the local part for unauthenticated surfaces such as
// /health ("alice@example.com" -> "a***@example.com"). Empty stays empty.
func maskEmail(email string) string {
	email = strings.TrimSpace(email)
	at := strings.LastIndex(email, "@")
	if at <= 0 {
		if email == "" {
			return ""
		}
		return "***"
	}
	return email[:1] + "***" + email[at:]
}

// HandleAdminAccountRole sets an account's role ("primary" | "education" | ""
// to clear) and persists it. Requires the dashboard session like the other
// admin endpoints.
func HandleAdminAccountRole(pool *AccountPool, accountsDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			AccountID string `json:"account_id"`
			Role      string `json:"role"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
			http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(body.AccountID) == "" {
			http.Error(w, `{"error":"account_id is required"}`, http.StatusBadRequest)
			return
		}
		if _, err := normalizeAccountRole(body.Role); err != nil {
			http.Error(w, `{"error":"role must be primary or education"}`, http.StatusBadRequest)
			return
		}
		acc := pool.FindByAccountID(strings.TrimSpace(body.AccountID))
		if acc == nil {
			http.Error(w, `{"error":"account not found in pool"}`, http.StatusNotFound)
			return
		}
		if err := pool.SetAccountRole(acc.AccountID, body.Role); err != nil {
			http.Error(w, `{"error":"could not set role"}`, http.StatusBadRequest)
			return
		}
		if accountsDir != "" {
			if err := saveAccountFile(accountsDir, acc); err != nil {
				http.Error(w, `{"error":"role updated in memory but could not be persisted"}`, http.StatusInternalServerError)
				return
			}
		}
		role, _ := normalizeAccountRole(body.Role)
		if role == "" {
			role = AccountRolePrimary
		}
		w.Write([]byte(`{"status":"ok","role":"` + role + `"}`))
	}
}

// HandleAdminAccountSwitch switches the active inference account in memory
// without deleting accounts or restarting client sessions.
func HandleAdminAccountSwitch(pool *AccountPool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			AccountID string `json:"account_id"`
			Email     string `json:"email"`
		}
		if r.Body != nil {
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
				http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
				return
			}
		}
		target := strings.TrimSpace(body.AccountID)
		if target == "" {
			target = strings.TrimSpace(body.Email)
		}
		acc, err := pool.SwitchToAccount(target)
		if err != nil {
			status := http.StatusBadRequest
			if strings.Contains(err.Error(), "not found") {
				status = http.StatusNotFound
			}
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"status":     "ok",
			"account_id": acc.AccountID,
			"email":      acc.UserEmail,
			"name":       acc.UserName,
			"space":      acc.SpaceName,
		})
	}
}

// currentAccountSummary picks the account flagged "current" out of the full
// details list and returns a compact, secret-free view for the dashboard
// header (so it is visible even when that account is on another page).
func currentAccountSummary(all []map[string]interface{}) map[string]interface{} {
	for _, a := range all {
		if cur, _ := a["current"].(bool); !cur {
			continue
		}
		out := map[string]interface{}{}
		for _, k := range []string{"account_id", "email", "email_known", "email_display", "name", "space", "role", "status", "last_used_at", "last_error"} {
			if v, ok := a[k]; ok {
				out[k] = v
			}
		}
		return out
	}
	return nil
}
