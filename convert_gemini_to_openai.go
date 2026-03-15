package main

import (
	"strings"
	"time"
)

// geminiFinishReasonMap maps Gemini finish reasons to OpenAI finish reasons.
var geminiFinishReasonMap = map[string]string{
	"STOP":          "stop",
	"MAX_TOKENS":    "length",
	"SAFETY":        "content_filter",
	"RECITATION":    "content_filter",
	"OTHER":         "stop",
	"FINISH_REASON_UNSPECIFIED": "stop",
}

// geminiToOpenAIResponse converts a Gemini response to an OpenAI chat completion response.
func geminiToOpenAIResponse(gemResp *GeminiGenerateResponse, model, requestID string) *OpenAIChatResponse {
	resp := &OpenAIChatResponse{
		ID:      "chatcmpl-" + requestID,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
	}

	for i, candidate := range gemResp.Candidates {
		// Concatenate all text parts
		var textParts []string
		for _, part := range candidate.Content.Parts {
			if part.Text != "" {
				textParts = append(textParts, part.Text)
			}
		}
		content := strings.Join(textParts, "")

		finishReason := mapGeminiFinishReason(candidate.FinishReason)

		resp.Choices = append(resp.Choices, OpenAIChoice{
			Index: i,
			Message: &OpenAIMessage{
				Role:    "assistant",
				Content: content,
			},
			FinishReason: &finishReason,
		})
	}

	if gemResp.UsageMetadata != nil {
		resp.Usage = &OpenAIUsage{
			PromptTokens:     gemResp.UsageMetadata.PromptTokenCount,
			CompletionTokens: gemResp.UsageMetadata.CandidatesTokenCount,
			TotalTokens:      gemResp.UsageMetadata.TotalTokenCount,
		}
	}

	return resp
}

// geminiStreamChunkToOpenAI converts a Gemini streaming chunk to an OpenAI streaming chunk.
func geminiStreamChunkToOpenAI(gemResp *GeminiGenerateResponse, model, requestID string, isFirst bool) *OpenAIChatStreamChunk {
	chunk := &OpenAIChatStreamChunk{
		ID:      "chatcmpl-" + requestID,
		Object:  "chat.completion.chunk",
		Created: time.Now().Unix(),
		Model:   model,
	}

	if len(gemResp.Candidates) == 0 {
		return chunk
	}

	candidate := gemResp.Candidates[0]

	// Extract text from parts
	var textParts []string
	for _, part := range candidate.Content.Parts {
		if part.Text != "" {
			textParts = append(textParts, part.Text)
		}
	}
	content := strings.Join(textParts, "")

	delta := OpenAIDelta{}
	if isFirst {
		delta.Role = "assistant"
	}
	delta.Content = content

	var finishReason *string
	if candidate.FinishReason != "" && candidate.FinishReason != "FINISH_REASON_UNSPECIFIED" {
		fr := mapGeminiFinishReason(candidate.FinishReason)
		finishReason = &fr
	}

	chunk.Choices = []OpenAIStreamChoice{
		{
			Index:        0,
			Delta:        delta,
			FinishReason: finishReason,
		},
	}

	// Include usage in the final chunk if available
	if gemResp.UsageMetadata != nil {
		chunk.Usage = &OpenAIUsage{
			PromptTokens:     gemResp.UsageMetadata.PromptTokenCount,
			CompletionTokens: gemResp.UsageMetadata.CandidatesTokenCount,
			TotalTokens:      gemResp.UsageMetadata.TotalTokenCount,
		}
	}

	return chunk
}

func mapGeminiFinishReason(reason string) string {
	if mapped, ok := geminiFinishReasonMap[reason]; ok {
		return mapped
	}
	return "stop"
}
