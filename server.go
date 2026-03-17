package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
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

	// Embeddings endpoint
	registerEmbeddingsRoute(mux, cfg, logger)

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

const (
	authMaxFailures      = 10
	authLockoutDuration  = 5 * time.Minute
)

type lockoutEntry struct {
	count    int
	lockedAt time.Time
	lastSeen time.Time
}

const authEntryTTL = 30 * time.Minute

func authMiddleware(next http.Handler, cfg *Config) http.Handler {
	var (
		mu       sync.Mutex
		lockouts = make(map[string]*lockoutEntry)
	)

	// Clean up expired/stale entries every 5 minutes
	go func() {
		for range time.Tick(5 * time.Minute) {
			now := time.Now()
			mu.Lock()
			for ip, e := range lockouts {
				// Remove locked-out entries past their lockout duration
				if e.count >= authMaxFailures && now.Sub(e.lockedAt) >= authLockoutDuration {
					delete(lockouts, ip)
					continue
				}
				// Remove stale entries that haven't seen activity
				if now.Sub(e.lastSeen) >= authEntryTTL {
					delete(lockouts, ip)
				}
			}
			mu.Unlock()
		}
	}()

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Skip auth for health check
		if r.URL.Path == "/health" {
			next.ServeHTTP(w, r)
			return
		}

		clientIP := getClientIP(r, cfg.TrustedProxyHeader)

		// Check lockout before doing any work
		mu.Lock()
		e := lockouts[clientIP]
		if e != nil && e.count >= authMaxFailures {
			if time.Since(e.lockedAt) < authLockoutDuration {
				mu.Unlock()
				remaining := authLockoutDuration - time.Since(e.lockedAt)
				w.Header().Set("Retry-After", fmt.Sprintf("%d", int(remaining.Seconds())))
				writeErrorJSON(w, http.StatusTooManyRequests, "too many authentication failures, try again later", "rate_limit_error")
				return
			}
			// Lockout expired — reset
			delete(lockouts, clientIP)
			e = nil
		}
		mu.Unlock()

		// Determine which header to check
		headerName := cfg.KeyHeaderName
		if headerName == "" {
			headerName = "Authorization"
		}

		key := r.Header.Get(headerName)
		key = strings.TrimPrefix(key, "Bearer ")

		if subtle.ConstantTimeCompare([]byte(key), []byte(cfg.MasterKey)) == 1 {
			// Valid key — clear failure record
			mu.Lock()
			delete(lockouts, clientIP)
			mu.Unlock()
			next.ServeHTTP(w, r)
			return
		}

		// Invalid key — record failure
		now := time.Now()
		mu.Lock()
		if lockouts[clientIP] == nil {
			lockouts[clientIP] = &lockoutEntry{}
		}
		lockouts[clientIP].count++
		lockouts[clientIP].lastSeen = now
		if lockouts[clientIP].count >= authMaxFailures {
			lockouts[clientIP].lockedAt = now
		}
		mu.Unlock()

		writeErrorJSON(w, http.StatusUnauthorized, "invalid api key", "authentication_error")
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

// getClientIP extracts the client IP, optionally using a trusted proxy header.
func getClientIP(r *http.Request, trustedHeader string) string {
	if trustedHeader != "" {
		if v := r.Header.Get(trustedHeader); v != "" {
			// Take the first IP (leftmost = original client)
			if ip, _, ok := strings.Cut(v, ","); ok {
				return strings.TrimSpace(ip)
			}
			return strings.TrimSpace(v)
		}
	}
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	return ip
}
