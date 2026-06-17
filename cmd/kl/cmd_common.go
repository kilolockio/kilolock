package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/user"
	"time"

	"github.com/kilolockio/kilolock/pkg/auth"
	"github.com/kilolockio/kilolock/pkg/config"
)

// loadConfigOrExit reads + validates config, terminating the process with
// exit code 2 on failure. Subcommands that don't need a valid DB URL
// (e.g. version, help) should not call this.
func loadConfigOrExit(subcommand string) config.Config {
	cfg := config.Load()
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "kl %s: %v\n", subcommand, err)
		os.Exit(2)
	}
	return cfg
}

// newLogger returns a slog logger configured per the user's settings.
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

	var h slog.Handler
	if format == "json" {
		h = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		h = slog.NewTextHandler(os.Stderr, opts)
	}
	return slog.New(h)
}

// cliActor returns a best-effort identifier for the user running the CLI.
// Used purely for the audit trail.
func cliActor() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username + "@cli"
	}
	return "cli"
}

// defaultTimeout is used by every CLI subcommand that doesn't manage its
// own context lifecycle (everything except serve).
const defaultTimeout = 30 * time.Second

// resourceQueryTimeout is used by backend-native resource inspection and
// repair commands. Big states require parsing both current and historical
// snapshots, which can legitimately take longer than the generic CLI
// timeout.
const resourceQueryTimeout = 2 * time.Minute

// cliContext returns a fresh background context already carrying the
// self-hosted CLI principal. Every subcommand uses this in place of
// context.Background() so the store layer's auth.TenantFromContext
// call has a Principal to read.
//
// The hosted-CLI bootstrap path will eventually replace this with a
// token-resolved Principal; for now there's exactly one tenant and
// this is a one-line redirect.
func cliContext() context.Context {
	return auth.WithPrincipal(context.Background(), auth.CLIPrincipal())
}

func registerStringFlagAlias(fs *flag.FlagSet, target *string, long, short, value, usage string) {
	fs.StringVar(target, long, value, usage)
	fs.StringVar(target, short, value, "alias for --"+long)
}

func registerFlagValueAlias(fs *flag.FlagSet, value flag.Value, long, short, usage string) {
	fs.Var(value, long, usage)
	fs.Var(value, short, "alias for --"+long)
}
