package main

// --- Request ---

type GeminiGenerateRequest struct {
	Contents          []GeminiContent         `json:"contents"`
	SystemInstruction *GeminiContent          `json:"systemInstruction,omitempty"`
	GenerationConfig  *GeminiGenerationConfig `json:"generationConfig,omitempty"`
}

type GeminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []GeminiPart `json:"parts"`
}

type GeminiPart struct {
	Text string `json:"text,omitempty"`
}

type GeminiGenerationConfig struct {
	Temperature     *float64 `json:"temperature,omitempty"`
	TopP            *float64 `json:"topP,omitempty"`
	TopK            *int     `json:"topK,omitempty"`
	MaxOutputTokens *int     `json:"maxOutputTokens,omitempty"`
	StopSequences   []string `json:"stopSequences,omitempty"`
	CandidateCount  *int     `json:"candidateCount,omitempty"`
}

// --- Embedding Request ---

type GeminiEmbedContentRequest struct {
	Model   string       `json:"model,omitempty"`
	Content GeminiContent `json:"content"`
}

type GeminiBatchEmbedContentsRequest struct {
	Requests []GeminiEmbedContentRequest `json:"requests"`
}

// --- Embedding Response ---

type GeminiEmbedContentResponse struct {
	Embedding *GeminiContentEmbedding `json:"embedding,omitempty"`
}

type GeminiBatchEmbedContentsResponse struct {
	Embeddings []GeminiContentEmbedding `json:"embeddings"`
}

type GeminiContentEmbedding struct {
	Values []float64 `json:"values"`
}

// --- Response ---

type GeminiGenerateResponse struct {
	Candidates    []GeminiCandidate    `json:"candidates"`
	UsageMetadata *GeminiUsageMetadata `json:"usageMetadata,omitempty"`
	ModelVersion  string               `json:"modelVersion,omitempty"`
}

type GeminiCandidate struct {
	Content      GeminiContent `json:"content"`
	FinishReason string        `json:"finishReason,omitempty"`
}

type GeminiUsageMetadata struct {
	PromptTokenCount     int `json:"promptTokenCount"`
	CandidatesTokenCount int `json:"candidatesTokenCount"`
	TotalTokenCount      int `json:"totalTokenCount"`
}
