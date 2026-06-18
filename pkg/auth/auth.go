// Package auth is the abstraction layer between Kilolock's HTTP
// surface and the identity that drives every store-side decision.
// It exists to make a one-way door reversible at the cheapest
// possible time:
//
//   - Self-hosted today: every request is the same Principal
//     (the singleton tenant). The implementation is a literal
//     no-op that returns a hardcoded value.
//
//   - SaaS tomorrow: the same Authenticator interface plugs into
//     an OIDC / API-key / dashboard-token resolver. No store
//     code, no handler dispatch code, no CLI wiring changes when
//     that swap happens — only this one package and the
//     Server.New constructor's argument.
//
// The interface is intentionally minimal. Authentication returns
// either a Principal or an error mapped to HTTP 401; that is all.
// Authorization (what this Principal is allowed to do) is the
// responsibility of the store layer's tenant filters, not of this
// package. Mixing the two concerns is one of the failure modes
// this split avoids — the "one tenant impersonating another by
// passing a state-name from a different tenant" footgun is closed
// at the SQL boundary (every store query filters by tenant_id)
// rather than relying on the auth layer alone to do the right
// thing.
package auth

import (
	"context"
	"errors"
	"net/http"
)

// SelfHostedTenantID is the well-known UUID seeded by migration
// 0009 for the default self-hosted tenant. Held as a constant so
// the singleton authenticator, the CLI bootstrap path, and any
// test fixture all agree on the same value without round-tripping
// through the database.
const SelfHostedTenantID = "00000000-0000-0000-0000-000000000000"

// Principal is the resolved identity for one request or one CLI
// invocation. It carries enough information for the store layer
// to filter every query and audit every write, and nothing more.
// Fields beyond TenantID are populated in hosted mode; in
// self-hosted mode UserID/Email/Source typically stay empty.
type Principal struct {
	// TenantID is the uuid of the tenant the caller belongs to.
	// Always set. Empty value indicates a bug — store functions
	// panic rather than silently leak.
	TenantID string

	// WorkspaceID is the stable public workspace identifier used in
	// Terraform backend paths (e.g. ws_ab12cd34ef56).
	WorkspaceID string

	// EnvironmentID is the uuid of the environment this request is
	// scoped to (hosted mode). Empty in legacy self-hosted paths
	// until migration 0013 backfill; the router treats empty as the
	// default pool.
	EnvironmentID string

	// TenantSlug is the friendly tenant identifier used in API auth.
	TenantSlug string

	// EnvironmentSlug is the friendly environment identifier.
	EnvironmentSlug string

	// EnvironmentPublicID is the stable public environment identifier
	// used in Terraform backend paths (e.g. env_ab12cd34ef56).
	EnvironmentPublicID string

	// EnvironmentStateLockDefaultMode carries the per-environment default
	// used when a brand-new state is first created.
	EnvironmentStateLockDefaultMode string

	// DatabaseInstanceKey is the configured data-plane routing key.
	DatabaseInstanceKey string

	// TenantLifecycleStatus mirrors the control-plane tenant lifecycle so
	// data-plane tenant rows can be kept in sync when routed requests
	// touch shared or dedicated databases.
	TenantLifecycleStatus string

	// BillingPlan and the entitlement caps are carried on the principal so
	// the router can upsert accurate quota settings into the data plane.
	BillingPlan             string
	MaxEnvironments         int
	MaxStateResources       int
	MaxEnvironmentResources int

	// UserID is the uuid of the individual user behind the
	// Principal. Empty in self-hosted mode where the operator
	// runs as a service account.
	UserID string

	// Email is the human-friendly identifier for the principal,
	// used for audit logs. Free-form; never trusted as an
	// authentication factor.
	Email string

	// Source records how the principal was resolved (e.g. "cli",
	// "http-basic", "oidc"). Surfaces in audit logs and is the
	// fastest path to "where did this request authenticate?"
	// during incident response.
	Source string
}

// ErrUnauthenticated is the sentinel returned by Authenticator
// implementations when the caller cannot be identified. The HTTP
// layer maps this to 401; the CLI layer treats it as a fatal
// error during bootstrap.
var ErrUnauthenticated = errors.New("unauthenticated")

// Authenticator resolves an HTTP request to a Principal. The
// interface is deliberately one method to keep alternative
// implementations trivially writable (a future OIDC adapter is
// ~50 lines; a JWT adapter is ~30; a static-token adapter is
// ~10).
type Authenticator interface {
	Authenticate(r *http.Request) (Principal, error)
}

// SingleTenantAuthenticator is the self-hosted-mode default:
// every request resolves to the same Principal, the singleton
// self-hosted tenant. Used by `kl serve` until SaaS
// mode lands.
//
// The Source field of the returned Principal is "http" so audit
// logs can still distinguish HTTP-served writes from
// CLI-originated ones (which use Source="cli"); the actor field
// on individual table writes is still the per-request value
// extracted by actorFromRequest in the HTTP handler.
type SingleTenantAuthenticator struct{}

// Authenticate implements Authenticator by returning the
// self-hosted singleton. It never errors.
func (SingleTenantAuthenticator) Authenticate(_ *http.Request) (Principal, error) {
	return Principal{
		TenantID: SelfHostedTenantID,
		Source:   "http",
	}, nil
}

// CLIPrincipal returns the Principal used by every CLI subcommand
// in self-hosted mode. The CLI doesn't go through an
// Authenticator at all — there's no request to authenticate
// — but every store call still needs a Principal in its context,
// so this is the canonical bootstrap. The hosted CLI will swap
// this for a token-derived Principal once that codepath exists.
func CLIPrincipal() Principal {
	return Principal{
		TenantID: SelfHostedTenantID,
		Source:   "cli",
	}
}

// principalKey is the unexported type used as the context key so
// no other package can collide with us on key identity. The
// canonical Go pattern: type ctxKey struct{}; ctx.Value(ctxKey{}).
type principalKey struct{}

// WithPrincipal returns a context that carries p. Handlers,
// orchestrators, and the CLI bootstrap call this once per
// request / invocation; all downstream store calls read the
// Principal back via FromContext.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// FromContext returns the Principal previously attached to ctx,
// and a bool indicating whether one was found. Callers that
// require a Principal use MustFromContext; callers in tests or
// edge cases that want to react to "no principal here" use this
// directly.
func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

// MustFromContext returns the Principal attached to ctx, panicking
// when one is missing. Store functions call this at the top of
// every tenant-scoped query: the cost is a single map lookup; the
// benefit is that "I forgot to wrap the context" becomes a hard
// crash in dev rather than a silent cross-tenant data leak in
// prod. The defensive-programming defaults are deliberately
// strict because this is the load-bearing tenant-isolation
// invariant of the whole system.
func MustFromContext(ctx context.Context) Principal {
	p, ok := FromContext(ctx)
	if !ok {
		panic("auth: no Principal in context — store call without auth bootstrap is a bug")
	}
	if p.TenantID == "" {
		panic("auth: Principal in context has empty TenantID — auth bootstrap is broken")
	}
	return p
}

// TenantFromContext is the short form used by every store query:
// `tenantID := auth.TenantFromContext(ctx)` then thread tenantID
// into the SQL. Equivalent to MustFromContext(ctx).TenantID; kept
// as its own function so the store call sites stay narrow (no
// `.TenantID` boilerplate) and so a future "ambient tenant
// override" hook has one obvious place to land.
func TenantFromContext(ctx context.Context) string {
	return MustFromContext(ctx).TenantID
}

// EnvironmentFromContext returns the environment id attached to ctx, or ""
// when the principal has no environment (unified self-hosted mode).
func EnvironmentFromContext(ctx context.Context) string {
	p, ok := FromContext(ctx)
	if !ok {
		return ""
	}
	return p.EnvironmentID
}
