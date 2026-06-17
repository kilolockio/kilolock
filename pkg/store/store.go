// Package store is the data access layer over the Kilolock Postgres
// schema. It exposes operations the HTTP backend protocol server needs:
// reading and writing state, acquiring and releasing locks.
//
// v0 deliberately keeps the API narrow. There are no facilities here for
// querying the normalized resource graph yet; those will come once the
// import path lands and exposes resources/dependencies as separate rows.
package store

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store is the data access entrypoint. Construct one per process and share.
type Store struct {
	pool     *pgxpool.Pool
	isolated bool // dedicated environment DB: omit tenant_id on reads
}

// New returns a Store backed by the given connection pool. The pool is
// owned by the caller; Store does not close it.
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Pool returns the underlying connection pool.
// This is primarily used by optional extensions (e.g. cloud billing adapters).
func (s *Store) Pool() *pgxpool.Pool {
	return s.pool
}

// LockInfo mirrors the JSON object Terraform's HTTP backend sends in the
// body of LOCK / UNLOCK requests. Field names use Pascal case to match
// Terraform's wire format. See:
//
//	https://developer.hashicorp.com/terraform/language/backend/http
type LockInfo struct {
	ID        string `json:"ID"`
	Operation string `json:"Operation"`
	Info      string `json:"Info"`
	Who       string `json:"Who"`
	Version   string `json:"Version"`
	Created   string `json:"Created"`
	Path      string `json:"Path"`
}

// isUniqueViolation reports whether err is a Postgres unique-constraint
// violation against the named constraint. The constraint name is supplied
// so the caller can disambiguate between, say, the (state_id, serial)
// uniqueness and another future constraint with similar semantics.
func isUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	if pgErr.Code != "23505" {
		return false
	}
	if constraint == "" {
		return true
	}
	return pgErr.ConstraintName == constraint
}
