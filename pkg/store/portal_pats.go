package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/davesade/kilolock/internal/auth"
)

type PortalPersonalAccessTokenRow struct {
	ID          string
	AccountID   string
	TokenPrefix string
	CreatedAt   time.Time
	LastUsedAt  *time.Time
	RevokedAt   *time.Time
	RevokedBy   string
}

var ErrPortalPersonalAccessTokenNotFound = errors.New("portal personal access token not found")

func (s *Store) GetActivePortalPersonalAccessToken(ctx context.Context, accountID string) (PortalPersonalAccessTokenRow, error) {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return PortalPersonalAccessTokenRow{}, fmt.Errorf("account id is required")
	}
	var row PortalPersonalAccessTokenRow
	err := s.pool.QueryRow(ctx, `
SELECT id::text, account_id::text, token_prefix, created_at, last_used_at, revoked_at, revoked_by
FROM portal_personal_access_tokens
WHERE account_id = $1
  AND revoked_at IS NULL
ORDER BY created_at DESC
LIMIT 1`, accountID).Scan(
		&row.ID, &row.AccountID, &row.TokenPrefix, &row.CreatedAt, &row.LastUsedAt, &row.RevokedAt, &row.RevokedBy,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return PortalPersonalAccessTokenRow{}, ErrPortalPersonalAccessTokenNotFound
	}
	if err != nil {
		return PortalPersonalAccessTokenRow{}, err
	}
	return row, nil
}

func (s *Store) RotatePortalPersonalAccessToken(ctx context.Context, accountID, actor string) (PortalPersonalAccessTokenRow, string, error) {
	accountID = strings.TrimSpace(accountID)
	actor = strings.TrimSpace(actor)
	if accountID == "" {
		return PortalPersonalAccessTokenRow{}, "", fmt.Errorf("account id is required")
	}
	plaintext, hash, prefix, err := auth.NewPortalPAT()
	if err != nil {
		return PortalPersonalAccessTokenRow{}, "", err
	}
	var row PortalPersonalAccessTokenRow
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var previousID string
		_ = tx.QueryRow(ctx, `
SELECT id::text
FROM portal_personal_access_tokens
WHERE account_id = $1
  AND revoked_at IS NULL
FOR UPDATE`, accountID).Scan(&previousID)

		if previousID != "" {
			if _, err := tx.Exec(ctx, `
UPDATE portal_personal_access_tokens
SET revoked_at = now(),
    revoked_by = $2
WHERE id = $1
  AND revoked_at IS NULL`, previousID, actor); err != nil {
				return err
			}
		}

		if err := tx.QueryRow(ctx, `
INSERT INTO portal_personal_access_tokens (account_id, token_hash, token_prefix)
VALUES ($1, $2, $3)
RETURNING id::text, account_id::text, token_prefix, created_at, last_used_at, revoked_at, revoked_by`,
			accountID, hash, prefix,
		).Scan(&row.ID, &row.AccountID, &row.TokenPrefix, &row.CreatedAt, &row.LastUsedAt, &row.RevokedAt, &row.RevokedBy); err != nil {
			return err
		}

		if previousID != "" {
			if _, err := tx.Exec(ctx, `
UPDATE portal_personal_access_tokens
SET replaced_by_token_id = $2
WHERE id = $1`, previousID, row.ID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return PortalPersonalAccessTokenRow{}, "", err
	}
	return row, plaintext, nil
}

func (s *Store) RevokePortalPersonalAccessToken(ctx context.Context, accountID, actor string) error {
	accountID = strings.TrimSpace(accountID)
	actor = strings.TrimSpace(actor)
	if accountID == "" {
		return fmt.Errorf("account id is required")
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
UPDATE portal_personal_access_tokens
SET revoked_at = now(),
    revoked_by = $2
WHERE account_id = $1
  AND revoked_at IS NULL`, accountID, actor)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrPortalPersonalAccessTokenNotFound
		}
		if _, err := tx.Exec(ctx, `
UPDATE portal_environment_pat_grants
SET revoked_at = now(),
    revoked_by = $2
WHERE account_id = $1
  AND revoked_at IS NULL`, accountID, actor); err != nil {
			return err
		}
		return nil
	})
}
