package main

import (
	"database/sql"
	"encoding/json"
	"log/slog"
	"time"

	_ "github.com/lib/pq"
)

const pgQueueSize = 1000

// PostgresLogger writes log entries to llm_logs and llm_payloads tables asynchronously.
type PostgresLogger struct {
	db    *sql.DB
	queue chan LogEntry
	done  chan struct{}
}

func NewPostgresLogger(dsn string) (*PostgresLogger, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}

	pl := &PostgresLogger{
		db:    db,
		queue: make(chan LogEntry, pgQueueSize),
		done:  make(chan struct{}),
	}
	go pl.worker()
	return pl, nil
}

func (pl *PostgresLogger) Log(entry LogEntry) {
	select {
	case pl.queue <- entry:
	default:
		slog.Warn("postgres log queue full, dropping entry", "request_id", entry.RequestID)
	}
}

func (pl *PostgresLogger) Close() error {
	close(pl.queue)
	<-pl.done
	return pl.db.Close()
}

func (pl *PostgresLogger) worker() {
	defer close(pl.done)
	for entry := range pl.queue {
		pl.insert(entry)
	}
}

func (pl *PostgresLogger) insert(entry LogEntry) {
	// Only log entries that have a model (handler-level logs, not middleware-level).
	// llm_logs.model_name is NOT NULL.
	modelName := entry.Model
	if modelName == "" {
		return
	}

	id := entry.RequestID
	if id == "" {
		return
	}

	// Build metadata JSONB: start from LogEntry.Metadata, then overlay extra fields.
	meta := make(map[string]any, len(entry.Metadata)+8)
	for k, v := range entry.Metadata {
		meta[k] = v
	}
	if entry.Provider != "" {
		meta["provider"] = entry.Provider
	}
	if entry.Path != "" {
		meta["path"] = entry.Path
	}
	if entry.Method != "" {
		meta["method"] = entry.Method
	}
	if entry.StatusCode != 0 {
		meta["status_code"] = entry.StatusCode
	}
	if entry.LatencyMs != 0 {
		meta["latency_ms"] = entry.LatencyMs
	}
	if entry.Stream {
		meta["stream"] = true
	}
	if entry.Error != "" {
		meta["error"] = entry.Error
	}
	if entry.ClientIP != "" {
		meta["client_ip"] = entry.ClientIP
	}
	if entry.User != "" {
		meta["user"] = entry.User
	}

	metaJSON, err := json.Marshal(meta)
	if err != nil {
		slog.Warn("postgres logger: failed to marshal metadata", "error", err)
		metaJSON = []byte("{}")
	}

	// Parse created_at from entry timestamp, fall back to now.
	createdAt := time.Now().UTC()
	if entry.Timestamp != "" {
		if t, err := time.Parse(time.RFC3339Nano, entry.Timestamp); err == nil {
			createdAt = t
		}
	}

	var inputTokens, outputTokens *int
	if entry.PromptTokens > 0 {
		v := entry.PromptTokens
		inputTokens = &v
	}
	if entry.CompletionTokens > 0 {
		v := entry.CompletionTokens
		outputTokens = &v
	}

	const insertLog = `
INSERT INTO llm_logs (id, created_at, model_name, input_tokens, output_tokens, metadata)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (id) DO NOTHING`

	if _, err := pl.db.Exec(insertLog, id, createdAt, modelName, inputTokens, outputTokens, metaJSON); err != nil {
		slog.Warn("postgres logger: failed to insert llm_log", "error", err, "request_id", id)
		return
	}

	// Insert payload only when body content exists.
	if entry.ReqBody == "" && entry.RespBody == "" {
		return
	}

	inputBody := toJSONB(entry.ReqBody)
	outputBody := toJSONB(entry.RespBody)

	const insertPayload = `
INSERT INTO llm_payloads (log_id, input_body, output_body)
VALUES ($1, $2, $3)
ON CONFLICT (log_id) DO NOTHING`

	if _, err := pl.db.Exec(insertPayload, id, inputBody, outputBody); err != nil {
		slog.Warn("postgres logger: failed to insert llm_payload", "error", err, "request_id", id)
	}
}

// toJSONB converts a string to a json.RawMessage suitable for a jsonb column.
// If the string is already valid JSON it is used as-is; otherwise it is wrapped as {"raw": "..."}.
func toJSONB(s string) json.RawMessage {
	if s == "" {
		return json.RawMessage("null")
	}
	if json.Valid([]byte(s)) {
		return json.RawMessage(s)
	}
	b, _ := json.Marshal(map[string]string{"raw": s})
	return json.RawMessage(b)
}
