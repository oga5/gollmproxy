package main

// --- Request ---

type OpenAIChatRequest struct {
	Model       string          `json:"model"`
	Messages    []OpenAIMessage `json:"messages"`
	Temperature *float64        `json:"temperature,omitempty"`
	TopP        *float64        `json:"top_p,omitempty"`
	MaxTokens   *int            `json:"max_tokens,omitempty"`
	Stream      bool            `json:"stream,omitempty"`
	Stop        []string        `json:"stop,omitempty"`
	N           *int            `json:"n,omitempty"`
	User        string          `json:"user,omitempty"`
	Metadata    map[string]any  `json:"metadata,omitempty"`
}

type OpenAIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// --- Non-streaming Response ---

type OpenAIChatResponse struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []OpenAIChoice `json:"choices"`
	Usage   *OpenAIUsage   `json:"usage,omitempty"`
}

type OpenAIChoice struct {
	Index        int            `json:"index"`
	Message      *OpenAIMessage `json:"message,omitempty"`
	FinishReason *string        `json:"finish_reason"`
}

type OpenAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// --- Streaming Response Chunk ---

type OpenAIChatStreamChunk struct {
	ID      string               `json:"id"`
	Object  string               `json:"object"`
	Created int64                `json:"created"`
	Model   string               `json:"model"`
	Choices []OpenAIStreamChoice `json:"choices"`
	Usage   *OpenAIUsage         `json:"usage,omitempty"`
}

type OpenAIStreamChoice struct {
	Index        int         `json:"index"`
	Delta        OpenAIDelta `json:"delta"`
	FinishReason *string     `json:"finish_reason"`
}

type OpenAIDelta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

// --- Embeddings Request ---

type OpenAIEmbeddingRequest struct {
	Input          any    `json:"input"` // string or []string
	Model          string `json:"model"`
	EncodingFormat string `json:"encoding_format,omitempty"` // "float" or "base64"
	User           string `json:"user,omitempty"`
	// Pooling specifies how to aggregate T×K SAE activations across the token (T) dimension.
	// Supported values: "sum", "mean", "max", "logsumexp" (default: "logsumexp" when upstream returns 2D).
	Pooling string `json:"pooling,omitempty"`
}

// upstreamEmbeddingResponse is used to parse upstream responses that may contain
// 2D embeddings (T×K token-level SAE activations) before applying pooling.
type upstreamEmbeddingResponse struct {
	Object string                  `json:"object"`
	Data   []upstreamEmbeddingItem `json:"data"`
	Model  string                  `json:"model"`
	Usage  OpenAIEmbeddingUsage    `json:"usage"`
}

type upstreamEmbeddingItem struct {
	Object    string `json:"object"`
	Embedding any    `json:"embedding"` // []float64 (1D) or [][]float64 (T×K SAE activations)
	Index     int    `json:"index"`
}

// --- Embeddings Response ---

type OpenAIEmbeddingResponse struct {
	Object string              `json:"object"` // "list"
	Data   []OpenAIEmbedding   `json:"data"`
	Model  string              `json:"model"`
	Usage  OpenAIEmbeddingUsage `json:"usage"`
}

type OpenAIEmbedding struct {
	Object    string    `json:"object"` // "embedding"
	Embedding []float64 `json:"embedding"`
	Index     int       `json:"index"`
}

type OpenAIEmbeddingUsage struct {
	PromptTokens int `json:"prompt_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// --- Error ---

type OpenAIErrorResponse struct {
	Error OpenAIError `json:"error"`
}

type OpenAIError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
}
