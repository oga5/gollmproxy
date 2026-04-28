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

	LogRequestBody  bool
	LogResponseBody bool

	MasterKey          string
	KeyHeaderName      string
	TrustedProxyHeader string

	OpenAIAPIKey            string
	GeminiAPIKey            string
	TavilyAPIKey            string
	OpenRouterAPIKey        string
	BedrockRegion           string
	BedrockIncludeReasoning bool

	OpenAIBaseURL     string
	GeminiBaseURL     string
	TavilyBaseURL     string
	OpenRouterBaseURL string

	// ModelAliases maps model_name to the provider-prefixed model (e.g. "gemini-2.5-flash" -> "gemini/gemini-2.5-flash")
	ModelAliases map[string]string

	// ModelConfigs maps request model keys to per-model config overrides.
	// If model_name is configured, that alias is used as the key so multiple aliases can
	// target the same upstream model with different settings.
	ModelConfigs map[string]ModelConfig

	// EmbeddingModels is the set of provider-prefixed models that have mode: embedding in model_info.
	EmbeddingModels map[string]bool

	// PassThroughEndpoints holds custom pass-through proxy endpoints from config.
	PassThroughEndpoints []PassThroughEndpoint
}

// ModelConfig holds per-model configuration overrides.
type ModelConfig struct {
	APIKey      string
	APIBase     string
	Region      string
	ExtraParams map[string]interface{}
}

// PassThroughEndpoint defines a custom pass-through proxy endpoint.
type PassThroughEndpoint struct {
	Path           string            // local route path prefix (e.g. "/myapi")
	Target         string            // upstream base URL (e.g. "https://api.example.com")
	Headers        map[string]string // static headers to inject into upstream requests
	ForwardHeaders bool              // if true, forward all incoming request headers to upstream
}

// YAML config types
type yamlConfig struct {
	ModelList                 []modelListEntry     `yaml:"model_list"`
	GeneralSettings           generalSettings      `yaml:"general_settings"`
	EnvironmentVariables      map[string]string    `yaml:"environment_variables"`
	SearchTools               []searchToolEntry    `yaml:"search_tools"`
	GoogleAIStudioPassthrough passthroughAPIConfig `yaml:"google_ai_studio_passthrough"`
}

type modelListEntry struct {
	ModelName string      `yaml:"model_name"`
	Params    modelParams `yaml:"litellm_params"`
	ModelInfo modelInfo   `yaml:"model_info"`
}

type modelInfo struct {
	Mode string `yaml:"mode"`
}

type modelParams struct {
	Model       string                 `yaml:"model"`
	APIKey      string                 `yaml:"api_key"`
	APIBase     string                 `yaml:"api_base"`
	Region      string                 `yaml:"region"`
	ExtraParams map[string]interface{} `yaml:"extra_params"`
}

type generalSettings struct {
	Port                    int                       `yaml:"port"`
	LogFile                 string                    `yaml:"log_file"`
	LogRequestBody          *bool                     `yaml:"log_request_body"`
	LogResponseBody         *bool                     `yaml:"log_response_body"`
	BedrockIncludeReasoning *bool                     `yaml:"bedrock_include_reasoning"`
	MasterKey               string                    `yaml:"master_key"`
	KeyHeaderName           string                    `yaml:"litellm_key_header_name"`
	TrustedProxyHeader      string                    `yaml:"trusted_proxy_header"`
	PassThroughEndpoints    []yamlPassThroughEndpoint `yaml:"pass_through_endpoints"`
}

type yamlPassThroughEndpoint struct {
	Path           string            `yaml:"path"`
	Target         string            `yaml:"target"`
	Headers        map[string]string `yaml:"headers"`
	ForwardHeaders bool              `yaml:"forward_headers"`
}

type searchToolEntry struct {
	SearchToolName string           `yaml:"search_tool_name"`
	Params         searchToolParams `yaml:"litellm_params"`
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
		Port:              8080,
		LogFile:           "gollmproxy.log",
		LogRequestBody:    true,
		LogResponseBody:   true,
		OpenAIBaseURL:     "https://api.openai.com",
		GeminiBaseURL:     "https://generativelanguage.googleapis.com",
		TavilyBaseURL:     "https://api.tavily.com",
		OpenRouterBaseURL: "https://openrouter.ai/api",
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
	if v := os.Getenv("OPENROUTER_API_KEY"); v != "" {
		cfg.OpenRouterAPIKey = v
	}
	if v := os.Getenv("AWS_REGION"); v != "" {
		cfg.BedrockRegion = v
	} else if v := os.Getenv("AWS_DEFAULT_REGION"); v != "" {
		cfg.BedrockRegion = v
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
	if v := os.Getenv("OPENROUTER_BASE_URL"); v != "" {
		cfg.OpenRouterBaseURL = v
	}

	return cfg
}

func loadYAMLConfig(path string, cfg *Config) {
	data, err := os.ReadFile(path)
	if err != nil {
		slog.Error("failed to read config file", "path", path, "error", err)
		return
	}

	var lc yamlConfig
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
	if lc.GeneralSettings.TrustedProxyHeader != "" {
		cfg.TrustedProxyHeader = lc.GeneralSettings.TrustedProxyHeader
	}
	if lc.GeneralSettings.LogRequestBody != nil {
		cfg.LogRequestBody = *lc.GeneralSettings.LogRequestBody
	}
	if lc.GeneralSettings.LogResponseBody != nil {
		cfg.LogResponseBody = *lc.GeneralSettings.LogResponseBody
	}
	if lc.GeneralSettings.BedrockIncludeReasoning != nil {
		cfg.BedrockIncludeReasoning = *lc.GeneralSettings.BedrockIncludeReasoning
	}

	// Load pass-through endpoints
	for _, ep := range lc.GeneralSettings.PassThroughEndpoints {
		if ep.Path == "" || ep.Target == "" {
			slog.Warn("skipping pass_through_endpoint with empty path or target")
			continue
		}
		headers := make(map[string]string, len(ep.Headers))
		for k, v := range ep.Headers {
			headers[k] = resolveEnvRef(v)
		}
		cfg.PassThroughEndpoints = append(cfg.PassThroughEndpoints, PassThroughEndpoint{
			Path:           ep.Path,
			Target:         ep.Target,
			Headers:        headers,
			ForwardHeaders: ep.ForwardHeaders,
		})
		slog.Info("loaded pass_through_endpoint", "path", ep.Path, "target", ep.Target, "forward_headers", ep.ForwardHeaders)
	}

	// Extract search tool config (e.g., Tavily)
	for _, entry := range lc.SearchTools {
		apiKey := resolveEnvRef(entry.Params.APIKey)
		switch entry.Params.SearchProvider {
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

	// Extract provider config and build model alias map from model_list
	if cfg.ModelAliases == nil {
		cfg.ModelAliases = make(map[string]string)
	}
	if cfg.ModelConfigs == nil {
		cfg.ModelConfigs = make(map[string]ModelConfig)
	}
	if cfg.EmbeddingModels == nil {
		cfg.EmbeddingModels = make(map[string]bool)
	}
	for _, entry := range lc.ModelList {
		model := entry.Params.Model
		apiKey := resolveEnvRef(entry.Params.APIKey)
		apiBase := entry.Params.APIBase
		region := resolveEnvRef(entry.Params.Region)

		// Register model_name -> provider-prefixed model alias
		if entry.ModelName != "" && model != "" {
			cfg.ModelAliases[entry.ModelName] = model
		}

		// Track embedding models
		if entry.ModelInfo.Mode == "embedding" && model != "" {
			cfg.EmbeddingModels[model] = true
			if entry.ModelName != "" {
				cfg.EmbeddingModels[entry.ModelName] = true
			}
		}

		// Store per-model config overrides. model_name takes precedence as the lookup key
		// so multiple aliases can point at the same upstream model without clobbering each other.
		configKey := model
		if entry.ModelName != "" {
			configKey = entry.ModelName
		}
		if configKey != "" && (apiKey != "" || apiBase != "" || region != "" || len(entry.Params.ExtraParams) > 0) {
			cfg.ModelConfigs[configKey] = ModelConfig{
				APIKey:      apiKey,
				APIBase:     apiBase,
				Region:      region,
				ExtraParams: entry.Params.ExtraParams,
			}
		}

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
		case strings.HasPrefix(model, "openrouter/"):
			if cfg.OpenRouterAPIKey == "" && apiKey != "" {
				cfg.OpenRouterAPIKey = apiKey
			}
			if apiBase != "" {
				cfg.OpenRouterBaseURL = apiBase
			}
		case strings.HasPrefix(model, "bedrock/"):
			if cfg.BedrockRegion == "" && region != "" {
				cfg.BedrockRegion = region
			}
		}
	}
}
