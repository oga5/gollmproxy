package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

func registerOpenAICompatRoutes(mux *http.ServeMux, cfg *Config, logger *RequestLogger) {
	mux.HandleFunc("POST /v1/chat/completions", handleChatCompletions(cfg, logger))
}

func handleChatCompletions(cfg *Config, logger *RequestLogger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		reqID := getRequestID(r)

		// Read request body
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			writeErrorJSON(w, http.StatusBadRequest, "failed to read request body", "invalid_request_error")
			return
		}
		defer r.Body.Close()

		var req OpenAIChatRequest
		if err := json.Unmarshal(bodyBytes, &req); err != nil {
			writeErrorJSON(w, http.StatusBadRequest, "invalid request body: "+err.Error(), "invalid_request_error")
			return
		}

		// Parse provider prefix
		provider, model := parseModelPrefix(req.Model)

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

	resp, err := http.DefaultClient.Do(upstreamReq)
	if err != nil {
		writeErrorJSON(w, http.StatusBadGateway, "upstream connection failed: "+err.Error(), "server_error")
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
		logRequest(logger, reqID, r, "openai", model, true, resp.StatusCode, start, string(bodyBytes), accumulated)
	} else {
		// Non-streaming: read full response and forward
		respBody, _ := io.ReadAll(resp.Body)
		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		w.Write(respBody)
		logRequest(logger, reqID, r, "openai", model, false, resp.StatusCode, start, string(bodyBytes), string(respBody))
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

	resp, err := http.DefaultClient.Do(upstreamReq)
	if err != nil {
		writeErrorJSON(w, http.StatusBadGateway, "upstream connection failed: "+err.Error(), "server_error")
		return
	}
	defer resp.Body.Close()

	// If upstream returned error, forward it
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		writeErrorJSON(w, resp.StatusCode, fmt.Sprintf("gemini API error: %s", string(respBody)), "server_error")
		logRequest(logger, reqID, r, "gemini", model, req.Stream, resp.StatusCode, start, string(bodyBytes), string(respBody))
		return
	}

	if req.Stream {
		handleGeminiStream(w, resp, model, reqID, logger, r, bodyBytes, start)
	} else {
		handleGeminiNonStream(w, resp, model, reqID, logger, r, bodyBytes, start)
	}
}

func handleGeminiNonStream(w http.ResponseWriter, resp *http.Response, model, reqID string, logger *RequestLogger, r *http.Request, bodyBytes []byte, start time.Time) {
	respBody, err := io.ReadAll(resp.Body)
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

	logRequest(logger, reqID, r, "gemini", model, false, http.StatusOK, start, string(bodyBytes), string(openaiBody))
}

func handleGeminiStream(w http.ResponseWriter, resp *http.Response, model, reqID string, logger *RequestLogger, r *http.Request, bodyBytes []byte, start time.Time) {
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

	logRequest(logger, reqID, r, "gemini", model, true, http.StatusOK, start, string(bodyBytes), accumulated)
}

func logRequest(logger *RequestLogger, reqID string, r *http.Request, provider, model string, stream bool, statusCode int, start time.Time, reqBody, respBody string) {
	logger.Log(LogEntry{
		Timestamp:  start.UTC().Format(time.RFC3339Nano),
		RequestID:  reqID,
		Method:     r.Method,
		Path:       r.URL.Path,
		Provider:   provider,
		Model:      model,
		Stream:     stream,
		StatusCode: statusCode,
		LatencyMs:  time.Since(start).Milliseconds(),
		ReqBody:    reqBody,
		RespBody:   respBody,
		ClientIP:   r.RemoteAddr,
	})
}
