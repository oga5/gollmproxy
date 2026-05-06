package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"
)

// geminiModelPattern matches /v1beta/models/{model}:{action} in a path.
var geminiModelPattern = regexp.MustCompile(`/models/([^/:]+)`)

func registerPassthroughRoutes(mux *http.ServeMux, cfg *Config, logger *RequestLogger) {
	mux.HandleFunc("/openai/", handleOpenAIPassthrough(cfg))
	mux.HandleFunc("/gemini/", handleGeminiPassthrough(cfg, logger))
	mux.HandleFunc("/tavily/", handleTavilyPassthrough(cfg))
	mux.HandleFunc("/openrouter/", handleOpenRouterPassthrough(cfg))

	// Register custom pass-through endpoints from config
	for _, ep := range cfg.PassThroughEndpoints {
		routePath := ep.Path
		if !strings.HasSuffix(routePath, "/") {
			routePath += "/"
		}
		mux.HandleFunc(routePath, handleConfiguredPassthrough(ep))
		slog.Info("registered pass-through endpoint", "path", routePath, "target", ep.Target)
	}
}

func handleOpenAIPassthrough(cfg *Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if cfg.OpenAIAPIKey == "" {
			writeErrorJSON(w, http.StatusInternalServerError, "OPENAI_API_KEY not configured", "server_error")
			return
		}

		// Strip /openai prefix and sanitize: /openai/v1/models -> /v1/models
		cleanPath := path.Clean(strings.TrimPrefix(r.URL.Path, "/openai"))
		u, err := url.Parse(cfg.OpenAIBaseURL)
		if err != nil {
			writeErrorJSON(w, http.StatusInternalServerError, "invalid base URL", "server_error")
			return
		}
		u.Path = cleanPath
		u.RawQuery = r.URL.RawQuery

		slog.Info("passthrough", "provider", "openai", "path", cleanPath)

		_, err = forwardRequest(w, r, u.String(), func(req *http.Request) {
			req.Header.Set("Authorization", "Bearer "+cfg.OpenAIAPIKey)
		})
		if err != nil {
			slog.Error("passthrough error", "provider", "openai", "error", sanitizeUpstreamError(err))
		}
	}
}

func handleGeminiPassthrough(cfg *Config, logger *RequestLogger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		reqID := getRequestID(r)

		if cfg.GeminiAPIKey == "" {
			writeErrorJSON(w, http.StatusInternalServerError, "GEMINI_API_KEY not configured", "server_error")
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, maxPassthroughRequestSize)
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			writeErrorJSON(w, http.StatusBadRequest, "failed to read request body", "invalid_request_error")
			return
		}

		// Strip /gemini prefix and sanitize: /gemini/v1beta/models/... -> /v1beta/models/...
		cleanPath := path.Clean(strings.TrimPrefix(r.URL.Path, "/gemini"))
		u, err := url.Parse(cfg.GeminiBaseURL)
		if err != nil {
			writeErrorJSON(w, http.StatusInternalServerError, "invalid base URL", "server_error")
			return
		}
		u.Path = cleanPath

		// Append client query params and pass Gemini auth via header.
		q := u.Query()
		for k, vs := range r.URL.Query() {
			for _, v := range vs {
				q.Add(k, v)
			}
		}
		u.RawQuery = q.Encode()

		// Extract model name from path e.g. /v1beta/models/gemini-2.5-flash:generateContent
		model := ""
		if m := geminiModelPattern.FindStringSubmatch(cleanPath); len(m) == 2 {
			model = strings.SplitN(m[1], ":", 2)[0]
		}

		// Strip metadata before forwarding; keep original body for request log.
		metadata, upstreamBody := extractAndStripMetadata(bodyBytes)

		slog.Info("passthrough", "provider", "gemini", "path", cleanPath)

		upstreamReq, err := http.NewRequestWithContext(r.Context(), r.Method, u.String(), bytes.NewReader(upstreamBody))
		if err != nil {
			writeErrorJSON(w, http.StatusInternalServerError, "failed to create upstream request", "server_error")
			return
		}
		for _, h := range []string{"Content-Type", "Accept", "User-Agent"} {
			if v := r.Header.Get(h); v != "" {
				upstreamReq.Header.Set(h, v)
			}
		}
		if upstreamReq.Header.Get("Content-Type") == "" {
			upstreamReq.Header.Set("Content-Type", "application/json")
		}
		upstreamReq.Header.Set("x-goog-api-key", cfg.GeminiAPIKey)

		resp, err := httpClient.Do(upstreamReq)
		if err != nil {
			slog.Error("passthrough error", "provider", "gemini", "error", sanitizeUpstreamError(err))
			writeErrorJSON(w, http.StatusBadGateway, "upstream connection failed", "server_error")
			return
		}
		defer resp.Body.Close()

		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}

		if isSSE(resp) {
			w.WriteHeader(resp.StatusCode)
			flusher, ok := w.(http.Flusher)
			if !ok {
				io.Copy(w, resp.Body)
				logGeminiPassthrough(logger, cfg, reqID, r, model, true, resp.StatusCode, start, bodyBytes, nil, nil, metadata)
				return
			}

			var chunks []string
			scanner := bufio.NewScanner(resp.Body)
			scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
			for scanner.Scan() {
				line := scanner.Text()
				fmt.Fprintf(w, "%s\n", line)
				flusher.Flush()
				if cfg.LogResponseBody && strings.HasPrefix(line, "data: ") {
					data := strings.TrimPrefix(line, "data: ")
					if data != "[DONE]" {
						chunks = append(chunks, data)
					}
				}
			}

			var usage *OpenAIUsage
			if len(chunks) > 0 {
				usage = extractGeminiUsageFromChunks(chunks)
			}
			respBody := strings.Join(chunks, "\n")
			logGeminiPassthrough(logger, cfg, reqID, r, model, true, resp.StatusCode, start, bodyBytes, []byte(respBody), usage, metadata)
			return
		}

		respBody, err := readUpstreamBody(resp.Body, maxResponseSize)
		if err != nil {
			slog.Warn("failed to read gemini passthrough response", "request_id", reqID, "error", err)
			writeErrorJSON(w, http.StatusBadGateway, "failed to read upstream response", "server_error")
			return
		}
		w.WriteHeader(resp.StatusCode)
		w.Write(respBody)

		var usage *OpenAIUsage
		if resp.StatusCode == http.StatusOK {
			usage = extractGeminiUsageFromBody(respBody)
		}
		logGeminiPassthrough(logger, cfg, reqID, r, model, false, resp.StatusCode, start, bodyBytes, respBody, usage, metadata)
	}
}

func extractGeminiUsageFromBody(body []byte) *OpenAIUsage {
	var resp GeminiGenerateResponse
	if err := json.Unmarshal(body, &resp); err != nil || resp.UsageMetadata == nil {
		return nil
	}
	return &OpenAIUsage{
		PromptTokens:     resp.UsageMetadata.PromptTokenCount,
		CompletionTokens: resp.UsageMetadata.CandidatesTokenCount,
		TotalTokens:      resp.UsageMetadata.TotalTokenCount,
	}
}

func extractGeminiUsageFromChunks(chunks []string) *OpenAIUsage {
	// The last chunk with usageMetadata is the authoritative source.
	for i := len(chunks) - 1; i >= 0; i-- {
		if u := extractGeminiUsageFromBody([]byte(chunks[i])); u != nil {
			return u
		}
	}
	return nil
}

func logGeminiPassthrough(logger *RequestLogger, cfg *Config, reqID string, r *http.Request, model string, stream bool, statusCode int, start time.Time, reqBody, respBody []byte, usage *OpenAIUsage, metadata map[string]any) {
	entry := LogEntry{
		Timestamp:  start.UTC().Format(time.RFC3339Nano),
		RequestID:  reqID,
		Method:     r.Method,
		Path:       r.URL.Path,
		Metadata:   metadata,
		Provider:   "gemini",
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
		entry.ReqBody = truncate(string(reqBody), maxBodyLogSize)
	}
	if cfg.LogResponseBody {
		entry.RespBody = truncate(string(respBody), maxBodyLogSize)
	}
	logger.Log(entry)
}

func handleOpenRouterPassthrough(cfg *Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if cfg.OpenRouterAPIKey == "" {
			writeErrorJSON(w, http.StatusInternalServerError, "OPENROUTER_API_KEY not configured", "server_error")
			return
		}

		// Strip /openrouter prefix and sanitize: /openrouter/v1/models -> /v1/models
		cleanPath := path.Clean(strings.TrimPrefix(r.URL.Path, "/openrouter"))
		u, err := url.Parse(cfg.OpenRouterBaseURL)
		if err != nil {
			writeErrorJSON(w, http.StatusInternalServerError, "invalid base URL", "server_error")
			return
		}
		u.Path = cleanPath
		u.RawQuery = r.URL.RawQuery

		slog.Info("passthrough", "provider", "openrouter", "path", cleanPath)

		_, err = forwardRequest(w, r, u.String(), func(req *http.Request) {
			req.Header.Set("Authorization", "Bearer "+cfg.OpenRouterAPIKey)
		})
		if err != nil {
			slog.Error("passthrough error", "provider", "openrouter", "error", sanitizeUpstreamError(err))
		}
	}
}

func handleTavilyPassthrough(cfg *Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if cfg.TavilyAPIKey == "" {
			writeErrorJSON(w, http.StatusInternalServerError, "TAVILY_API_KEY not configured", "server_error")
			return
		}

		// Strip /tavily prefix and sanitize: /tavily/search -> /search
		cleanPath := path.Clean(strings.TrimPrefix(r.URL.Path, "/tavily"))
		u, err := url.Parse(cfg.TavilyBaseURL)
		if err != nil {
			writeErrorJSON(w, http.StatusInternalServerError, "invalid base URL", "server_error")
			return
		}
		u.Path = cleanPath
		u.RawQuery = r.URL.RawQuery

		slog.Info("passthrough", "provider", "tavily", "path", cleanPath)

		_, err = forwardRequest(w, r, u.String(), func(req *http.Request) {
			req.Header.Set("Authorization", "Bearer "+cfg.TavilyAPIKey)
		})
		if err != nil {
			slog.Error("passthrough error", "provider", "tavily", "error", sanitizeUpstreamError(err))
		}
	}
}

func handleConfiguredPassthrough(ep PassThroughEndpoint) http.HandlerFunc {
	prefix := ep.Path
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}

	return func(w http.ResponseWriter, r *http.Request) {
		// Strip prefix and sanitize path
		cleanPath := path.Clean(strings.TrimPrefix(r.URL.Path, strings.TrimSuffix(prefix, "/")))
		u, err := url.Parse(ep.Target)
		if err != nil {
			writeErrorJSON(w, http.StatusInternalServerError, "invalid target URL", "server_error")
			return
		}
		u.Path = path.Join(u.Path, cleanPath)
		u.RawQuery = r.URL.RawQuery

		slog.Info("passthrough", "endpoint", ep.Path, "path", cleanPath)

		_, err = forwardRequest(w, r, u.String(), func(req *http.Request) {
			// Forward all incoming headers if enabled
			if ep.ForwardHeaders {
				for k, vs := range r.Header {
					for _, v := range vs {
						req.Header.Add(k, v)
					}
				}
			}
			// Set static headers from config (overrides forwarded headers)
			for k, v := range ep.Headers {
				req.Header.Set(k, v)
			}
		})
		if err != nil {
			slog.Error("passthrough error", "endpoint", ep.Path, "error", sanitizeUpstreamError(err))
		}
	}
}
