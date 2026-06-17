package store

import (
	"context"
	"strings"
	"time"
)

type RetentionPurgeAuditEvent struct {
	TenantSlug          string
	CutoffAt            time.Time
	Actor               string
	Reason              string
	ApplyMode           bool
	Status              string
	DeletedTenants      int64
	DeletedEnvironments int64
	DeletedAPITokens    int64
	ErrorMessage        string
}

func (s *Store) RecordRetentionPurgeAudit(ctx context.Context, ev RetentionPurgeAuditEvent) error {
	_, err := s.pool.Exec(ctx, `
INSERT INTO retention_purge_audit (
    tenant_slug, cutoff_at, actor, reason, apply_mode, status,
    deleted_tenants, deleted_environments, deleted_api_tokens, error_message
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
`,
		strings.TrimSpace(ev.TenantSlug),
		ev.CutoffAt.UTC(),
		strings.TrimSpace(ev.Actor),
		strings.TrimSpace(ev.Reason),
		ev.ApplyMode,
		strings.TrimSpace(ev.Status),
		ev.DeletedTenants,
		ev.DeletedEnvironments,
		ev.DeletedAPITokens,
		strings.TrimSpace(ev.ErrorMessage),
	)
	return err
}
