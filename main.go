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

	handler := NewServer(cfg, logger)

	addr := fmt.Sprintf(":%d", cfg.Port)
	slog.Info("starting gollmproxy", "addr", addr, "logfile", cfg.LogFile)

	srv := &http.Server{
		Addr:         addr,
		Handler:      handler,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 10 * time.Minute, // long for streaming responses
		IdleTimeout:  2 * time.Minute,
	}
	if err := srv.ListenAndServe(); err != nil {
		slog.Error("server error", "error", err)
		os.Exit(1)
	}
}
