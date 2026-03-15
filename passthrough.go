package main

import (
	"log/slog"
	"net/http"
	"net/url"
	"path"
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

		// Strip /gemini prefix and sanitize: /gemini/v1beta/models/... -> /v1beta/models/...
		cleanPath := path.Clean(strings.TrimPrefix(r.URL.Path, "/gemini"))
		u, err := url.Parse(cfg.GeminiBaseURL)
		if err != nil {
			writeErrorJSON(w, http.StatusInternalServerError, "invalid base URL", "server_error")
			return
		}
		u.Path = cleanPath

		// Append client query params + API key
		q := u.Query()
		for k, vs := range r.URL.Query() {
			for _, v := range vs {
				q.Add(k, v)
			}
		}
		q.Set("key", cfg.GeminiAPIKey)
		u.RawQuery = q.Encode()

		slog.Info("passthrough", "provider", "gemini", "path", cleanPath)

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
			slog.Error("passthrough error", "provider", "tavily", "error", err)
		}
	}
}
