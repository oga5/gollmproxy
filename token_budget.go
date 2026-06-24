package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	_ "github.com/lib/pq" // registers postgres driver
)

var (
	ErrBudgetIdentifiersRequired = errors.New("app_id and model_name are required")
	ErrBudgetNotConfigured       = errors.New("token budget not configured")
	ErrBudgetExceeded            = errors.New("token budget exceeded")
	ErrInvalidTokenUsage         = errors.New("token usage must be non-negative")
)

const dailyUsageDateFormat = "2006-01-02"

type TokenBudgetStore interface {
	CheckAllowed(ctx context.Context, appID, modelName string, day time.Time) error
	AddUsage(ctx context.Context, appID, modelName string, tokens int, day time.Time) error
}

type cachedBudget struct {
	tokensPerDay int64
	found        bool
	expiresAt    time.Time
}

type PostgresTokenBudgetStore struct {
	db    *sql.DB
	ttl   time.Duration
	mu    sync.RWMutex
	cache map[string]cachedBudget
}

func NewPostgresTokenBudgetStore(dsn string, ttl time.Duration) (*PostgresTokenBudgetStore, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}

	store := &PostgresTokenBudgetStore{
		db:    db,
		ttl:   ttl,
		cache: make(map[string]cachedBudget),
	}
	if err := store.ensureSchema(); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func (s *PostgresTokenBudgetStore) Close() error {
	return s.db.Close()
}

func (s *PostgresTokenBudgetStore) ensureSchema() error {
	// Schema is created opportunistically on startup. If schema changes are needed
	// later, apply explicit migrations (ALTER TABLE etc.) before deployment.
	const createBudgetsTable = `
CREATE TABLE IF NOT EXISTS token_budgets (
  app_id text NOT NULL,
  model_name text NOT NULL,
  tokens_per_day bigint NOT NULL CHECK (tokens_per_day >= 0),
  PRIMARY KEY (app_id, model_name)
)`

	const createUsageTable = `
CREATE TABLE IF NOT EXISTS token_usage_daily (
  usage_date date NOT NULL,
  app_id text NOT NULL,
  model_name text NOT NULL,
  token bigint NOT NULL CHECK (token >= 0),
  PRIMARY KEY (usage_date, app_id, model_name)
)`

	if _, err := s.db.Exec(createBudgetsTable); err != nil {
		return err
	}
	if _, err := s.db.Exec(createUsageTable); err != nil {
		return err
	}
	return nil
}

func (s *PostgresTokenBudgetStore) CheckAllowed(ctx context.Context, appID, modelName string, day time.Time) error {
	appID = strings.TrimSpace(appID)
	modelName = strings.TrimSpace(modelName)
	if appID == "" || modelName == "" {
		return ErrBudgetIdentifiersRequired
	}

	budget, found, err := s.getBudget(ctx, appID, modelName)
	if err != nil {
		return err
	}
	if !found {
		return ErrBudgetNotConfigured
	}

	used, err := s.getTodayUsage(ctx, appID, modelName, day)
	if err != nil {
		return err
	}
	if used >= budget {
		return ErrBudgetExceeded
	}
	return nil
}

// getBudget returns the daily token budget for (appID, modelName).
// Results are cached with the configured TTL. A missing row (not found) is
// also cached as a negative entry so repeated misses avoid DB round-trips.
func (s *PostgresTokenBudgetStore) getBudget(ctx context.Context, appID, modelName string) (int64, bool, error) {
	key := appID + "\x00" + modelName
	now := time.Now()

	if s.ttl > 0 {
		s.mu.RLock()
		entry, ok := s.cache[key]
		s.mu.RUnlock()
		if ok && now.Before(entry.expiresAt) {
			return entry.tokensPerDay, entry.found, nil
		}
	}

	const q = `SELECT tokens_per_day FROM token_budgets WHERE app_id = $1 AND model_name = $2`
	var budget int64
	err := s.db.QueryRowContext(ctx, q, appID, modelName).Scan(&budget)
	if errors.Is(err, sql.ErrNoRows) {
		if s.ttl > 0 {
			s.mu.Lock()
			s.cache[key] = cachedBudget{found: false, expiresAt: now.Add(s.ttl)}
			s.mu.Unlock()
		}
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}

	if s.ttl > 0 {
		s.mu.Lock()
		s.cache[key] = cachedBudget{tokensPerDay: budget, found: true, expiresAt: now.Add(s.ttl)}
		s.mu.Unlock()
	}
	return budget, true, nil
}

// getTodayUsage returns the current day's token usage from token_usage_daily.
// This is never cached so that usage counts are always up to date.
func (s *PostgresTokenBudgetStore) getTodayUsage(ctx context.Context, appID, modelName string, day time.Time) (int64, error) {
	const q = `SELECT COALESCE(token, 0) FROM token_usage_daily WHERE usage_date = $1 AND app_id = $2 AND model_name = $3`
	usageDate := day.UTC().Format(dailyUsageDateFormat)
	var used int64
	err := s.db.QueryRowContext(ctx, q, usageDate, appID, modelName).Scan(&used)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return used, err
}

func (s *PostgresTokenBudgetStore) AddUsage(ctx context.Context, appID, modelName string, tokens int, day time.Time) error {
	appID = strings.TrimSpace(appID)
	modelName = strings.TrimSpace(modelName)
	if appID == "" || modelName == "" {
		return ErrBudgetIdentifiersRequired
	}
	if tokens < 0 {
		return ErrInvalidTokenUsage
	}
	if tokens == 0 {
		return nil
	}

	const q = `
INSERT INTO token_usage_daily (usage_date, app_id, model_name, token)
VALUES ($1, $2, $3, $4)
ON CONFLICT (usage_date, app_id, model_name)
DO UPDATE SET token = token_usage_daily.token + EXCLUDED.token`

	usageDate := day.UTC().Format(dailyUsageDateFormat)
	if _, err := s.db.ExecContext(ctx, q, usageDate, appID, modelName, tokens); err != nil {
		return err
	}
	return nil
}

func extractBudgetIdentifiers(metadata map[string]any, modelName string) (string, string, error) {
	appID, ok := metadataStringValue(metadata, "app_id")
	if !ok {
		return "", "", fmt.Errorf("%w: app_id", ErrBudgetIdentifiersRequired)
	}
	modelName = strings.TrimSpace(modelName)
	if modelName == "" {
		return "", "", fmt.Errorf("%w: model_name", ErrBudgetIdentifiersRequired)
	}
	return appID, modelName, nil
}

func metadataStringValue(metadata map[string]any, key string) (string, bool) {
	if len(metadata) == 0 {
		return "", false
	}
	v, ok := metadata[key]
	if !ok || v == nil {
		return "", false
	}
	s, ok := v.(string)
	if !ok {
		return "", false
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false
	}
	return s, true
}
