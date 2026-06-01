package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadJSONConfigKeepsPerModelNameOverrides(t *testing.T) {
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "")

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	configBody := []byte(`{
  "model_list": [
    {
      "model_name": "gpt-oss-20b-a",
      "proxy_params": {
        "model": "bedrock/openai.gpt-oss-20b-1:0",
        "region": "ap-northeast-1"
      }
    },
    {
      "model_name": "gpt-oss-20b-b",
      "proxy_params": {
        "model": "bedrock/openai.gpt-oss-20b-1:0",
        "region": "ap-northeast-3"
      }
    }
  ]
}`)
	if err := os.WriteFile(configPath, configBody, 0644); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	cfg := &Config{}
	loadJSONConfig(configPath, cfg)

	if got := cfg.ModelAliases["gpt-oss-20b-a"]; got != "bedrock/openai.gpt-oss-20b-1:0" {
		t.Fatalf("unexpected alias mapping for a: %q", got)
	}
	if got := cfg.ModelAliases["gpt-oss-20b-b"]; got != "bedrock/openai.gpt-oss-20b-1:0" {
		t.Fatalf("unexpected alias mapping for b: %q", got)
	}
	if got := cfg.ModelConfigs["gpt-oss-20b-a"].Region; got != "ap-northeast-1" {
		t.Fatalf("unexpected region for alias a: %q", got)
	}
	if got := cfg.ModelConfigs["gpt-oss-20b-b"].Region; got != "ap-northeast-3" {
		t.Fatalf("unexpected region for alias b: %q", got)
	}
	if _, ok := cfg.ModelConfigs["bedrock/openai.gpt-oss-20b-1:0"]; ok {
		t.Fatal("expected per-alias Bedrock config to avoid shared model-key override")
	}
}

func TestLoadJSONConfigKeepsDirectModelOverridesWithoutAlias(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	configBody := []byte(`{
  "model_list": [
    {
      "proxy_params": {
        "model": "openai/gpt-4o",
        "api_base": "https://example.invalid"
      }
    }
  ]
}`)
	if err := os.WriteFile(configPath, configBody, 0644); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	cfg := &Config{}
	loadJSONConfig(configPath, cfg)

	if got := cfg.ModelConfigs["openai/gpt-4o"].APIBase; got != "https://example.invalid" {
		t.Fatalf("unexpected direct model config: %q", got)
	}
}

func TestLoadJSONConfigStoresSearchProviderInModelConfig(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	configBody := []byte(`{
  "model_list": [
    {
      "model_name": "tavily-proxy",
      "proxy_params": {
        "model": "openai/gpt-4o",
        "search_provider": "tavily"
      }
    }
  ]
}`)
	if err := os.WriteFile(configPath, configBody, 0644); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	cfg := &Config{}
	loadJSONConfig(configPath, cfg)

	if got := cfg.ModelConfigs["tavily-proxy"].SearchProvider; got != "tavily" {
		t.Fatalf("unexpected search_provider: %q", got)
	}
}

func TestLoadJSONConfigStoresPerModelTimeout(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	configBody := []byte(`{
  "model_list": [
    {
      "model_name": "gpt-4o-fast",
      "proxy_params": {
        "model": "openai/gpt-4o",
        "timeout": "45s"
      }
    }
  ]
}`)
	if err := os.WriteFile(configPath, configBody, 0644); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	cfg := &Config{}
	loadJSONConfig(configPath, cfg)

	if got := cfg.ModelConfigs["gpt-4o-fast"].Timeout.String(); got != "45s" {
		t.Fatalf("unexpected timeout: %q", got)
	}
}

func TestBuildLogMetadataAddsWhitelistedLitellmParams(t *testing.T) {
	metadata := buildLogMetadata(map[string]any{
		"app_id":         "app1",
		"litellm_params": "client-value",
	}, "openai/gpt-4o", ModelConfig{
		APIBase:        "https://api.openai.com",
		Region:         "ap-northeast-1",
		SearchProvider: "tavily",
		ExtraParams:    map[string]any{"service_tier": "flex"},
	}, defaultLogMetadataLitellmParamsWhitelist)

	got, ok := metadata["litellm_params"].(map[string]any)
	if !ok {
		t.Fatalf("expected litellm_params map, got %#v", metadata["litellm_params"])
	}
	if got["model"] != "openai/gpt-4o" {
		t.Fatalf("unexpected model metadata: %#v", got["model"])
	}
	if got["api_base"] != "https://api.openai.com" {
		t.Fatalf("unexpected api_base metadata: %#v", got["api_base"])
	}
	if got["region"] != "ap-northeast-1" {
		t.Fatalf("unexpected region metadata: %#v", got["region"])
	}
	if got["search_provider"] != "tavily" {
		t.Fatalf("unexpected search_provider metadata: %#v", got["search_provider"])
	}
	if got["service_tier"] != "flex" {
		t.Fatalf("unexpected service_tier metadata: %#v", got["service_tier"])
	}
	if _, ok := got["api_key"]; ok {
		t.Fatal("api_key must not be included in litellm_params metadata")
	}
	if metadata["app_id"] != "app1" {
		t.Fatalf("unexpected app_id metadata: %#v", metadata["app_id"])
	}
}

func TestBuildLogMetadataWhitelistCanExcludeFields(t *testing.T) {
	metadata := buildLogMetadata(nil, "openai/gpt-4o", ModelConfig{
		APIBase:     "https://api.openai.com",
		ExtraParams: map[string]any{"service_tier": "flex"},
	}, []string{"model", "service_tier"})

	got, ok := metadata["litellm_params"].(map[string]any)
	if !ok {
		t.Fatalf("expected litellm_params map, got %#v", metadata["litellm_params"])
	}
	if got["model"] != "openai/gpt-4o" {
		t.Fatalf("unexpected model metadata: %#v", got["model"])
	}
	if got["service_tier"] != "flex" {
		t.Fatalf("unexpected service_tier metadata: %#v", got["service_tier"])
	}
	if _, exists := got["api_base"]; exists {
		t.Fatalf("api_base must be excluded by whitelist, got %#v", got["api_base"])
	}
}

func TestLoadJSONConfigConcurrencyControlSettings(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	configBody := []byte(`{
  "general_settings": {
    "concurrency_control_enabled": true,
    "concurrency_control_scope": "app_id+model_name",
    "concurrency_control_max_concurrency": 3,
    "concurrency_control_max_queue": 5,
    "concurrency_control_max_wait": "7s"
  }
}`)
	if err := os.WriteFile(configPath, configBody, 0644); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	cfg := &Config{
		ConcurrencyControlScope:   concurrencyScopeAppModel,
		ConcurrencyMaxConcurrency: 2,
		ConcurrencyMaxQueue:       4,
		ConcurrencyMaxWait:        3 * time.Second,
	}
	loadJSONConfig(configPath, cfg)

	if !cfg.ConcurrencyControlEnabled {
		t.Fatalf("expected concurrency control enabled")
	}
	if cfg.ConcurrencyControlScope != concurrencyScopeAppModel {
		t.Fatalf("unexpected scope: %q", cfg.ConcurrencyControlScope)
	}
	if cfg.ConcurrencyMaxConcurrency != 3 {
		t.Fatalf("unexpected max concurrency: %d", cfg.ConcurrencyMaxConcurrency)
	}
	if cfg.ConcurrencyMaxQueue != 5 {
		t.Fatalf("unexpected max queue: %d", cfg.ConcurrencyMaxQueue)
	}
	if cfg.ConcurrencyMaxWait != 7*time.Second {
		t.Fatalf("unexpected max wait: %s", cfg.ConcurrencyMaxWait)
	}
}
