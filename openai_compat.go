package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// validModelName allows alphanumeric, dots, hyphens, underscores, colons, and slashes.
// Slashes are allowed for providers like OpenRouter where model names contain them (e.g. "stepfun/step-3.5-flash:free").
var validModelName = regexp.MustCompile(`^[a-zA-Z0-9._:/-]+$`)

const (
	maxRequestSize  = 10 << 20 // 10MB
	maxResponseSize = 64 << 20 // 64MB
)

func registerOpenAICompatRoutes(mux *http.ServeMux, cfg *Config, logger *RequestLogger) {
	mux.HandleFunc("POST /v1/chat/completions", handleChatCompletions(cfg, logger))
}

func handleChatCompletions(cfg *Config, logger *RequestLogger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		reqID := getRequestID(r)

		// Read request body with size limit
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestSize)
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			writeErrorJSON(w, http.StatusRequestEntityTooLarge, "request body too large", "invalid_request_error")
			return
		}

		var req OpenAIChatRequest
		if err := json.Unmarshal(bodyBytes, &req); err != nil {
			writeErrorJSON(w, http.StatusBadRequest, "invalid JSON in request body", "invalid_request_error")
			return
		}

		// Resolve model alias (model_name -> provider-prefixed model)
		requestedModel := req.Model
		modelField := requestedModel
		if mapped, ok := cfg.ModelAliases[modelField]; ok {
			modelField = mapped
		}

		// Parse provider prefix
		provider, model := parseModelPrefix(modelField)

		// Look up per-model config overrides
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

		slog.Info("chat completions", "request_id", reqID, "provider", provider, "model", model, "stream", req.Stream)

		switch provider {
		case "openai":
			handleOpenAIProvider(w, r, cfg, logger, req, model, bodyBytes, reqID, start, perModelCfg)
		case "gemini":
			handleGeminiProvider(w, r, cfg, logger, req, model, bodyBytes, reqID, start, perModelCfg)
		case "ollama_chat":
			handleOllamaChatProvider(w, r, cfg, logger, req, model, bodyBytes, reqID, start, perModelCfg)
		case "openrouter":
			handleOpenRouterProvider(w, r, cfg, logger, req, model, bodyBytes, reqID, start, perModelCfg)
		case "bedrock":
			handleBedrockProvider(w, r, cfg, logger, req, model, bodyBytes, reqID, start, perModelCfg)
		default:
			writeErrorJSON(w, http.StatusBadRequest, fmt.Sprintf("unsupported provider: %s", provider), "invalid_request_error")
		}
	}
}

// parseModelPrefix splits "provider/model" into provider and model.
// If no prefix, defaults to "openai".
func parseModelPrefix(model string) (provider, modelName string) {
	parts := strings.SplitN(model, "/", 2)
	if len(parts) == 2 {
		return parts[0], parts[1]
	}
	return "openai", model
}

func forwardOpenAICompatChat(w http.ResponseWriter, r *http.Request, cfg *Config, logger *RequestLogger, req OpenAIChatRequest, provider, providerLabel, model, targetURL, authHeader string, bodyBytes, upstreamBody []byte, reqID string, start time.Time) {
	upstreamCtx, cancel := withUpstreamTimeout(r.Context(), !req.Stream)
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
		slog.Error(providerLabel+" upstream error", "request_id", reqID, "error", sanitizeUpstreamError(err))
		writeErrorJSON(w, http.StatusBadGateway, "upstream connection failed", "server_error")
		return
	}
	defer resp.Body.Close()

	if req.Stream {
		setSSEHeaders(w)
		var onChunk onChunkFunc
		if cfg.LogResponseBody {
			onChunk = func(index int, data []byte) {
				logger.LogChunk(ChunkLogEntry{
					RequestID:  reqID,
					ChunkIndex: index,
					Data:       string(data),
				})
			}
		}
		if err := proxySSEStream(w, resp.Body, nil, onChunk); err != nil {
			slog.Error("streaming error", "error", err)
		}
		logRequest(logger, cfg, reqID, r, provider, model, true, resp.StatusCode, start, string(bodyBytes), "", req.User, req.Metadata, nil)
		return
	}

	respBody, err := readUpstreamBody(resp.Body, maxResponseSize)
	if err != nil {
		slog.Warn("failed to read "+providerLabel+" upstream response", "request_id", reqID, "error", err)
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
		logResponseWriteError("failed to write "+providerLabel+" response", reqID, err)
		return
	}

	var usage *OpenAIUsage
	var openaiResp OpenAIChatResponse
	if json.Unmarshal(respBody, &openaiResp) == nil {
		usage = openaiResp.Usage
	}
	logRequest(logger, cfg, reqID, r, provider, model, false, resp.StatusCode, start, string(bodyBytes), string(respBody), req.User, req.Metadata, usage)
}

func handleOpenAIProvider(w http.ResponseWriter, r *http.Request, cfg *Config, logger *RequestLogger, req OpenAIChatRequest, model string, bodyBytes []byte, reqID string, start time.Time, perModelCfg ModelConfig) {
	apiKey := perModelCfg.APIKey
	if apiKey == "" {
		apiKey = cfg.OpenAIAPIKey
	}
	if apiKey == "" {
		writeErrorJSON(w, http.StatusInternalServerError, "OPENAI_API_KEY not configured", "server_error")
		return
	}

	// Rewrite model field (strip provider prefix)
	modifiedBody := rewriteModelField(bodyBytes, model)

	baseURL := perModelCfg.APIBase
	if baseURL == "" {
		baseURL = cfg.OpenAIBaseURL
	}
	targetURL := baseURL + "/v1/chat/completions"

	forwardOpenAICompatChat(w, r, cfg, logger, req, "openai", "openai", model, targetURL, "Bearer "+apiKey, bodyBytes, modifiedBody, reqID, start)
}

// rewriteModelField replaces the model field value in the JSON body.
func rewriteModelField(body []byte, newModel string) []byte {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return body
	}
	modelBytes, _ := json.Marshal(newModel)
	raw["model"] = modelBytes
	result, err := json.Marshal(raw)
	if err != nil {
		return body
	}
	return result
}

func handleOllamaChatProvider(w http.ResponseWriter, r *http.Request, cfg *Config, logger *RequestLogger, req OpenAIChatRequest, model string, bodyBytes []byte, reqID string, start time.Time, perModelCfg ModelConfig) {
	// Rewrite model field (strip provider prefix)
	modifiedBody := rewriteModelField(bodyBytes, model)

	baseURL := perModelCfg.APIBase
	if baseURL == "" {
		baseURL = "http://localhost:11434"
	}
	targetURL := baseURL + "/v1/chat/completions"
	authHeader := ""
	if apiKey := perModelCfg.APIKey; apiKey != "" {
		authHeader = "Bearer " + apiKey
	}
	forwardOpenAICompatChat(w, r, cfg, logger, req, "ollama_chat", "ollama", model, targetURL, authHeader, bodyBytes, modifiedBody, reqID, start)
}

func handleOpenRouterProvider(w http.ResponseWriter, r *http.Request, cfg *Config, logger *RequestLogger, req OpenAIChatRequest, model string, bodyBytes []byte, reqID string, start time.Time, perModelCfg ModelConfig) {
	apiKey := perModelCfg.APIKey
	if apiKey == "" {
		apiKey = cfg.OpenRouterAPIKey
	}
	if apiKey == "" {
		writeErrorJSON(w, http.StatusInternalServerError, "OPENROUTER_API_KEY not configured", "server_error")
		return
	}

	// Model name is passed as-is (e.g. "stepfun/step-3.5-flash:free")
	modifiedBody := rewriteModelField(bodyBytes, model)

	baseURL := perModelCfg.APIBase
	if baseURL == "" {
		baseURL = cfg.OpenRouterBaseURL
	}
	targetURL := baseURL + "/v1/chat/completions"

	forwardOpenAICompatChat(w, r, cfg, logger, req, "openrouter", "openrouter", model, targetURL, "Bearer "+apiKey, bodyBytes, modifiedBody, reqID, start)
}

func handleGeminiProvider(w http.ResponseWriter, r *http.Request, cfg *Config, logger *RequestLogger, req OpenAIChatRequest, model string, bodyBytes []byte, reqID string, start time.Time, perModelCfg ModelConfig) {
	apiKey := perModelCfg.APIKey
	if apiKey == "" {
		apiKey = cfg.GeminiAPIKey
	}
	if apiKey == "" {
		writeErrorJSON(w, http.StatusInternalServerError, "GEMINI_API_KEY not configured", "server_error")
		return
	}

	// Convert OpenAI request to Gemini format
	geminiReq := openaiToGeminiRequest(&req)
	geminiBody, err := json.Marshal(geminiReq)
	if err != nil {
		writeErrorJSON(w, http.StatusInternalServerError, "failed to marshal gemini request", "server_error")
		return
	}

	// Build Gemini URL
	baseURL := perModelCfg.APIBase
	if baseURL == "" {
		baseURL = cfg.GeminiBaseURL
	}
	extraQuery := map[string]string{}
	if req.Stream {
		extraQuery["alt"] = "sse"
	}
	action := "generateContent"
	if req.Stream {
		action = "streamGenerateContent"
	}
	targetURL, err := buildGeminiAPIURL(baseURL, model, action, extraQuery)
	if err != nil {
		writeErrorJSON(w, http.StatusInternalServerError, "failed to build gemini URL", "server_error")
		return
	}

	upstreamCtx, cancel := withUpstreamTimeout(r.Context(), !req.Stream)
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
		slog.Error("gemini upstream error", "request_id", reqID, "error", sanitizeUpstreamError(err))
		writeErrorJSON(w, http.StatusBadGateway, "upstream connection failed", "server_error")
		return
	}
	defer resp.Body.Close()

	// If upstream returned error, log details server-side, return generic message to client
	if resp.StatusCode != http.StatusOK {
		respBody, err := readUpstreamBody(resp.Body, maxResponseSize)
		if err != nil {
			slog.Warn("failed to read gemini error response", "request_id", reqID, "error", err)
			writeErrorJSON(w, http.StatusBadGateway, "failed to read upstream response", "server_error")
			return
		}
		slog.Error("gemini API error", "request_id", reqID, "status", resp.StatusCode, "body", string(respBody))
		writeErrorJSON(w, resp.StatusCode, "gemini API error", "server_error")
		logRequest(logger, cfg, reqID, r, "gemini", model, req.Stream, resp.StatusCode, start, string(bodyBytes), string(respBody), req.User, req.Metadata, nil)
		return
	}

	if req.Stream {
		handleGeminiStream(w, resp, cfg, model, reqID, logger, r, bodyBytes, start, req.User, req.Metadata)
	} else {
		handleGeminiNonStream(w, resp, cfg, model, reqID, logger, r, bodyBytes, start, req.User, req.Metadata)
	}
}

func handleGeminiNonStream(w http.ResponseWriter, resp *http.Response, cfg *Config, model, reqID string, logger *RequestLogger, r *http.Request, bodyBytes []byte, start time.Time, user string, metadata map[string]any) {
	respBody, err := readUpstreamBody(resp.Body, maxResponseSize)
	if err != nil {
		slog.Warn("failed to read gemini upstream response", "request_id", reqID, "error", err)
		writeErrorJSON(w, http.StatusBadGateway, "failed to read upstream response", "server_error")
		return
	}

	var geminiResp GeminiGenerateResponse
	if err := json.Unmarshal(respBody, &geminiResp); err != nil {
		writeErrorJSON(w, http.StatusBadGateway, "failed to parse gemini response", "server_error")
		return
	}

	openaiResp := geminiToOpenAIResponse(&geminiResp, model, reqID)
	openaiBody, _ := json.Marshal(openaiResp)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := writeResponseBody(w, openaiBody); err != nil {
		logResponseWriteError("failed to write gemini response", reqID, err)
		return
	}

	logRequest(logger, cfg, reqID, r, "gemini", model, false, http.StatusOK, start, string(bodyBytes), string(openaiBody), user, metadata, openaiResp.Usage)
}

func handleGeminiStream(w http.ResponseWriter, resp *http.Response, cfg *Config, model, reqID string, logger *RequestLogger, r *http.Request, bodyBytes []byte, start time.Time, user string, metadata map[string]any) {
	isFirst := true

	transformLine := func(data []byte) ([]byte, error) {
		var geminiResp GeminiGenerateResponse
		if err := json.Unmarshal(data, &geminiResp); err != nil {
			return nil, err
		}

		chunk := geminiStreamChunkToOpenAI(&geminiResp, model, reqID, isFirst)
		isFirst = false

		return json.Marshal(chunk)
	}

	setSSEHeaders(w)
	var onChunk onChunkFunc
	if cfg.LogResponseBody {
		onChunk = func(index int, data []byte) {
			logger.LogChunk(ChunkLogEntry{
				RequestID:  reqID,
				ChunkIndex: index,
				Data:       string(data),
			})
		}
	}
	if err := proxySSEStream(w, resp.Body, transformLine, onChunk); err != nil {
		slog.Error("gemini streaming error", "error", err)
	}

	// Write [DONE] sentinel
	if flusher, ok := w.(http.Flusher); ok {
		fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
	}

	logRequest(logger, cfg, reqID, r, "gemini", model, true, http.StatusOK, start, string(bodyBytes), "", user, metadata, nil)
}

func logRequest(logger *RequestLogger, cfg *Config, reqID string, r *http.Request, provider, model string, stream bool, statusCode int, start time.Time, reqBody, respBody string, user string, metadata map[string]any, usage *OpenAIUsage) {
	entry := LogEntry{
		Timestamp:  start.UTC().Format(time.RFC3339Nano),
		RequestID:  reqID,
		Method:     r.Method,
		Path:       r.URL.Path,
		User:       user,
		Metadata:   metadata,
		Provider:   provider,
		Model:      model,
		Stream:     stream,
		StatusCode: statusCode,
		LatencyMs:  time.Since(start).Milliseconds(),
		ClientIP:   r.RemoteAddr,
	}
	if usage != nil {
		entry.PromptTokens = usage.PromptTokens
		entry.CompletionTokens = usage.CompletionTokens
		entry.TotalTokens = usage.TotalTokens
	}
	if cfg.LogRequestBody {
		entry.ReqBody = reqBody
	}
	if cfg.LogResponseBody {
		entry.RespBody = respBody
	}
	logger.Log(entry)
}
