package main

import (
	"encoding/json"
	"strings"
)

type BedrockStreamChunkResult struct {
	Body  []byte
	Skip  bool
	Usage *OpenAIUsage
}

const (
	reasoningStartTag = "<reasoning>"
	reasoningEndTag   = "</reasoning>"
)

type ReasoningStripper struct {
	pending     string
	inReasoning bool
}

func NewReasoningStripper() *ReasoningStripper {
	return &ReasoningStripper{}
}

func (s *ReasoningStripper) FilterChunk(input string) string {
	s.pending += input
	var out strings.Builder

	for {
		if s.inReasoning {
			idx := strings.Index(s.pending, reasoningEndTag)
			if idx >= 0 {
				s.pending = s.pending[idx+len(reasoningEndTag):]
				s.inReasoning = false
				continue
			}
			keep := longestSuffixPrefix(s.pending, reasoningEndTag)
			if keep > 0 {
				s.pending = s.pending[len(s.pending)-keep:]
			} else {
				s.pending = ""
			}
			break
		}

		idx := strings.Index(s.pending, reasoningStartTag)
		if idx >= 0 {
			out.WriteString(s.pending[:idx])
			s.pending = s.pending[idx+len(reasoningStartTag):]
			s.inReasoning = true
			continue
		}

		keep := longestSuffixPrefix(s.pending, reasoningStartTag)
		if keep > 0 {
			out.WriteString(s.pending[:len(s.pending)-keep])
			s.pending = s.pending[len(s.pending)-keep:]
		} else {
			out.WriteString(s.pending)
			s.pending = ""
		}
		break
	}

	return out.String()
}

func (s *ReasoningStripper) Flush() string {
	if s.inReasoning {
		s.pending = ""
		return ""
	}
	out := s.pending
	s.pending = ""
	return out
}

func stripReasoning(content string) string {
	stripper := NewReasoningStripper()
	result := stripper.FilterChunk(content)
	result += stripper.Flush()
	return result
}

func sanitizeBedrockResponseBody(body []byte, includeReasoning bool) []byte {
	if includeReasoning {
		return body
	}
	return rewriteJSONContent(body, func(content string) string {
		return stripReasoning(content)
	}, false)
}

func sanitizeBedrockStreamChunk(body []byte, filter *ReasoningStripper) BedrockStreamChunkResult {
	result := BedrockStreamChunkResult{Body: body}

	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return result
	}

	result.Usage = extractBedrockInvocationUsage(raw)
	delete(raw, "amazon-bedrock-invocationMetrics")

	if filter != nil {
		rewriteJSONContentMap(raw, filter.FilterChunk, true)
	}

	rewritten, err := json.Marshal(raw)
	if err != nil {
		return result
	}

	result.Body = rewritten
	result.Skip = streamChunkHasNoVisiblePayload(raw)
	return result
}

func rewriteJSONContent(body []byte, rewrite func(string) string, stream bool) []byte {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return body
	}

	rewriteJSONContentMap(raw, rewrite, stream)

	rewritten, err := json.Marshal(raw)
	if err != nil {
		return body
	}
	return rewritten
}

func rewriteJSONContentMap(raw map[string]any, rewrite func(string) string, stream bool) {
	choices, ok := raw["choices"].([]any)
	if !ok {
		return
	}

	for _, choiceValue := range choices {
		choice, ok := choiceValue.(map[string]any)
		if !ok {
			continue
		}
		key := "message"
		if stream {
			key = "delta"
		}
		message, ok := choice[key].(map[string]any)
		if !ok {
			continue
		}
		content, ok := message["content"].(string)
		if !ok {
			continue
		}
		message["content"] = rewrite(content)
	}
	raw["choices"] = choices
}

func extractBedrockInvocationUsage(raw map[string]any) *OpenAIUsage {
	metrics, ok := raw["amazon-bedrock-invocationMetrics"].(map[string]any)
	if !ok {
		return nil
	}
	promptTokens, okPrompt := jsonNumberToInt(metrics["inputTokenCount"])
	completionTokens, okCompletion := jsonNumberToInt(metrics["outputTokenCount"])
	if !okPrompt || !okCompletion {
		return nil
	}
	return &OpenAIUsage{
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		TotalTokens:      promptTokens + completionTokens,
	}
}

func buildUsageStreamChunk(body []byte, usage *OpenAIUsage) []byte {
	if usage == nil {
		return nil
	}

	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil
	}

	usageChunk := map[string]any{
		"object":  "chat.completion.chunk",
		"choices": []any{},
		"usage": map[string]any{
			"prompt_tokens":     usage.PromptTokens,
			"completion_tokens": usage.CompletionTokens,
			"total_tokens":      usage.TotalTokens,
		},
	}
	for _, key := range []string{"id", "created", "model", "object"} {
		if value, ok := raw[key]; ok {
			usageChunk[key] = value
		}
	}

	out, err := json.Marshal(usageChunk)
	if err != nil {
		return nil
	}
	return out
}

func streamChunkHasNoVisiblePayload(raw map[string]any) bool {
	choices, ok := raw["choices"].([]any)
	if !ok || len(choices) == 0 {
		return true
	}

	for _, choiceValue := range choices {
		choice, ok := choiceValue.(map[string]any)
		if !ok {
			continue
		}
		if finishReason, ok := choice["finish_reason"]; ok && finishReason != nil {
			return false
		}
		delta, ok := choice["delta"].(map[string]any)
		if !ok {
			return false
		}
		if role, ok := delta["role"].(string); ok && role != "" {
			return false
		}
		if content, ok := delta["content"].(string); ok && content != "" {
			return false
		}
	}

	return true
}

func jsonNumberToInt(value any) (int, bool) {
	switch typed := value.(type) {
	case float64:
		return int(typed), true
	case int:
		return typed, true
	case int64:
		return int(typed), true
	default:
		return 0, false
	}
}

func longestSuffixPrefix(value, prefix string) int {
	max := len(value)
	if len(prefix)-1 < max {
		max = len(prefix) - 1
	}
	for i := max; i > 0; i-- {
		if strings.HasSuffix(value, prefix[:i]) {
			return i
		}
	}
	return 0
}
