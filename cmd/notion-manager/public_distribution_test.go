package main

import (
	"net/http"
	"net/http/httptest"
	"notion-manager/internal/proxy"
	"testing"
)

func TestPublicDistributionOmitsRegistration(t *testing.T) {
	pool := proxy.NewAccountPool()
	mux := newMux(pool, t.TempDir(), "test-local-key", proxy.InitUsageStats(""), &proxy.AccountAdminDeps{Pool: pool})
	for _, path := range []string{"/admin/register", "/admin/register/providers", "/admin/register/start", "/admin/register/jobs", "/admin/register/jobs/example/events"} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
			if rec.Code != http.StatusNotFound {
				t.Fatalf("%s %s returned %d, want 404", method, path, rec.Code)
			}
		}
	}
}

func TestPublicDistributionUsesPrivateDefaults(t *testing.T) {
	c := proxy.DefaultConfig()
	if c.Server.Host != "127.0.0.1" {
		t.Fatalf("default host = %q", c.Server.Host)
	}
	if c.Server.DebugLogging || c.Server.APILogInput || c.Server.APILogOutput || c.Server.NotionLogReq || c.Server.NotionLogResp || c.Server.DumpAPIInput {
		t.Fatal("request capture must be disabled by default")
	}
}
