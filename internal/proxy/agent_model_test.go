package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// --- merged from agent_model_test.go ---
func TestAgentModelSummarySonnetLow(t *testing.T) {
	got := agentModelSummary("sonnet-5.5", "low")
	for _, want := range []string{`requested="sonnet-5.5"`, "upstream=achira-donut", "effort=low"} {
		if !strings.Contains(got, want) {
			t.Fatalf("summary %q missing %q", got, want)
		}
	}
}

func TestAgentModelSummaryDefaultsAndMappings(t *testing.T) {
	if got := agentModelSummary("auto", ""); !strings.Contains(got, "upstream=<auto>") || !strings.Contains(got, "effort=<unset>") {
		t.Fatalf("auto summary: %s", got)
	}
	if got := agentModelSummary("sonnet-5.5", "minimal"); !strings.Contains(got, "effort=low") {
		t.Fatalf("minimal should map to low: %s", got)
	}
	if got := agentModelSummary("sonnet-5.5", ""); !strings.Contains(got, "effort=medium") {
		t.Fatalf("explicit model keeps medium default: %s", got)
	}
}

func TestAgentModelSummaryUnknownIsRejectedNotSubstituted(t *testing.T) {
	got := agentModelSummary("claude-sonnet-4.6", "low")
	if !strings.Contains(got, "rejected") || strings.Contains(got, "achira-donut") {
		t.Fatalf("unknown model must be reported as rejected: %s", got)
	}
}

func TestAgentSonnetLowRequestBody(t *testing.T) {
	id, err := agentThreadModelID("sonnet-5.5")
	if err != nil {
		t.Fatal(err)
	}
	body := agentThreadBody(&Account{SpaceID: "space"}, []ChatMessage{{Role: "user", Content: "hi"}}, id)
	if body["model"] != "achira-donut" {
		t.Fatalf("model = %v", body["model"])
	}
	// Mirrors callAgentThreadWithClient: a client-provided effort overrides the default.
	effort, _ := normalizeReasoningEffort("low")
	if effort != "" {
		body["reasoningEffort"] = effort
	}
	if body["reasoningEffort"] != "low" {
		t.Fatalf("reasoningEffort = %v", body["reasoningEffort"])
	}
	if v, ok := body["model"].(string); ok && strings.Contains(v, "albuquerque") {
		t.Fatal("must never select Opus")
	}
}

func TestPublicModelsAdvertiseAgentAliasesOnlyInAgentMode(t *testing.T) {
	old := AppConfig
	defer func() { AppConfig = old }()
	fetch := func() []string {
		rec := httptest.NewRecorder()
		HandlePublicModels(NewAccountPool())(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
		var resp publicModelResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, m := range resp.Data {
			ids = append(ids, m.ID)
		}
		return ids
	}
	has := func(ids []string, want string) bool {
		for _, id := range ids {
			if id == want {
				return true
			}
		}
		return false
	}
	AppConfig = DefaultConfig()
	AppConfig.Proxy.AgentThread = false
	base := fetch()
	for _, id := range agentAdvertisedModels {
		if has(base, id) {
			t.Fatalf("non-agent mode must not add %s: %v", id, base)
		}
	}
	AppConfig.Proxy.AgentThread = true
	ids := fetch()
	for _, id := range agentAdvertisedModels {
		if !has(ids, id) {
			t.Fatalf("agent mode missing %s: %v", id, ids)
		}
	}
	if len(ids) != len(base)+len(agentAdvertisedModels) {
		t.Fatalf("existing entries must be preserved: base=%v agent=%v", base, ids)
	}
	AppConfig = nil
	if got := fetch(); len(got) != len(base) {
		t.Fatalf("nil config: %v", got)
	}
}

// --- merged from agent_thread_sol_test.go ---
func TestAgentThreadSolSelection(t *testing.T) {
	for _, alias := range []string{
		"gpt-6.1-sol",
		"gpt-6.1-sol[1m]",
		"claude-sol-6-1",
		"claude-sol-6-1[1m]",
		"claude-sol-6.1",
		"claude-sol-6.1[1m]",
		"omniberry-sundae",
		" GPT-6.1-SOL ",
		" Claude-Sol-6-1[1m] ",
	} {
		id, err := agentThreadModelID(alias)
		if err != nil || id != "omniberry-sundae" {
			t.Fatalf("%q: id=%q err=%v", alias, id, err)
		}
		body := agentThreadBody(&Account{SpaceID: "space"}, nil, id)
		if body["model"] != "omniberry-sundae" {
			t.Fatalf("wrong upstream selection: %v", body["model"])
		}
	}
	for _, unsupported := range []string{"gpt-6.1", "gpt-6.2-sol", "gpt-6.1-sol[2m]"} {
		if _, err := agentThreadModelID(unsupported); err == nil {
			t.Fatalf("silently accepted %q", unsupported)
		}
	}
}

func TestAgentThreadDesktopAutoAlias(t *testing.T) {
	for _, alias := range []string{"auto", "claude-auto", " CLAUDE-AUTO "} {
		id, err := agentThreadModelID(alias)
		if err != nil || id != "" {
			t.Fatalf("%q: id=%q err=%v", alias, id, err)
		}
	}
}

func TestAgentPublicModelsIncludeDesktopDiscoveryMetadata(t *testing.T) {
	old := AppConfig
	defer func() { AppConfig = old }()

	AppConfig = DefaultConfig()
	AppConfig.Proxy.AgentThread = true

	rec := httptest.NewRecorder()
	HandlePublicModels(NewAccountPool())(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	var resp publicModelResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	byID := make(map[string]publicModel, len(resp.Data))
	for _, m := range resp.Data {
		byID[m.ID] = m
	}

	checks := []struct {
		id             string
		displayName    string
		description    string
		maxInputTokens int
		tier           string
		familyDefault  bool
		supports1M     bool
		adaptive       bool
		effort         bool
	}{
		{
			id:             "auto",
			displayName:    "Auto (Notion Agent)",
			description:    "Notion Agent automatic model selection",
			maxInputTokens: 200000,
			adaptive:       true,
			effort:         false,
		},
		{
			id:             "claude-auto",
			displayName:    "Auto (Notion Agent)",
			description:    "Notion Agent automatic model selection",
			maxInputTokens: 200000,
			adaptive:       true,
			effort:         false,
		},
		{
			id:             "sonnet-5.5",
			displayName:    "Claude Sonnet 5.5",
			description:    "Notion Agent Sonnet 5.5 (achira-donut)",
			maxInputTokens: 200000,
			tier:           "sonnet",
			supports1M:     false,
			adaptive:       true,
			effort:         true,
		},
		{
			id:             "claude-sonnet-5-5",
			displayName:    "Claude Sonnet 5.5",
			description:    "Notion Agent Sonnet 5.5 (achira-donut)",
			maxInputTokens: 200000,
			tier:           "sonnet",
			familyDefault:  true,
			supports1M:     false,
			adaptive:       true,
			effort:         true,
		},
		{
			id:             "opus-5.5",
			displayName:    "Claude Opus 5.5",
			description:    "Notion Agent Opus 5.5 (albuquerque-quinn)",
			maxInputTokens: 200000,
			tier:           "opus",
			supports1M:     false,
			adaptive:       true,
			effort:         true,
		},
		{
			id:             "claude-opus-5-5",
			displayName:    "Claude Opus 5.5",
			description:    "Notion Agent Opus 5.5 (albuquerque-quinn)",
			maxInputTokens: 200000,
			tier:           "opus",
			familyDefault:  true,
			supports1M:     false,
			adaptive:       true,
			effort:         true,
		},
		{
			id:             "gpt-6.1-sol",
			displayName:    "GPT 6.1 Sol",
			description:    "Notion Agent GPT 6.1 Sol (omniberry-sundae)",
			maxInputTokens: 1000000,
			supports1M:     true,
			adaptive:       true,
			effort:         true,
		},
		{
			id:             "claude-sol-6-1",
			displayName:    "GPT 6.1 Sol",
			description:    "Notion Agent GPT 6.1 Sol (omniberry-sundae)",
			maxInputTokens: 1000000,
			supports1M:     true,
			adaptive:       true,
			effort:         true,
		},
	}

	for _, tc := range checks {
		m, ok := byID[tc.id]
		if !ok {
			t.Fatalf("missing model %q in /v1/models response", tc.id)
		}
		if m.DisplayName != tc.displayName {
			t.Fatalf("%s display_name = %q, want %q", tc.id, m.DisplayName, tc.displayName)
		}
		if m.Description != tc.description {
			t.Fatalf("%s description = %q, want %q", tc.id, m.Description, tc.description)
		}
		if m.MaxInputTokens != tc.maxInputTokens {
			t.Fatalf("%s max_input_tokens = %d, want %d", tc.id, m.MaxInputTokens, tc.maxInputTokens)
		}
		if m.AnthropicFamilyTier != tc.tier {
			t.Fatalf("%s anthropic_family_tier = %q, want %q", tc.id, m.AnthropicFamilyTier, tc.tier)
		}
		if m.IsFamilyDefault != tc.familyDefault {
			t.Fatalf("%s is_family_default = %v, want %v", tc.id, m.IsFamilyDefault, tc.familyDefault)
		}
		if m.Supports1M != tc.supports1M {
			t.Fatalf("%s supports_1m = %v, want %v", tc.id, m.Supports1M, tc.supports1M)
		}
		if m.SupportsAdaptive == nil || *m.SupportsAdaptive != tc.adaptive {
			t.Fatalf("%s supports_adaptive_thinking = %v, want %v", tc.id, m.SupportsAdaptive, tc.adaptive)
		}
		if m.SupportsEffort == nil || *m.SupportsEffort != tc.effort {
			t.Fatalf("%s supports_effort = %v, want %v", tc.id, m.SupportsEffort, tc.effort)
		}
	}
}

func TestAgentDesktopProbeInference(t *testing.T) {
	f, store, acc := bindingSetup(t)
	pool := NewAccountPool()
	pool.accounts = []*Account{acc}

	// Non-streaming probeInference request
	nonStreamBody := `{"model":"sonnet-5.5","max_tokens":1,"messages":[{"role":"user","content":"."}]}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(nonStreamBody))
	req.Header.Set("Content-Type", "application/json")
	HandleAnthropicMessages(pool)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("non-stream probe status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp AnthropicResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode non-stream probe response: %v", err)
	}
	if len(resp.Content) != 1 || resp.Content[0].Text != "." {
		t.Fatalf("unexpected non-stream probe content: %+v", resp.Content)
	}
	if resp.Usage == nil || resp.Usage.InputTokens != 1 || resp.Usage.OutputTokens != 1 {
		t.Fatalf("unexpected non-stream probe usage: %+v", resp.Usage)
	}

	// Streaming probeInference request with Desktop alias claude-sol-6-1[1m]
	streamBody := `{"model":"claude-sol-6-1[1m]","max_tokens":1,"stream":true,"messages":[{"role":"user","content":"."}]}`
	recStream := httptest.NewRecorder()
	reqStream := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(streamBody))
	reqStream.Header.Set("Content-Type", "application/json")
	HandleAnthropicMessages(pool)(recStream, reqStream)

	if recStream.Code != http.StatusOK {
		t.Fatalf("stream probe status = %d, body = %s", recStream.Code, recStream.Body.String())
	}
	sse := recStream.Body.String()
	for _, want := range []string{"event: message_start", "event: content_block_delta", `"text":"."`, "event: message_stop"} {
		if !strings.Contains(sse, want) {
			t.Fatalf("stream probe SSE missing %q in:\n%s", want, sse)
		}
	}

	// CLI-shaped cliProbe request (`claude -p "." --model claude-sonnet-5-5 --tools ""`)
	cliProbeBody := `{"model":"claude-sonnet-5-5","max_tokens":128000,"stream":true,"system":"You are Claude Code.","messages":[{"role":"user","content":[{"type":"text","text":"<system-reminder>\nContext\n</system-reminder>"},{"type":"text","text":"."}]}]}`
	recCLI := httptest.NewRecorder()
	reqCLI := httptest.NewRequest(http.MethodPost, "/v1/messages?beta=true", strings.NewReader(cliProbeBody))
	reqCLI.Header.Set("Content-Type", "application/json")
	HandleAnthropicMessages(pool)(recCLI, reqCLI)
	if recCLI.Code != http.StatusOK || !strings.Contains(recCLI.Body.String(), `"text":"."`) {
		t.Fatalf("CLI probe status = %d, body = %s", recCLI.Code, recCLI.Body.String())
	}

	// Neither probe should create any Notion threads or send upstream events
	if c, s := f.counts(); c != 0 || s != 0 {
		t.Fatalf("probeInference must not hit Notion upstream: creates=%d sends=%d", c, s)
	}
	store.mu.Lock()
	bindingCount := len(store.records)
	store.mu.Unlock()
	if bindingCount != 0 {
		t.Fatalf("probeInference must not record thread bindings: got %d", bindingCount)
	}

	// Unsupported model on probe still fails with 400 model_unavailable
	badBody := `{"model":"claude-sonnet-4.6","max_tokens":1,"messages":[{"role":"user","content":"."}]}`
	recBad := httptest.NewRecorder()
	reqBad := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(badBody))
	reqBad.Header.Set("Content-Type", "application/json")
	HandleAnthropicMessages(pool)(recBad, reqBad)
	if recBad.Code != http.StatusBadRequest || !strings.Contains(recBad.Body.String(), "[bridge:model_unavailable]") {
		t.Fatalf("unsupported model probe should return 400 [bridge:model_unavailable], got %d: %s", recBad.Code, recBad.Body.String())
	}
}

// --- merged from agent_thread_sonnet_test.go ---
func TestAgentThreadSonnet55SelectionResolvesVerifiedModel(t *testing.T) {
	for _, alias := range []string{"sonnet-5.5", "claude-sonnet-5-5", "claude-sonnet-5.5", "achira-donut", " Sonnet-5.5 "} {
		id, err := agentThreadModelID(alias)
		if err != nil || id != "achira-donut" {
			t.Fatalf("%q resolved to %q: %v", alias, id, err)
		}
		body := agentThreadBody(&Account{SpaceID: "space"}, nil, id)
		if body["model"] != "achira-donut" {
			t.Fatalf("%q did not send the Sonnet 5.5 model", alias)
		}
	}
	for _, alias := range []string{"sonnet-5.6", "sonnet-4.6", "claude-sonnet"} {
		if _, err := agentThreadModelID(alias); err == nil {
			t.Fatalf("%q silently fell back to another model", alias)
		}
	}
	if id, err := agentThreadModelID("opus-5.5"); err != nil || id != "albuquerque-quinn" {
		t.Fatalf("Opus 5.5 mapping changed: %q %v", id, err)
	}
	if _, err := agentThreadModelID("gpt-5.4"); err == nil || !strings.Contains(err.Error(), "sonnet-5.5") {
		t.Fatalf("unsupported-model error should list sonnet-5.5, got %v", err)
	}
}


