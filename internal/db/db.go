// Package db wraps the PostgreSQL connection pool used by the rest of
// Kilolock. It exists to centralize pool configuration, ping behavior on
// startup, and any future tracing or pooling concerns.
package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Pool is the connection pool used by all data access in Kilolock.
type Pool struct {
	*pgxpool.Pool
}

// OpenOptions controls optional pgx pool tuning.
type OpenOptions struct {
	MaxConns int32
	MinConns int32
}

// Open returns a connected Pool, having verified connectivity with a ping.
// The caller owns the returned pool and must call Close when done.
func Open(ctx context.Context, databaseURL string) (*Pool, error) {
	return OpenWithOptions(ctx, databaseURL, OpenOptions{})
}

// OpenWithOptions returns a connected Pool with optional tuning overrides.
func OpenWithOptions(ctx context.Context, databaseURL string, opts OpenOptions) (*Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	if opts.MaxConns > 0 {
		cfg.MaxConns = opts.MaxConns
	}
	if opts.MinConns > 0 {
		cfg.MinConns = opts.MinConns
	}
	// Keep health checks reasonably frequent for routed data-plane pools.
	if cfg.HealthCheckPeriod <= 0 {
		cfg.HealthCheckPeriod = 30 * time.Second
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open pool: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	return &Pool{Pool: pool}, nil
}
