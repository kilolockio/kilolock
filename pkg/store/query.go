package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ColumnInfo describes one column returned by a query. The TypeOID is the
// Postgres data type OID, useful for callers that want to format JSONB
// columns as nested JSON rather than escaped strings.
type ColumnInfo struct {
	Name    string
	TypeOID uint32
}

// QueryColumnsFunc is called exactly once, before any rows, with the
// resolved column metadata for a query.
type QueryColumnsFunc func(cols []ColumnInfo) error

// QueryRowFunc is called once per row with the column values in column
// order. Returning a non-nil error aborts iteration.
type QueryRowFunc func(values []any) error

// Query executes an ad-hoc SQL statement against the Kilolock database
// inside a read-only transaction with a bounded statement timeout.
//
// Read-only enforcement is delegated to Postgres via
// SET TRANSACTION READ ONLY: any INSERT / UPDATE / DELETE / DDL inside
// the transaction is rejected by the server with SQLSTATE 25006
// ("read-only SQL transaction"). This is more reliable than parsing the
// SQL client-side.
//
// The statement timeout is set per-transaction via SET LOCAL
// statement_timeout and applies for the duration of the query only.
// A timeout of 0 or less leaves the server-default timeout in place
// (effectively unbounded for the v0 deployment).
//
// Results are streamed: colFn is invoked once with column metadata,
// then rowFn is invoked once per row. Neither callback may retain its
// arguments past the call: row values are reused between iterations.
func (s *Store) Query(
	ctx context.Context,
	sqlText string,
	timeout time.Duration,
	colFn QueryColumnsFunc,
	rowFn QueryRowFunc,
) error {
	opts := pgx.TxOptions{AccessMode: pgx.ReadOnly}
	return pgx.BeginTxFunc(ctx, s.pool, opts, func(tx pgx.Tx) error {
		if timeout > 0 {
			// statement_timeout takes a string with units; milliseconds is
			// the safest form across Postgres versions.
			ms := timeout.Milliseconds()
			if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = %d", ms)); err != nil {
				return fmt.Errorf("set statement timeout: %w", err)
			}
		}

		rows, err := tx.Query(ctx, sqlText)
		if err != nil {
			return fmt.Errorf("execute query: %w", err)
		}
		defer rows.Close()

		descs := rows.FieldDescriptions()
		cols := make([]ColumnInfo, len(descs))
		for i, fd := range descs {
			cols[i] = ColumnInfo{Name: string(fd.Name), TypeOID: fd.DataTypeOID}
		}
		if err := colFn(cols); err != nil {
			return err
		}

		for rows.Next() {
			vals, err := rows.Values()
			if err != nil {
				return fmt.Errorf("scan row: %w", err)
			}
			if err := rowFn(vals); err != nil {
				return err
			}
		}
		return rows.Err()
	})
}
