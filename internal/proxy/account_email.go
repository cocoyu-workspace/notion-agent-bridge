package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// emailFetcher is a seam for tests. It resolves an account's own email from
// its authoritative Notion user record.
var emailFetcher = FetchAccountEmail

// extractUserEmail returns the email stored in recordMap.notion_user[userID].
// Only the record whose key is exactly userID is trusted: other users that may
// appear in the same response (collaborators, workspace members) are ignored,
// and an empty/missing record yields "" (unknown), never a guess.
func extractUserEmail(body []byte, userID string) string {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return ""
	}
	var parsed struct {
		RecordMap struct {
			NotionUser map[string]struct {
				Value struct {
					Email string `json:"email"`
					Value struct {
						Email string `json:"email"`
					} `json:"value"`
				} `json:"value"`
			} `json:"notion_user"`
		} `json:"recordMap"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return ""
	}
	rec, ok := parsed.RecordMap.NotionUser[userID]
	if !ok {
		return ""
	}
	email := strings.TrimSpace(rec.Value.Value.Email)
	if email == "" {
		email = strings.TrimSpace(rec.Value.Email)
	}
	if !strings.Contains(email, "@") {
		return ""
	}
	return email
}

// FetchAccountEmail loads the account's own user record (loadUserContent) and
// returns its email. Errors are short codes; the email is never logged.
func FetchAccountEmail(acc *Account) (string, error) {
	req, err := http.NewRequest("POST", NotionAPIBase+"/loadUserContent", bytes.NewReader([]byte(`{}`)))
	if err != nil {
		return "", fmt.Errorf("request")
	}
	setNotionHeadersJSON(req, acc)
	resp, err := nativeHTTPClient(AppConfig.APITimeoutDuration()).Do(req)
	if err != nil {
		return "", fmt.Errorf("network")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("http_%d", resp.StatusCode)
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	email := extractUserEmail(data, acc.UserID)
	if email == "" {
		return "", fmt.Errorf("not_found")
	}
	return email, nil
}

// backfillAccountEmail fills an empty UserEmail from the account's own user
// record. It never overwrites an existing email and never changes identity
// fields (account_id, user_id, space_id, role).
func (p *AccountPool) backfillAccountEmail(acc *Account) bool {
	p.mu.RLock()
	has := strings.TrimSpace(acc.UserEmail) != ""
	p.mu.RUnlock()
	if has || strings.TrimSpace(acc.UserID) == "" {
		return false
	}
	email, err := emailFetcher(acc)
	if err != nil || email == "" {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if strings.TrimSpace(acc.UserEmail) != "" {
		return false
	}
	acc.UserEmail = email
	return true
}
