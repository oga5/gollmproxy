package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"
)

func main() {
	cfg := LoadConfig()

	logger, err := NewRequestLogger(cfg.LogFile)
	if err != nil {
		slog.Error("failed to open log file", "error", err)
		os.Exit(1)
	}
	defer logger.Close()

	if cfg.PostgresDSN != "" {
		pg, err := NewPostgresLogger(cfg.PostgresDSN)
		if err != nil {
			slog.Error("failed to connect to postgres for logging", "error", err)
			os.Exit(1)
		}
		logger.AttachPostgresLogger(pg)
		slog.Info("postgres request logging enabled")
	}

	if cfg.TokenBudgetEnabled {
		if cfg.PostgresDSN == "" {
			slog.Error("token budget enabled but postgres_dsn is not configured")
			os.Exit(1)
		}
		tokenBudgetStore, err := NewPostgresTokenBudgetStore(cfg.PostgresDSN)
		if err != nil {
			slog.Error("failed to connect to postgres for token budget", "error", err)
			os.Exit(1)
		}
		defer tokenBudgetStore.Close()
		cfg.TokenBudgetStore = tokenBudgetStore
		slog.Info("token budget control enabled")
	}

	handler := NewServer(cfg, logger)

	addr := fmt.Sprintf(":%d", cfg.Port)
	slog.Info("starting gollmproxy", "addr", addr, "logfile", cfg.LogFile)

	srv := &http.Server{
		Addr:         addr,
		Handler:      handler,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 0, // streaming responses may legitimately stay open longer than any fixed write deadline
		IdleTimeout:  2 * time.Minute,
	}
	if err := srv.ListenAndServe(); err != nil {
		slog.Error("server error", "error", err)
		os.Exit(1)
	}
}
