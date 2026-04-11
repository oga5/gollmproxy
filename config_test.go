package main

import (
	"os"
	"path/filepath"
	"testing"
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
