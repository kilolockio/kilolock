package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// TenantNotActiveError is returned when a tenant is suspended or archived
// and attempts a mutating operation (write/delete/lock).
//
// This is enforced best-effort: in self-hosted and some isolated-database
// configurations the tenants table may be absent or the tenant row may not
// exist. In those cases, enforcement fails open.
type TenantNotActiveError struct {
	Status LifecycleStatus
}

func (e *TenantNotActiveError) Error() string {
	if e == nil {
		return ""
	}
	if e.Status == "" {
		return "tenant is not active"
	}
	return fmt.Sprintf("tenant is %s", e.Status)
}

func enforceTenantLifecycleActive(ctx context.Context, tx pgx.Tx, tenantID string) error {
	var status LifecycleStatus
	err := tx.QueryRow(ctx, `SELECT lifecycle_status FROM tenants WHERE id = $1`, tenantID).Scan(&status)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// Self-hosted/bootstrap and legacy DBs may not have a tenant row for
		// the well-known principal. Fail open to avoid breaking those flows.
		return nil
	case err != nil:
		// Some isolated environment DBs may not include the tenants table.
		// Fail open to preserve backward compatibility.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "42P01" {
			return nil
		}
		return err
	case status == LifecycleStatusActive:
		return nil
	default:
		return &TenantNotActiveError{Status: status}
	}
}
