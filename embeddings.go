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

func forwardOpenAICompatEmbeddings(w http.ResponseWriter, r *http.Request, cfg *Config, logger *RequestLogger, req OpenAIEmbeddingRequest, provider, providerLabel, model, targetURL, authHeader string, bodyBytes, upstreamBody []byte, reqID string, start time.Time) {
	upstreamCtx, cancel := withUpstreamTimeout(r.Context(), true)
	defer cancel()

	upstreamReq, err := http.NewRequestWithContext(upstreamCtx, "POST", targetURL, bytes.NewReader(upstreamBody))
	if err != nil {
		writeErrorJSON(w, http.StatusInternalServerError, "failed to create upstream request", "server_error")
		return
	}
	upstreamReq.Header.Set("Content-Type", "application/json")
	if authHeader != "" {
		upstreamReq.Header.Set("Authorization", authHeader)
	}

	resp, err := httpClient.Do(upstreamReq)
	if err != nil {
		slog.Error(providerLabel+" embeddings upstream error", "request_id", reqID, "error", sanitizeUpstreamError(err))
		writeErrorJSON(w, upstreamErrorStatusCode(), "upstream connection failed", "server_error")
		return
	}
	defer resp.Body.Close()

	respBody, err := readUpstreamBody(resp.Body, maxResponseSize)
	if err != nil {
		slog.Warn("failed to read "+providerLabel+" embeddings response", "request_id", reqID, "error", err)
		writeErrorJSON(w, http.StatusBadGateway, "failed to read upstream response", "server_error")
		return
	}
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if err := writeResponseBody(w, respBody); err != nil {
		logResponseWriteError("failed to write "+providerLabel+" embeddings response", reqID, err)
		return
	}

	logEmbeddingRequest(logger, cfg, reqID, r, provider, model, resp.StatusCode, start, string(bodyBytes), string(respBody), req.User)
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
		requestedModel := req.Model
		modelField := requestedModel
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
		if mc, ok := cfg.ModelConfigs[requestedModel]; ok {
			perModelCfg = mc
		} else if mc, ok := cfg.ModelConfigs[modelField]; ok {
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

	forwardOpenAICompatEmbeddings(w, r, cfg, logger, req, "openai", "openai", model, targetURL, "Bearer "+apiKey, bodyBytes, modifiedBody, reqID, start)
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

		targetURL, err := buildGeminiAPIURL(baseURL, model, "embedContent", nil)
		if err != nil {
			writeErrorJSON(w, http.StatusInternalServerError, "failed to build gemini URL", "server_error")
			return
		}

		upstreamCtx, cancel := withUpstreamTimeout(r.Context(), true)
		defer cancel()

		upstreamReq, err := http.NewRequestWithContext(upstreamCtx, "POST", targetURL, bytes.NewReader(geminiBody))
		if err != nil {
			writeErrorJSON(w, http.StatusInternalServerError, "failed to create upstream request", "server_error")
			return
		}
		upstreamReq.Header.Set("Content-Type", "application/json")
		upstreamReq.Header.Set("x-goog-api-key", apiKey)

		resp, err := httpClient.Do(upstreamReq)
		if err != nil {
			slog.Error("gemini embeddings upstream error", "request_id", reqID, "error", sanitizeUpstreamError(err))
			writeErrorJSON(w, upstreamErrorStatusCode(), "upstream connection failed", "server_error")
			return
		}
		defer resp.Body.Close()

		respBody, err := readUpstreamBody(resp.Body, maxResponseSize)
		if err != nil {
			slog.Warn("failed to read gemini embeddings response", "request_id", reqID, "error", err)
			writeErrorJSON(w, http.StatusBadGateway, "failed to read upstream response", "server_error")
			return
		}
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
		if err := writeResponseBody(w, openaiBody); err != nil {
			logResponseWriteError("failed to write gemini embeddings response", reqID, err)
			return
		}

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

		targetURL, err := buildGeminiAPIURL(baseURL, model, "batchEmbedContents", nil)
		if err != nil {
			writeErrorJSON(w, http.StatusInternalServerError, "failed to build gemini URL", "server_error")
			return
		}

		upstreamCtx, cancel := withUpstreamTimeout(r.Context(), true)
		defer cancel()

		upstreamReq, err := http.NewRequestWithContext(upstreamCtx, "POST", targetURL, bytes.NewReader(geminiBody))
		if err != nil {
			writeErrorJSON(w, http.StatusInternalServerError, "failed to create upstream request", "server_error")
			return
		}
		upstreamReq.Header.Set("Content-Type", "application/json")
		upstreamReq.Header.Set("x-goog-api-key", apiKey)

		resp, err := httpClient.Do(upstreamReq)
		if err != nil {
			slog.Error("gemini batch embeddings upstream error", "request_id", reqID, "error", sanitizeUpstreamError(err))
			writeErrorJSON(w, upstreamErrorStatusCode(), "upstream connection failed", "server_error")
			return
		}
		defer resp.Body.Close()

		respBody, err := readUpstreamBody(resp.Body, maxResponseSize)
		if err != nil {
			slog.Warn("failed to read gemini batch embeddings response", "request_id", reqID, "error", err)
			writeErrorJSON(w, http.StatusBadGateway, "failed to read upstream response", "server_error")
			return
		}
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
		if err := writeResponseBody(w, openaiBody); err != nil {
			logResponseWriteError("failed to write gemini batch embeddings response", reqID, err)
			return
		}

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
	authHeader := ""
	if apiKey := perModelCfg.APIKey; apiKey != "" {
		authHeader = "Bearer " + apiKey
	}
	forwardOpenAICompatEmbeddings(w, r, cfg, logger, req, "ollama_chat", "ollama", model, targetURL, authHeader, bodyBytes, modifiedBody, reqID, start)
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

	forwardOpenAICompatEmbeddings(w, r, cfg, logger, req, "openrouter", "openrouter", model, targetURL, "Bearer "+apiKey, bodyBytes, modifiedBody, reqID, start)
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
