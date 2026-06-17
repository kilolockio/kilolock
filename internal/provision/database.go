package provision

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/davesade/kilolock/internal/migrate"
)

// CreateDatabase creates databaseName on the Postgres instance reachable via
// adminDSN (must be a superuser or CREATEDB role). idempotent when the
// database already exists.
func CreateDatabase(ctx context.Context, adminDSN, databaseName string) error {
	databaseName = strings.TrimSpace(databaseName)
	if databaseName == "" {
		return fmt.Errorf("database name is required")
	}
	cfg, err := pgxpool.ParseConfig(adminDSN)
	if err != nil {
		return fmt.Errorf("parse admin dsn: %w", err)
	}
	// Connect to the maintenance database on the same host.
	cfg.ConnConfig.Database = "postgres"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return fmt.Errorf("open admin pool: %w", err)
	}
	defer pool.Close()

	var exists bool
	err = pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`,
		databaseName,
	).Scan(&exists)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	// database names cannot be parameterized in CREATE DATABASE.
	if !validDatabaseName(databaseName) {
		return fmt.Errorf("invalid database name %q", databaseName)
	}
	_, err = pool.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{databaseName}.Sanitize())
	return err
}

// DSNForDatabase returns a copy of baseDSN pointed at databaseName.
func DSNForDatabase(baseDSN, databaseName string) (string, error) {
	u, err := url.Parse(baseDSN)
	if err != nil {
		return "", fmt.Errorf("parse base dsn: %w", err)
	}
	u.Path = "/" + databaseName
	return u.String(), nil
}

// MigrateEnvironment applies embedded migrations to the environment database.
func MigrateEnvironment(ctx context.Context, envDSN string, logger *slog.Logger) error {
	pool, err := pgxpool.New(ctx, envDSN)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return err
	}
	if logger == nil {
		logger = slog.Default()
	}
	return migrate.Run(ctx, pool, logger)
}

func validDatabaseName(name string) bool {
	if name == "" || len(name) > 63 {
		return false
	}
	for i, r := range name {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' {
			continue
		}
		if r == '-' && i > 0 {
			continue
		}
		return false
	}
	return true
}
