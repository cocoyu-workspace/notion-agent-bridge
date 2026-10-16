package proxy

import (
	"strings"
	"testing"
)

func TestCitationReplacerRepairsCurrentClaudeSonnetExample(t *testing.T) {
	known := []string{
		"https://www.anthropic.com/news/claude-sonnet-4-6",
		"https://www.datastudios.org/post/claude-opus-4-5-vs-claude-sonnet-4-5-full-report-and-comparison-of-features-performance-pricing-a",
		"https://www.nxcode.io/resources/news/claude-sonnet-4-6-vs-opus-4-6-which-model-to-choose-2026",
	}

	cr := newCitationReplacer(&known, nil, nil)
	var got strings.Builder
	deltas := []string{
		"- **Claude Sonnet 4.6**：2026 年 2 月 17 日发布，是目前最新的 Sonnet 版本[^{{https://www.anthropic.com/news/claude-son",
		"view://2b87f824-7ce8-40e3-b61c-6c7c375686f5]\n- **速度快**：响应延迟低，比 Opus 快很多，适合交互式应用和高频任务[^{{https://www.datastudios.org/post/claude-opus-4-5-vs-claude-sonnet-4-5-full-report-and-comparison-of-features-performance",
		"view://2b87f824-7ce8-40e3-b61c-6c7c375686f5]\n- **编码能力强**：在 SWE-bench 等编码基准上接近 Opus 水平（Sonnet 4.6 得分 79.6% vs Opus 4.6 的 80.8%）[^{{https://www.nxcode.io/resources/news/claude-sonnet-4-6-vs-opus-4-6-which-model-to-choose",
		"view://2b87f824-7ce8-40e3-b61c-6c7c375686f5]\n",
	}

	for _, delta := range deltas {
		got.WriteString(cr.Process(delta))
	}
	got.WriteString(cr.Flush())

	out := got.String()
	if !strings.Contains(out, "Sonnet 版本 [1]") {
		t.Fatalf("expected repaired first citation, got %q", out)
	}
	if !strings.Contains(out, "高频任务 [2]") {
		t.Fatalf("expected repaired second citation, got %q", out)
	}
	if !strings.Contains(out, "80.8%） [3]") {
		t.Fatalf("expected repaired third citation, got %q", out)
	}

	urls := cr.URLs()
	if len(urls) != len(known) {
		t.Fatalf("unexpected URL count: got %d want %d (%q)", len(urls), len(known), urls)
	}
	for i := range known {
		if urls[i] != known[i] {
			t.Fatalf("unexpected URL at %d: got %q want %q", i, urls[i], known[i])
		}
	}
}

func TestCitationReplacerRecoversMissingClaudeSonnetCitationsFromContext(t *testing.T) {
	known := []string{
		"https://www.anthropic.com/news/claude-4",
		"https://www.anthropic.com/news/claude-3-5-sonnet",
		"https://www.anthropic.com/news/claude-sonnet-4-6",
	}
	docs := []CitationCandidate{
		{
			URL:   "https://www.anthropic.com/news/claude-4",
			Title: "Introducing Claude 4 - Anthropic",
			Text:  "Claude Opus 4 and Sonnet 4 are hybrid models. Sonnet 4 pricing is $3/$15 per million tokens.",
		},
		{
			URL:   "https://www.anthropic.com/news/claude-3-5-sonnet",
			Title: "Introducing Claude 3.5 Sonnet - Anthropic",
			Text:  "Claude 3.5 Sonnet launches with the speed and cost of our mid-tier model.",
		},
		{
			URL:   "https://www.anthropic.com/news/claude-sonnet-4-6",
			Title: "Introducing Claude Sonnet 4.6 - Anthropic",
			Text:  "Sonnet 4.6 features a 1M token context window in beta. Pricing remains the same as Sonnet 4.5, starting at $3/$15 per million tokens. They often even prefer it to Claude Opus 4.5.",
		},
	}

	cr := newCitationReplacer(&known, &docs, nil)
	var got strings.Builder
	deltas := []string{
		"- **性价比高**：定价 $3/$15 每百万 token（输入/输出），仅为 Opus 的 1/5[^{{https://www.anthrop",
		"ic.com/news/claude-",
		"view://2b87f824-7ce8-40e3-b61c-6c7c375686f5]\n",
		"- 实际上，很多开发者发现 Sonnet 4.6 在多数场景中甚至可以媲美 Opus，性价比非常突出。[^{{https://www.anthrop",
		"view://2b87f824-7ce8-40e3-b61c-6c7c375686f5]\n",
	}

	for _, delta := range deltas {
		got.WriteString(cr.Process(delta))
	}
	got.WriteString(cr.Flush())

	out := got.String()
	if !strings.Contains(out, "仅为 Opus 的 1/5 [1]") {
		t.Fatalf("expected price citation to be recovered, got %q", out)
	}
	if !strings.Contains(out, "性价比非常突出。 [1]") {
		t.Fatalf("expected final summary citation to be recovered, got %q", out)
	}

	urls := cr.URLs()
	if len(urls) != 1 {
		t.Fatalf("expected one recovered URL, got %d (%q)", len(urls), urls)
	}
	if urls[0] != "https://www.anthropic.com/news/claude-sonnet-4-6" {
		t.Fatalf("unexpected recovered URL: %q", urls[0])
	}
}

func TestRenderAnthropicCitationTextMatchesStreamingCitationReplacer(t *testing.T) {
	known := []string{
		"https://www.anthropic.com/news/claude-4",
		"https://www.anthropic.com/news/claude-sonnet-4-5",
		"https://www.anthropic.com/news/claude-sonnet-4-6",
		"https://www.datastudios.org/post/claude-opus-4-5-vs-claude-sonnet-4-5-full-report-and-comparison-of-features-performance-pricing-a",
	}
	docs := []CitationCandidate{
		{
			URL:   "https://www.anthropic.com/news/claude-4",
			Title: "Introducing Claude 4 - Anthropic",
			Text:  "May 22, 2025. Claude Opus 4 and Claude Sonnet 4 are hybrid models with pricing at $3/$15.",
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
		{
			URL:   "https://www.datastudios.org/post/claude-opus-4-5-vs-claude-sonnet-4-5-full-report-and-comparison-of-features-performance-pricing-a",
			Title: "Claude Opus 4.5 vs Claude Sonnet 4.5",
			Text:  "One of the most noticeable differences between Sonnet and Opus 4.5 is speed and latency.",
		},
	}

	deltas := []string{
		"### 各版本 Sonnet 发布时间\n\n- **Claude Sonnet 4**：2025 年 5 月 22 日发布[^{{https://www.anthropic.com/news/claude",
		"]\n- **Claude Sonnet 4.5**：2025 年 9 月 29 日发布[^{{https://www.anthropic.com/news/claude-son",
		"]\n- **Claude Sonnet 4.6**：2026 年 2 月 17 日发布[^view://artifact-123]\n- **速度与延迟**：Sonnet 针对低延迟和快速响应进行了优化[^{{https://www.datastudios.org/post/claude-opus-4-5-vs-claude-sonnet-4-5-full-report-and-comparison-of-features-performance-pricing-a}}]\n",
	}

	cr := newCitationReplacer(&known, &docs, nil)
	var streamed strings.Builder
	for _, delta := range deltas {
		streamed.WriteString(cr.Process(delta))
	}
	streamed.WriteString(cr.Flush())
	streamed.WriteString(formatCitationSources(cr.URLs()))

	got := renderAnthropicCitationText(strings.Join(deltas, ""), known, docs, nil)
	if got != streamed.String() {
		t.Fatalf("rendered text mismatch:\n got: %q\nwant: %q", got, streamed.String())
	}
}
