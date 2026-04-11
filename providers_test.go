package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// --- Test helpers shared by provider tests ---

// capturedUpstreamRequest records what the proxy forwarded to the fake upstream server.
type capturedUpstreamRequest struct {
	Method   string
	Path     string
	RawQuery string
	Headers  http.Header
	Body     []byte
}

// newCaptureUpstream returns an httptest.Server that records incoming requests
// and replies with the given status / body. Registered for cleanup on t.
func newCaptureUpstream(t *testing.T, status int, respContentType, respBody string) (*httptest.Server, *capturedUpstreamRequest) {
	t.Helper()
	captured := &capturedUpstreamRequest{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		captured.Method = r.Method
		captured.Path = r.URL.Path
		captured.RawQuery = r.URL.RawQuery
		captured.Headers = r.Header.Clone()
		captured.Body = body

		if respContentType != "" {
			w.Header().Set("Content-Type", respContentType)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(respBody))
	}))
	t.Cleanup(server.Close)
	return server, captured
}

// newTestLogger creates a RequestLogger backed by a tempfile scoped to the test.
func newTestLogger(t *testing.T) *RequestLogger {
	t.Helper()
	path := filepath.Join(t.TempDir(), "request.log")
	logger, err := NewRequestLogger(path)
	if err != nil {
		t.Fatalf("failed to create test logger: %v", err)
	}
	t.Cleanup(func() { _ = logger.Close() })
	return logger
}

// newHandlerWithConfig builds the proxy handler for the given config using a tempfile logger.
func newHandlerWithConfig(t *testing.T, cfg *Config) http.Handler {
	t.Helper()
	logger := newTestLogger(t)
	return NewServer(cfg, logger)
}

// postJSON marshals body and sends a POST request through handler. Uses httptest.NewRecorder.
func postJSON(t *testing.T, handler http.Handler, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("failed to marshal request body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	return rr
}

// --- OpenAI provider tests ---

func TestChatCompletionsOpenAIForwardsRequestAndStripsModelPrefix(t *testing.T) {
	upstreamResp := `{"id":"chatcmpl-xxx","object":"chat.completion","created":1,"model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`
	upstream, captured := newCaptureUpstream(t, http.StatusOK, "application/json", upstreamResp)

	cfg := &Config{
		ModelAliases: map[string]string{"gpt-4o": "openai/gpt-4o"},
		ModelConfigs: map[string]ModelConfig{
			"gpt-4o": {APIKey: "test-openai-key", APIBase: upstream.URL},
		},
	}
	handler := newHandlerWithConfig(t, cfg)

	reqBody := map[string]any{
		"model": "gpt-4o",
		"messages": []map[string]string{
			{"role": "system", "content": "be terse"},
			{"role": "user", "content": "hello"},
		},
	}
	rr := postJSON(t, handler, "/v1/chat/completions", reqBody)

	if rr.Code != http.StatusOK {
		t.Fatalf("unexpected status %d body=%s", rr.Code, rr.Body.String())
	}

	if captured.Method != http.MethodPost {
		t.Fatalf("unexpected upstream method: %s", captured.Method)
	}
	if captured.Path != "/v1/chat/completions" {
		t.Fatalf("unexpected upstream path: %q", captured.Path)
	}
	if got := captured.Headers.Get("Authorization"); got != "Bearer test-openai-key" {
		t.Fatalf("unexpected Authorization header: %q", got)
	}
	if got := captured.Headers.Get("Content-Type"); got != "application/json" {
		t.Fatalf("unexpected Content-Type: %q", got)
	}

	var forwarded map[string]any
	if err := json.Unmarshal(captured.Body, &forwarded); err != nil {
		t.Fatalf("failed to unmarshal forwarded body: %v", err)
	}
	if forwarded["model"] != "gpt-4o" {
		t.Fatalf("expected forwarded model 'gpt-4o', got %v", forwarded["model"])
	}
	msgs, ok := forwarded["messages"].([]any)
	if !ok || len(msgs) != 2 {
		t.Fatalf("expected 2 forwarded messages, got %#v", forwarded["messages"])
	}

	// Upstream response should pass through verbatim.
	var openaiResp OpenAIChatResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &openaiResp); err != nil {
		t.Fatalf("failed to unmarshal OpenAI response: %v", err)
	}
	if len(openaiResp.Choices) != 1 || openaiResp.Choices[0].Message == nil ||
		openaiResp.Choices[0].Message.Content != "hi" {
		t.Fatalf("unexpected response choices: %+v", openaiResp.Choices)
	}
}

func TestChatCompletionsOpenAIMissingAPIKeyReturnsError(t *testing.T) {
	cfg := &Config{
		ModelAliases: map[string]string{"gpt-4o": "openai/gpt-4o"},
	}
	handler := newHandlerWithConfig(t, cfg)

	rr := postJSON(t, handler, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4o",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "OPENAI_API_KEY") {
		t.Fatalf("expected error message to mention missing key, got %s", rr.Body.String())
	}
}

func TestChatCompletionsUnknownProviderReturns400(t *testing.T) {
	cfg := &Config{
		ModelAliases: map[string]string{"foo": "bogus/foo"},
	}
	handler := newHandlerWithConfig(t, cfg)

	rr := postJSON(t, handler, "/v1/chat/completions", map[string]any{
		"model":    "foo",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestChatCompletionsInvalidModelNameRejected(t *testing.T) {
	cfg := &Config{
		// Directly supply a provider-prefixed model with illegal characters via alias.
		ModelAliases: map[string]string{"bad": "openai/has space"},
	}
	handler := newHandlerWithConfig(t, cfg)

	rr := postJSON(t, handler, "/v1/chat/completions", map[string]any{
		"model":    "bad",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", rr.Code, rr.Body.String())
	}
}

// --- OpenRouter provider tests ---

func TestChatCompletionsOpenRouterForwardsRequestWithBearerKey(t *testing.T) {
	upstreamResp := `{"id":"x","object":"chat.completion","created":1,"model":"stepfun/step-3.5-flash:free","choices":[],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}`
	upstream, captured := newCaptureUpstream(t, http.StatusOK, "application/json", upstreamResp)

	cfg := &Config{
		ModelAliases: map[string]string{
			"step-flash": "openrouter/stepfun/step-3.5-flash:free",
		},
		ModelConfigs: map[string]ModelConfig{
			"step-flash": {APIKey: "or-key", APIBase: upstream.URL},
		},
	}
	handler := newHandlerWithConfig(t, cfg)

	rr := postJSON(t, handler, "/v1/chat/completions", map[string]any{
		"model":    "step-flash",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})

	if rr.Code != http.StatusOK {
		t.Fatalf("unexpected status %d body=%s", rr.Code, rr.Body.String())
	}
	if captured.Path != "/v1/chat/completions" {
		t.Fatalf("unexpected upstream path: %q", captured.Path)
	}
	if got := captured.Headers.Get("Authorization"); got != "Bearer or-key" {
		t.Fatalf("unexpected Authorization header: %q", got)
	}

	var forwarded map[string]any
	if err := json.Unmarshal(captured.Body, &forwarded); err != nil {
		t.Fatalf("failed to unmarshal forwarded body: %v", err)
	}
	// OpenRouter keeps multi-segment model names (prefix stripped only at the first slash).
	if forwarded["model"] != "stepfun/step-3.5-flash:free" {
		t.Fatalf("unexpected forwarded model: %v", forwarded["model"])
	}
}

// --- Ollama provider tests ---

func TestChatCompletionsOllamaChatDefaultsWithoutAuthHeader(t *testing.T) {
	upstreamResp := `{"id":"x","object":"chat.completion","created":1,"model":"llama3","choices":[]}`
	upstream, captured := newCaptureUpstream(t, http.StatusOK, "application/json", upstreamResp)

	cfg := &Config{
		ModelAliases: map[string]string{"llama3": "ollama_chat/llama3"},
		ModelConfigs: map[string]ModelConfig{
			"llama3": {APIBase: upstream.URL},
		},
	}
	handler := newHandlerWithConfig(t, cfg)

	rr := postJSON(t, handler, "/v1/chat/completions", map[string]any{
		"model":    "llama3",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("unexpected status %d body=%s", rr.Code, rr.Body.String())
	}
	if captured.Path != "/v1/chat/completions" {
		t.Fatalf("unexpected upstream path: %q", captured.Path)
	}
	if got := captured.Headers.Get("Authorization"); got != "" {
		t.Fatalf("ollama should not send Authorization without api_key, got %q", got)
	}

	var forwarded map[string]any
	if err := json.Unmarshal(captured.Body, &forwarded); err != nil {
		t.Fatalf("failed to unmarshal forwarded body: %v", err)
	}
	if forwarded["model"] != "llama3" {
		t.Fatalf("expected forwarded model 'llama3', got %v", forwarded["model"])
	}
}

// --- Gemini provider tests ---

func TestChatCompletionsGeminiConvertsOpenAIRequestToGeminiFormat(t *testing.T) {
	geminiResp := `{"candidates":[{"content":{"parts":[{"text":"hello"}],"role":"model"},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":1,"totalTokenCount":4}}`
	upstream, captured := newCaptureUpstream(t, http.StatusOK, "application/json", geminiResp)

	cfg := &Config{
		ModelAliases: map[string]string{"gemini-flash": "gemini/gemini-2.5-flash"},
		ModelConfigs: map[string]ModelConfig{
			"gemini-flash": {APIKey: "gem-key", APIBase: upstream.URL},
		},
	}
	handler := newHandlerWithConfig(t, cfg)

	temperature := 0.7
	maxTokens := 100
	rr := postJSON(t, handler, "/v1/chat/completions", map[string]any{
		"model": "gemini-flash",
		"messages": []map[string]string{
			{"role": "system", "content": "be terse"},
			{"role": "user", "content": "hi"},
			{"role": "assistant", "content": "hello"},
			{"role": "user", "content": "again"},
		},
		"temperature": temperature,
		"max_tokens":  maxTokens,
	})

	if rr.Code != http.StatusOK {
		t.Fatalf("unexpected status %d body=%s", rr.Code, rr.Body.String())
	}

	wantPath := "/v1beta/models/gemini-2.5-flash:generateContent"
	if captured.Path != wantPath {
		t.Fatalf("unexpected upstream path: %q want %q", captured.Path, wantPath)
	}
	// Non-streaming: no alt=sse
	if strings.Contains(captured.RawQuery, "alt=sse") {
		t.Fatalf("non-streaming request should not include alt=sse, got %q", captured.RawQuery)
	}
	if got := captured.Headers.Get("x-goog-api-key"); got != "gem-key" {
		t.Fatalf("unexpected x-goog-api-key: %q", got)
	}
	// API key must not leak into the URL.
	if strings.Contains(captured.RawQuery, "gem-key") {
		t.Fatalf("api key leaked into URL query: %q", captured.RawQuery)
	}

	var gemBody GeminiGenerateRequest
	if err := json.Unmarshal(captured.Body, &gemBody); err != nil {
		t.Fatalf("failed to unmarshal gemini body: %v body=%s", err, captured.Body)
	}

	if gemBody.SystemInstruction == nil || len(gemBody.SystemInstruction.Parts) != 1 ||
		gemBody.SystemInstruction.Parts[0].Text != "be terse" {
		t.Fatalf("systemInstruction not set: %+v", gemBody.SystemInstruction)
	}
	if len(gemBody.Contents) != 3 {
		t.Fatalf("expected 3 contents (non-system messages), got %d: %+v", len(gemBody.Contents), gemBody.Contents)
	}
	wantRoles := []string{"user", "model", "user"}
	for i, c := range gemBody.Contents {
		if c.Role != wantRoles[i] {
			t.Fatalf("content[%d] role = %q want %q", i, c.Role, wantRoles[i])
		}
	}
	if gemBody.GenerationConfig == nil {
		t.Fatalf("generationConfig missing")
	}
	if gemBody.GenerationConfig.Temperature == nil || *gemBody.GenerationConfig.Temperature != 0.7 {
		t.Fatalf("temperature mismatch: %+v", gemBody.GenerationConfig.Temperature)
	}
	if gemBody.GenerationConfig.MaxOutputTokens == nil || *gemBody.GenerationConfig.MaxOutputTokens != 100 {
		t.Fatalf("maxOutputTokens mismatch: %+v", gemBody.GenerationConfig.MaxOutputTokens)
	}

	var openaiResp OpenAIChatResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &openaiResp); err != nil {
		t.Fatalf("failed to unmarshal OpenAI response: %v body=%s", err, rr.Body.String())
	}
	if len(openaiResp.Choices) != 1 || openaiResp.Choices[0].Message == nil ||
		openaiResp.Choices[0].Message.Content != "hello" {
		t.Fatalf("unexpected converted choices: %+v", openaiResp.Choices)
	}
	if openaiResp.Choices[0].FinishReason == nil || *openaiResp.Choices[0].FinishReason != "stop" {
		t.Fatalf("unexpected finish_reason: %+v", openaiResp.Choices[0].FinishReason)
	}
	if openaiResp.Usage == nil || openaiResp.Usage.PromptTokens != 3 ||
		openaiResp.Usage.CompletionTokens != 1 || openaiResp.Usage.TotalTokens != 4 {
		t.Fatalf("unexpected converted usage: %+v", openaiResp.Usage)
	}
}

func TestChatCompletionsGeminiStreamingUsesStreamEndpoint(t *testing.T) {
	// Return a single Gemini SSE line, followed by an end of stream.
	geminiSSE := "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hi\"}],\"role\":\"model\"},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":1,\"candidatesTokenCount\":1,\"totalTokenCount\":2}}\n\n"
	upstream, captured := newCaptureUpstream(t, http.StatusOK, "text/event-stream", geminiSSE)

	cfg := &Config{
		ModelAliases: map[string]string{"gemini-flash": "gemini/gemini-2.5-flash"},
		ModelConfigs: map[string]ModelConfig{
			"gemini-flash": {APIKey: "gem-key", APIBase: upstream.URL},
		},
	}
	handler := newHandlerWithConfig(t, cfg)

	rr := postJSON(t, handler, "/v1/chat/completions", map[string]any{
		"model":    "gemini-flash",
		"stream":   true,
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})

	if rr.Code != http.StatusOK {
		t.Fatalf("unexpected status %d body=%s", rr.Code, rr.Body.String())
	}
	wantPath := "/v1beta/models/gemini-2.5-flash:streamGenerateContent"
	if captured.Path != wantPath {
		t.Fatalf("unexpected upstream path: %q want %q", captured.Path, wantPath)
	}
	if !strings.Contains(captured.RawQuery, "alt=sse") {
		t.Fatalf("expected alt=sse in streaming query, got %q", captured.RawQuery)
	}
	// Ensure we wrote SSE out and ended with [DONE] sentinel.
	if !strings.Contains(rr.Body.String(), "data: ") {
		t.Fatalf("expected SSE data lines in response, got %q", rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "[DONE]") {
		t.Fatalf("expected [DONE] sentinel in response, got %q", rr.Body.String())
	}
}

func TestChatCompletionsGeminiUpstreamErrorIsMasked(t *testing.T) {
	upstream, _ := newCaptureUpstream(t, http.StatusInternalServerError, "application/json",
		`{"error":{"message":"upstream details that should not leak"}}`)

	cfg := &Config{
		ModelAliases: map[string]string{"gemini-flash": "gemini/gemini-2.5-flash"},
		ModelConfigs: map[string]ModelConfig{
			"gemini-flash": {APIKey: "gem-key", APIBase: upstream.URL},
		},
	}
	handler := newHandlerWithConfig(t, cfg)

	rr := postJSON(t, handler, "/v1/chat/completions", map[string]any{
		"model":    "gemini-flash",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d body=%s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "upstream details that should not leak") {
		t.Fatalf("upstream error body leaked: %s", rr.Body.String())
	}
}
