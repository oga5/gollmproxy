package main

import (
	"flag"
	"log/slog"
	"os"
	"strconv"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Port    int    `yaml:"port"`
	LogFile string `yaml:"log_file"`

	OpenAIAPIKey string `yaml:"openai_api_key"`
	GeminiAPIKey string `yaml:"gemini_api_key"`
	TavilyAPIKey string `yaml:"tavily_api_key"`

	OpenAIBaseURL string `yaml:"openai_base_url"`
	GeminiBaseURL string `yaml:"gemini_base_url"`
	TavilyBaseURL string `yaml:"tavily_base_url"`
}

func LoadConfig() *Config {
	var configFile string

	cfg := &Config{
		Port:          8080,
		LogFile:       "gollmproxy.log",
		OpenAIBaseURL: "https://api.openai.com",
		GeminiBaseURL: "https://generativelanguage.googleapis.com",
		TavilyBaseURL: "https://api.tavily.com",
	}

	flag.StringVar(&configFile, "config", "", "config file path (YAML)")
	flag.IntVar(&cfg.Port, "port", cfg.Port, "server port")
	flag.StringVar(&cfg.LogFile, "logfile", cfg.LogFile, "request log file path")
	flag.Parse()

	// Load YAML config file if specified
	if configFile != "" {
		loadYAMLConfig(configFile, cfg)
	}

	// Environment variables override YAML / defaults
	if v := os.Getenv("PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			cfg.Port = p
		}
	}
	if v := os.Getenv("LOG_FILE"); v != "" {
		cfg.LogFile = v
	}

	if v := os.Getenv("OPENAI_API_KEY"); v != "" {
		cfg.OpenAIAPIKey = v
	}
	if v := os.Getenv("GEMINI_API_KEY"); v != "" {
		cfg.GeminiAPIKey = v
	}
	if v := os.Getenv("TAVILY_API_KEY"); v != "" {
		cfg.TavilyAPIKey = v
	}

	if v := os.Getenv("OPENAI_BASE_URL"); v != "" {
		cfg.OpenAIBaseURL = v
	}
	if v := os.Getenv("GEMINI_BASE_URL"); v != "" {
		cfg.GeminiBaseURL = v
	}
	if v := os.Getenv("TAVILY_BASE_URL"); v != "" {
		cfg.TavilyBaseURL = v
	}

	// Warn about missing API keys
	if cfg.OpenAIAPIKey == "" {
		slog.Warn("OPENAI_API_KEY not set")
	}
	if cfg.GeminiAPIKey == "" {
		slog.Warn("GEMINI_API_KEY not set")
	}
	if cfg.TavilyAPIKey == "" {
		slog.Warn("TAVILY_API_KEY not set")
	}

	return cfg
}

func loadYAMLConfig(path string, cfg *Config) {
	data, err := os.ReadFile(path)
	if err != nil {
		slog.Error("failed to read config file", "path", path, "error", err)
		return
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		slog.Error("failed to parse config file", "path", path, "error", err)
	}
}
