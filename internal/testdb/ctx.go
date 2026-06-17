package testdb

import (
	"context"

	"github.com/davesade/kilolock/internal/auth"
)

// TenantCtx returns a context derived from parent that carries the
// self-hosted CLI principal. Used by integration tests so the
// store's auth.TenantFromContext call has a Principal to read
// without each test having to wire up the auth package directly.
//
// In hosted-mode tests (none today) a test would set its own
// Principal via auth.WithPrincipal directly; this helper is the
// self-hosted shortcut, and the only one the bulk of integration
// tests need.
func TenantCtx(parent context.Context) context.Context {
	return auth.WithPrincipal(parent, auth.CLIPrincipal())
}

// BackgroundTenantCtx is the no-argument convenience for
// `TenantCtx(context.Background())`. Useful in setup helpers that
// don't have a parent ctx of their own.
func BackgroundTenantCtx() context.Context {
	return TenantCtx(context.Background())
}
