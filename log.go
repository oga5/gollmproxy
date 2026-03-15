package main

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

type LogEntry struct {
	Timestamp        string         `json:"timestamp"`
	RequestID        string         `json:"request_id"`
	Method           string         `json:"method"`
	Path             string         `json:"path"`
	User             string         `json:"user,omitempty"`
	Metadata         map[string]any `json:"metadata,omitempty"`
	Model            string         `json:"model,omitempty"`
	Provider         string         `json:"provider,omitempty"`
	Stream           bool           `json:"stream,omitempty"`
	StatusCode       int            `json:"status_code"`
	LatencyMs        int64          `json:"latency_ms"`
	PromptTokens     int            `json:"prompt_tokens,omitempty"`
	CompletionTokens int            `json:"completion_tokens,omitempty"`
	TotalTokens      int            `json:"total_tokens,omitempty"`
	ReqBody          string         `json:"req_body,omitempty"`
	RespBody         string         `json:"resp_body,omitempty"`
	Error            string         `json:"error,omitempty"`
	ClientIP         string         `json:"client_ip,omitempty"`
}

type ChunkLogEntry struct {
	Timestamp  string `json:"timestamp"`
	RequestID  string `json:"request_id"`
	ChunkIndex int    `json:"chunk_index"`
	Data       string `json:"data"`
}

const maxBodyLogSize = 10 * 1024 // 10KB

type RequestLogger struct {
	file *os.File
	mu   sync.Mutex
}

func NewRequestLogger(path string) (*RequestLogger, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, err
	}
	return &RequestLogger{file: f}, nil
}

func (l *RequestLogger) Log(entry LogEntry) {
	if entry.Timestamp == "" {
		entry.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)
	}
	entry.ReqBody = truncate(entry.ReqBody, maxBodyLogSize)
	entry.RespBody = truncate(entry.RespBody, maxBodyLogSize)

	data, err := json.Marshal(entry)
	if err != nil {
		return
	}
	data = append(data, '\n')

	l.mu.Lock()
	defer l.mu.Unlock()
	l.file.Write(data)
}

func (l *RequestLogger) LogChunk(entry ChunkLogEntry) {
	if entry.Timestamp == "" {
		entry.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)
	}
	entry.Data = truncate(entry.Data, maxBodyLogSize)

	data, err := json.Marshal(entry)
	if err != nil {
		return
	}
	data = append(data, '\n')

	l.mu.Lock()
	defer l.mu.Unlock()
	l.file.Write(data)
}

func (l *RequestLogger) Close() error {
	return l.file.Close()
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "...(truncated)"
}
