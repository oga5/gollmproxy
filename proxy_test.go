package main

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestWithUpstreamTimeoutSetsDeadlineForNonStreamingRequests(t *testing.T) {
	ctx, cancel := withUpstreamTimeout(context.Background(), true, 0)
	defer cancel()

	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("expected non-streaming request context to have a deadline")
	}

	remaining := time.Until(deadline)
	if remaining <= 0 || remaining > upstreamNonStreamTimeout {
		t.Fatalf("unexpected remaining timeout: %v", remaining)
	}
}

func TestWithUpstreamTimeoutDoesNotWrapStreamingRequests(t *testing.T) {
	ctx, cancel := withUpstreamTimeout(context.Background(), false, 0)
	defer cancel()

	if deadline, ok := ctx.Deadline(); ok {
		t.Fatalf("streaming request context should not have a deadline, got %v", deadline)
	}
}

func TestWithUpstreamTimeoutUsesPerModelTimeoutWhenProvided(t *testing.T) {
	old := upstreamNonStreamTimeout
	upstreamNonStreamTimeout = 5 * time.Minute
	defer func() { upstreamNonStreamTimeout = old }()

	override := 42 * time.Second
	ctx, cancel := withUpstreamTimeout(context.Background(), true, override)
	defer cancel()

	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("expected non-streaming request context to have a deadline")
	}

	remaining := time.Until(deadline)
	if remaining <= 0 || remaining > override {
		t.Fatalf("unexpected remaining timeout for override: %v", remaining)
	}
	if remaining > upstreamNonStreamTimeout {
		t.Fatalf("override timeout should take precedence over global timeout: remaining=%v global=%v", remaining, upstreamNonStreamTimeout)
	}
}

func TestHTTPClientUsesTransportTimeoutsForStreamingSafety(t *testing.T) {
	if httpClient.Timeout != 0 {
		t.Fatalf("httpClient.Timeout must be disabled for streaming responses, got %v", httpClient.Timeout)
	}

	transport, ok := httpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("httpClient.Transport has unexpected type %T", httpClient.Transport)
	}
	if transport.ResponseHeaderTimeout != upstreamResponseHeaderTimeout {
		t.Fatalf("unexpected ResponseHeaderTimeout: got %v want %v", transport.ResponseHeaderTimeout, upstreamResponseHeaderTimeout)
	}
	if transport.TLSHandshakeTimeout != upstreamTLSHandshakeTimeout {
		t.Fatalf("unexpected TLSHandshakeTimeout: got %v want %v", transport.TLSHandshakeTimeout, upstreamTLSHandshakeTimeout)
	}
	if transport.ExpectContinueTimeout != upstreamExpectContinueTimeout {
		t.Fatalf("unexpected ExpectContinueTimeout: got %v want %v", transport.ExpectContinueTimeout, upstreamExpectContinueTimeout)
	}
	if transport.IdleConnTimeout != upstreamIdleConnTimeout {
		t.Fatalf("unexpected IdleConnTimeout: got %v want %v", transport.IdleConnTimeout, upstreamIdleConnTimeout)
	}
	if transport.Proxy == nil {
		t.Fatal("transport.Proxy should be configured")
	}
	if !transport.ForceAttemptHTTP2 {
		t.Fatal("transport should attempt HTTP/2")
	}
	if transport.MaxIdleConns != 100 {
		t.Fatalf("unexpected MaxIdleConns: got %d want 100", transport.MaxIdleConns)
	}
	if transport.DialContext == nil {
		t.Fatal("transport.DialContext should be configured")
	}

	req, err := http.NewRequest("GET", "https://example.com", nil)
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}
	if _, err := transport.Proxy(req); err != nil {
		t.Fatalf("transport proxy function returned error: %v", err)
	}
}

func TestConfigureUpstreamTimeoutsAppliesConfig(t *testing.T) {
	oldNonStream := upstreamNonStreamTimeout
	oldDial := upstreamDialTimeout
	oldKeepAlive := upstreamKeepAlive
	oldTLS := upstreamTLSHandshakeTimeout
	oldHeader := upstreamResponseHeaderTimeout
	oldExpect := upstreamExpectContinueTimeout
	oldIdle := upstreamIdleConnTimeout
	defer func() {
		upstreamNonStreamTimeout = oldNonStream
		upstreamDialTimeout = oldDial
		upstreamKeepAlive = oldKeepAlive
		upstreamTLSHandshakeTimeout = oldTLS
		upstreamResponseHeaderTimeout = oldHeader
		upstreamExpectContinueTimeout = oldExpect
		upstreamIdleConnTimeout = oldIdle
		httpClient.Transport = newUpstreamTransport()
	}()

	cfg := &Config{
		UpstreamNonStreamTimeout:      42 * time.Second,
		UpstreamDialTimeout:           2 * time.Second,
		UpstreamKeepAlive:             11 * time.Second,
		UpstreamTLSHandshakeTimeout:   3 * time.Second,
		UpstreamResponseHeaderTimeout: 4 * time.Second,
		UpstreamExpectContinueTimeout: 5 * time.Second,
		UpstreamIdleConnTimeout:       6 * time.Second,
	}

	configureUpstreamTimeouts(cfg)

	if upstreamNonStreamTimeout != cfg.UpstreamNonStreamTimeout {
		t.Fatalf("unexpected upstreamNonStreamTimeout: got %v want %v", upstreamNonStreamTimeout, cfg.UpstreamNonStreamTimeout)
	}

	transport, ok := httpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("httpClient.Transport has unexpected type %T", httpClient.Transport)
	}
	if transport.ResponseHeaderTimeout != cfg.UpstreamResponseHeaderTimeout {
		t.Fatalf("unexpected ResponseHeaderTimeout: got %v want %v", transport.ResponseHeaderTimeout, cfg.UpstreamResponseHeaderTimeout)
	}
	if transport.TLSHandshakeTimeout != cfg.UpstreamTLSHandshakeTimeout {
		t.Fatalf("unexpected TLSHandshakeTimeout: got %v want %v", transport.TLSHandshakeTimeout, cfg.UpstreamTLSHandshakeTimeout)
	}
	if transport.ExpectContinueTimeout != cfg.UpstreamExpectContinueTimeout {
		t.Fatalf("unexpected ExpectContinueTimeout: got %v want %v", transport.ExpectContinueTimeout, cfg.UpstreamExpectContinueTimeout)
	}
	if transport.IdleConnTimeout != cfg.UpstreamIdleConnTimeout {
		t.Fatalf("unexpected IdleConnTimeout: got %v want %v", transport.IdleConnTimeout, cfg.UpstreamIdleConnTimeout)
	}
}

func TestFallbackPositiveDuration(t *testing.T) {
	defaultValue := 10 * time.Second

	tests := []struct {
		name  string
		value time.Duration
		want  time.Duration
	}{
		{name: "positive value", value: 3 * time.Second, want: 3 * time.Second},
		{name: "zero value", value: 0, want: defaultValue},
		{name: "negative value", value: -1 * time.Second, want: defaultValue},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := fallbackPositiveDuration(tt.value, defaultValue)
			if got != tt.want {
				t.Fatalf("unexpected duration: got %v want %v", got, tt.want)
			}
		})
	}
}

func TestRedactSensitiveURLRedactsKnownKeys(t *testing.T) {
	rawURL := "https://example.com/v1beta/models/test:generateContent?alt=sse&key=secret&token=abc&keep=ok"

	got := redactSensitiveURL(rawURL)

	if strings.Contains(got, "secret") {
		t.Fatalf("redacted URL leaked API key: %q", got)
	}
	if strings.Contains(got, "abc") {
		t.Fatalf("redacted URL leaked token: %q", got)
	}
	if !strings.Contains(got, "key=REDACTED") {
		t.Fatalf("redacted URL missing key placeholder: %q", got)
	}
	if !strings.Contains(got, "token=REDACTED") {
		t.Fatalf("redacted URL missing token placeholder: %q", got)
	}
	if !strings.Contains(got, "keep=ok") {
		t.Fatalf("redacted URL should preserve unrelated query params: %q", got)
	}
}

func TestBuildGeminiAPIURLSetsPathAndQuery(t *testing.T) {
	got, err := buildGeminiAPIURL("https://generativelanguage.googleapis.com/base?existing=1", "gemini-2.5-flash", "streamGenerateContent", map[string]string{"alt": "sse"})
	if err != nil {
		t.Fatalf("buildGeminiAPIURL returned error: %v", err)
	}

	parsed, err := url.Parse(got)
	if err != nil {
		t.Fatalf("failed to parse built URL: %v", err)
	}

	if parsed.Path != "/base/v1beta/models/gemini-2.5-flash:streamGenerateContent" {
		t.Fatalf("unexpected path: %q", parsed.Path)
	}
	if parsed.Query().Get("existing") != "1" {
		t.Fatalf("expected existing query param to be preserved: %q", got)
	}
	if parsed.Query().Get("alt") != "sse" {
		t.Fatalf("expected alt=sse: %q", got)
	}
	if parsed.Query().Get("key") != "" {
		t.Fatalf("gemini URL should not include key query param: %q", got)
	}
}

func TestSanitizeUpstreamErrorRedactsURLQuerySecrets(t *testing.T) {
	upstreamErr := &url.Error{
		Op:  "Post",
		URL: "https://example.com/path?api_key=secret&x=1",
		Err: url.InvalidHostError("bad host"),
	}

	got := sanitizeUpstreamError(upstreamErr)

	if strings.Contains(got, "secret") {
		t.Fatalf("sanitized error leaked secret: %q", got)
	}
	if !strings.Contains(got, "api_key=REDACTED") {
		t.Fatalf("sanitized error missing redaction marker: %q", got)
	}
	if !strings.Contains(got, "x=1") {
		t.Fatalf("sanitized error should preserve non-sensitive query params: %q", got)
	}
}
