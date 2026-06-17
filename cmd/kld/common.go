package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/davesade/kilolock/internal/auth"
	"github.com/davesade/kilolock/internal/config"
	"github.com/davesade/kilolock/internal/db"
)

func loadConfigOrExit(subcommand string) config.Config {
	cfg := config.Load()
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "kld %s: %v\n", subcommand, err)
		os.Exit(2)
	}
	return cfg
}

func newLogger(format, level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lvl}
	if format == "json" {
		return slog.New(slog.NewJSONHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, opts))
}

func openDBURLOrExit(ctx context.Context, databaseURL string, logger *slog.Logger) *db.Pool {
	pool, err := db.Open(ctx, databaseURL)
	if err != nil {
		logger.Error("connect to database", "err", err)
		os.Exit(1)
	}
	return pool
}

const defaultTimeout = 30 * time.Second

func cliContext() context.Context {
	return auth.WithPrincipal(context.Background(), auth.CLIPrincipal())
}
