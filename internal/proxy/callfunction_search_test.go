package proxy

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// TestParseCallFunctionSearchOutput verifies parsing of the new callFunction
// result.output JSON format into CitationCandidate docs.
func TestParseCallFunctionSearchOutput(t *testing.T) {
	output := `{"results":[` +
		`{"id":"f50e1ef1","type":"page","title":"Weekly To-do List","snippet":"March 4","url":"https://www.notion.so/f50e1ef1"},` +
		`{"id":"webpage://?url=https%3A%2F%2Ftechcrunch.com%2F2026%2Fai-news","type":"webpage","title":"Biggest AI Stories","snippet":"AI news","url":"https://techcrunch.com/2026/ai-news"},` +
		`{"id":"webpage://?url=https%3A%2F%2Fexample.com%2Fai","type":"webpage","title":"Example AI","snippet":"more","url":"https://example.com/ai"}` +
		`]}`

	docs := parseCallFunctionSearchOutput(output)
	if len(docs) != 3 {
		t.Fatalf("expected 3 docs, got %d", len(docs))
	}
	if docs[0].URL != "https://www.notion.so/f50e1ef1" || docs[0].Title != "Weekly To-do List" {
		t.Errorf("doc[0] mismatch: %+v", docs[0])
	}
	if docs[1].URL != "https://techcrunch.com/2026/ai-news" || docs[1].Title != "Biggest AI Stories" {
		t.Errorf("doc[1] mismatch: %+v", docs[1])
	}
	if docs[2].URL != "https://example.com/ai" {
		t.Errorf("doc[2] mismatch: %+v", docs[2])
	}
}

// TestParseCallFunctionSearchOutputDeduplicatesURLs ensures duplicate URLs are skipped.
func TestParseCallFunctionSearchOutputDeduplicatesURLs(t *testing.T) {
	output := `{"results":[` +
		`{"id":"1","type":"webpage","title":"A","url":"https://example.com/a"},` +
		`{"id":"2","type":"webpage","title":"A copy","url":"https://example.com/a"},` +
		`{"id":"3","type":"webpage","title":"B","url":"https://example.com/b"}` +
		`]}`

	docs := parseCallFunctionSearchOutput(output)
	if len(docs) != 2 {
		t.Fatalf("expected 2 unique docs, got %d", len(docs))
	}
}

// TestParseCallFunctionSearchOutputFallsBackToID tests URL extraction from
// the "webpage://?url=..." ID format when URL field is empty.
func TestParseCallFunctionSearchOutputFallsBackToID(t *testing.T) {
	output := `{"results":[{"id":"webpage://?url=https%3A%2F%2Fexample.com%2Ftest","type":"webpage","title":"Test"}]}`

	docs := parseCallFunctionSearchOutput(output)
	if len(docs) != 1 {
		t.Fatalf("expected 1 doc, got %d", len(docs))
	}
	if docs[0].URL != "https://example.com/test" {
		t.Errorf("expected URL extracted from ID, got %q", docs[0].URL)
	}
}

// TestParseCallFunctionSearchOutputInvalidJSON returns nil for bad input.
func TestParseCallFunctionSearchOutputInvalidJSON(t *testing.T) {
	docs := parseCallFunctionSearchOutput("not json")
	if docs != nil {
		t.Fatalf("expected nil for invalid JSON, got %v", docs)
	}
}

// TestFormatCitationDocsSummary checks formatting of citation docs into thinking summary.
func TestFormatCitationDocsSummary(t *testing.T) {
	docs := []CitationCandidate{
		{URL: "https://example.com/a", Title: "First Result"},
		{URL: "https://example.com/b", Title: "Second Result"},
		{URL: "https://example.com/c", Title: "Third Result"},
	}
	summary := formatCitationDocsSummary(docs)
	if !strings.Contains(summary, "**Found 3 Results**") {
		t.Errorf("expected result count, got %q", summary)
	}
	if !strings.Contains(summary, "1. First Result") {
		t.Errorf("expected first result title, got %q", summary)
	}
	if !strings.Contains(summary, "3. Third Result") {
		t.Errorf("expected third result title, got %q", summary)
	}
}

// TestFormatCitationDocsSummaryEmpty returns empty string for no docs.
func TestFormatCitationDocsSummaryEmpty(t *testing.T) {
	if s := formatCitationDocsSummary(nil); s != "" {
		t.Errorf("expected empty, got %q", s)
	}
}

// TestParseNDJSONStreamCallFunctionSearchThinking is an end-to-end regression test
// that simulates the new Notion callFunction search flow and verifies that detailed
// thinking content (Web Search, Searching, Search Complete, Found N Results) is emitted.
func TestParseNDJSONStreamCallFunctionSearchThinking(t *testing.T) {
	// Build callFunction search result output JSON (double-encoded as a JSON string,
	// matching real Notion format where result.output is a string containing JSON)
	searchOutput := map[string]interface{}{
		"results": []map[string]string{
			{"id": "page1", "type": "page", "title": "Weekly To-do List", "url": "https://www.notion.so/page1"},
			{"id": "webpage://?url=https%3A%2F%2Ftechcrunch.com%2Fai-news", "type": "webpage", "title": "AI Stories 2026", "url": "https://techcrunch.com/ai-news"},
			{"id": "webpage://?url=https%3A%2F%2Fexample.com%2Fai", "type": "webpage", "title": "AI Updates", "url": "https://example.com/ai"},
		},
	}
	searchOutputInner, _ := json.Marshal(searchOutput)
	// Double-encode: the outer JSON field "output" is a string containing the inner JSON
	searchOutputJSON, _ := json.Marshal(string(searchOutputInner))

	stream := strings.Join([]string{
		// Step 1: initial thinking
		`{"type":"agent-inference","id":"step1","value":[{"type":"thinking","content":"The user wants AI news from March."}]}`,
		// Step 1: thinking + callFunction tool_use (empty input, like real Notion)
		`{"type":"agent-inference","id":"step1","value":[{"type":"thinking","content":"The user wants AI news from March."},{"type":"tool_use","id":"toolu_01abc","name":"callFunction"}],"finishedAt":1,"inputTokens":10,"outputTokens":2}`,
		// agent-tool-result: streaming, no queries yet
		`{"type":"agent-tool-result","toolCallId":"toolu_01abc","toolName":"callFunction","toolType":"callFunction","state":"streaming","input":{"function":"connections.search.search"},"result":{"headerLabel":[["Searching"]],"cycleQueries":[]}}`,
		// agent-tool-result: streaming, queries appear
		`{"type":"agent-tool-result","toolCallId":"toolu_01abc","toolName":"callFunction","toolType":"callFunction","state":"streaming","input":{"function":"connections.search.search","args":{"queries":[{"question":"AI news March 2026","keywords":"AI news March"}]}},"result":{"headerLabel":[["Searching: AI news March 2026"]],"cycleQueries":["AI news March 2026"]}}`,
		// agent-tool-result: streaming, more queries
		`{"type":"agent-tool-result","toolCallId":"toolu_01abc","toolName":"callFunction","toolType":"callFunction","state":"streaming","input":{"function":"connections.search.search","args":{"queries":[{"question":"AI news March 2026","keywords":"AI news March"},{"question":"AI breakthroughs March","keywords":"AI breakthroughs"}],"includeWebResults":true}},"result":{"headerLabel":[["Searching: AI news March 2026"]],"cycleQueries":["AI news March 2026","AI breakthroughs March"]}}`,
		// agent-tool-result: applied (complete)
		`{"type":"agent-tool-result","toolCallId":"toolu_01abc","toolName":"callFunction","toolType":"callFunction","state":"applied","input":{"function":"connections.search.search","args":{"queries":[{"question":"AI news March 2026","keywords":"AI news March"},{"question":"AI breakthroughs March","keywords":"AI breakthroughs"}],"includeWebResults":true}},"result":{"headerLabel":[["Searched: AI news March 2026"]],"cycleQueries":["AI news March 2026","AI breakthroughs March"],"output":` + string(searchOutputJSON) + `}}`,
		// Step 2: second inference with thinking + text
		`{"type":"agent-inference","id":"step2","value":[{"type":"thinking","content":"I have search results to summarize."}]}`,
		`{"type":"agent-inference","id":"step2","value":[{"type":"thinking","content":"I have search results to summarize."},{"type":"text","content":"Here are the March AI news highlights."}],"finishedAt":2,"inputTokens":20,"outputTokens":4}`,
	}, "\n")

	var thinking strings.Builder
	var text strings.Builder
	doneCount := 0

	err := parseNDJSONStream(bytes.NewBufferString(stream), "", func(delta string, done bool, usage *UsageInfo) {
		if delta != "" {
			text.WriteString(delta)
		}
	}, nil, nil, func(delta string, done bool, signature string) {
		if done {
			doneCount++
			return
		}
		if delta != "" {
			thinking.WriteString(delta)
		}
	}, nil, nil, nil)
	if err != nil {
		t.Fatalf("parseNDJSONStream returned error: %v", err)
	}

	thinkingText := thinking.String()

	// Verify model thinking is present
	if !strings.Contains(thinkingText, "The user wants AI news from March.") {
		t.Errorf("missing initial thinking, got %q", thinkingText)
	}
	if !strings.Contains(thinkingText, "I have search results to summarize.") {
		t.Errorf("missing second step thinking, got %q", thinkingText)
	}

	// Verify search progress is emitted
	if !strings.Contains(thinkingText, "**Web Search**: AI news March 2026") {
		t.Errorf("missing Web Search query line, got %q", thinkingText)
	}
	if !strings.Contains(thinkingText, "**Searching**: AI news March 2026") {
		t.Errorf("missing Searching status, got %q", thinkingText)
	}
	if !strings.Contains(thinkingText, "**Search Complete**: AI news March 2026") {
		t.Errorf("missing Search Complete status, got %q", thinkingText)
	}

	// Verify results summary
	if !strings.Contains(thinkingText, "**Found 3 Results**") {
		t.Errorf("missing results count, got %q", thinkingText)
	}
	if !strings.Contains(thinkingText, "AI Stories 2026") {
		t.Errorf("missing result title, got %q", thinkingText)
	}

	// Verify text output
	if text.String() != "Here are the March AI news highlights." {
		t.Errorf("unexpected text output: %q", text.String())
	}

	// Verify thinking was closed exactly once
	if doneCount != 1 {
		t.Errorf("expected 1 thinking_done, got %d", doneCount)
	}
}

// TestParseNDJSONStreamCallFunctionNonSearchIgnored verifies that non-search
// callFunction tools (e.g., fs.readFiles) do NOT emit search thinking.
func TestParseNDJSONStreamCallFunctionNonSearchIgnored(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"agent-tool-result","toolCallId":"toolu_fs","toolName":"callFunction","toolType":"callFunction","state":"applied","input":{"function":"connections.fs.readFiles","args":{"files":["README.md"]}},"result":{"output":"{\"files\":[{\"path\":\"README.md\",\"content\":\"hello\"}]}"}}`,
		`{"type":"agent-inference","id":"step1","value":[{"type":"text","content":"The file says hello."}],"finishedAt":1,"inputTokens":5,"outputTokens":2}`,
	}, "\n")

	var thinking strings.Builder
	var text strings.Builder

	err := parseNDJSONStream(bytes.NewBufferString(stream), "", func(delta string, done bool, usage *UsageInfo) {
		if delta != "" {
			text.WriteString(delta)
		}
	}, nil, nil, func(delta string, done bool, signature string) {
		if delta != "" {
			thinking.WriteString(delta)
		}
	}, nil, nil, nil)
	if err != nil {
		t.Fatalf("parseNDJSONStream returned error: %v", err)
	}

	// Non-search callFunction should NOT emit any search thinking
	thinkingText := thinking.String()
	if strings.Contains(thinkingText, "Web Search") || strings.Contains(thinkingText, "Searching") || strings.Contains(thinkingText, "Search Complete") {
		t.Errorf("non-search callFunction should not emit search thinking, got %q", thinkingText)
	}
	if text.String() != "The file says hello." {
		t.Errorf("unexpected text: %q", text.String())
	}
}

// TestParseNDJSONStreamCallFunctionSearchCycleQueriesFallback verifies that
// when input.args.queries is empty, cycleQueries from result are used instead.
func TestParseNDJSONStreamCallFunctionSearchCycleQueriesFallback(t *testing.T) {
	stream := strings.Join([]string{
		// agent-tool-result with cycleQueries but no input.args.queries
		`{"type":"agent-tool-result","toolCallId":"toolu_cq","toolName":"callFunction","toolType":"callFunction","state":"streaming","input":{"function":"connections.search.search"},"result":{"cycleQueries":["fallback query"]}}`,
		`{"type":"agent-tool-result","toolCallId":"toolu_cq","toolName":"callFunction","toolType":"callFunction","state":"applied","input":{"function":"connections.search.search"},"result":{"cycleQueries":["fallback query"],"output":"{\"results\":[]}"}}`,
		`{"type":"agent-inference","id":"step1","value":[{"type":"text","content":"No results."}],"finishedAt":1,"inputTokens":1,"outputTokens":1}`,
	}, "\n")

	var thinking strings.Builder

	err := parseNDJSONStream(bytes.NewBufferString(stream), "", func(delta string, done bool, usage *UsageInfo) {
	}, nil, nil, func(delta string, done bool, signature string) {
		if delta != "" {
			thinking.WriteString(delta)
		}
	}, nil, nil, nil)
	if err != nil {
		t.Fatalf("parseNDJSONStream returned error: %v", err)
	}

	thinkingText := thinking.String()
	if !strings.Contains(thinkingText, "**Web Search**: fallback query") {
		t.Errorf("expected cycleQueries fallback, got %q", thinkingText)
	}
	if !strings.Contains(thinkingText, "**Searching**: fallback query") {
		t.Errorf("expected Searching with fallback query, got %q", thinkingText)
	}
	if !strings.Contains(thinkingText, "**Search Complete**: fallback query") {
		t.Errorf("expected Search Complete with fallback query, got %q", thinkingText)
	}
}

// TestParseNDJSONStreamLegacySearchStillWorks ensures the old search tool format
// (name="search" with input containing query structure) still works correctly.
func TestParseNDJSONStreamLegacySearchStillWorks(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"agent-inference","id":"step1","value":[{"type":"thinking","content":"Searching now."},{"type":"tool_use","id":"toolu_old","name":"search","content":"{\"web\":{\"queries\":[\"legacy search query\"]}}"}],"finishedAt":1,"inputTokens":5,"outputTokens":2}`,
		`{"type":"agent-search-extracted-results","toolCallId":"toolu_old","results":[{"id":"webpage://?url=https%3A%2F%2Fexample.com%2Fold","title":"Old Result"}]}`,
		`{"type":"agent-inference","id":"step2","value":[{"type":"thinking","content":"Got results."},{"type":"text","content":"Here are the results."}],"finishedAt":2,"inputTokens":10,"outputTokens":3}`,
	}, "\n")

	var thinking strings.Builder

	err := parseNDJSONStream(bytes.NewBufferString(stream), "", func(delta string, done bool, usage *UsageInfo) {
	}, nil, nil, func(delta string, done bool, signature string) {
		if delta != "" {
			thinking.WriteString(delta)
		}
	}, nil, nil, nil)
	if err != nil {
		t.Fatalf("parseNDJSONStream returned error: %v", err)
	}

	thinkingText := thinking.String()
	if !strings.Contains(thinkingText, "**Web Search**: legacy search query") {
		t.Errorf("missing legacy search query, got %q", thinkingText)
	}
	if !strings.Contains(thinkingText, "**Searching**: legacy search query") {
		t.Errorf("missing legacy Searching status, got %q", thinkingText)
	}
	if !strings.Contains(thinkingText, "**Found 1 Results**") {
		t.Errorf("missing legacy results count, got %q", thinkingText)
	}
	if !strings.Contains(thinkingText, "Old Result") {
		t.Errorf("missing legacy result title, got %q", thinkingText)
	}
}
