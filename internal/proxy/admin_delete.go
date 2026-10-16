package proxy

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

type AccountAdminDeps struct {
	Pool        *AccountPool
	AccountsDir string
}

func (d *AccountAdminDeps) ready(w http.ResponseWriter) bool {
	if d == nil {
		http.Error(w, `{"error":"server not configured"}`, http.StatusInternalServerError)
		return false
	}
	return true
}

// HandleAdminDeleteAccount removes one account JSON file from disk and from
// the live AccountPool. Path:
//
//	DELETE /admin/accounts/{identifier}
//
// The identifier is either an account_id (64-char hex) or an email (legacy).
func HandleAdminDeleteAccount(deps *AccountAdminDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !deps.ready(w) {
			return
		}
		if r.Method != http.MethodDelete {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		raw := strings.TrimPrefix(r.URL.Path, "/admin/accounts/")
		raw = strings.Trim(raw, "/")
		identifier, err := url.PathUnescape(raw)
		if err != nil || identifier == "" {
			http.Error(w, `{"error":"missing identifier"}`, http.StatusBadRequest)
			return
		}

		// If identifier looks like a 64-char hex account_id, delete by that;
		// otherwise fall back to email for backward compatibility.
		if isAccountID(identifier) {
			if err := deleteAccountByID(deps.Pool, deps.AccountsDir, identifier); err != nil {
				if os.IsNotExist(err) {
					http.Error(w, `{"error":"account not found"}`, http.StatusNotFound)
					return
				}
				http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err), http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "deleted": identifier})
		} else {
			if err := deleteAccountByEmail(deps.Pool, deps.AccountsDir, identifier); err != nil {
				if os.IsNotExist(err) {
					http.Error(w, `{"error":"account not found"}`, http.StatusNotFound)
					return
				}
				http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err), http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "deleted": identifier})
		}
	}
}

// deleteAccountByEmail removes the account JSON file whose user_email field
// matches and drops the corresponding pool entry. Returns os.ErrNotExist if
// no file matches; that's mapped to a 404 by the handler.
func deleteAccountByEmail(pool *AccountPool, dir, email string) error {
	entries, err := readPrivateAccountsDir(dir)
	if err != nil {
		return err
	}
	var target string
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, err := readPrivateAccountFile(path)
		if err != nil {
			return err
		}
		var raw map[string]interface{}
		if err := json.Unmarshal(data, &raw); err != nil {
			continue
		}
		if got, _ := raw["user_email"].(string); strings.EqualFold(got, email) {
			target = path
			break
		}
	}
	if target == "" {
		return os.ErrNotExist
	}
	if err := os.Remove(target); err != nil {
		return err
	}
	if pool != nil {
		pool.RemoveAccountByEmail(email)
	}
	log.Printf("[admin] deleted account file: %s (%s)", target, email)
	return nil
}

// isAccountID returns true if s looks like a 64-char lowercase hex account_id.
func isAccountID(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// deleteAccountByID removes the account JSON file whose account_id field
// matches (or can be computed from user_id+space_id) and drops the pool entry.
func deleteAccountByID(pool *AccountPool, dir, accountID string) error {
	entries, err := readPrivateAccountsDir(dir)
	if err != nil {
		return err
	}
	var target string
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, err := readPrivateAccountFile(path)
		if err != nil {
			return err
		}
		var raw map[string]interface{}
		if err := json.Unmarshal(data, &raw); err != nil {
			continue
		}
		aid, _ := raw["account_id"].(string)
		if aid == "" {
			uid, _ := raw["user_id"].(string)
			sid, _ := raw["space_id"].(string)
			if uid != "" && sid != "" {
				aid = ComputeAccountID(uid, sid)
			}
		}
		if aid == accountID {
			target = path
			break
		}
	}
	if target == "" {
		return os.ErrNotExist
	}
	if err := os.Remove(target); err != nil {
		return err
	}
	if pool != nil {
		pool.RemoveAccountByAccountID(accountID)
	}
	log.Printf("[admin] deleted account file by account_id: %s", target)
	return nil
}
