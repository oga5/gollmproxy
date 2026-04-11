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

		// Append client query params and pass Gemini auth via header.
		q := u.Query()
		for k, vs := range r.URL.Query() {
			for _, v := range vs {
				q.Add(k, v)
			}
		}
		u.RawQuery = q.Encode()

		slog.Info("passthrough", "provider", "gemini", "path", cleanPath)

		_, err = forwardRequest(w, r, u.String(), func(req *http.Request) {
			req.Header.Set("x-goog-api-key", cfg.GeminiAPIKey)
		})
		if err != nil {
			slog.Error("passthrough error", "provider", "gemini", "error", sanitizeUpstreamError(err))
		}
	}
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
