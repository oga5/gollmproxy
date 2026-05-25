package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadYAMLConfigKeepsPerModelNameOverrides(t *testing.T) {
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "")

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	configBody := []byte(`model_list:
  - model_name: gpt-oss-20b-a
    litellm_params:
      model: bedrock/openai.gpt-oss-20b-1:0
      region: ap-northeast-1
  - model_name: gpt-oss-20b-b
    litellm_params:
      model: bedrock/openai.gpt-oss-20b-1:0
      region: ap-northeast-3
`)
	if err := os.WriteFile(configPath, configBody, 0644); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	cfg := &Config{}
	loadYAMLConfig(configPath, cfg)

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

func TestLoadYAMLConfigKeepsDirectModelOverridesWithoutAlias(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	configBody := []byte(`model_list:
  - litellm_params:
      model: openai/gpt-4o
      api_base: https://example.invalid
`)
	if err := os.WriteFile(configPath, configBody, 0644); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	cfg := &Config{}
	loadYAMLConfig(configPath, cfg)

	if got := cfg.ModelConfigs["openai/gpt-4o"].APIBase; got != "https://example.invalid" {
		t.Fatalf("unexpected direct model config: %q", got)
	}
}

func TestLoadYAMLConfigStoresSearchProviderInModelConfig(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	configBody := []byte(`model_list:
  - model_name: tavily-proxy
    litellm_params:
      model: openai/gpt-4o
      search_provider: tavily
`)
	if err := os.WriteFile(configPath, configBody, 0644); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	func TestLoadYAMLConfigStoresPerModelTimeout(t *testing.T) {
		dir := t.TempDir()
		configPath := filepath.Join(dir, "config.yaml")
		configBody := []byte(`model_list:
	  - model_name: gpt-4o-fast
	    litellm_params:
	      model: openai/gpt-4o
	      timeout: 30s
	`)
		if err := os.WriteFile(configPath, configBody, 0644); err != nil {
			t.Fatalf("failed to write temp config: %v", err)
		}

		cfg := &Config{}
		loadYAMLConfig(configPath, cfg)

		if got := cfg.ModelConfigs["gpt-4o-fast"].UpstreamNonStreamTimeout; got != 30*time.Second {
			t.Fatalf("unexpected per-model timeout: got %v want %v", got, 30*time.Second)
		}
	}

	cfg := &Config{}
	loadYAMLConfig(configPath, cfg)

	if got := cfg.ModelConfigs["tavily-proxy"].SearchProvider; got != "tavily" {
		t.Fatalf("unexpected search_provider: %q", got)
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
	})

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
	if _, ok := got["api_key"]; ok {
		t.Fatal("api_key must not be included in litellm_params metadata")
	}
	if metadata["app_id"] != "app1" {
		t.Fatalf("unexpected app_id metadata: %#v", metadata["app_id"])
	}
}
