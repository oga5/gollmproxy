package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/google/uuid"
)

type contextKey string

const requestIDKey contextKey = "request_id"

func NewServer(cfg *Config, logger *RequestLogger) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	// OpenAI-compatible unified endpoint
	registerOpenAICompatRoutes(mux, cfg, logger)

	// Pass-through routes
	registerPassthroughRoutes(mux, cfg)

	return applyMiddleware(mux, cfg, logger)
}

func applyMiddleware(handler http.Handler, cfg *Config, logger *RequestLogger) http.Handler {
	// Order: recovery -> requestID -> logging -> auth -> handler
	h := handler
	if cfg.MasterKey != "" {
		h = authMiddleware(h, cfg)
	}
	h = loggingMiddleware(h, logger)
	h = requestIDMiddleware(h)
	h = recoveryMiddleware(h)
	return h
}

func recoveryMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				slog.Error("panic recovered", "error", err, "stack", string(debug.Stack()))
				http.Error(w, `{"error":{"message":"internal server error","type":"server_error"}}`, http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func authMiddleware(next http.Handler, cfg *Config) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Skip auth for health check
		if r.URL.Path == "/health" {
			next.ServeHTTP(w, r)
			return
		}

		// Determine which header to check
		headerName := cfg.KeyHeaderName
		if headerName == "" {
			headerName = "Authorization"
		}

		key := r.Header.Get(headerName)
		// Support "Bearer <key>" format
		key = strings.TrimPrefix(key, "Bearer ")

		if key != cfg.MasterKey {
			writeErrorJSON(w, http.StatusUnauthorized, "invalid api key", "authentication_error")
			return
		}

		next.ServeHTTP(w, r)
	})
}

func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := uuid.New().String()
		ctx := context.WithValue(r.Context(), requestIDKey, id)
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func loggingMiddleware(next http.Handler, logger *RequestLogger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, statusCode: http.StatusOK}

		next.ServeHTTP(sw, r)

		reqID, _ := r.Context().Value(requestIDKey).(string)
		logger.Log(LogEntry{
			Timestamp:  start.UTC().Format(time.RFC3339Nano),
			RequestID:  reqID,
			Method:     r.Method,
			Path:       r.URL.Path,
			StatusCode: sw.statusCode,
			LatencyMs:  time.Since(start).Milliseconds(),
			ClientIP:   r.RemoteAddr,
		})
	})
}

// statusWriter wraps http.ResponseWriter to capture the status code.
type statusWriter struct {
	http.ResponseWriter
	statusCode int
	written    bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.written {
		w.statusCode = code
		w.written = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.written {
		w.written = true
	}
	return w.ResponseWriter.Write(b)
}

// Flush implements http.Flusher for streaming support.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func getRequestID(r *http.Request) string {
	id, _ := r.Context().Value(requestIDKey).(string)
	return id
}
