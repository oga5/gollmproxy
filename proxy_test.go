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
	ctx, cancel := withUpstreamTimeout(context.Background(), true)
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
	ctx, cancel := withUpstreamTimeout(context.Background(), false)
	defer cancel()

	if deadline, ok := ctx.Deadline(); ok {
		t.Fatalf("streaming request context should not have a deadline, got %v", deadline)
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