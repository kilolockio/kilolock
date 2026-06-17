// Package migrate runs the embedded SQL migrations against a Postgres
// database. The migration set is intentionally minimal in v0: numbered
// files in migrations/*.sql, applied in lexical order, tracked in a
// schema_migrations table.
//
// The runner is idempotent. It can race safely with the docker-compose
// init scripts that load the same files into a fresh database, because
// schema_migrations is the single source of truth for "what's applied".
package migrate

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"regexp"
	"sort"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// migrationFilenameRe matches files like 0001_init.sql, 0042_rename_foo.sql.
var migrationFilenameRe = regexp.MustCompile(`^(\d+)_.+\.sql$`)

// migration represents one numbered SQL file to be applied.
type migration struct {
	version int
	name    string
	sql     string
}

// Run applies any pending embedded migrations to db.
//
// The function reads the schema_migrations table to determine the highest
// applied version, then applies each subsequent migration in its own
// transaction. If any migration fails, the transaction is rolled back
// and the error returned.
func Run(ctx context.Context, db *pgxpool.Pool, logger *slog.Logger) error {
	migrations, err := load()
	if err != nil {
		return fmt.Errorf("load embedded migrations: %w", err)
	}

	applied, err := readApplied(ctx, db)
	if err != nil {
		return fmt.Errorf("read applied migrations: %w", err)
	}

	for _, m := range migrations {
		if _, ok := applied[m.version]; ok {
			logger.Debug("migration already applied, skipping", "version", m.version, "name", m.name)
			continue
		}
		if err := apply(ctx, db, m); err != nil {
			return fmt.Errorf("apply migration %s: %w", m.name, wrapDuplicateObjectErr(m, err))
		}
		logger.Info("migration applied", "version", m.version, "name", m.name)
	}

	return nil
}

// wrapDuplicateObjectErr augments "object already exists" errors with a
// hint that the most common cause is a `schema_migrations` table that
// is behind the actual database schema — typically the aftermath of
// restoring a pg_dump captured before this migration existed.
//
// The fix in that case is to mark the migration as applied without
// re-running its SQL. Generating a copy-pasteable command saves users
// (and the on-call assistant rerolling a demo) from rediscovering this
// every time it bites.
func wrapDuplicateObjectErr(m migration, err error) error {
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	switch pgErr.Code {
	// 42P07 duplicate_table, 42P06 duplicate_schema, 42710 duplicate_object
	// (covers indexes, constraints, types, functions, triggers).
	case "42P07", "42P06", "42710":
	default:
		return err
	}
	return fmt.Errorf("%w\n\nhint: the object this migration would create already exists, "+
		"but schema_migrations does not list version %d as applied. This usually "+
		"means the database was restored from a pg_dump taken before this "+
		"migration existed, or schema_migrations was wiped after the migration "+
		"ran. If the existing schema is correct, mark this migration as applied:\n\n"+
		"  psql \"$KL_DATABASE_URL\" -c \\\n"+
		"    \"INSERT INTO schema_migrations (version) VALUES (%d) ON CONFLICT DO NOTHING;\"\n",
		err, m.version, m.version)
}

// load reads and sorts all migrations embedded into the binary.
func load() ([]migration, error) {
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return nil, err
	}

	out := make([]migration, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := migrationFilenameRe.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		version, err := strconv.Atoi(m[1])
		if err != nil {
			return nil, fmt.Errorf("invalid migration version in %s: %w", e.Name(), err)
		}
		body, err := fs.ReadFile(migrationsFS, path.Join("migrations", e.Name()))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", e.Name(), err)
		}
		out = append(out, migration{
			version: version,
			name:    e.Name(),
			sql:     string(body),
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	if err := validateMigrationSet(out); err != nil {
		return nil, err
	}
	return out, nil
}

func validateMigrationSet(migrations []migration) error {
	seen := make(map[int]string, len(migrations))
	for _, m := range migrations {
		if prev, ok := seen[m.version]; ok {
			return fmt.Errorf("duplicate migration version %04d: %s and %s", m.version, prev, m.name)
		}
		seen[m.version] = m.name
	}
	return nil
}

// readApplied returns the set of migration versions already recorded in
// schema_migrations. The table is created on demand if it does not exist;
// this matters when running against a database where the very first
// migration has never been applied.
func readApplied(ctx context.Context, db *pgxpool.Pool) (map[int]struct{}, error) {
	const ensureTable = `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    integer     PRIMARY KEY,
			applied_at timestamptz NOT NULL DEFAULT now()
		)
	`
	if _, err := db.Exec(ctx, ensureTable); err != nil {
		return nil, fmt.Errorf("ensure schema_migrations table: %w", err)
	}

	rows, err := db.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	applied := make(map[int]struct{})
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		applied[v] = struct{}{}
	}
	return applied, rows.Err()
}

// apply runs one migration in a single transaction. The migration SQL is
// responsible for inserting its own schema_migrations row; the runner
// guards against missing inserts by upserting the version after the SQL
// completes.
func apply(ctx context.Context, db *pgxpool.Pool, m migration) error {
	return pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, m.sql); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO schema_migrations (version) VALUES ($1)
			 ON CONFLICT (version) DO NOTHING`,
			m.version,
		)
		return err
	})
}
