package proxy

import (
	"strings"
	"testing"
)

func TestNormalizeCitationTargetResolvesMappedToolRef(t *testing.T) {
	toolMap := map[string][]string{
		"toolu_test123": {"https://example.com/a", "https://example.com/b"},
	}
	got, ok := normalizeCitationTarget("2toolu_test123", toolMap)
	if !ok {
		t.Fatalf("expected tool ref to resolve, got ok=false")
	}
	if got != "https://example.com/b" {
		t.Fatalf("unexpected resolved URL: %q", got)
	}
}

func TestNormalizeCitationTargetRejectsUnmappedToolRef(t *testing.T) {
	if _, ok := normalizeCitationTarget("toolu_unknown", nil); ok {
		t.Fatalf("expected unmapped tool ref to be rejected")
	}
}

func TestNormalizeCitationTargetStripsEmbeddedInternalSchemeSuffix(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "thread suffix",
			raw:  "https://www.anthropic.com/news/claude-3-5-thread://bf5c73d0-a3d9-8123-aa0f-0003c7719587/99e0df84-2c4c-420c-92c3-6f8ee959ef31",
			want: "https://www.anthropic.com/news/claude-3-5",
		},
		{
			name: "view suffix",
			raw:  "https://aws.amazon.com/blogs/aws/introducing-claude-sonnet-4-5-in-amazon-bedrock-anthropics-most-intelligent-model-best-for-coding-and-complex-agents/view://f5a7c6c7-26cf-4984-85a1-b57b72dd05e7",
			want: "https://aws.amazon.com/blogs/aws/introducing-claude-sonnet-4-5-in-amazon-bedrock-anthropics-most-intelligent-model-best-for-coding-and-complex-agents",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := normalizeCitationTarget(tt.raw, nil)
			if !ok {
				t.Fatalf("expected citation to be accepted")
			}
			if got != tt.want {
				t.Fatalf("unexpected normalized URL: got %q want %q", got, tt.want)
			}
		})
	}
}

func TestNormalizeCitationTargetPreservesBalancedTrailingParenthesis(t *testing.T) {
	got, ok := normalizeCitationTarget("https://en.wikipedia.org/wiki/Claude_(language_model)", nil)
	if !ok {
		t.Fatalf("expected citation to be accepted")
	}
	if got != "https://en.wikipedia.org/wiki/Claude_(language_model)" {
		t.Fatalf("unexpected normalized URL: %q", got)
	}
}

func TestNormalizeCitationTargetCompletesUniquePrefixFromKnownURLs(t *testing.T) {
	known := []string{
		"https://www.anthropic.com/news/claude-sonnet-4-6",
	}
	got, ok := normalizeCitationTargetWithCandidates(
		"https://www.anthropic.com/news/claude-sonview://artifact-123",
		nil,
		known,
	)
	if !ok {
		t.Fatalf("expected truncated citation prefix to be repaired")
	}
	if got != known[0] {
		t.Fatalf("unexpected normalized URL: got %q want %q", got, known[0])
	}
}

func TestNormalizeCitationTargetRejectsAmbiguousPrefixFromKnownURLs(t *testing.T) {
	known := []string{
		"https://www.anthropic.com/news/claude-4",
		"https://www.anthropic.com/news/claude-sonnet-4-6",
	}
	if _, ok := normalizeCitationTargetWithCandidates(
		"https://www.anthropic.com/news/claudeview://artifact-123",
		nil,
		known,
	); ok {
		t.Fatalf("expected ambiguous truncated citation to be rejected")
	}
}

func TestNormalizeCitationTargetUsesContextForAmbiguousPrefix(t *testing.T) {
	known := []string{
		"https://www.anthropic.com/news/claude-4",
		"https://www.anthropic.com/news/claude-3-5-sonnet",
		"https://www.anthropic.com/news/claude-sonnet-4-6",
	}
	docs := []CitationCandidate{
		{
			URL:   "https://www.anthropic.com/news/claude-4",
			Title: "Introducing Claude 4 - Anthropic",
			Text:  "Claude Opus 4 and Sonnet 4 are hybrid models. Pricing remains consistent with previous Opus and Sonnet models: Sonnet 4 at $3/$15.",
		},
		{
			URL:   "https://www.anthropic.com/news/claude-3-5-sonnet",
			Title: "Introducing Claude 3.5 Sonnet - Anthropic",
			Text:  "Jun 21, 2024. Claude 3.5 Sonnet raises the bar with the speed and cost of our mid-tier model.",
		},
		{
			URL:   "https://www.anthropic.com/news/claude-sonnet-4-6",
			Title: "Introducing Claude Sonnet 4.6 - Anthropic",
			Text:  "Feb 17, 2026. Sonnet 4.6 features a 1M token context window in beta. Pricing remains the same as Sonnet 4.5, starting at $3/$15 per million tokens. They often even prefer it to Claude Opus 4.5.",
		},
	}

	got, ok := normalizeCitationTargetWithContext(
		"https://www.anthropic.com/news/claude-view://artifact-123",
		nil,
		known,
		docs,
		"定价 $3/$15 每百万 token（输入/输出），仅为 Opus 的 1/5",
	)
	if !ok {
		t.Fatalf("expected ambiguous prefix to be resolved from context")
	}
	if got != "https://www.anthropic.com/news/claude-sonnet-4-6" {
		t.Fatalf("unexpected normalized URL: %q", got)
	}
}

func TestNormalizeCitationTargetUsesContextForAmbiguousHTTPPrefix(t *testing.T) {
	known := []string{
		"https://www.anthropic.com/news/claude-3-family",
		"https://www.anthropic.com/news/claude-4",
		"https://www.anthropic.com/news/claude-sonnet-4-5",
		"https://www.anthropic.com/news/claude-sonnet-4-6",
	}
	docs := []CitationCandidate{
		{
			URL:   "https://www.anthropic.com/news/claude-3-family",
			Title: "Introducing the next generation of Claude - Anthropic",
			Text:  "Claude 3 Sonnet launched on March 4, 2024.",
		},
		{
			URL:   "https://www.anthropic.com/news/claude-4",
			Title: "Introducing Claude 4 - Anthropic",
			Text:  "May 22, 2025. Today, we're introducing Claude Opus 4 and Claude Sonnet 4.",
		},
		{
			URL:   "https://www.anthropic.com/news/claude-sonnet-4-5",
			Title: "Introducing Claude Sonnet 4.5 - Anthropic",
			Text:  "September 29, 2025. Sonnet 4.5 is our best coding model for real-world agents.",
		},
		{
			URL:   "https://www.anthropic.com/news/claude-sonnet-4-6",
			Title: "Introducing Claude Sonnet 4.6 - Anthropic",
			Text:  "February 17, 2026. Sonnet 4.6 is our most capable Sonnet model yet.",
		},
	}

	cases := []struct {
		name    string
		raw     string
		context string
		want    string
	}{
		{
			name:    "claude 4 release",
			raw:     "https://www.anthropic.com/news/claude",
			context: "- **Claude Sonnet 4**：2025 年 5 月 22 日发布",
			want:    "https://www.anthropic.com/news/claude-4",
		},
		{
			name:    "claude sonnet 4.5 release",
			raw:     "https://www.anthropic.com/news/claude-son",
			context: "- **Claude Sonnet 4.5**：2025 年 9 月 29 日发布",
			want:    "https://www.anthropic.com/news/claude-sonnet-4-5",
		},
		{
			name:    "claude sonnet 4.6 release",
			raw:     "https://www.anthropic.com/news/claude-sonnet-4",
			context: "- **Claude Sonnet 4.6**：2026 年 2 月 17 日发布",
			want:    "https://www.anthropic.com/news/claude-sonnet-4-6",
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := normalizeCitationTargetWithContext(tt.raw, nil, known, docs, tt.context)
			if !ok {
				t.Fatalf("expected ambiguous HTTP prefix to be resolved")
			}
			if got != tt.want {
				t.Fatalf("unexpected normalized URL: got %q want %q", got, tt.want)
			}
		})
	}
}

func TestExpandToolCitationReferences(t *testing.T) {
	toolMap := map[string][]string{
		"toolu_test123": {"https://example.com/a"},
	}
	text := "hello[^{{toolu_test123}}]"
	got := expandToolCitationReferences(text, toolMap)
	want := "hello[^https://example.com/a]"
	if got != want {
		t.Fatalf("unexpected expanded text: got %q want %q", got, want)
	}
}

func TestExpandToolCitationReferencesUsesContextForAmbiguousToolRef(t *testing.T) {
	toolMap := map[string][]string{
		"toolu_test123": {
			"https://galileo.ai/blog/claude-3-5-sonnet-complete-guide-ai-capabilities-analysis",
			"https://www.anthropic.com/news/claude-4",
			"https://www.anthropic.com/news/claude-sonnet-4-6",
		},
	}
	text := "- **Claude Sonnet 4**：2025 年 5 月 22 日发布，与 Claude Opus 4 同时推出[^toolu_test123]\n"
	got := expandToolCitationReferencesWithKnowledge(text, toolMap, []CitationCandidate{
		{
			URL:   "https://galileo.ai/blog/claude-3-5-sonnet-complete-guide-ai-capabilities-analysis",
			Title: "Claude 3.5 Sonnet Complete Guide",
			Text:  "Coding and software engineering performance.",
		},
		{
			URL:   "https://www.anthropic.com/news/claude-4",
			Title: "Introducing Claude 4 - Anthropic",
			Text:  "May 22, 2025. Today, we're introducing Claude Opus 4 and Claude Sonnet 4.",
		},
		{
			URL:   "https://www.anthropic.com/news/claude-sonnet-4-6",
			Title: "Introducing Claude Sonnet 4.6 - Anthropic",
			Text:  "Feb 17, 2026. Claude Sonnet 4.6 is our most capable Sonnet model yet.",
		},
	})
	want := "- **Claude Sonnet 4**：2025 年 5 月 22 日发布，与 Claude Opus 4 同时推出[^https://www.anthropic.com/news/claude-4]\n"
	if got != want {
		t.Fatalf("unexpected expanded text: got %q want %q", got, want)
	}
}

func TestCleanCitationsWithContextResolvesAmbiguousToolCitationByContext(t *testing.T) {
	text := "- **Claude Sonnet 4**：2025 年 5 月 22 日发布[^toolu_test123]"
	toolMap := map[string][]string{
		"toolu_test123": {
			"https://galileo.ai/blog/claude-3-5-sonnet-complete-guide-ai-capabilities-analysis",
			"https://www.anthropic.com/news/claude-4",
			"https://www.anthropic.com/news/claude-sonnet-4-6",
		},
	}
	got := cleanCitationsWithContext(text, toolMap, nil, []CitationCandidate{
		{
			URL:   "https://galileo.ai/blog/claude-3-5-sonnet-complete-guide-ai-capabilities-analysis",
			Title: "Claude 3.5 Sonnet Complete Guide",
			Text:  "Coding and software engineering performance.",
		},
		{
			URL:   "https://www.anthropic.com/news/claude-4",
			Title: "Introducing Claude 4 - Anthropic",
			Text:  "May 22, 2025. Today, we're introducing Claude Opus 4 and Claude Sonnet 4.",
		},
		{
			URL:   "https://www.anthropic.com/news/claude-sonnet-4-6",
			Title: "Introducing Claude Sonnet 4.6 - Anthropic",
			Text:  "Feb 17, 2026. Claude Sonnet 4.6 is our most capable Sonnet model yet.",
		},
	})
	if !strings.Contains(got, "发布 [1]") {
		t.Fatalf("expected inline citation number, got %q", got)
	}
	if !strings.Contains(got, "Sources:\n[1] https://www.anthropic.com/news/claude-4") {
		t.Fatalf("expected Claude 4 source, got %q", got)
	}
}

func TestTrimTrailingIncompleteCitation(t *testing.T) {
	in := "abc[^{{https://www.anthrop"
	got := trimTrailingIncompleteCitation(in)
	if got != "abc" {
		t.Fatalf("unexpected trimmed text: got %q want %q", got, "abc")
	}

	complete := "abc[^view://artifact-123]"
	if trimTrailingIncompleteCitation(complete) != complete {
		t.Fatalf("expected complete citation to remain untouched")
	}
}

func TestCleanCitationsDropsUnresolvedToolCitation(t *testing.T) {
	in := "Answer[^{{toolu_test123}}]"
	got := cleanCitations(in)
	if strings.Contains(got, "toolu_") {
		t.Fatalf("expected unresolved tool citation to be removed, got %q", got)
	}
	if strings.Contains(got, "Sources:") {
		t.Fatalf("expected no Sources section for unresolved tool citation, got %q", got)
	}
	if got != "Answer" {
		t.Fatalf("unexpected cleaned text: %q", got)
	}
}

func TestCleanCitationsWithCandidatesRepairsKnownPrefix(t *testing.T) {
	in := "Answer[^{{https://www.anthropic.com/news/claude-sonview://artifact-123]"
	got := cleanCitationsWithCandidates(in, []string{
		"https://www.anthropic.com/news/claude-sonnet-4-6",
	})
	if !strings.Contains(got, "Sources:\n[1] https://www.anthropic.com/news/claude-sonnet-4-6") {
		t.Fatalf("expected repaired source URL, got %q", got)
	}
}

func TestCleanCitationsWithKnowledgeRecoversViewCitationFromContext(t *testing.T) {
	in := "很多开发者发现 Sonnet 4.6 在多数场景中甚至可以媲美 Opus，性价比非常突出。[^view://artifact-123]"
	got := cleanCitationsWithKnowledge(in, nil, []CitationCandidate{
		{
			URL:   "https://www.anthropic.com/news/claude-4",
			Title: "Introducing Claude 4 - Anthropic",
			Text:  "Claude Opus 4 and Sonnet 4 are hybrid models with pricing at $3/$15.",
		},
		{
			URL:   "https://www.anthropic.com/news/claude-sonnet-4-6",
			Title: "Introducing Claude Sonnet 4.6 - Anthropic",
			Text:  "Claude Sonnet 4.6 is our most capable Sonnet model yet. They often even prefer it to our smartest model from November 2025, Claude Opus 4.5.",
		},
		{
			URL:   "https://www.anthropic.com/claude/sonnet",
			Title: "Claude Sonnet 4.6 - Anthropic",
			Text:  "Pricing for Sonnet 4.6 starts at $3 per million input tokens and $15 per million output tokens.",
		},
	})

	if !strings.Contains(got, "性价比非常突出。 [1]") {
		t.Fatalf("expected inline citation number, got %q", got)
	}
	if !strings.Contains(got, "Sources:\n[1] https://www.anthropic.com/news/claude-sonnet-4-6") {
		t.Fatalf("expected recovered source URL, got %q", got)
	}
}

func TestCleanCitationsWithContextResolvesRepeatedToolCitationByLineContext(t *testing.T) {
	text := "" +
		"- **Claude Sonnet 4**：2025 年 5 月 22 日发布[^toolu_test123]\n" +
		"- **Claude Sonnet 4.5**：2025 年 9 月 29 日发布[^toolu_test123]\n" +
		"- **Claude Sonnet 4.6**：2026 年 2 月 17 日发布[^toolu_test123]"

	toolMap := map[string][]string{
		"toolu_test123": {
			"https://en.wikipedia.org/wiki/Claude_(language_model)",
			"https://www.anthropic.com/news/claude-4",
			"https://www.anthropic.com/news/claude-sonnet-4-5",
			"https://www.anthropic.com/news/claude-sonnet-4-6",
		},
	}

	got := cleanCitationsWithContext(text, toolMap, nil, []CitationCandidate{
		{
			URL:   "https://en.wikipedia.org/wiki/Claude_(language_model)",
			Title: "Claude (language model) - Wikipedia",
			Text:  "Claude 3 Sonnet released on March 4, 2024. Claude 3.5 Sonnet released on June 20, 2024. Claude Sonnet 4 released on May 22, 2025. Claude Sonnet 4.5 released on September 29, 2025. Claude Sonnet 4.6 released on February 17, 2026.",
		},
		{
			URL:   "https://www.anthropic.com/news/claude-4",
			Title: "Introducing Claude 4 - Anthropic",
			Text:  "May 22, 2025. Today, we're introducing Claude Opus 4 and Claude Sonnet 4.",
		},
		{
			URL:   "https://www.anthropic.com/news/claude-sonnet-4-5",
			Title: "Introducing Claude Sonnet 4.5",
			Text:  "September 29, 2025. Sonnet 4.5 is our best coding model for real-world agents.",
		},
		{
			URL:   "https://www.anthropic.com/news/claude-sonnet-4-6",
			Title: "Introducing Claude Sonnet 4.6 - Anthropic",
			Text:  "February 17, 2026. Sonnet 4.6 is our most capable Sonnet model yet.",
		},
	})

	if !strings.Contains(got, "Claude Sonnet 4**：2025 年 5 月 22 日发布 [1]") {
		t.Fatalf("expected Claude Sonnet 4 citation, got %q", got)
	}
	if !strings.Contains(got, "Claude Sonnet 4.5**：2025 年 9 月 29 日发布 [2]") {
		t.Fatalf("expected Claude Sonnet 4.5 citation, got %q", got)
	}
	if !strings.Contains(got, "Claude Sonnet 4.6**：2026 年 2 月 17 日发布 [3]") {
		t.Fatalf("expected Claude Sonnet 4.6 citation, got %q", got)
	}
	if !strings.Contains(got, "Sources:\n[1] https://www.anthropic.com/news/claude-4\n[2] https://www.anthropic.com/news/claude-sonnet-4-5\n[3] https://www.anthropic.com/news/claude-sonnet-4-6") {
		t.Fatalf("expected ordered source list, got %q", got)
	}
}

func TestCleanCitationsWithContextResolvesAmbiguousHTTPPrefixesByContext(t *testing.T) {
	text := "" +
		"- **Claude Sonnet 4**：2025 年 5 月 22 日发布[^https://www.anthropic.com/news/claude]\n" +
		"- **Claude Sonnet 4.5**：2025 年 9 月 29 日发布[^https://www.anthropic.com/news/claude-son]\n" +
		"- **Claude Sonnet 4.6**：2026 年 2 月 17 日发布[^https://www.anthropic.com/news/claude-sonnet-4]"

	known := []string{
		"https://www.anthropic.com/news/claude-3-family",
		"https://www.anthropic.com/news/claude-4",
		"https://www.anthropic.com/news/claude-sonnet-4-5",
		"https://www.anthropic.com/news/claude-sonnet-4-6",
	}

	got := cleanCitationsWithContext(text, nil, known, []CitationCandidate{
		{
			URL:   "https://www.anthropic.com/news/claude-3-family",
			Title: "Introducing the next generation of Claude - Anthropic",
			Text:  "Claude 3 Sonnet launched on March 4, 2024.",
		},
		{
			URL:   "https://www.anthropic.com/news/claude-4",
			Title: "Introducing Claude 4 - Anthropic",
			Text:  "May 22, 2025. Today, we're introducing Claude Opus 4 and Claude Sonnet 4.",
		},
		{
			URL:   "https://www.anthropic.com/news/claude-sonnet-4-5",
			Title: "Introducing Claude Sonnet 4.5 - Anthropic",
			Text:  "September 29, 2025. Sonnet 4.5 is our best coding model for real-world agents.",
		},
		{
			URL:   "https://www.anthropic.com/news/claude-sonnet-4-6",
			Title: "Introducing Claude Sonnet 4.6 - Anthropic",
			Text:  "February 17, 2026. Sonnet 4.6 is our most capable Sonnet model yet.",
		},
	})

	if !strings.Contains(got, "Claude Sonnet 4**：2025 年 5 月 22 日发布 [1]") {
		t.Fatalf("expected Claude Sonnet 4 citation, got %q", got)
	}
	if !strings.Contains(got, "Claude Sonnet 4.5**：2025 年 9 月 29 日发布 [2]") {
		t.Fatalf("expected Claude Sonnet 4.5 citation, got %q", got)
	}
	if !strings.Contains(got, "Claude Sonnet 4.6**：2026 年 2 月 17 日发布 [3]") {
		t.Fatalf("expected Claude Sonnet 4.6 citation, got %q", got)
	}
	if strings.Contains(got, "https://www.anthropic.com/news/claude-son\n") || strings.Contains(got, "https://www.anthropic.com/news/claude-sonnet-4\n") {
		t.Fatalf("expected truncated URLs to be removed, got %q", got)
	}
	if !strings.Contains(got, "Sources:\n[1] https://www.anthropic.com/news/claude-4\n[2] https://www.anthropic.com/news/claude-sonnet-4-5\n[3] https://www.anthropic.com/news/claude-sonnet-4-6") {
		t.Fatalf("expected repaired source list, got %q", got)
	}
}
