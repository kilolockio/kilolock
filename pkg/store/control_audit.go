package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

func insertControlEvent(ctx context.Context, tx pgx.Tx, kind, tenantID, actor string, payload any) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO events (kind, tenant_id, actor, payload)
		 VALUES ($1, $2, $3, $4::jsonb)`,
		kind, tenantID, actor, payload,
	)
	return err
}
