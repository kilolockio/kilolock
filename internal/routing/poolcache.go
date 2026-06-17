package routing

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/davesade/kilolock/internal/db"
)

// PoolCache holds pgx pools keyed by DSN with a simple LRU eviction policy.
type PoolCache struct {
	mu                      sync.Mutex
	max                     int
	order                   []string // DSN keys, most recent at end
	pools                   map[string]*pgxpool.Pool
	byDSN                   map[string]poolMeta
	byKey                   map[string]*instanceStats
	onEvict                 func(*pgxpool.Pool)
	hits                    atomic.Uint64
	misses                  atomic.Uint64
	opens                   atomic.Uint64
	evicts                  atomic.Uint64
	circuitFailureThreshold int
	circuitCooldown         time.Duration
	defaultMaxPools         int
	maxPoolsByInstance      map[string]int
}

type poolMeta struct {
	instanceKey string
}

type instanceStats struct {
	OpenPools           int
	Hits                uint64
	Misses              uint64
	Opens               uint64
	Evicts              uint64
	ConnectFailures     uint64
	LastError           string
	LastErrorAt         time.Time
	LastSuccessAt       time.Time
	ConsecutiveFailures int
	CooldownUntil       time.Time
}

type GetOptions struct {
	InstanceKey string
	MaxConns    int32
}

var ErrInstanceCircuitOpen = errors.New("instance circuit open")
var ErrInstancePoolCapExceeded = errors.New("instance pool cap exceeded")

// NewPoolCache returns a cache that keeps at most max pools open.
func NewPoolCache(max int) *PoolCache {
	if max < 1 {
		max = 32
	}
	return &PoolCache{
		max:                     max,
		pools:                   make(map[string]*pgxpool.Pool),
		byDSN:                   make(map[string]poolMeta),
		byKey:                   make(map[string]*instanceStats),
		circuitFailureThreshold: 2,
		circuitCooldown:         10 * time.Second,
	}
}

// WithCircuitBreaker configures per-instance fail-fast behavior.
func (c *PoolCache) WithCircuitBreaker(failureThreshold int, cooldown time.Duration) *PoolCache {
	if failureThreshold < 1 {
		failureThreshold = 1
	}
	if cooldown <= 0 {
		cooldown = 10 * time.Second
	}
	c.circuitFailureThreshold = failureThreshold
	c.circuitCooldown = cooldown
	return c
}

// WithInstancePoolCaps configures max open pools per instance key.
// defaultMax <= 0 disables the cap unless an explicit per-instance
// override is provided.
func (c *PoolCache) WithInstancePoolCaps(defaultMax int, byInstance map[string]int) *PoolCache {
	c.defaultMaxPools = defaultMax
	if byInstance != nil {
		c.maxPoolsByInstance = make(map[string]int, len(byInstance))
		for k, v := range byInstance {
			c.maxPoolsByInstance[normalizeInstanceKey(k)] = v
		}
	}
	return c
}

// Get returns a pool for dsn, opening and caching it on first use.
func (c *PoolCache) Get(ctx context.Context, dsn string, opts GetOptions) (*pgxpool.Pool, error) {
	instanceKey := normalizeInstanceKey(opts.InstanceKey)
	c.mu.Lock()
	is := c.instanceLocked(instanceKey)
	now := time.Now().UTC()
	if !is.CooldownUntil.IsZero() && now.Before(is.CooldownUntil) {
		c.mu.Unlock()
		return nil, fmt.Errorf("%w: instance=%s cooldown_until=%s", ErrInstanceCircuitOpen, instanceKey, is.CooldownUntil.Format(time.RFC3339))
	}
	if p, ok := c.pools[dsn]; ok {
		is.Hits++
		c.hits.Add(1)
		c.touchLocked(dsn)
		c.mu.Unlock()
		return p, nil
	}
	if cap := c.maxPoolsForInstance(instanceKey); cap > 0 && is.OpenPools >= cap {
		c.mu.Unlock()
		return nil, fmt.Errorf("%w: instance=%s open_pools=%d cap=%d", ErrInstancePoolCapExceeded, instanceKey, is.OpenPools, cap)
	}
	is.Misses++
	c.misses.Add(1)
	c.mu.Unlock()

	p, err := db.OpenWithOptions(ctx, dsn, db.OpenOptions{MaxConns: opts.MaxConns})
	if err != nil {
		c.mu.Lock()
		is := c.instanceLocked(instanceKey)
		is.ConnectFailures++
		is.ConsecutiveFailures++
		is.LastError = err.Error()
		is.LastErrorAt = time.Now().UTC()
		if is.ConsecutiveFailures >= c.circuitFailureThreshold {
			is.CooldownUntil = time.Now().UTC().Add(c.circuitCooldown)
		}
		c.mu.Unlock()
		return nil, err
	}
	c.opens.Add(1)

	c.mu.Lock()
	defer c.mu.Unlock()
	is = c.instanceLocked(instanceKey)
	if existing, ok := c.pools[dsn]; ok {
		p.Close()
		c.touchLocked(dsn)
		return existing, nil
	}
	if cap := c.maxPoolsForInstance(instanceKey); cap > 0 && is.OpenPools >= cap {
		p.Close()
		return nil, fmt.Errorf("%w: instance=%s open_pools=%d cap=%d", ErrInstancePoolCapExceeded, instanceKey, is.OpenPools, cap)
	}
	for len(c.pools) >= c.max && len(c.order) > 0 {
		evict := c.order[0]
		c.order = c.order[1:]
		if old, ok := c.pools[evict]; ok {
			meta := c.byDSN[evict]
			if ev, ok := c.byKey[meta.instanceKey]; ok {
				if ev.OpenPools > 0 {
					ev.OpenPools--
				}
				ev.Evicts++
			}
			delete(c.pools, evict)
			delete(c.byDSN, evict)
			if c.onEvict != nil {
				c.onEvict(old)
			}
			old.Close()
			c.evicts.Add(1)
		}
	}
	c.pools[dsn] = p.Pool
	c.byDSN[dsn] = poolMeta{instanceKey: instanceKey}
	is.OpenPools++
	is.Opens++
	is.ConsecutiveFailures = 0
	is.CooldownUntil = time.Time{}
	is.LastSuccessAt = time.Now().UTC()
	c.order = append(c.order, dsn)
	return p.Pool, nil
}

type PoolCacheStats struct {
	OpenPools int
	Hits      uint64
	Misses    uint64
	Opens     uint64
	Evicts    uint64
	Instances map[string]InstanceStats
}

type InstanceStats struct {
	OpenPools           int       `json:"open_pools"`
	Hits                uint64    `json:"hits"`
	Misses              uint64    `json:"misses"`
	Opens               uint64    `json:"opens"`
	Evicts              uint64    `json:"evicts"`
	ConnectFailures     uint64    `json:"connect_failures"`
	LastError           string    `json:"last_error,omitempty"`
	LastErrorAt         time.Time `json:"last_error_at,omitempty"`
	LastSuccessAt       time.Time `json:"last_success_at,omitempty"`
	ConsecutiveFailures int       `json:"consecutive_failures"`
	CooldownUntil       time.Time `json:"cooldown_until,omitempty"`
	Status              string    `json:"status"`
}

func (c *PoolCache) Stats() PoolCacheStats {
	c.mu.Lock()
	openPools := len(c.pools)
	instances := make(map[string]InstanceStats, len(c.byKey))
	for key, st := range c.byKey {
		status := "healthy"
		if !st.CooldownUntil.IsZero() && time.Now().UTC().Before(st.CooldownUntil) {
			status = "open_circuit"
		} else if st.LastError != "" && (st.LastSuccessAt.IsZero() || st.LastErrorAt.After(st.LastSuccessAt)) {
			status = "degraded"
		}
		instances[key] = InstanceStats{
			OpenPools:           st.OpenPools,
			Hits:                st.Hits,
			Misses:              st.Misses,
			Opens:               st.Opens,
			Evicts:              st.Evicts,
			ConnectFailures:     st.ConnectFailures,
			LastError:           st.LastError,
			LastErrorAt:         st.LastErrorAt,
			LastSuccessAt:       st.LastSuccessAt,
			ConsecutiveFailures: st.ConsecutiveFailures,
			CooldownUntil:       st.CooldownUntil,
			Status:              status,
		}
	}
	c.mu.Unlock()
	return PoolCacheStats{
		OpenPools: openPools,
		Hits:      c.hits.Load(),
		Misses:    c.misses.Load(),
		Opens:     c.opens.Load(),
		Evicts:    c.evicts.Load(),
		Instances: instances,
	}
}

func (c *PoolCache) instanceLocked(key string) *instanceStats {
	is, ok := c.byKey[key]
	if !ok {
		is = &instanceStats{}
		c.byKey[key] = is
	}
	return is
}

func normalizeInstanceKey(k string) string {
	if k == "" {
		return "shared"
	}
	return k
}

func (c *PoolCache) maxPoolsForInstance(instanceKey string) int {
	if c.maxPoolsByInstance != nil {
		if n, ok := c.maxPoolsByInstance[normalizeInstanceKey(instanceKey)]; ok {
			return n
		}
	}
	return c.defaultMaxPools
}

func (s InstanceStats) String() string {
	return fmt.Sprintf("status=%s open=%d hits=%d misses=%d opens=%d evicts=%d failures=%d",
		s.Status, s.OpenPools, s.Hits, s.Misses, s.Opens, s.Evicts, s.ConnectFailures)
}

func (c *PoolCache) touchLocked(dsn string) {
	for i, k := range c.order {
		if k == dsn {
			c.order = append(append(c.order[:i], c.order[i+1:]...), dsn)
			return
		}
	}
	c.order = append(c.order, dsn)
}

// Close closes every cached pool.
func (c *PoolCache) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, p := range c.pools {
		p.Close()
	}
	c.pools = make(map[string]*pgxpool.Pool)
	c.order = nil
}
