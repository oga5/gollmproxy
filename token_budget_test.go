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

	lastCheckAppID     string
	lastCheckModelName string

	lastAddAppID     string
	lastAddModelName string
	lastAddTokens    int
}

func (f *mockTokenBudgetStore) CheckAllowed(_ context.Context, appID, modelName string, _ time.Time) error {
	f.checkCalls++
	f.lastCheckAppID = appID
	f.lastCheckModelName = modelName
	return f.checkErr
}

func (f *mockTokenBudgetStore) AddUsage(_ context.Context, appID, modelName string, tokens int, _ time.Time) error {
	f.addCalls++
	f.lastAddAppID = appID
	f.lastAddModelName = modelName
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
		"metadata": map[string]any{},
	})

	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d body=%s", rr.Code, rr.Body.String())
	}
	if store.checkCalls != 0 {
		t.Fatalf("expected budget check not to run when ids are missing, got %d", store.checkCalls)
	}
	if store.addCalls != 0 {
		t.Fatalf("expected usage update not to run when ids are missing, got %d", store.addCalls)
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
		"metadata": map[string]any{"app_id": "app1"},
	})

	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d body=%s", rr.Code, rr.Body.String())
	}
	if store.checkCalls != 1 {
		t.Fatalf("expected one budget check, got %d", store.checkCalls)
	}
	if store.lastCheckAppID != "app1" || store.lastCheckModelName != "gpt-4o" {
		t.Fatalf("unexpected check identifiers: app_id=%q model_name=%q", store.lastCheckAppID, store.lastCheckModelName)
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
		"metadata": map[string]any{"app_id": "app1"},
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
	if store.lastAddAppID != "app1" || store.lastAddModelName != "gpt-4o" {
		t.Fatalf("unexpected add identifiers: app_id=%q model_name=%q", store.lastAddAppID, store.lastAddModelName)
	}
	if store.lastAddTokens != 5 {
		t.Fatalf("unexpected added tokens: %d", store.lastAddTokens)
	}
}

func TestExtractBudgetIdentifiersRequiresAppIDAndModelName(t *testing.T) {
	appID, modelName, err := extractBudgetIdentifiers(map[string]any{"app_id": "app1"}, "gpt-4o")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if appID != "app1" || modelName != "gpt-4o" {
		t.Fatalf("unexpected identifiers: app_id=%q model_name=%q", appID, modelName)
	}

	if _, _, err := extractBudgetIdentifiers(map[string]any{}, "gpt-4o"); err == nil {
		t.Fatal("expected error for missing app_id")
	}
	if _, _, err := extractBudgetIdentifiers(map[string]any{"app_id": "app1"}, ""); err == nil {
		t.Fatal("expected error for missing model_name")
	}
}
