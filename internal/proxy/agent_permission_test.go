package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// loadApprovalConfig exercises the real config loader (YAML -> AppConfig) so the
// seam under test is "configured mode -> Notion request body", not a helper.
func loadApprovalConfig(t *testing.T, yamlProxyLine string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "server:\n  api_key: test\nproxy:\n  agent_thread: true\n"
	if yamlProxyLine != "" {
		content += "  " + yamlProxyLine + "\n"
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	old := AppConfig
	AppConfig = cfg
	t.Cleanup(func() { AppConfig = old })
}

func approvalModeOf(t *testing.T, body map[string]interface{}) string {
	t.Helper()
	policies, ok := body["policies"].(map[string]string)
	if !ok {
		t.Fatalf("policies missing or wrong type: %#v", body["policies"])
	}
	return policies["approval_mode"]
}

func TestAgentApprovalConfiguredModeReachesBothBodies(t *testing.T) {
	loadApprovalConfig(t, "agent_approval_mode: review_for_me")
	acc := &Account{SpaceID: "space"}
	create := agentThreadBodyFromPrompt(acc, "p", "", "thread", "msg")
	follow := agentThreadFollowUpBody(acc, "p", "", "", "thread", "evt", nil)
	if got := approvalModeOf(t, create); got != "review_for_me" {
		t.Errorf("createAgentThread approval_mode = %q, want review_for_me", got)
	}
	if got := approvalModeOf(t, follow); got != "review_for_me" {
		t.Errorf("sendEventToAgentThread approval_mode = %q, want review_for_me", got)
	}
}

func TestAgentApprovalModeTable(t *testing.T) {
	cases := []struct {
		name, yaml, env, wantMode, wantSource string
	}{
		{"missing field defaults to yolo", "", "", "yolo", "default"},
		{"explicit ask", "agent_approval_mode: ask", "", "ask", "config"},
		{"review_for_me", "agent_approval_mode: review_for_me", "", "review_for_me", "config"},
		{"yolo explicit", "agent_approval_mode: yolo", "", "yolo", "config"},
		{"case and space tolerated", "agent_approval_mode: ' YOLO '", "", "yolo", "config"},
		{"unknown value never relaxes", "agent_approval_mode: auto", "", "ask", "unknown-fallback"},
		{"near miss never relaxes", "agent_approval_mode: yolo!", "", "ask", "unknown-fallback"},
		{"env overrides config", "agent_approval_mode: yolo", "ask", "ask", "env"},
		{"env sets mode", "", "review_for_me", "review_for_me", "env"},
		{"unknown env beats valid yaml and falls back to ask", "agent_approval_mode: yolo", "bogus", "ask", "unknown-fallback"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AGENT_APPROVAL_MODE", tc.env)
			loadApprovalConfig(t, tc.yaml)
			mode, source := agentApprovalEffective()
			if mode != tc.wantMode || source != tc.wantSource {
				t.Fatalf("got (%s,%s), want (%s,%s)", mode, source, tc.wantMode, tc.wantSource)
			}
			acc := &Account{SpaceID: "space"}
			for name, body := range map[string]map[string]interface{}{
				"create":   agentThreadBodyFromPrompt(acc, "p", "", "t", "m"),
				"followup": agentThreadFollowUpBody(acc, "p", "", "", "t", "e", nil),
			} {
				if got := approvalModeOf(t, body); got != tc.wantMode {
					t.Errorf("%s body approval_mode = %q, want %q", name, got, tc.wantMode)
				}
			}
		})
	}
}

func TestAgentApprovalNilConfigIsYolo(t *testing.T) {
	old := AppConfig
	AppConfig = nil
	defer func() { AppConfig = old }()
	t.Setenv("AGENT_APPROVAL_MODE", "")
	if mode, _ := agentApprovalEffective(); mode != "yolo" {
		t.Fatalf("nil config mode = %q, want yolo", mode)
	}
}

func TestAgentApprovalModelsHeadersReportSentValue(t *testing.T) {
	t.Setenv("AGENT_APPROVAL_MODE", "")
	loadApprovalConfig(t, "agent_approval_mode: review_for_me")
	rec := httptest.NewRecorder()
	HandlePublicModels(&AccountPool{}).ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if got := rec.Header().Get("X-Agent-Approval-Sent-Mode"); got != "review_for_me" {
		t.Errorf("X-Agent-Approval-Sent-Mode = %q", got)
	}
	if got := rec.Header().Get("X-Agent-Approval-Sent-Source"); got != "config" {
		t.Errorf("X-Agent-Approval-Sent-Source = %q", got)
	}
}

func TestAgentApprovalModelsHeadersAbsentWithoutAgentThread(t *testing.T) {
	old := AppConfig
	AppConfig = DefaultConfig()
	defer func() { AppConfig = old }()
	rec := httptest.NewRecorder()
	HandlePublicModels(&AccountPool{}).ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Header().Get("X-Agent-Approval-Sent-Mode") != "" {
		t.Error("approval header reported although agent_thread is off (no Agent requests are sent)")
	}
}

// Wire level: the JSON actually POSTed to createAgentThread carries the mode
// (reconciliation target for the local Claude-side check).
func TestAgentApprovalModeOnTheWire(t *testing.T) {
	for _, tc := range []struct{ yaml, want string }{
		{"agent_approval_mode: yolo", "yolo"},
		{"agent_approval_mode: review_for_me", "review_for_me"},
		{"agent_approval_mode: ask", "ask"},
		{"", "yolo"},
		{"agent_approval_mode: bogus", "ask"},
	} {
		t.Run(tc.want+"/"+tc.yaml, func(t *testing.T) {
			t.Setenv("AGENT_APPROVAL_MODE", "")
			loadApprovalConfig(t, tc.yaml)
			AppConfig.Timeouts.InferenceTimeout = 5
			oldBase := NotionAPIBase
			defer func() { NotionAPIBase = oldBase }()
			var sent []byte
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "createAgentThread") {
					sent, _ = io.ReadAll(r.Body)
					w.Write([]byte(`{}`))
					return
				}
				w.Write([]byte(capturedShape))
			}))
			defer server.Close()
			NotionAPIBase = server.URL
			if err := callAgentThreadWithClient(&Account{SpaceID: "space"}, nil, "", "", func(string, bool, *UsageInfo) {}, server.Client()); err != nil {
				t.Fatal(err)
			}
			var body struct {
				Policies map[string]string `json:"policies"`
			}
			if err := json.Unmarshal(sent, &body); err != nil {
				t.Fatalf("no createAgentThread body captured: %v", err)
			}
			if body.Policies["approval_mode"] != tc.want {
				t.Fatalf("wire approval_mode = %q, want %q", body.Policies["approval_mode"], tc.want)
			}
		})
	}
}
