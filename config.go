package main

import (
	"encoding/json"
	"flag"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Port    int
	LogFile string

	ServerReadTimeout  time.Duration
	ServerWriteTimeout time.Duration
	ServerIdleTimeout  time.Duration

	LogRequestBody  bool
	LogResponseBody bool

	MasterKey          string
	KeyHeaderName      string
	TrustedProxyHeader string
	// TrustedProxyCIDRs lists the CIDR ranges of proxies allowed to set
	// TrustedProxyHeader. Requests from any other peer have the header ignored.
	TrustedProxyCIDRs []*net.IPNet

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

	// PostgresDSN is the optional DSN for PostgreSQL request logging.
	// When set, log entries are written to llm_logs / llm_payloads tables in addition to the log file.
	PostgresDSN string

	// TokenBudgetEnabled enables app_id/model_name daily token budget checks.
	TokenBudgetEnabled bool

	// TokenBudgetStore manages token budget checks and usage updates.
	// It is initialized at runtime when TokenBudgetEnabled is true.
	TokenBudgetStore TokenBudgetStore

	ConcurrencyControlEnabled bool
	ConcurrencyControlScope   string
	ConcurrencyMaxConcurrency int
	ConcurrencyMaxQueue       int
	ConcurrencyMaxWait        time.Duration
	ConcurrencyController     *KeyedConcurrencyController

	// PassThroughEndpoints holds custom pass-through proxy endpoints from config.
	PassThroughEndpoints []PassThroughEndpoint

	// RequiredMetadataKeys lists metadata keys that must be present and non-empty
	// in every /v1/chat/completions request. Missing keys result in a 400 error.
	RequiredMetadataKeys []string

	// LogMetadataLitellmParamsWhitelist controls which server-side litellm_params
	// keys are written into log metadata.
	LogMetadataLitellmParamsWhitelist []string

	UpstreamNonStreamTimeout      time.Duration
	UpstreamDialTimeout           time.Duration
	UpstreamKeepAlive             time.Duration
	UpstreamTLSHandshakeTimeout   time.Duration
	UpstreamResponseHeaderTimeout time.Duration
	UpstreamExpectContinueTimeout time.Duration
	UpstreamIdleConnTimeout       time.Duration
}

// ModelConfig holds per-model configuration overrides.
type ModelConfig struct {
	APIKey         string
	APIBase        string
	Region         string
	SearchProvider string
	ExtraParams    map[string]interface{}
	Timeout        time.Duration
}

// PassThroughEndpoint defines a custom pass-through proxy endpoint.
type PassThroughEndpoint struct {
	Path           string            // local route path prefix (e.g. "/myapi")
	Target         string            // upstream base URL (e.g. "https://api.example.com")
	Headers        map[string]string // static headers to inject into upstream requests
	ForwardHeaders bool              // if true, forward all incoming request headers to upstream
}

// JSON config types
type jsonConfig struct {
	ModelList                 []modelListEntry     `json:"model_list"`
	GeneralSettings           generalSettings      `json:"general_settings"`
	EnvironmentVariables      map[string]string    `json:"environment_variables"`
	SearchTools               []searchToolEntry    `json:"search_tools"`
	GoogleAIStudioPassthrough passthroughAPIConfig `json:"google_ai_studio_passthrough"`
}

type modelListEntry struct {
	ModelName string      `json:"model_name"`
	Params    modelParams `json:"proxy_params"`
	ModelInfo modelInfo   `json:"model_info"`
}

type modelInfo struct {
	Mode string `json:"mode"`
}

type modelParams struct {
	Model          string                 `json:"model"`
	APIKey         string                 `json:"api_key"`
	APIBase        string                 `json:"api_base"`
	Region         string                 `json:"region"`
	SearchProvider string                 `json:"search_provider"`
	AwsRegionName  string                 `json:"aws_region_name"`
	ExtraParams    map[string]interface{} `json:"extra_params"`
	Timeout        string                 `json:"timeout"`
}

type generalSettings struct {
	Port                          int                       `json:"port"`
	LogFile                       string                    `json:"log_file"`
	ServerReadTimeout             string                    `json:"server_read_timeout"`
	ServerWriteTimeout            string                    `json:"server_write_timeout"`
	ServerIdleTimeout             string                    `json:"server_idle_timeout"`
	LogRequestBody                *bool                     `json:"log_request_body"`
	LogResponseBody               *bool                     `json:"log_response_body"`
	BedrockIncludeReasoning       *bool                     `json:"bedrock_include_reasoning"`
	MasterKey                     string                    `json:"master_key"`
	KeyHeaderName                 string                    `json:"litellm_key_header_name"`
	TrustedProxyHeader            string                    `json:"trusted_proxy_header"`
	TrustedProxyCIDRs             []string                  `json:"trusted_proxy_cidrs"`
	PostgresDSN                   string                    `json:"postgres_dsn"`
	TokenBudgetEnabled            *bool                     `json:"token_budget_enabled"`
	ConcurrencyControlEnabled     *bool                     `json:"concurrency_control_enabled"`
	ConcurrencyControlScope       string                    `json:"concurrency_control_scope"`
	ConcurrencyControlMax         int                       `json:"concurrency_control_max_concurrency"`
	ConcurrencyControlMaxQueue    int                       `json:"concurrency_control_max_queue"`
	ConcurrencyControlMaxWait     string                    `json:"concurrency_control_max_wait"`
	PassThroughEndpoints          []jsonPassThroughEndpoint `json:"pass_through_endpoints"`
	RequiredMetadataKeys          []string                  `json:"required_metadata_keys"`
	LogMetadataLitellmParams      []string                  `json:"log_metadata_litellm_params_whitelist"`
	UpstreamNonStreamTimeout      string                    `json:"upstream_non_stream_timeout"`
	UpstreamDialTimeout           string                    `json:"upstream_dial_timeout"`
	UpstreamKeepAlive             string                    `json:"upstream_keep_alive_timeout"`
	UpstreamTLSHandshakeTimeout   string                    `json:"upstream_tls_handshake_timeout"`
	UpstreamResponseHeaderTimeout string                    `json:"upstream_response_header_timeout"`
	UpstreamExpectContinueTimeout string                    `json:"upstream_expect_continue_timeout"`
	UpstreamIdleConnTimeout       string                    `json:"upstream_idle_conn_timeout"`
}

type jsonPassThroughEndpoint struct {
	Path           string            `json:"path"`
	Target         string            `json:"target"`
	Headers        map[string]string `json:"headers"`
	ForwardHeaders bool              `json:"forward_headers"`
}

type searchToolEntry struct {
	SearchToolName string           `json:"search_tool_name"`
	Params         searchToolParams `json:"proxy_params"`
}

type searchToolParams struct {
	SearchProvider string `json:"search_provider"`
	APIKey         string `json:"api_key"`
}

type passthroughAPIConfig struct {
	APIKey string `json:"api_key"`
}

var defaultLogMetadataLitellmParamsWhitelist = []string{
	"model",
	"api_base",
	"region",
	"search_provider",
	"service_tier",
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
		ServerReadTimeout: 30 * time.Second,
		// streaming responses may legitimately stay open longer than any fixed write deadline
		ServerWriteTimeout:            0,
		ServerIdleTimeout:             2 * time.Minute,
		LogRequestBody:                true,
		LogResponseBody:               true,
		OpenAIBaseURL:                 "https://api.openai.com",
		GeminiBaseURL:                 "https://generativelanguage.googleapis.com",
		TavilyBaseURL:                 "https://api.tavily.com",
		OpenRouterBaseURL:             "https://openrouter.ai/api",
		UpstreamNonStreamTimeout:      defaultUpstreamNonStreamTimeout,
		UpstreamDialTimeout:           defaultUpstreamDialTimeout,
		UpstreamKeepAlive:             defaultUpstreamKeepAlive,
		UpstreamTLSHandshakeTimeout:   defaultUpstreamTLSHandshakeTimeout,
		UpstreamResponseHeaderTimeout: defaultUpstreamResponseHeaderTimeout,
		UpstreamExpectContinueTimeout: defaultUpstreamExpectContinueTimeout,
		UpstreamIdleConnTimeout:       defaultUpstreamIdleConnTimeout,
		ConcurrencyControlScope:       concurrencyScopeAppModel,
		ConcurrencyMaxConcurrency:     2,
		ConcurrencyMaxQueue:           4,
		ConcurrencyMaxWait:            3 * time.Second,
		LogMetadataLitellmParamsWhitelist: append([]string(nil),
			defaultLogMetadataLitellmParamsWhitelist...),
	}

	flag.StringVar(&configFile, "config", "", "config file path (JSON)")
	flag.IntVar(&cfg.Port, "port", cfg.Port, "server port")
	flag.StringVar(&cfg.LogFile, "logfile", cfg.LogFile, "request log file path")
	flag.Parse()

	// Load JSON config file if specified
	if configFile != "" {
		loadJSONConfig(configFile, cfg)
	}

	// Environment variables override JSON / defaults
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
	if v := os.Getenv("POSTGRES_DSN"); v != "" {
		cfg.PostgresDSN = v
	} else if cfg.PostgresDSN == "" {
		// Fall back to standard libpq PG* environment variables.
		if dsn := buildPostgresDSNFromEnv(); dsn != "" {
			cfg.PostgresDSN = dsn
		}
	}
	if v := os.Getenv("TOKEN_BUDGET_ENABLED"); v != "" {
		if enabled, err := strconv.ParseBool(v); err == nil {
			cfg.TokenBudgetEnabled = enabled
		}
	}
	if v := os.Getenv("CONCURRENCY_CONTROL_ENABLED"); v != "" {
		if enabled, err := strconv.ParseBool(v); err == nil {
			cfg.ConcurrencyControlEnabled = enabled
		}
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

	cfg.ConcurrencyControlScope = normalizeConcurrencyScope(cfg.ConcurrencyControlScope)
	if cfg.ConcurrencyControlEnabled {
		controller, err := NewKeyedConcurrencyController(cfg.ConcurrencyMaxConcurrency, cfg.ConcurrencyMaxQueue)
		if err != nil {
			slog.Warn("invalid concurrency control config; disabling", "error", err)
			cfg.ConcurrencyControlEnabled = false
		} else {
			cfg.ConcurrencyController = controller
		}
	}

	return cfg
}

func loadJSONConfig(path string, cfg *Config) {
	data, err := os.ReadFile(path)
	if err != nil {
		slog.Error("failed to read config file", "path", path, "error", err)
		return
	}

	var lc jsonConfig
	if err := json.Unmarshal(data, &lc); err != nil {
		slog.Error("failed to parse JSON config file", "path", path, "error", err)
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
	applyDurationSetting("server_read_timeout", lc.GeneralSettings.ServerReadTimeout, &cfg.ServerReadTimeout)
	applyDurationSetting("server_write_timeout", lc.GeneralSettings.ServerWriteTimeout, &cfg.ServerWriteTimeout)
	applyDurationSetting("server_idle_timeout", lc.GeneralSettings.ServerIdleTimeout, &cfg.ServerIdleTimeout)
	if v := resolveEnvRef(lc.GeneralSettings.MasterKey); v != "" {
		cfg.MasterKey = v
	}
	if lc.GeneralSettings.KeyHeaderName != "" {
		cfg.KeyHeaderName = lc.GeneralSettings.KeyHeaderName
	}
	if lc.GeneralSettings.TrustedProxyHeader != "" {
		cfg.TrustedProxyHeader = lc.GeneralSettings.TrustedProxyHeader
	}
	for _, c := range lc.GeneralSettings.TrustedProxyCIDRs {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		// Allow bare IPs by treating them as /32 or /128.
		if !strings.Contains(c, "/") {
			if ip := net.ParseIP(c); ip != nil {
				if ip.To4() != nil {
					c += "/32"
				} else {
					c += "/128"
				}
			}
		}
		_, ipnet, err := net.ParseCIDR(c)
		if err != nil {
			slog.Warn("invalid trusted_proxy_cidrs entry, skipping", "value", c, "error", err)
			continue
		}
		cfg.TrustedProxyCIDRs = append(cfg.TrustedProxyCIDRs, ipnet)
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
	if v := resolveEnvRef(lc.GeneralSettings.PostgresDSN); v != "" {
		cfg.PostgresDSN = v
	}
	if lc.GeneralSettings.TokenBudgetEnabled != nil {
		cfg.TokenBudgetEnabled = *lc.GeneralSettings.TokenBudgetEnabled
	}
	if lc.GeneralSettings.ConcurrencyControlEnabled != nil {
		cfg.ConcurrencyControlEnabled = *lc.GeneralSettings.ConcurrencyControlEnabled
	}
	if lc.GeneralSettings.ConcurrencyControlScope != "" {
		cfg.ConcurrencyControlScope = normalizeConcurrencyScope(lc.GeneralSettings.ConcurrencyControlScope)
	}
	if lc.GeneralSettings.ConcurrencyControlMax > 0 {
		cfg.ConcurrencyMaxConcurrency = lc.GeneralSettings.ConcurrencyControlMax
	}
	if lc.GeneralSettings.ConcurrencyControlMaxQueue >= 0 {
		cfg.ConcurrencyMaxQueue = lc.GeneralSettings.ConcurrencyControlMaxQueue
	}
	applyDurationSetting("concurrency_control_max_wait", lc.GeneralSettings.ConcurrencyControlMaxWait, &cfg.ConcurrencyMaxWait)
	if len(lc.GeneralSettings.RequiredMetadataKeys) > 0 {
		cfg.RequiredMetadataKeys = lc.GeneralSettings.RequiredMetadataKeys
	}
	if len(lc.GeneralSettings.LogMetadataLitellmParams) > 0 {
		cfg.LogMetadataLitellmParamsWhitelist = lc.GeneralSettings.LogMetadataLitellmParams
	}
	applyDurationSetting("upstream_non_stream_timeout", lc.GeneralSettings.UpstreamNonStreamTimeout, &cfg.UpstreamNonStreamTimeout)
	applyDurationSetting("upstream_dial_timeout", lc.GeneralSettings.UpstreamDialTimeout, &cfg.UpstreamDialTimeout)
	applyDurationSetting("upstream_keep_alive_timeout", lc.GeneralSettings.UpstreamKeepAlive, &cfg.UpstreamKeepAlive)
	applyDurationSetting("upstream_tls_handshake_timeout", lc.GeneralSettings.UpstreamTLSHandshakeTimeout, &cfg.UpstreamTLSHandshakeTimeout)
	applyDurationSetting("upstream_response_header_timeout", lc.GeneralSettings.UpstreamResponseHeaderTimeout, &cfg.UpstreamResponseHeaderTimeout)
	applyDurationSetting("upstream_expect_continue_timeout", lc.GeneralSettings.UpstreamExpectContinueTimeout, &cfg.UpstreamExpectContinueTimeout)
	applyDurationSetting("upstream_idle_conn_timeout", lc.GeneralSettings.UpstreamIdleConnTimeout, &cfg.UpstreamIdleConnTimeout)

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
		searchProvider := resolveEnvRef(entry.Params.SearchProvider)
		timeout := time.Duration(0)
		if v := resolveEnvRef(entry.Params.AwsRegionName); v != "" {
			region = v
		}
		if entry.Params.Timeout != "" {
			d, err := time.ParseDuration(entry.Params.Timeout)
			if err != nil {
				slog.Warn("invalid model timeout in config, ignoring", "model_name", entry.ModelName, "model", model, "value", entry.Params.Timeout, "error", err)
			} else if d > 0 {
				timeout = d
			}
		}

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
		if configKey != "" && (apiKey != "" || apiBase != "" || region != "" || searchProvider != "" || len(entry.Params.ExtraParams) > 0 || timeout > 0) {
			cfg.ModelConfigs[configKey] = ModelConfig{
				APIKey:         apiKey,
				APIBase:        apiBase,
				Region:         region,
				SearchProvider: searchProvider,
				ExtraParams:    entry.Params.ExtraParams,
				Timeout:        timeout,
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
		case strings.HasPrefix(model, "bedrock/"), strings.HasPrefix(model, "bedrock_openai/"):
			if cfg.BedrockRegion == "" && region != "" {
				cfg.BedrockRegion = region
			}
		}
	}
}

func applyDurationSetting(name, value string, dst *time.Duration) {
	if value == "" {
		return
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		slog.Warn("invalid duration in config, keeping current/default value", "name", name, "value", value, "error", err)
		return
	}
	*dst = d
}

// buildPostgresDSNFromEnv constructs a key=value connection string from the
// standard libpq PG* environment variables. Returns an empty string if none
// of the relevant variables are set.
func buildPostgresDSNFromEnv() string {
	params := []struct{ key, env string }{
		{"host", "PGHOST"},
		{"port", "PGPORT"},
		{"user", "PGUSER"},
		{"password", "PGPASSWORD"},
		{"dbname", "PGDATABASE"},
		{"sslmode", "PGSSLMODE"},
	}

	var parts []string
	for _, p := range params {
		if v := os.Getenv(p.env); v != "" {
			parts = append(parts, p.key+"="+pgEscape(v))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " ")
}

// pgEscape escapes a value for use in a libpq key=value connection string.
// Values containing spaces, single quotes, or backslashes must be single-quoted.
func pgEscape(v string) string {
	if !strings.ContainsAny(v, " '\\") {
		return v
	}
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, `'`, `\'`)
	return "'" + v + "'"
}
