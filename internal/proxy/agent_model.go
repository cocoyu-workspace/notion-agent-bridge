package proxy

import (
	"fmt"
	"sort"
)

// agentAdvertisedModels are the aliases the Agent transport accepts (see
// agentThreadModelID). They are advertised on /v1/models in agent_thread mode
// so CLI and Claude Desktop 3P Gateway clients can discover supported models.
var agentAdvertisedModels = []string{
	"auto",
	"claude-auto",
	"claude-opus-5-5",
	"claude-sol-6-1",
	"claude-sonnet-5-5",
	"gpt-6.1-sol",
	"opus-5.5",
	"sonnet-5.5",
}

type agentModelMeta struct {
	DisplayName         string
	Description         string
	MaxInputTokens      int
	AnthropicFamilyTier string
	IsFamilyDefault     bool
	Supports1M          bool
	SupportsAdaptive    bool
	SupportsEffort      bool
}

var agentAdvertisedModelMeta = map[string]agentModelMeta{
	"auto": {
		DisplayName:      "Auto (Notion Agent)",
		Description:      "Notion Agent automatic model selection",
		MaxInputTokens:   200000,
		SupportsAdaptive: true,
		SupportsEffort:   false,
	},
	"claude-auto": {
		DisplayName:      "Auto (Notion Agent)",
		Description:      "Notion Agent automatic model selection",
		MaxInputTokens:   200000,
		SupportsAdaptive: true,
		SupportsEffort:   false,
	},
	"sonnet-5.5": {
		DisplayName:         "Claude Sonnet 5.5",
		Description:         "Notion Agent Sonnet 5.5 (achira-donut)",
		MaxInputTokens:      200000,
		AnthropicFamilyTier: "sonnet",
		Supports1M:          false,
		SupportsAdaptive:    true,
		SupportsEffort:      true,
	},
	"claude-sonnet-5-5": {
		DisplayName:         "Claude Sonnet 5.5",
		Description:         "Notion Agent Sonnet 5.5 (achira-donut)",
		MaxInputTokens:      200000,
		AnthropicFamilyTier: "sonnet",
		IsFamilyDefault:     true,
		Supports1M:          false,
		SupportsAdaptive:    true,
		SupportsEffort:      true,
	},
	"opus-5.5": {
		DisplayName:         "Claude Opus 5.5",
		Description:         "Notion Agent Opus 5.5 (albuquerque-quinn)",
		MaxInputTokens:      200000,
		AnthropicFamilyTier: "opus",
		Supports1M:          false,
		SupportsAdaptive:    true,
		SupportsEffort:      true,
	},
	"claude-opus-5-5": {
		DisplayName:         "Claude Opus 5.5",
		Description:         "Notion Agent Opus 5.5 (albuquerque-quinn)",
		MaxInputTokens:      200000,
		AnthropicFamilyTier: "opus",
		IsFamilyDefault:     true,
		Supports1M:          false,
		SupportsAdaptive:    true,
		SupportsEffort:      true,
	},
	"gpt-6.1-sol": {
		DisplayName:      "GPT 6.1 Sol",
		Description:      "Notion Agent GPT 6.1 Sol (omniberry-sundae)",
		MaxInputTokens:   1000000,
		Supports1M:       true,
		SupportsAdaptive: true,
		SupportsEffort:   true,
	},
	"claude-sol-6-1": {
		DisplayName:      "GPT 6.1 Sol",
		Description:      "Notion Agent GPT 6.1 Sol (omniberry-sundae)",
		MaxInputTokens:   1000000,
		Supports1M:       true,
		SupportsAdaptive: true,
		SupportsEffort:   true,
	},
}

func applyAgentModelMeta(item publicModel) publicModel {
	meta, ok := agentAdvertisedModelMeta[item.ID]
	if !ok {
		return item
	}
	if item.DisplayName == "" {
		item.DisplayName = meta.DisplayName
	}
	if item.Description == "" {
		item.Description = meta.Description
	}
	if item.MaxInputTokens == 0 {
		item.MaxInputTokens = meta.MaxInputTokens
	}
	if item.AnthropicFamilyTier == "" {
		item.AnthropicFamilyTier = meta.AnthropicFamilyTier
	}
	if !item.IsFamilyDefault {
		item.IsFamilyDefault = meta.IsFamilyDefault
	}
	if !item.Supports1M {
		item.Supports1M = meta.Supports1M
	}
	if item.SupportsAdaptive == nil {
		adaptive := meta.SupportsAdaptive
		item.SupportsAdaptive = &adaptive
	}
	if item.SupportsEffort == nil {
		effort := meta.SupportsEffort
		item.SupportsEffort = &effort
	}
	return item
}

// withAgentModels adds the Agent transport aliases to a public model list.
// It never removes existing entries and populates discovery metadata for Agent aliases.
func withAgentModels(items []publicModel) []publicModel {
	seen := make(map[string]bool, len(items))
	out := make([]publicModel, len(items))
	for i, item := range items {
		seen[item.ID] = true
		out[i] = applyAgentModelMeta(item)
	}
	for _, id := range agentAdvertisedModels {
		if seen[id] {
			continue
		}
		out = append(out, applyAgentModelMeta(publicModel{
			ID:      id,
			Object:  "model",
			Created: publicModelCreatedAt,
			OwnedBy: "notion-manager",
		}))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// agentModelSummary renders the non-secret model/effort that an Agent
// transport request resolves to, for logs. It mirrors the resolution in
// callAgentThreadInference without sending anything: unknown models are
// reported as rejected rather than substituted.
func agentModelSummary(requested, effort string) string {
	upstream, err := agentThreadModelID(requested)
	if err != nil {
		return fmt.Sprintf("requested=%q upstream=<rejected: unsupported> effort=%q", requested, effort)
	}
	if upstream == "" {
		upstream = "<auto>"
	}
	eff, err := normalizeReasoningEffort(effort)
	if err != nil {
		eff = "<invalid>"
	}
	switch eff {
	case "none", "minimal":
		eff = "low"
	case "":
		if upstream == "<auto>" {
			eff = "<unset>"
		} else {
			eff = "medium"
		}
	}
	return fmt.Sprintf("requested=%q upstream=%s effort=%s", requested, upstream, eff)
}
