package proxy

import (
	"encoding/json"
	"testing"
)

func TestNotionResponseLogDeduperAgentInference(t *testing.T) {
	deduper := newNotionResponseLogDeduper("req-1", "runInferenceTranscript ndjson deduped")

	first := deduper.DedupLine(`{"id":"step-1","type":"agent-inference","value":[{"type":"thinking","content":"plan"},{"type":"text","content":"hello"},{"type":"tool_use","id":"tool-1","name":"search","input":{"query":"golang"}}]}`)
	firstPayload := decodeJSONMap(t, first)
	firstValues := decodeValueSlice(t, firstPayload["value"])
	if len(firstValues) != 3 {
		t.Fatalf("expected 3 entries in first payload, got %d", len(firstValues))
	}
	if firstValues[0]["content"] != "plan" {
		t.Fatalf("expected first thinking content to be logged in full, got %#v", firstValues[0]["content"])
	}
	if firstValues[1]["content"] != "hello" {
		t.Fatalf("expected first text content to be logged in full, got %#v", firstValues[1]["content"])
	}
	if firstValues[2]["id"] != "tool-1" {
		t.Fatalf("expected first tool_use to be logged once, got %#v", firstValues[2]["id"])
	}

	second := deduper.DedupLine(`{"id":"step-1","type":"agent-inference","value":[{"type":"thinking","content":"plan more","signature":"sig-1"},{"type":"text","content":"hello world"},{"type":"tool_use","id":"tool-1","name":"search","input":{"query":"golang"}}],"finishedAt":1,"inputTokens":10,"outputTokens":20}`)
	secondPayload := decodeJSONMap(t, second)
	secondValues := decodeValueSlice(t, secondPayload["value"])
	if len(secondValues) != 2 {
		t.Fatalf("expected only changed thinking/text entries in second payload, got %d", len(secondValues))
	}
	if secondValues[0]["content"] != " more" {
		t.Fatalf("expected thinking delta in second payload, got %#v", secondValues[0]["content"])
	}
	if secondValues[0]["signature"] != "sig-1" {
		t.Fatalf("expected updated signature in second payload, got %#v", secondValues[0]["signature"])
	}
	if secondValues[1]["content"] != " world" {
		t.Fatalf("expected text delta in second payload, got %#v", secondValues[1]["content"])
	}
	if secondPayload["finishedAt"] != float64(1) {
		t.Fatalf("expected finishedAt to be preserved, got %#v", secondPayload["finishedAt"])
	}
	if secondPayload["inputTokens"] != float64(10) || secondPayload["outputTokens"] != float64(20) {
		t.Fatalf("expected token usage to be preserved, got input=%#v output=%#v", secondPayload["inputTokens"], secondPayload["outputTokens"])
	}

	if third := deduper.DedupLine(`{"id":"step-1","type":"agent-inference","value":[{"type":"thinking","content":"plan more","signature":"sig-1"},{"type":"text","content":"hello world"},{"type":"tool_use","id":"tool-1","name":"search","input":{"query":"golang"}}],"finishedAt":1,"inputTokens":10,"outputTokens":20}`); third != nil {
		t.Fatalf("expected identical cumulative event to be fully deduped, got %s", string(third))
	}
}

func TestNotionResponseLogDeduperResearcherEvents(t *testing.T) {
	deduper := newNotionResponseLogDeduper("req-2", "runInferenceTranscript researcher ndjson deduped")

	firstSteps := deduper.DedupLine(`{"id":"step-r1","type":"researcher-next-steps","value":{"nextSteps":[{"agent":"search","question":"q","key":"s1","searchType":"web","displayName":"Find docs"}],"userQuestion":"question"},"output":[{"type":"thinking","content":"plan"}],"rawOutput":[{"type":"thinking","content":"full plan","signature":"sig-a","modelProvider":"anthropic","notionModelName":"anthropic-sonnet-4"}]}`)
	firstPayload := decodeJSONMap(t, firstSteps)
	if _, ok := firstPayload["value"]; !ok {
		t.Fatalf("expected first researcher-next-steps payload to include value")
	}
	firstOutput := decodeValueSlice(t, firstPayload["output"])
	if len(firstOutput) != 1 || firstOutput[0]["content"] != "plan" {
		t.Fatalf("expected first condensed thinking to be logged in full, got %#v", firstPayload["output"])
	}
	firstRaw := decodeValueSlice(t, firstPayload["rawOutput"])
	if len(firstRaw) != 1 || firstRaw[0]["content"] != "full plan" {
		t.Fatalf("expected first rawOutput to be logged in full, got %#v", firstPayload["rawOutput"])
	}

	secondSteps := deduper.DedupLine(`{"id":"step-r1","type":"researcher-next-steps","done":true,"value":{"nextSteps":[{"agent":"search","question":"q","key":"s1","searchType":"web","displayName":"Find docs"}],"userQuestion":"question"},"output":[{"type":"thinking","content":"plan more"}],"rawOutput":[{"type":"thinking","content":"full plan more","signature":"sig-b","modelProvider":"anthropic","notionModelName":"anthropic-sonnet-4"}]}`)
	secondPayload := decodeJSONMap(t, secondSteps)
	if _, ok := secondPayload["value"]; ok {
		t.Fatalf("expected unchanged nextSteps block to be omitted from second payload")
	}
	if secondPayload["done"] != true {
		t.Fatalf("expected done=true in second payload, got %#v", secondPayload["done"])
	}
	secondOutput := decodeValueSlice(t, secondPayload["output"])
	if len(secondOutput) != 1 || secondOutput[0]["content"] != " more" {
		t.Fatalf("expected condensed thinking delta in second payload, got %#v", secondPayload["output"])
	}
	secondRaw := decodeValueSlice(t, secondPayload["rawOutput"])
	if len(secondRaw) != 1 || secondRaw[0]["content"] != " more" || secondRaw[0]["signature"] != "sig-b" {
		t.Fatalf("expected rawOutput delta plus updated signature in second payload, got %#v", secondPayload["rawOutput"])
	}

	firstReport := deduper.DedupLine(`{"id":"report-1","type":"researcher-report","value":"hello"}`)
	firstReportPayload := decodeJSONMap(t, firstReport)
	if firstReportPayload["value"] != "hello" {
		t.Fatalf("expected first researcher report chunk in full, got %#v", firstReportPayload["value"])
	}

	secondReport := deduper.DedupLine(`{"id":"report-1","type":"researcher-report","value":"hello world"}`)
	secondReportPayload := decodeJSONMap(t, secondReport)
	if secondReportPayload["value"] != " world" {
		t.Fatalf("expected cumulative researcher report delta, got %#v", secondReportPayload["value"])
	}
}

func decodeJSONMap(t *testing.T, raw []byte) map[string]interface{} {
	t.Helper()
	if len(raw) == 0 {
		t.Fatal("expected non-empty JSON payload")
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	return payload
}

func decodeValueSlice(t *testing.T, raw interface{}) []map[string]interface{} {
	t.Helper()
	items, ok := raw.([]interface{})
	if !ok {
		t.Fatalf("expected []interface{}, got %T", raw)
	}
	out := make([]map[string]interface{}, 0, len(items))
	for _, item := range items {
		entry, ok := item.(map[string]interface{})
		if !ok {
			t.Fatalf("expected map[string]interface{}, got %T", item)
		}
		out = append(out, entry)
	}
	return out
}
