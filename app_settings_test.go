package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

type stubAppSettingsStore struct {
	settings map[string]AppSettings
	err      error
}

func (s stubAppSettingsStore) Get(_ context.Context, appID string) (AppSettings, bool, error) {
	if s.err != nil {
		return AppSettings{}, false, s.err
	}
	settings, ok := s.settings[appID]
	return settings, ok, nil
}

func (s stubAppSettingsStore) Close() error {
	return nil
}

func boolPtr(v bool) *bool {
	return &v
}

func readTestLogEntries(t *testing.T, path string) []LogEntry {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read log file: %v", err)
	}

	var entries []LogEntry
	for _, line := range bytesSplitLines(data) {
		if len(line) == 0 {
			continue
		}
		var entry LogEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatalf("failed to unmarshal log entry %q: %v", string(line), err)
		}
		entries = append(entries, entry)
	}
	return entries
}

func bytesSplitLines(data []byte) [][]byte {
	var lines [][]byte
	start := 0
	for i, b := range data {
		if b == '\n' {
			lines = append(lines, data[start:i])
			start = i + 1
		}
	}
	if start < len(data) {
		lines = append(lines, data[start:])
	}
	return lines
}

func TestChatCompletionsAppSettingsCanDisableBodyLogging(t *testing.T) {
	upstreamResp := `{"id":"chatcmpl-xxx","object":"chat.completion","created":1,"model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`
	upstream, _ := newCaptureUpstream(t, http.StatusOK, "application/json", upstreamResp)

	logPath := filepath.Join(t.TempDir(), "request.log")
	logger, err := NewRequestLogger(logPath)
	if err != nil {
		t.Fatalf("failed to create test logger: %v", err)
	}
	defer logger.Close()

	cfg := &Config{
		LogRequestBody:  true,
		LogResponseBody: true,
		ModelAliases:    map[string]string{"gpt-4o": "openai/gpt-4o"},
		ModelConfigs: map[string]ModelConfig{
			"gpt-4o": {APIKey: "test-openai-key", APIBase: upstream.URL},
		},
		AppSettingsStore: stubAppSettingsStore{
			settings: map[string]AppSettings{
				"app-1": {
					LogLevel:        "error",
					LogRequestBody:  boolPtr(false),
					LogResponseBody: boolPtr(false),
				},
			},
		},
	}
	handler := NewServer(cfg, logger)

	rr := postJSON(t, handler, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4o",
		"messages": []map[string]string{{"role": "user", "content": "hello"}},
		"metadata": map[string]any{"app_id": "app-1"},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("unexpected status %d body=%s", rr.Code, rr.Body.String())
	}

	entries := readTestLogEntries(t, logPath)
	var matched *LogEntry
	for i := range entries {
		if entries[i].Model == "gpt-4o" {
			matched = &entries[i]
		}
	}
	if matched == nil {
		t.Fatal("expected handler log entry with model")
	}
	if matched.ReqBody != "" {
		t.Fatalf("expected request body logging disabled, got %q", matched.ReqBody)
	}
	if matched.RespBody != "" {
		t.Fatalf("expected response body logging disabled, got %q", matched.RespBody)
	}
}

func TestResolveEffectiveAppLogSettingsFallsBackOnStoreError(t *testing.T) {
	cfg := &Config{
		LogRequestBody:  true,
		LogResponseBody: true,
		AppSettingsStore: stubAppSettingsStore{
			err: errors.New("boom"),
		},
	}

	settings, err := resolveEffectiveAppLogSettings(context.Background(), cfg, map[string]any{"app_id": "app-1"})
	if err == nil {
		t.Fatal("expected store error")
	}
	if !settings.LogRequestBody || !settings.LogResponseBody {
		t.Fatalf("expected default body logging to be preserved on error: %+v", settings)
	}
	if !settings.Enabled(slog.LevelInfo) {
		t.Fatalf("expected default log level to allow info logs: %+v", settings)
	}
}

func TestResolveEffectiveAppLogSettingsAppliesOverrides(t *testing.T) {
	cfg := &Config{
		LogRequestBody:  true,
		LogResponseBody: true,
		AppSettingsStore: stubAppSettingsStore{
			settings: map[string]AppSettings{
				"app-1": {
					LogLevel:        "error",
					LogRequestBody:  boolPtr(false),
					LogResponseBody: boolPtr(false),
				},
			},
		},
	}

	settings, err := resolveEffectiveAppLogSettings(context.Background(), cfg, map[string]any{"app_id": "app-1"})
	if err != nil {
		t.Fatalf("resolveEffectiveAppLogSettings returned error: %v", err)
	}
	if settings.LogRequestBody || settings.LogResponseBody {
		t.Fatalf("expected body logging overrides to disable request/response bodies: %+v", settings)
	}
	if settings.Enabled(slog.LevelInfo) {
		t.Fatalf("expected error log level to suppress info logs: %+v", settings)
	}
	if !settings.Enabled(slog.LevelError) {
		t.Fatalf("expected error log level to allow error logs: %+v", settings)
	}
}
