package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

func registerEmbeddingsRoute(mux *http.ServeMux, cfg *Config, logger *RequestLogger) {
	mux.HandleFunc("POST /v1/embeddings", handleEmbeddings(cfg, logger))
}

func handleEmbeddings(cfg *Config, logger *RequestLogger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		reqID := getRequestID(r)

		r.Body = http.MaxBytesReader(w, r.Body, maxRequestSize)
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			writeErrorJSON(w, http.StatusRequestEntityTooLarge, "request body too large", "invalid_request_error")
			return
		}

		var req OpenAIEmbeddingRequest
		if err := json.Unmarshal(bodyBytes, &req); err != nil {
			writeErrorJSON(w, http.StatusBadRequest, "invalid JSON in request body", "invalid_request_error")
			return
		}

		// Resolve model alias
		modelField := req.Model
		if mapped, ok := cfg.ModelAliases[modelField]; ok {
			modelField = mapped
		}

		// Validate that the model is configured as an embedding model
		if !cfg.EmbeddingModels[modelField] && !cfg.EmbeddingModels[req.Model] {
			writeErrorJSON(w, http.StatusBadRequest, fmt.Sprintf("model %q is not configured as an embedding model", req.Model), "invalid_request_error")
			return
		}

		provider, model := parseModelPrefix(modelField)

		var perModelCfg ModelConfig
		if mc, ok := cfg.ModelConfigs[modelField]; ok {
			perModelCfg = mc
		}

		if !validModelName.MatchString(model) {
			writeErrorJSON(w, http.StatusBadRequest, "invalid model name", "invalid_request_error")
			return
		}

		slog.Info("embeddings", "request_id", reqID, "provider", provider, "model", model)

		switch provider {
		case "openai":
			handleOpenAIEmbeddings(w, r, cfg, logger, req, model, bodyBytes, reqID, start, perModelCfg)
		case "gemini":
			handleGeminiEmbeddings(w, r, cfg, logger, req, model, bodyBytes, reqID, start, perModelCfg)
		case "ollama_chat":
			handleOllamaEmbeddings(w, r, cfg, logger, req, model, bodyBytes, reqID, start, perModelCfg)
		case "openrouter":
			handleOpenRouterEmbeddings(w, r, cfg, logger, req, model, bodyBytes, reqID, start, perModelCfg)
		default:
			writeErrorJSON(w, http.StatusBadRequest, fmt.Sprintf("unsupported provider for embeddings: %s", provider), "invalid_request_error")
		}
	}
}

// handleOpenAIEmbeddings forwards the embedding request to OpenAI as-is.
func handleOpenAIEmbeddings(w http.ResponseWriter, r *http.Request, cfg *Config, logger *RequestLogger, req OpenAIEmbeddingRequest, model string, bodyBytes []byte, reqID string, start time.Time, perModelCfg ModelConfig) {
	apiKey := perModelCfg.APIKey
	if apiKey == "" {
		apiKey = cfg.OpenAIAPIKey
	}
	if apiKey == "" {
		writeErrorJSON(w, http.StatusInternalServerError, "OPENAI_API_KEY not configured", "server_error")
		return
	}

	modifiedBody := rewriteModelField(bodyBytes, model)

	baseURL := perModelCfg.APIBase
	if baseURL == "" {
		baseURL = cfg.OpenAIBaseURL
	}
	targetURL := baseURL + "/v1/embeddings"

	upstreamReq, err := http.NewRequestWithContext(r.Context(), "POST", targetURL, bytes.NewReader(modifiedBody))
	if err != nil {
		writeErrorJSON(w, http.StatusInternalServerError, "failed to create upstream request", "server_error")
		return
	}
	upstreamReq.Header.Set("Content-Type", "application/json")
	upstreamReq.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := httpClient.Do(upstreamReq)
	if err != nil {
		slog.Error("openai embeddings upstream error", "request_id", reqID, "error", err)
		writeErrorJSON(w, http.StatusBadGateway, "upstream connection failed", "server_error")
		return
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))

	if resp.StatusCode == http.StatusOK {
		if pooled, err := applyPoolingIfNeeded(respBody, req.Pooling); err == nil {
			respBody = pooled
		}
	}

	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	w.Write(respBody)

	logEmbeddingRequest(logger, cfg, reqID, r, "openai", model, resp.StatusCode, start, string(bodyBytes), string(respBody), req.User)
}

// handleGeminiEmbeddings converts OpenAI embedding request to Gemini format and back.
func handleGeminiEmbeddings(w http.ResponseWriter, r *http.Request, cfg *Config, logger *RequestLogger, req OpenAIEmbeddingRequest, model string, bodyBytes []byte, reqID string, start time.Time, perModelCfg ModelConfig) {
	apiKey := perModelCfg.APIKey
	if apiKey == "" {
		apiKey = cfg.GeminiAPIKey
	}
	if apiKey == "" {
		writeErrorJSON(w, http.StatusInternalServerError, "GEMINI_API_KEY not configured", "server_error")
		return
	}

	// Normalize input to []string
	inputs := normalizeEmbeddingInput(req.Input)
	if len(inputs) == 0 {
		writeErrorJSON(w, http.StatusBadRequest, "input is required", "invalid_request_error")
		return
	}

	baseURL := perModelCfg.APIBase
	if baseURL == "" {
		baseURL = cfg.GeminiBaseURL
	}

	geminiModel := "models/" + model

	if len(inputs) == 1 {
		// Single input: use embedContent
		geminiReq := GeminiEmbedContentRequest{
			Model: geminiModel,
			Content: GeminiContent{
				Parts: []GeminiPart{{Text: inputs[0]}},
			},
		}
		geminiBody, _ := json.Marshal(geminiReq)

		targetURL := fmt.Sprintf("%s/v1beta/models/%s:embedContent?key=%s", baseURL, model, apiKey)

		upstreamReq, err := http.NewRequestWithContext(r.Context(), "POST", targetURL, bytes.NewReader(geminiBody))
		if err != nil {
			writeErrorJSON(w, http.StatusInternalServerError, "failed to create upstream request", "server_error")
			return
		}
		upstreamReq.Header.Set("Content-Type", "application/json")

		resp, err := httpClient.Do(upstreamReq)
		if err != nil {
			slog.Error("gemini embeddings upstream error", "request_id", reqID, "error", err)
			writeErrorJSON(w, http.StatusBadGateway, "upstream connection failed", "server_error")
			return
		}
		defer resp.Body.Close()

		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
		if resp.StatusCode != http.StatusOK {
			slog.Error("gemini embeddings API error", "request_id", reqID, "status", resp.StatusCode, "body", string(respBody))
			writeErrorJSON(w, resp.StatusCode, "gemini API error", "server_error")
			return
		}

		var geminiResp GeminiEmbedContentResponse
		if err := json.Unmarshal(respBody, &geminiResp); err != nil {
			writeErrorJSON(w, http.StatusBadGateway, "failed to parse gemini embedding response", "server_error")
			return
		}

		openaiResp := geminiEmbedToOpenAIResponse([]GeminiContentEmbedding{*geminiResp.Embedding}, model)
		openaiBody, _ := json.Marshal(openaiResp)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(openaiBody)

		logEmbeddingRequest(logger, cfg, reqID, r, "gemini", model, http.StatusOK, start, string(bodyBytes), string(openaiBody), req.User)
	} else {
		// Multiple inputs: use batchEmbedContents
		var requests []GeminiEmbedContentRequest
		for _, input := range inputs {
			requests = append(requests, GeminiEmbedContentRequest{
				Model: geminiModel,
				Content: GeminiContent{
					Parts: []GeminiPart{{Text: input}},
				},
			})
		}
		geminiReq := GeminiBatchEmbedContentsRequest{Requests: requests}
		geminiBody, _ := json.Marshal(geminiReq)

		targetURL := fmt.Sprintf("%s/v1beta/models/%s:batchEmbedContents?key=%s", baseURL, model, apiKey)

		upstreamReq, err := http.NewRequestWithContext(r.Context(), "POST", targetURL, bytes.NewReader(geminiBody))
		if err != nil {
			writeErrorJSON(w, http.StatusInternalServerError, "failed to create upstream request", "server_error")
			return
		}
		upstreamReq.Header.Set("Content-Type", "application/json")

		resp, err := httpClient.Do(upstreamReq)
		if err != nil {
			slog.Error("gemini batch embeddings upstream error", "request_id", reqID, "error", err)
			writeErrorJSON(w, http.StatusBadGateway, "upstream connection failed", "server_error")
			return
		}
		defer resp.Body.Close()

		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
		if resp.StatusCode != http.StatusOK {
			slog.Error("gemini batch embeddings API error", "request_id", reqID, "status", resp.StatusCode, "body", string(respBody))
			writeErrorJSON(w, resp.StatusCode, "gemini API error", "server_error")
			return
		}

		var geminiResp GeminiBatchEmbedContentsResponse
		if err := json.Unmarshal(respBody, &geminiResp); err != nil {
			writeErrorJSON(w, http.StatusBadGateway, "failed to parse gemini batch embedding response", "server_error")
			return
		}

		openaiResp := geminiEmbedToOpenAIResponse(geminiResp.Embeddings, model)
		openaiBody, _ := json.Marshal(openaiResp)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(openaiBody)

		logEmbeddingRequest(logger, cfg, reqID, r, "gemini", model, http.StatusOK, start, string(bodyBytes), string(openaiBody), req.User)
	}
}

// handleOllamaEmbeddings forwards embedding request to Ollama's OpenAI-compatible endpoint.
func handleOllamaEmbeddings(w http.ResponseWriter, r *http.Request, cfg *Config, logger *RequestLogger, req OpenAIEmbeddingRequest, model string, bodyBytes []byte, reqID string, start time.Time, perModelCfg ModelConfig) {
	modifiedBody := rewriteModelField(bodyBytes, model)

	baseURL := perModelCfg.APIBase
	if baseURL == "" {
		baseURL = "http://localhost:11434"
	}
	targetURL := baseURL + "/v1/embeddings"

	upstreamReq, err := http.NewRequestWithContext(r.Context(), "POST", targetURL, bytes.NewReader(modifiedBody))
	if err != nil {
		writeErrorJSON(w, http.StatusInternalServerError, "failed to create upstream request", "server_error")
		return
	}
	upstreamReq.Header.Set("Content-Type", "application/json")
	if apiKey := perModelCfg.APIKey; apiKey != "" {
		upstreamReq.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := httpClient.Do(upstreamReq)
	if err != nil {
		slog.Error("ollama embeddings upstream error", "request_id", reqID, "error", err)
		writeErrorJSON(w, http.StatusBadGateway, "upstream connection failed", "server_error")
		return
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))

	if resp.StatusCode == http.StatusOK {
		if pooled, err := applyPoolingIfNeeded(respBody, req.Pooling); err == nil {
			respBody = pooled
		}
	}

	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	w.Write(respBody)

	logEmbeddingRequest(logger, cfg, reqID, r, "ollama_chat", model, resp.StatusCode, start, string(bodyBytes), string(respBody), req.User)
}

// handleOpenRouterEmbeddings forwards embedding request to OpenRouter.
func handleOpenRouterEmbeddings(w http.ResponseWriter, r *http.Request, cfg *Config, logger *RequestLogger, req OpenAIEmbeddingRequest, model string, bodyBytes []byte, reqID string, start time.Time, perModelCfg ModelConfig) {
	apiKey := perModelCfg.APIKey
	if apiKey == "" {
		apiKey = cfg.OpenRouterAPIKey
	}
	if apiKey == "" {
		writeErrorJSON(w, http.StatusInternalServerError, "OPENROUTER_API_KEY not configured", "server_error")
		return
	}

	modifiedBody := rewriteModelField(bodyBytes, model)

	baseURL := perModelCfg.APIBase
	if baseURL == "" {
		baseURL = cfg.OpenRouterBaseURL
	}
	targetURL := baseURL + "/v1/embeddings"

	upstreamReq, err := http.NewRequestWithContext(r.Context(), "POST", targetURL, bytes.NewReader(modifiedBody))
	if err != nil {
		writeErrorJSON(w, http.StatusInternalServerError, "failed to create upstream request", "server_error")
		return
	}
	upstreamReq.Header.Set("Content-Type", "application/json")
	upstreamReq.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := httpClient.Do(upstreamReq)
	if err != nil {
		slog.Error("openrouter embeddings upstream error", "request_id", reqID, "error", err)
		writeErrorJSON(w, http.StatusBadGateway, "upstream connection failed", "server_error")
		return
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))

	if resp.StatusCode == http.StatusOK {
		if pooled, err := applyPoolingIfNeeded(respBody, req.Pooling); err == nil {
			respBody = pooled
		}
	}

	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	w.Write(respBody)

	logEmbeddingRequest(logger, cfg, reqID, r, "openrouter", model, resp.StatusCode, start, string(bodyBytes), string(respBody), req.User)
}

// normalizeEmbeddingInput converts the input field (string or []string) to []string.
func normalizeEmbeddingInput(input any) []string {
	switch v := input.(type) {
	case string:
		return []string{v}
	case []any:
		var result []string
		for _, item := range v {
			if s, ok := item.(string); ok {
				result = append(result, s)
			}
		}
		return result
	case []string:
		return v
	default:
		return nil
	}
}

// geminiEmbedToOpenAIResponse converts Gemini embedding results to OpenAI format.
func geminiEmbedToOpenAIResponse(embeddings []GeminiContentEmbedding, model string) *OpenAIEmbeddingResponse {
	var data []OpenAIEmbedding
	for i, emb := range embeddings {
		data = append(data, OpenAIEmbedding{
			Object:    "embedding",
			Embedding: emb.Values,
			Index:     i,
		})
	}
	return &OpenAIEmbeddingResponse{
		Object: "list",
		Data:   data,
		Model:  model,
		Usage: OpenAIEmbeddingUsage{
			PromptTokens: 0, // Gemini doesn't return token counts for embeddings
			TotalTokens:  0,
		},
	}
}

func logEmbeddingRequest(logger *RequestLogger, cfg *Config, reqID string, r *http.Request, provider, model string, statusCode int, start time.Time, reqBody, respBody string, user string) {
	entry := LogEntry{
		Timestamp:  start.UTC().Format(time.RFC3339Nano),
		RequestID:  reqID,
		Method:     r.Method,
		Path:       r.URL.Path,
		User:       user,
		Provider:   provider,
		Model:      model,
		StatusCode: statusCode,
		LatencyMs:  time.Since(start).Milliseconds(),
		ClientIP:   r.RemoteAddr,
	}
	if cfg.LogRequestBody {
		entry.ReqBody = reqBody
	}
	if cfg.LogResponseBody {
		entry.RespBody = respBody
	}
	logger.Log(entry)
}
