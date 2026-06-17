// Package quota is the abstraction layer between Kilolock's
// store-side write paths and per-tenant resource limits. Like
// internal/auth, this package exists to make a one-way door
// reversible at the cheapest possible time:
//
//   - Self-hosted today: every check passes (Unlimited
//     implementation). The cost is one virtual call per write,
//     measured in nanoseconds.
//
//   - SaaS tomorrow: the same Quota interface plugs into a
//     billing-aware impl that consults
//     subscription/usage data. No store code, no handler code
//     changes when that swap happens.
//
// The interface is intentionally narrow. The three checks here
// are the ones the v0 codebase actually has insertion points
// for; richer plans (event-driven quotas, per-organization
// hierarchies, retention controls) compose on top without
// reshaping this surface. Adding a fourth check means another
// method here; that explicit growth is what we want, because
// every quota dimension implies a billing dimension and both
// should be debated when they land.
package quota

import (
	"context"
	"errors"
)

// ErrExceeded is the sentinel returned by every Quota method
// when the requested action would push the tenant past its
// limit. Carries no detail by design — the implementation
// stuffs the human-readable message into the error chain via
// fmt.Errorf("%w: ...", ErrExceeded, …).
var ErrExceeded = errors.New("quota exceeded")

// Quota gates writes that increase a tenant's resource usage.
// Implementations may consult an external billing system, an
// in-memory limiter, or anything else; the store layer treats
// them as a black box that says yes or no.
//
// All methods take a tenant id explicitly (rather than reading
// it from ctx like the store does) so a non-tenant-scoped
// caller — e.g. a system maintenance job — can issue checks
// without going through the auth layer. The tenant id passed
// MUST come from the auth-vetted Principal of the request the
// store call is serving; never trust caller-supplied tenant
// values.
type Quota interface {
	// CheckStateCount returns nil if the tenant may create one
	// additional state. The store call site is the upsertState
	// path inside WriteState; the check happens BEFORE the
	// INSERT so a tenant at quota cannot create a state row.
	CheckStateCount(ctx context.Context, tenantID string) error

	// CheckStateVersion returns nil if the tenant may write one
	// additional state version for the given state. The store
	// call site is the state-version INSERT inside
	// writeStateInternal; the check happens BEFORE the INSERT.
	// state-version count quotas exist primarily for retention
	// (a single tenant should not be able to flood the
	// state_versions append-only history).
	CheckStateVersion(ctx context.Context, tenantID, stateID string) error

	// CheckStorageBytes returns nil if the tenant may store an
	// additional addedBytes of state data. Called from the
	// state-version INSERT path with addedBytes =
	// len(rawState). Negative values (state shrunk) always
	// pass; positive values may be rejected.
	CheckStorageBytes(ctx context.Context, tenantID string, addedBytes int64) error
}

// Unlimited is the self-hosted-mode default: every check
// passes. Used by `kl serve` and the CLI bootstrap
// until SaaS billing lands. Zero-allocation; the call cost is a
// single indirect call returning nil.
type Unlimited struct{}

// CheckStateCount implements Quota.CheckStateCount as a no-op.
func (Unlimited) CheckStateCount(context.Context, string) error { return nil }

// CheckStateVersion implements Quota.CheckStateVersion as a no-op.
func (Unlimited) CheckStateVersion(context.Context, string, string) error { return nil }

// CheckStorageBytes implements Quota.CheckStorageBytes as a no-op.
func (Unlimited) CheckStorageBytes(context.Context, string, int64) error { return nil }
