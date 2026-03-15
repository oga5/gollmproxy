package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
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

	if err := http.ListenAndServe(addr, handler); err != nil {
		slog.Error("server error", "error", err)
		os.Exit(1)
	}
}
