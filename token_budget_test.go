package main

import (
	"context"
	"net/http"
	"testing"
	"time"
)

type mockTokenBudgetStore struct {
	checkErr error
	addErr   error

	checkCalls int
	addCalls   int

	lastCheckAppID   string
	lastCheckModelID string

	lastAddAppID   string
	lastAddModelID string
	lastAddTokens  int
}

func (f *mockTokenBudgetStore) CheckAllowed(_ context.Context, appID, modelID string, _ time.Time) error {
	f.checkCalls++
	f.lastCheckAppID = appID
	f.lastCheckModelID = modelID
	return f.checkErr
}

func (f *mockTokenBudgetStore) AddUsage(_ context.Context, appID, modelID string, tokens int, _ time.Time) error {
	f.addCalls++
	f.lastAddAppID = appID
	f.lastAddModelID = modelID
	f.lastAddTokens = tokens
	return f.addErr
}

func TestChatCompletionsTokenBudgetMissingIdentifiersReturns429(t *testing.T) {
	upstream, _ := newCaptureUpstream(t, http.StatusOK, "application/json", `{"id":"x","object":"chat.completion","created":1,"model":"gpt-4o","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	store := &mockTokenBudgetStore{}

	cfg := &Config{
		TokenBudgetEnabled: true,
		TokenBudgetStore:   store,
		ModelAliases:       map[string]string{"gpt-4o": "openai/gpt-4o"},
		ModelConfigs: map[string]ModelConfig{
			"gpt-4o": {APIKey: "k", APIBase: upstream.URL},
		},
	}
	handler := newHandlerWithConfig(t, cfg)

	rr := postJSON(t, handler, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4o",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
		"metadata": map[string]any{"appid": "app-only"},
	})

	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d body=%s", rr.Code, rr.Body.String())
	}
	if store.checkCalls != 0 {
		t.Fatalf("expected budget check not to run when ids are missing, got %d", store.checkCalls)
	}
}

func TestChatCompletionsTokenBudgetNotConfiguredReturns429(t *testing.T) {
	upstream, _ := newCaptureUpstream(t, http.StatusOK, "application/json", `{"id":"x","object":"chat.completion","created":1,"model":"gpt-4o","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	store := &mockTokenBudgetStore{checkErr: ErrBudgetNotConfigured}

	cfg := &Config{
		TokenBudgetEnabled: true,
		TokenBudgetStore:   store,
		ModelAliases:       map[string]string{"gpt-4o": "openai/gpt-4o"},
		ModelConfigs: map[string]ModelConfig{
			"gpt-4o": {APIKey: "k", APIBase: upstream.URL},
		},
	}
	handler := newHandlerWithConfig(t, cfg)

	rr := postJSON(t, handler, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4o",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
		"metadata": map[string]any{"appid": "app1", "modelid": "modelA"},
	})

	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d body=%s", rr.Code, rr.Body.String())
	}
	if store.checkCalls != 1 {
		t.Fatalf("expected one budget check, got %d", store.checkCalls)
	}
}

func TestChatCompletionsTokenBudgetAddsUsageAfterInvoke(t *testing.T) {
	upstreamResp := `{"id":"x","object":"chat.completion","created":1,"model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`
	upstream, _ := newCaptureUpstream(t, http.StatusOK, "application/json", upstreamResp)
	store := &mockTokenBudgetStore{}

	cfg := &Config{
		TokenBudgetEnabled: true,
		TokenBudgetStore:   store,
		ModelAliases:       map[string]string{"gpt-4o": "openai/gpt-4o"},
		ModelConfigs: map[string]ModelConfig{
			"gpt-4o": {APIKey: "k", APIBase: upstream.URL},
		},
	}
	handler := newHandlerWithConfig(t, cfg)

	rr := postJSON(t, handler, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4o",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
		"metadata": map[string]any{"appid": "app1", "modelid": "modelA"},
	})

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	if store.checkCalls != 1 {
		t.Fatalf("expected one budget check, got %d", store.checkCalls)
	}
	if store.addCalls != 1 {
		t.Fatalf("expected one usage update, got %d", store.addCalls)
	}
	if store.lastAddAppID != "app1" || store.lastAddModelID != "modelA" {
		t.Fatalf("unexpected add identifiers: appid=%q modelid=%q", store.lastAddAppID, store.lastAddModelID)
	}
	if store.lastAddTokens != 5 {
		t.Fatalf("unexpected added tokens: %d", store.lastAddTokens)
	}
}
