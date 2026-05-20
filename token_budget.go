package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "github.com/lib/pq" // registers postgres driver
)

var (
	ErrBudgetIdentifiersRequired = errors.New("appid and modelid are required")
	ErrBudgetNotConfigured       = errors.New("token budget not configured")
	ErrBudgetExceeded            = errors.New("token budget exceeded")
	ErrInvalidTokenUsage         = errors.New("token usage must be non-negative")
)

const dailyUsageDateFormat = "2006-01-02"

type TokenBudgetStore interface {
	CheckAllowed(ctx context.Context, appID, modelID string, day time.Time) error
	AddUsage(ctx context.Context, appID, modelID string, tokens int, day time.Time) error
}

type PostgresTokenBudgetStore struct {
	db *sql.DB
}

func NewPostgresTokenBudgetStore(dsn string) (*PostgresTokenBudgetStore, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}

	store := &PostgresTokenBudgetStore{db: db}
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
  appid text NOT NULL,
  modelid text NOT NULL,
  token_budget bigint NOT NULL CHECK (token_budget >= 0),
  PRIMARY KEY (appid, modelid)
)`

	const createUsageTable = `
CREATE TABLE IF NOT EXISTS token_usage_daily (
  usage_date date NOT NULL,
  appid text NOT NULL,
  modelid text NOT NULL,
  token bigint NOT NULL CHECK (token >= 0),
  PRIMARY KEY (usage_date, appid, modelid)
)`

	if _, err := s.db.Exec(createBudgetsTable); err != nil {
		return err
	}
	if _, err := s.db.Exec(createUsageTable); err != nil {
		return err
	}
	return nil
}

func (s *PostgresTokenBudgetStore) CheckAllowed(ctx context.Context, appID, modelID string, day time.Time) error {
	appID = strings.TrimSpace(appID)
	modelID = strings.TrimSpace(modelID)
	if appID == "" || modelID == "" {
		return ErrBudgetIdentifiersRequired
	}

	const q = `
SELECT b.token_budget, COALESCE(u.token, 0)
FROM token_budgets b
LEFT JOIN token_usage_daily u
  ON u.appid = b.appid
 AND u.modelid = b.modelid
 AND u.usage_date = $3
WHERE b.appid = $1
  AND b.modelid = $2`

	usageDate := day.UTC().Format(dailyUsageDateFormat)
	var budget, used int64
	err := s.db.QueryRowContext(ctx, q, appID, modelID, usageDate).Scan(&budget, &used)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrBudgetNotConfigured
	}
	if err != nil {
		return err
	}
	if used >= budget {
		return ErrBudgetExceeded
	}
	return nil
}

func (s *PostgresTokenBudgetStore) AddUsage(ctx context.Context, appID, modelID string, tokens int, day time.Time) error {
	appID = strings.TrimSpace(appID)
	modelID = strings.TrimSpace(modelID)
	if appID == "" || modelID == "" {
		return ErrBudgetIdentifiersRequired
	}
	if tokens < 0 {
		return ErrInvalidTokenUsage
	}
	if tokens == 0 {
		return nil
	}

	const q = `
INSERT INTO token_usage_daily (usage_date, appid, modelid, token)
VALUES ($1, $2, $3, $4)
ON CONFLICT (usage_date, appid, modelid)
DO UPDATE SET token = token_usage_daily.token + EXCLUDED.token`

	usageDate := day.UTC().Format(dailyUsageDateFormat)
	if _, err := s.db.ExecContext(ctx, q, usageDate, appID, modelID, tokens); err != nil {
		return err
	}
	return nil
}

func extractBudgetIdentifiers(metadata map[string]any) (string, string, error) {
	appID, ok := metadataStringValue(metadata, "appid")
	if !ok {
		return "", "", fmt.Errorf("%w: appid", ErrBudgetIdentifiersRequired)
	}
	modelID, ok := metadataStringValue(metadata, "modelid")
	if !ok {
		return "", "", fmt.Errorf("%w: modelid", ErrBudgetIdentifiersRequired)
	}
	return appID, modelID, nil
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
