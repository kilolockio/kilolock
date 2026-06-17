package routing

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/davesade/kilolock/internal/auth"
	"github.com/davesade/kilolock/pkg/store"
)

var ErrEnvironmentUnavailable = errors.New("environment unavailable")

// EnvironmentLookup loads environment metadata from the control plane.
type EnvironmentLookup interface {
	GetEnvironmentByID(ctx context.Context, id string) (store.EnvironmentRow, error)
}

// Router resolves a data-plane store for the authenticated environment.
type Router struct {
	defaultPool        *pgxpool.Pool
	lookup             EnvironmentLookup
	cache              *PoolCache
	maxConnsByInstance map[string]int32
	defaultMaxConns    int32
	maxPoolsByInstance map[string]int
	defaultMaxPools    int
}

// NewRouter returns a router that uses defaultPool when an environment has no
// database_dsn (unified / self-hosted mode).
func NewRouter(defaultPool *pgxpool.Pool, lookup EnvironmentLookup, cache *PoolCache) *Router {
	if cache == nil {
		cache = NewPoolCache(32)
	}
	return &Router{
		defaultPool: defaultPool,
		lookup:      lookup,
		cache:       cache,
	}
}

// WithInstanceMaxConns configures per-instance pool max conns and default max
// conns for routed environment pools.
func (r *Router) WithInstanceMaxConns(defaultMax int32, byInstance map[string]int32) *Router {
	c := *r
	c.defaultMaxConns = defaultMax
	if byInstance != nil {
		c.maxConnsByInstance = make(map[string]int32, len(byInstance))
		for k, v := range byInstance {
			c.maxConnsByInstance[k] = v
		}
	}
	return &c
}

// WithInstanceMaxPools configures max open pools per instance key.
func (r *Router) WithInstanceMaxPools(defaultMax int, byInstance map[string]int) *Router {
	c := *r
	c.defaultMaxPools = defaultMax
	if byInstance != nil {
		c.maxPoolsByInstance = make(map[string]int, len(byInstance))
		for k, v := range byInstance {
			c.maxPoolsByInstance[k] = v
		}
	}
	if c.cache != nil {
		c.cache.WithInstancePoolCaps(defaultMax, byInstance)
	}
	return &c
}

// StoreFor returns the store backing the request's environment.
func (r *Router) StoreFor(ctx context.Context) (*store.Store, error) {
	envID := auth.EnvironmentFromContext(ctx)
	if envID == "" {
		if err := ensureTenantOnPool(ctx, r.defaultPool); err != nil {
			return nil, err
		}
		return store.New(r.defaultPool), nil
	}
	env, err := r.lookup.GetEnvironmentByID(ctx, envID)
	if err != nil {
		return nil, err
	}
	if env.LifecycleStatus != store.LifecycleStatusActive {
		return nil, fmt.Errorf("%w: environment %s is %s", ErrEnvironmentUnavailable, env.Slug, env.LifecycleStatus)
	}
	if strings.TrimSpace(env.DatabaseDSN) == "" {
		if err := ensureTenantOnPool(ctx, r.defaultPool); err != nil {
			return nil, err
		}
		return store.New(r.defaultPool), nil
	}
	if env.Status != store.EnvironmentStatusReady {
		return nil, fmt.Errorf("environment %s is not ready (status=%s)", env.Slug, env.Status)
	}
	key := strings.TrimSpace(env.DatabaseInstanceKey)
	if key == "" {
		key = "shared"
	}
	maxConns := r.defaultMaxConns
	if v, ok := r.maxConnsByInstance[key]; ok && v > 0 {
		maxConns = v
	}
	pool, err := r.cache.Get(ctx, env.DatabaseDSN, GetOptions{
		InstanceKey: key,
		MaxConns:    maxConns,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: open environment database: %w", ErrEnvironmentUnavailable, err)
	}
	if err := ensureTenantOnPool(ctx, pool); err != nil {
		return nil, fmt.Errorf("%w: sync tenant row: %w", ErrEnvironmentUnavailable, err)
	}
	return store.NewIsolated(pool), nil
}

func ensureTenantOnPool(ctx context.Context, pool *pgxpool.Pool) error {
	p, ok := auth.FromContext(ctx)
	if !ok || p.TenantID == "" {
		return nil
	}
	slug := strings.TrimSpace(p.TenantSlug)
	if slug == "" {
		return nil
	}
	name := slug
	st := store.New(pool)
	return st.EnsureTenantOnDataPlane(
		ctx,
		p.TenantID,
		slug,
		name,
		p.TenantLifecycleStatus,
		p.BillingPlan,
		p.MaxEnvironments,
		p.MaxStateResources,
		p.MaxEnvironmentResources,
	)
}

// Close evicts cached environment pools.
func (r *Router) Close() {
	if r.cache != nil {
		r.cache.Close()
	}
}
