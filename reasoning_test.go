package main

import (
	"encoding/json"
	"testing"
)

func TestStripReasoning(t *testing.T) {
	input := "prefix<reasoning>hidden</reasoning>suffix"
	got := stripReasoning(input)
	if got != "prefixsuffix" {
		t.Fatalf("unexpected stripped content: %q", got)
	}
}

func TestReasoningStripperAcrossChunks(t *testing.T) {
	stripper := NewReasoningStripper()
	part1 := stripper.FilterChunk("<reason")
	part2 := stripper.FilterChunk("ing>hidden</reason")
	part3 := stripper.FilterChunk("ing>visible")
	tail := stripper.Flush()

	got := part1 + part2 + part3 + tail
	if got != "visible" {
		t.Fatalf("unexpected streaming strip result: %q", got)
	}
}

func TestSanitizeBedrockStreamChunkPreservesUnknownFields(t *testing.T) {
	filter := NewReasoningStripper()
	chunk := []byte(`{"choices":[{"delta":{"content":"<reasoning>hidden</reasoning>VISIBLE"},"index":0,"obfuscation":"keep-me"}],"id":"x","object":"chat.completion.chunk"}`)
	result := sanitizeBedrockStreamChunk(chunk, filter)
	out := result.Body

	var raw map[string]any
	if err := json.Unmarshal(out, &raw); err != nil {
		t.Fatalf("failed to unmarshal sanitized chunk: %v", err)
	}
	choices, ok := raw["choices"].([]any)
	if !ok || len(choices) != 1 {
		t.Fatalf("unexpected choices: %#v", raw["choices"])
	}
	choice := choices[0].(map[string]any)
	if choice["obfuscation"] != "keep-me" {
		t.Fatalf("obfuscation field was lost: %#v", choice)
	}
	delta := choice["delta"].(map[string]any)
	if delta["content"] != "VISIBLE" {
		t.Fatalf("unexpected sanitized content: %#v", delta["content"])
	}
	if result.Skip {
		t.Fatal("sanitized chunk should not be skipped")
	}
}

func TestSanitizeBedrockStreamChunkSkipsReasoningOnlyChunk(t *testing.T) {
	filter := NewReasoningStripper()
	chunk := []byte(`{"choices":[{"delta":{"content":"<reasoning>hidden</reasoning>"},"index":0}],"id":"x","object":"chat.completion.chunk"}`)
	result := sanitizeBedrockStreamChunk(chunk, filter)
	if !result.Skip {
		t.Fatal("reasoning-only chunk should be skipped")
	}
}

func TestSanitizeBedrockStreamChunkExtractsUsage(t *testing.T) {
	chunk := []byte(`{"choices":[{"delta":{"content":"VISIBLE"},"finish_reason":"stop","index":0}],"id":"x","object":"chat.completion.chunk","created":123,"model":"m","amazon-bedrock-invocationMetrics":{"inputTokenCount":10,"outputTokenCount":4}}`)
	result := sanitizeBedrockStreamChunk(chunk, nil)
	if result.Usage == nil {
		t.Fatal("expected usage to be extracted")
	}
	if result.Usage.PromptTokens != 10 || result.Usage.CompletionTokens != 4 || result.Usage.TotalTokens != 14 {
		t.Fatalf("unexpected usage: %#v", result.Usage)
	}

	var raw map[string]any
	if err := json.Unmarshal(result.Body, &raw); err != nil {
		t.Fatalf("failed to unmarshal sanitized chunk: %v", err)
	}
	if _, ok := raw["amazon-bedrock-invocationMetrics"]; ok {
		t.Fatal("provider-specific metrics should be removed from emitted chunk")
	}

	usageChunk := buildUsageStreamChunk(result.Body, result.Usage)
	if len(usageChunk) == 0 {
		t.Fatal("expected usage chunk to be built")
	}
	if err := json.Unmarshal(usageChunk, &raw); err != nil {
		t.Fatalf("failed to unmarshal usage chunk: %v", err)
	}
	choices, ok := raw["choices"].([]any)
	if !ok || len(choices) != 0 {
		t.Fatalf("unexpected usage chunk choices: %#v", raw["choices"])
	}
	usage, ok := raw["usage"].(map[string]any)
	if !ok {
		t.Fatal("usage chunk is missing usage field")
	}
	if usage["total_tokens"] != float64(14) {
		t.Fatalf("unexpected usage payload: %#v", usage)
	}
}
