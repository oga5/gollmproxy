package main

import (
	"flag"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Port    int
	LogFile string

	MasterKey     string
	KeyHeaderName string

	OpenAIAPIKey string
	GeminiAPIKey string
	TavilyAPIKey string

	OpenAIBaseURL string
	GeminiBaseURL string
	TavilyBaseURL string
}

// LiteLLM-compatible YAML config types
type litellmConfig struct {
	ModelList                  []modelListEntry     `yaml:"model_list"`
	GeneralSettings            generalSettings      `yaml:"general_settings"`
	EnvironmentVariables       map[string]string    `yaml:"environment_variables"`
	SearchTools                []searchToolEntry    `yaml:"search_tools"`
	GoogleAIStudioPassthrough  passthroughAPIConfig `yaml:"google_ai_studio_passthrough"`
}

type modelListEntry struct {
	ModelName     string        `yaml:"model_name"`
	LiteLLMParams litellmParams `yaml:"litellm_params"`
}

type litellmParams struct {
	Model   string `yaml:"model"`
	APIKey  string `yaml:"api_key"`
	APIBase string `yaml:"api_base"`
}

type generalSettings struct {
	Port              int    `yaml:"port"`
	LogFile           string `yaml:"log_file"`
	MasterKey         string `yaml:"master_key"`
	KeyHeaderName     string `yaml:"litellm_key_header_name"`
}

type searchToolEntry struct {
	SearchToolName string             `yaml:"search_tool_name"`
	LiteLLMParams  searchToolParams   `yaml:"litellm_params"`
}

type searchToolParams struct {
	SearchProvider string `yaml:"search_provider"`
	APIKey         string `yaml:"api_key"`
}

type passthroughAPIConfig struct {
	APIKey string `yaml:"api_key"`
}

// resolveEnvRef resolves "os.environ/VARNAME" references to environment variable values.
func resolveEnvRef(value string) string {
	if after, ok := strings.CutPrefix(value, "os.environ/"); ok {
		return os.Getenv(after)
	}
	return value
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
	if v := os.Getenv("LITELLM_MASTER_KEY"); v != "" {
		cfg.MasterKey = v
	}
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

	var lc litellmConfig
	if err := yaml.Unmarshal(data, &lc); err != nil {
		slog.Error("failed to parse config file", "path", path, "error", err)
		return
	}

	// Set environment variables from config (skip if already set — OS env takes priority)
	for key, value := range lc.EnvironmentVariables {
		if os.Getenv(key) == "" {
			os.Setenv(key, value)
		}
	}

	// Apply general_settings
	if lc.GeneralSettings.Port != 0 {
		cfg.Port = lc.GeneralSettings.Port
	}
	if lc.GeneralSettings.LogFile != "" {
		cfg.LogFile = lc.GeneralSettings.LogFile
	}
	if v := resolveEnvRef(lc.GeneralSettings.MasterKey); v != "" {
		cfg.MasterKey = v
	}
	if lc.GeneralSettings.KeyHeaderName != "" {
		cfg.KeyHeaderName = lc.GeneralSettings.KeyHeaderName
	}

	// Extract search tool config (e.g., Tavily)
	for _, entry := range lc.SearchTools {
		apiKey := resolveEnvRef(entry.LiteLLMParams.APIKey)
		switch entry.LiteLLMParams.SearchProvider {
		case "tavily":
			if cfg.TavilyAPIKey == "" && apiKey != "" {
				cfg.TavilyAPIKey = apiKey
			}
		}
	}

	// Google AI Studio passthrough → GeminiAPIKey
	if v := resolveEnvRef(lc.GoogleAIStudioPassthrough.APIKey); v != "" && cfg.GeminiAPIKey == "" {
		cfg.GeminiAPIKey = v
	}

	// Extract provider config from model_list
	for _, entry := range lc.ModelList {
		model := entry.LiteLLMParams.Model
		apiKey := resolveEnvRef(entry.LiteLLMParams.APIKey)
		apiBase := entry.LiteLLMParams.APIBase

		switch {
		case strings.HasPrefix(model, "openai/"):
			if cfg.OpenAIAPIKey == "" && apiKey != "" {
				cfg.OpenAIAPIKey = apiKey
			}
			if apiBase != "" {
				cfg.OpenAIBaseURL = apiBase
			}
		case strings.HasPrefix(model, "gemini/"):
			if cfg.GeminiAPIKey == "" && apiKey != "" {
				cfg.GeminiAPIKey = apiKey
			}
			if apiBase != "" {
				cfg.GeminiBaseURL = apiBase
			}
		}
	}
}
