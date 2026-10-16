package proxy

import (
	"io/fs"
	"net/http"
	"strings"

	"notion-manager/internal/web"
)

// --- Account Pool helpers ---

// GetAccountByEmail returns a specific account by email regardless of
// usability (callers like the dashboard "copy token" action want the raw
// record even for accounts that the picker would skip).
func (p *AccountPool) GetAccountByEmail(email string) *Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, acc := range p.accounts {
		if acc.UserEmail == email {
			return acc
		}
	}
	return nil
}

// GetBestAccount returns the best available account for a new conversation.
// Prefer accounts with remaining basic quota.
func (p *AccountPool) GetBestAccount() *Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.pickBestAccountLocked(nil)
}

// --- Reverse Proxy helpers ---

// CreateTargetedSession creates a proxy session for a specific account
func (rp *ReverseProxy) CreateTargetedSession(w http.ResponseWriter, acc *Account) {
	id := generateUUIDv4()
	sess := newProxySession(acc)
	rp.sessions.Store(id, sess)
	http.SetCookie(w, &http.Cookie{
		Name: "np_session", Value: id, Path: "/",
		HttpOnly: true, MaxAge: 86400,
	})
}

// --- HTTP Handlers ---

// HandleDashboard serves the React SPA dashboard.
// It injects the API key into index.html via a <meta> tag so the frontend
// can authenticate against /admin/* endpoints.
func HandleDashboard(apiKey string) http.Handler {
	// Serve from embedded dist/ filesystem
	distFS, err := fs.Sub(web.DistFS, "dist")
	if err != nil {
		panic("failed to get dist sub-filesystem: " + err.Error())
	}
	fileServer := http.FileServer(http.FS(distFS))

	// Inner handler that serves files
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/dashboard")
		if path == "" || path == "/" {
			path = "/index.html"
		}

		// For index.html, inject the API key meta tag
		if path == "/index.html" {
			data, err := fs.ReadFile(distFS, "index.html")
			if err != nil {
				http.Error(w, "index.html not found", http.StatusInternalServerError)
				return
			}
			html := strings.Replace(string(data), "<head>",
				`<head><meta name="api-key" content="`+apiKey+`">`, 1)
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
			w.Write([]byte(html))
			return
		}

		// Serve static assets (JS, CSS) with caching
		if strings.HasPrefix(path, "/assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}

		// Serve file from embedded FS
		r.URL.Path = path
		fileServer.ServeHTTP(w, r)
	})

	return inner
}

// HandleProxyStart creates a session for a specific account and redirects to /ai.
func HandleProxyStart(pool *AccountPool, rp *ReverseProxy) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		email := r.URL.Query().Get("email")
		accountID := r.URL.Query().Get("account_id")
		best := r.URL.Query().Get("best")

		var acc *Account
		if best == "true" {
			acc = pool.GetBestAccount()
		} else if accountID != "" {
			acc = pool.FindByAccountID(accountID)
		} else if email != "" {
			acc = pool.GetAccountByEmail(email)
		}

		if acc == nil {
			w.Header().Set("Content-Type", "application/json")
			http.Error(w, `{"error":"account not found or all exhausted"}`, http.StatusNotFound)
			return
		}

		// Refuse to redirect into an account whose Notion workspace is
		// missing — the SPA loops on a skeleton screen forever and the
		// user perceives it as a reverse-proxy hang. Surface a clear
		// error so the dashboard can show "this account has no
		// workspace" instead of opening a dead tab.
		if pool.HasNoWorkspace(acc) {
			w.Header().Set("Content-Type", "application/json")
			http.Error(w, `{"error":"account has no accessible workspace; check your login credentials"}`, http.StatusConflict)
			return
		}

		rp.CreateTargetedSession(w, acc)
		http.Redirect(w, r, "/ai", http.StatusFound)
	}
}
