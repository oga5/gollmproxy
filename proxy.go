package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const maxPassthroughRequestSize = 10 << 20 // 10MB

var errResponseBodyTooLarge = errors.New("upstream response body too large")

const (
	defaultUpstreamNonStreamTimeout      = 5 * time.Minute
	defaultUpstreamDialTimeout           = 10 * time.Second
	defaultUpstreamKeepAlive             = 30 * time.Second
	defaultUpstreamTLSHandshakeTimeout   = 10 * time.Second
	defaultUpstreamResponseHeaderTimeout = 30 * time.Second
	defaultUpstreamExpectContinueTimeout = 1 * time.Second
	defaultUpstreamIdleConnTimeout       = 90 * time.Second
)

var (
	upstreamNonStreamTimeout      = defaultUpstreamNonStreamTimeout
	upstreamDialTimeout           = defaultUpstreamDialTimeout
	upstreamKeepAlive             = defaultUpstreamKeepAlive
	upstreamTLSHandshakeTimeout   = defaultUpstreamTLSHandshakeTimeout
	upstreamResponseHeaderTimeout = defaultUpstreamResponseHeaderTimeout
	upstreamExpectContinueTimeout = defaultUpstreamExpectContinueTimeout
	upstreamIdleConnTimeout       = defaultUpstreamIdleConnTimeout
)

// httpClient is used for all upstream requests.
// Streaming responses must not be bounded by an absolute client timeout because
// http.Client.Timeout includes the full response body read.
var httpClient = &http.Client{
	Transport: newUpstreamTransport(),
}

func newUpstreamTransport() *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   upstreamDialTimeout,
			KeepAlive: upstreamKeepAlive,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       upstreamIdleConnTimeout,
		TLSHandshakeTimeout:   upstreamTLSHandshakeTimeout,
		ExpectContinueTimeout: upstreamExpectContinueTimeout,
		ResponseHeaderTimeout: upstreamResponseHeaderTimeout,
	}
}

func configureUpstreamTimeouts(cfg *Config) {
	upstreamNonStreamTimeout = fallbackPositiveDuration(cfg.UpstreamNonStreamTimeout, defaultUpstreamNonStreamTimeout)
	upstreamDialTimeout = fallbackPositiveDuration(cfg.UpstreamDialTimeout, defaultUpstreamDialTimeout)
	upstreamKeepAlive = fallbackPositiveDuration(cfg.UpstreamKeepAlive, defaultUpstreamKeepAlive)
	upstreamTLSHandshakeTimeout = fallbackPositiveDuration(cfg.UpstreamTLSHandshakeTimeout, defaultUpstreamTLSHandshakeTimeout)
	upstreamResponseHeaderTimeout = fallbackPositiveDuration(cfg.UpstreamResponseHeaderTimeout, defaultUpstreamResponseHeaderTimeout)
	upstreamExpectContinueTimeout = fallbackPositiveDuration(cfg.UpstreamExpectContinueTimeout, defaultUpstreamExpectContinueTimeout)
	upstreamIdleConnTimeout = fallbackPositiveDuration(cfg.UpstreamIdleConnTimeout, defaultUpstreamIdleConnTimeout)

	if oldTransport, ok := httpClient.Transport.(*http.Transport); ok {
		oldTransport.CloseIdleConnections()
	}
	httpClient.Transport = newUpstreamTransport()
}

func fallbackPositiveDuration(value, defaultValue time.Duration) time.Duration {
	if value <= 0 {
		return defaultValue
	}
	return value
}

func withUpstreamTimeout(parent context.Context, enable bool) (context.Context, context.CancelFunc) {
	if !enable {
		return parent, func() {}
	}
	return context.WithTimeout(parent, upstreamNonStreamTimeout)
}

func readUpstreamBody(body io.Reader, maxSize int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, maxSize+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxSize {
		return nil, errResponseBodyTooLarge
	}
	return data, nil
}

func writeResponseBody(w http.ResponseWriter, body []byte) error {
	if _, err := w.Write(body); err != nil {
		return err
	}
	return nil
}

func logResponseWriteError(msg, reqID string, err error) {
	slog.Warn(msg, "request_id", reqID, "error", err)
}

// forwardRequest forwards an HTTP request to targetURL.
// modifyReq is called to inject auth headers/query params before sending.
// Returns the upstream response status code.
func forwardRequest(w http.ResponseWriter, r *http.Request, targetURL string, modifyReq func(*http.Request)) (int, error) {
	// Read the original request body with size limit
	r.Body = http.MaxBytesReader(w, r.Body, maxPassthroughRequestSize)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return 0, err
	}

	req, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, strings.NewReader(string(body)))
	if err != nil {
		return 0, err
	}

	// Copy relevant headers
	for _, h := range []string{"Content-Type", "Accept", "User-Agent"} {
		if v := r.Header.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	if req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}

	// Let caller inject auth
	if modifyReq != nil {
		modifyReq(req)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	// Copy response headers
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	// Stream response body (supports SSE if upstream sends it)
	if isSSE(resp) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			io.Copy(w, resp.Body)
			return resp.StatusCode, nil
		}
		buf := make([]byte, 4096)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				w.Write(buf[:n])
				flusher.Flush()
			}
			if err != nil {
				break
			}
		}
	} else {
		io.Copy(w, resp.Body)
	}

	return resp.StatusCode, nil
}

func isSSE(resp *http.Response) bool {
	return strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream")
}

func buildGeminiAPIURL(baseURL, model, action string, extraQuery map[string]string) (string, error) {
	base, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}

	base.Path = strings.TrimRight(base.Path, "/") + fmt.Sprintf("/v1beta/models/%s:%s", model, action)
	query := base.Query()
	for k, v := range extraQuery {
		query.Set(k, v)
	}
	base.RawQuery = query.Encode()

	return base.String(), nil
}

func sanitizeUpstreamError(err error) string {
	if err == nil {
		return ""
	}

	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return fmt.Sprintf("%s %s: %v", urlErr.Op, redactSensitiveURL(urlErr.URL), urlErr.Err)
	}

	return err.Error()
}

func redactSensitiveURL(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}

	query := parsed.Query()
	for _, key := range []string{"key", "api_key", "access_token", "token"} {
		if _, ok := query[key]; ok {
			query.Set(key, "REDACTED")
		}
	}
	parsed.RawQuery = query.Encode()

	return parsed.String()
}

// extractAndStripMetadata removes the top-level "metadata" field from a JSON
// object body and returns the extracted metadata alongside the stripped body.
// If no "metadata" key is present, the original body is returned unchanged.
func extractAndStripMetadata(body []byte) (map[string]any, []byte) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, body
	}
	metaRaw, ok := raw["metadata"]
	if !ok {
		return nil, body
	}
	var metadata map[string]any
	json.Unmarshal(metaRaw, &metadata)
	delete(raw, "metadata")
	stripped, err := json.Marshal(raw)
	if err != nil {
		return metadata, body
	}
	return metadata, stripped
}

// writeErrorJSON writes an OpenAI-format error response.
func writeErrorJSON(w http.ResponseWriter, statusCode int, message, errType string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{
			"message": message,
			"type":    errType,
		},
	})
}
