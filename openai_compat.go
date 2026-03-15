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

// validModelName allows alphanumeric, dots, hyphens, underscores, and colons.
// Slashes are NOT allowed (provider prefix is already stripped).
var validModelName = regexp.MustCompile(`^[a-zA-Z0-9._:-]+$`)

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
		modelField := req.Model
		if mapped, ok := cfg.ModelAliases[modelField]; ok {
			modelField = mapped
		}

		// Parse provider prefix
		provider, model := parseModelPrefix(modelField)

		if !validModelName.MatchString(model) {
			writeErrorJSON(w, http.StatusBadRequest, "invalid model name", "invalid_request_error")
			return
		}

		slog.Info("chat completions", "request_id", reqID, "provider", provider, "model", model, "stream", req.Stream)

		switch provider {
		case "openai":
			handleOpenAIProvider(w, r, cfg, logger, req, model, bodyBytes, reqID, start)
		case "gemini":
			handleGeminiProvider(w, r, cfg, logger, req, model, bodyBytes, reqID, start)
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

func handleOpenAIProvider(w http.ResponseWriter, r *http.Request, cfg *Config, logger *RequestLogger, req OpenAIChatRequest, model string, bodyBytes []byte, reqID string, start time.Time) {
	if cfg.OpenAIAPIKey == "" {
		writeErrorJSON(w, http.StatusInternalServerError, "OPENAI_API_KEY not configured", "server_error")
		return
	}

	// Rewrite model field (strip provider prefix)
	modifiedBody := rewriteModelField(bodyBytes, model)

	targetURL := cfg.OpenAIBaseURL + "/v1/chat/completions"

	upstreamReq, err := http.NewRequestWithContext(r.Context(), "POST", targetURL, bytes.NewReader(modifiedBody))
	if err != nil {
		writeErrorJSON(w, http.StatusInternalServerError, "failed to create upstream request", "server_error")
		return
	}
	upstreamReq.Header.Set("Content-Type", "application/json")
	upstreamReq.Header.Set("Authorization", "Bearer "+cfg.OpenAIAPIKey)

	resp, err := httpClient.Do(upstreamReq)
	if err != nil {
		slog.Error("openai upstream error", "request_id", reqID, "error", err)
		writeErrorJSON(w, http.StatusBadGateway, "upstream connection failed", "server_error")
		return
	}
	defer resp.Body.Close()

	if req.Stream {
		// Stream SSE response directly
		setSSEHeaders(w)
		accumulated, err := proxySSEStream(w, resp.Body, nil)
		if err != nil {
			slog.Error("streaming error", "error", err)
		}
		logRequest(logger, cfg, reqID, r, "openai", model, true, resp.StatusCode, start, string(bodyBytes), accumulated, req.User, req.Metadata, nil)
	} else {
		// Non-streaming: read full response and forward
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		w.Write(respBody)

		// Extract usage from OpenAI response
		var usage *OpenAIUsage
		var openaiResp OpenAIChatResponse
		if json.Unmarshal(respBody, &openaiResp) == nil {
			usage = openaiResp.Usage
		}
		logRequest(logger, cfg, reqID, r, "openai", model, false, resp.StatusCode, start, string(bodyBytes), string(respBody), req.User, req.Metadata, usage)
	}
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

func handleGeminiProvider(w http.ResponseWriter, r *http.Request, cfg *Config, logger *RequestLogger, req OpenAIChatRequest, model string, bodyBytes []byte, reqID string, start time.Time) {
	if cfg.GeminiAPIKey == "" {
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
	var targetURL string
	if req.Stream {
		targetURL = fmt.Sprintf("%s/v1beta/models/%s:streamGenerateContent?alt=sse&key=%s",
			cfg.GeminiBaseURL, model, cfg.GeminiAPIKey)
	} else {
		targetURL = fmt.Sprintf("%s/v1beta/models/%s:generateContent?key=%s",
			cfg.GeminiBaseURL, model, cfg.GeminiAPIKey)
	}

	upstreamReq, err := http.NewRequestWithContext(r.Context(), "POST", targetURL, bytes.NewReader(geminiBody))
	if err != nil {
		writeErrorJSON(w, http.StatusInternalServerError, "failed to create upstream request", "server_error")
		return
	}
	upstreamReq.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(upstreamReq)
	if err != nil {
		slog.Error("gemini upstream error", "request_id", reqID, "error", err)
		writeErrorJSON(w, http.StatusBadGateway, "upstream connection failed", "server_error")
		return
	}
	defer resp.Body.Close()

	// If upstream returned error, log details server-side, return generic message to client
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
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
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
	if err != nil {
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
	w.Write(openaiBody)

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
	accumulated, err := proxySSEStream(w, resp.Body, transformLine)
	if err != nil {
		slog.Error("gemini streaming error", "error", err)
	}

	// Write [DONE] sentinel
	if flusher, ok := w.(http.Flusher); ok {
		fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
	}

	logRequest(logger, cfg, reqID, r, "gemini", model, true, http.StatusOK, start, string(bodyBytes), accumulated, user, metadata, nil)
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
