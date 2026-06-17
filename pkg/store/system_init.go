package store

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type SystemInitStatus struct {
	Initialized   bool
	InitMode      string
	InitializedAt *time.Time
	InitializedBy string
	UpdatedAt     time.Time
}

func (s *Store) GetSystemInitStatus(ctx context.Context) (SystemInitStatus, error) {
	var out SystemInitStatus
	err := s.pool.QueryRow(ctx, `
		SELECT initialized,
		       COALESCE(init_mode, ''),
		       initialized_at,
		       COALESCE(initialized_by, ''),
		       updated_at
		FROM system_init
		WHERE singleton = TRUE`,
	).Scan(&out.Initialized, &out.InitMode, &out.InitializedAt, &out.InitializedBy, &out.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return SystemInitStatus{}, nil
	}
	if err != nil {
		return SystemInitStatus{}, err
	}
	return out, nil
}

func (s *Store) MarkSystemInitialized(ctx context.Context, mode, by string) error {
	mode = strings.TrimSpace(mode)
	by = strings.TrimSpace(by)
	_, err := s.pool.Exec(ctx, `
		INSERT INTO system_init (singleton, initialized, init_mode, initialized_at, initialized_by, updated_at)
		VALUES (TRUE, TRUE, $1, now(), $2, now())
		ON CONFLICT (singleton) DO UPDATE
		  SET initialized = TRUE,
		      init_mode = CASE WHEN system_init.init_mode = '' THEN EXCLUDED.init_mode ELSE system_init.init_mode END,
		      initialized_at = COALESCE(system_init.initialized_at, EXCLUDED.initialized_at),
		      initialized_by = CASE WHEN system_init.initialized_by = '' THEN EXCLUDED.initialized_by ELSE system_init.initialized_by END,
		      updated_at = now()`,
		mode, by,
	)
	return err
}
