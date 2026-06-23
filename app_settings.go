package main

import (
	"context"
	"database/sql"
	"log/slog"
	"strings"
	"sync"
	"time"
)

const (
	defaultAppLogLevel  = "info"
	appSettingsCacheTTL = 30 * time.Second
)

const appLogSettingsKey contextKey = "app_log_settings"

type AppSettings struct {
	LogLevel        string
	LogRequestBody  *bool
	LogResponseBody *bool
}

type AppSettingsStore interface {
	Get(ctx context.Context, appID string) (AppSettings, bool, error)
	Close() error
}

type EffectiveAppLogSettings struct {
	LogLevel        slog.Level
	LogRequestBody  bool
	LogResponseBody bool
}

func (s EffectiveAppLogSettings) Enabled(level slog.Level) bool {
	return level >= s.LogLevel
}

type cachedAppSettings struct {
	settings  AppSettings
	found     bool
	expiresAt time.Time
}

type PostgresAppSettingsStore struct {
	db    *sql.DB
	ttl   time.Duration
	mu    sync.RWMutex
	cache map[string]cachedAppSettings
}

func NewPostgresAppSettingsStore(dsn string) (*PostgresAppSettingsStore, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}

	store := &PostgresAppSettingsStore{
		db:    db,
		ttl:   appSettingsCacheTTL,
		cache: make(map[string]cachedAppSettings),
	}
	if err := store.ensureSchema(); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func (s *PostgresAppSettingsStore) Close() error {
	return s.db.Close()
}

func (s *PostgresAppSettingsStore) ensureSchema() error {
	const createAppSettingsTable = `
CREATE TABLE IF NOT EXISTS app_settings (
  app_id text PRIMARY KEY,
  log_level text NOT NULL DEFAULT 'info' CHECK (log_level IN ('debug', 'info', 'warn', 'error')),
  log_request_body boolean,
  log_response_body boolean,
  updated_at timestamptz NOT NULL DEFAULT now()
)`

	_, err := s.db.Exec(createAppSettingsTable)
	return err
}

func (s *PostgresAppSettingsStore) Get(ctx context.Context, appID string) (AppSettings, bool, error) {
	appID = strings.TrimSpace(appID)
	if appID == "" {
		return AppSettings{}, false, nil
	}

	now := time.Now()
	s.mu.RLock()
	cached, ok := s.cache[appID]
	s.mu.RUnlock()
	if ok && now.Before(cached.expiresAt) {
		return cached.settings, cached.found, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	cached, ok = s.cache[appID]
	if ok && now.Before(cached.expiresAt) {
		return cached.settings, cached.found, nil
	}

	const q = `
SELECT log_level, log_request_body, log_response_body
FROM app_settings
WHERE app_id = $1`

	var (
		logLevel        string
		logRequestBody  sql.NullBool
		logResponseBody sql.NullBool
	)
	err := s.db.QueryRowContext(ctx, q, appID).Scan(&logLevel, &logRequestBody, &logResponseBody)
	if err == sql.ErrNoRows {
		s.cache[appID] = cachedAppSettings{
			settings:  AppSettings{},
			found:     false,
			expiresAt: now.Add(s.ttl),
		}
		return AppSettings{}, false, nil
	}
	if err != nil {
		return AppSettings{}, false, err
	}

	settings := AppSettings{
		LogLevel: normalizeAppLogLevel(logLevel),
	}
	if logRequestBody.Valid {
		v := logRequestBody.Bool
		settings.LogRequestBody = &v
	}
	if logResponseBody.Valid {
		v := logResponseBody.Bool
		settings.LogResponseBody = &v
	}

	s.cache[appID] = cachedAppSettings{
		settings:  settings,
		found:     true,
		expiresAt: now.Add(s.ttl),
	}
	return settings, true, nil
}

func normalizeAppLogLevel(level string) string {
	normalized := strings.ToLower(strings.TrimSpace(level))
	switch normalized {
	case "debug", "warn", "error":
		return normalized
	case "info":
		return "info"
	default:
		return defaultAppLogLevel
	}
}

func appLogLevelFromString(level string) slog.Level {
	switch normalizeAppLogLevel(level) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func defaultEffectiveAppLogSettings(cfg *Config) EffectiveAppLogSettings {
	return EffectiveAppLogSettings{
		LogLevel:        slog.LevelInfo,
		LogRequestBody:  cfg.LogRequestBody,
		LogResponseBody: cfg.LogResponseBody,
	}
}

func resolveEffectiveAppLogSettings(ctx context.Context, cfg *Config, metadata map[string]any) (EffectiveAppLogSettings, error) {
	settings := defaultEffectiveAppLogSettings(cfg)

	if cfg.AppSettingsStore == nil {
		return settings, nil
	}

	appID, ok := metadataStringValue(metadata, "app_id")
	if !ok {
		return settings, nil
	}

	appSettings, found, err := cfg.AppSettingsStore.Get(ctx, appID)
	if err != nil {
		return settings, err
	}
	if !found {
		return settings, nil
	}

	settings.LogLevel = appLogLevelFromString(appSettings.LogLevel)
	if appSettings.LogRequestBody != nil {
		settings.LogRequestBody = *appSettings.LogRequestBody
	}
	if appSettings.LogResponseBody != nil {
		settings.LogResponseBody = *appSettings.LogResponseBody
	}

	return settings, nil
}

func appLogSettingsFromContext(ctx context.Context, cfg *Config) EffectiveAppLogSettings {
	if ctx != nil {
		if settings, ok := ctx.Value(appLogSettingsKey).(EffectiveAppLogSettings); ok {
			return settings
		}
	}
	return defaultEffectiveAppLogSettings(cfg)
}
