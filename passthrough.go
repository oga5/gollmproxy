package main

import (
	"log/slog"
	"net/http"
	"net/url"
	"strings"
)

func registerPassthroughRoutes(mux *http.ServeMux, cfg *Config) {
	mux.HandleFunc("/openai/", handleOpenAIPassthrough(cfg))
	mux.HandleFunc("/gemini/", handleGeminiPassthrough(cfg))
	mux.HandleFunc("/tavily/", handleTavilyPassthrough(cfg))
}

func handleOpenAIPassthrough(cfg *Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if cfg.OpenAIAPIKey == "" {
			writeErrorJSON(w, http.StatusInternalServerError, "OPENAI_API_KEY not configured", "server_error")
			return
		}

		// Strip /openai prefix: /openai/v1/models -> /v1/models
		path := strings.TrimPrefix(r.URL.Path, "/openai")
		targetURL := cfg.OpenAIBaseURL + path
		if r.URL.RawQuery != "" {
			targetURL += "?" + r.URL.RawQuery
		}

		slog.Info("passthrough", "provider", "openai", "target", targetURL)

		_, err := forwardRequest(w, r, targetURL, func(req *http.Request) {
			req.Header.Set("Authorization", "Bearer "+cfg.OpenAIAPIKey)
		})
		if err != nil {
			slog.Error("passthrough error", "provider", "openai", "error", err)
		}
	}
}

func handleGeminiPassthrough(cfg *Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if cfg.GeminiAPIKey == "" {
			writeErrorJSON(w, http.StatusInternalServerError, "GEMINI_API_KEY not configured", "server_error")
			return
		}

		// Strip /gemini prefix: /gemini/v1beta/models/... -> /v1beta/models/...
		path := strings.TrimPrefix(r.URL.Path, "/gemini")
		u, err := url.Parse(cfg.GeminiBaseURL + path)
		if err != nil {
			writeErrorJSON(w, http.StatusBadRequest, "invalid URL", "invalid_request_error")
			return
		}

		// Append API key as query parameter
		q := u.Query()
		for k, vs := range r.URL.Query() {
			for _, v := range vs {
				q.Add(k, v)
			}
		}
		q.Set("key", cfg.GeminiAPIKey)
		u.RawQuery = q.Encode()

		slog.Info("passthrough", "provider", "gemini", "target", u.String())

		_, err = forwardRequest(w, r, u.String(), nil)
		if err != nil {
			slog.Error("passthrough error", "provider", "gemini", "error", err)
		}
	}
}

func handleTavilyPassthrough(cfg *Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if cfg.TavilyAPIKey == "" {
			writeErrorJSON(w, http.StatusInternalServerError, "TAVILY_API_KEY not configured", "server_error")
			return
		}

		// Strip /tavily prefix: /tavily/search -> /search
		path := strings.TrimPrefix(r.URL.Path, "/tavily")
		targetURL := cfg.TavilyBaseURL + path
		if r.URL.RawQuery != "" {
			targetURL += "?" + r.URL.RawQuery
		}

		slog.Info("passthrough", "provider", "tavily", "target", targetURL)

		_, err := forwardRequest(w, r, targetURL, func(req *http.Request) {
			req.Header.Set("Authorization", "Bearer "+cfg.TavilyAPIKey)
		})
		if err != nil {
			slog.Error("passthrough error", "provider", "tavily", "error", err)
		}
	}
}
