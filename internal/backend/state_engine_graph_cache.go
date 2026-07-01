package backend

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kilolockio/kilolock/pkg/store"
)

type stateEngineGraphSnapshot struct {
	Resources           []store.StateEngineResourceInventory
	DependencyAdjacency map[string][]string
	ModuleMembers       map[string][]string
}

type stateEngineGraphCache struct {
	mu         sync.Mutex
	maxEntries int
	ttl        time.Duration
	entries    map[string]stateEngineGraphCacheEntry
}

type stateEngineGraphCacheEntry struct {
	snapshot  stateEngineGraphSnapshot
	expiresAt time.Time
}

func newStateEngineGraphCache(maxEntries int, ttl time.Duration) *stateEngineGraphCache {
	if maxEntries <= 0 || ttl <= 0 {
		return nil
	}
	return &stateEngineGraphCache{
		maxEntries: maxEntries,
		ttl:        ttl,
		entries:    make(map[string]stateEngineGraphCacheEntry, maxEntries),
	}
}

func (c *stateEngineGraphCache) get(stateID string, serial int64) (stateEngineGraphSnapshot, bool) {
	if c == nil {
		return stateEngineGraphSnapshot{}, false
	}
	now := time.Now().UTC()
	key := stateEngineGraphCacheKey(stateID, serial)

	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.entries[key]
	if !ok {
		return stateEngineGraphSnapshot{}, false
	}
	if !entry.expiresAt.After(now) {
		delete(c.entries, key)
		return stateEngineGraphSnapshot{}, false
	}
	return entry.snapshot, true
}

func (c *stateEngineGraphCache) put(stateID string, serial int64, snapshot stateEngineGraphSnapshot) {
	if c == nil {
		return
	}
	now := time.Now().UTC()
	key := stateEngineGraphCacheKey(stateID, serial)

	c.mu.Lock()
	defer c.mu.Unlock()

	c.purgeExpiredLocked(now)
	if len(c.entries) >= c.maxEntries {
		c.evictOneLocked()
	}
	c.entries[key] = stateEngineGraphCacheEntry{
		snapshot:  snapshot,
		expiresAt: now.Add(c.ttl),
	}
}

func (c *stateEngineGraphCache) purgeExpiredLocked(now time.Time) {
	for key, entry := range c.entries {
		if !entry.expiresAt.After(now) {
			delete(c.entries, key)
		}
	}
}

func (c *stateEngineGraphCache) evictOneLocked() {
	var (
		oldestKey string
		oldestAt  time.Time
	)
	for key, entry := range c.entries {
		if oldestKey == "" || entry.expiresAt.Before(oldestAt) {
			oldestKey = key
			oldestAt = entry.expiresAt
		}
	}
	if oldestKey != "" {
		delete(c.entries, oldestKey)
	}
}

func stateEngineGraphCacheKey(stateID string, serial int64) string {
	return stateID + "|" + strconv.FormatInt(serial, 10)
}

func (s *Server) loadStateEngineGraphSnapshot(
	ctx context.Context,
	st *store.Store,
	stateName string,
) (*store.StateEngineStateInfo, stateEngineGraphSnapshot, bool, error) {
	started := time.Now()
	info, err := st.ResolveStateEngineStateInfo(ctx, stateName)
	if err != nil {
		return nil, stateEngineGraphSnapshot{}, false, err
	}
	if snapshot, ok := s.stateEngineGraph.get(info.StateID, info.Serial); ok {
		s.logger.Debug("state-engine graph cache hit",
			append(requestLogAttrs(ctx, stateName),
				"state_id", info.StateID,
				"serial", info.Serial,
				"resource_count", len(snapshot.Resources),
				"dependency_edges", countStateEngineAdjacencyEdges(snapshot.DependencyAdjacency),
				"duration_ms", time.Since(started).Milliseconds(),
			)...,
		)
		return info, snapshot, true, nil
	}

	resources, dependencyAdjacency, err := st.LoadCurrentGraphSnapshotForStateEngine(ctx, stateName)
	if err != nil {
		return nil, stateEngineGraphSnapshot{}, false, fmt.Errorf("load current state-engine graph snapshot: %w", err)
	}
	snapshot := stateEngineGraphSnapshot{
		Resources:           resources,
		DependencyAdjacency: dependencyAdjacency,
		ModuleMembers:       buildStateEngineModuleMembers(resources),
	}
	s.stateEngineGraph.put(info.StateID, info.Serial, snapshot)
	s.logger.Debug("state-engine graph cache miss",
		append(requestLogAttrs(ctx, stateName),
			"state_id", info.StateID,
			"serial", info.Serial,
			"resource_count", len(snapshot.Resources),
			"dependency_edges", countStateEngineAdjacencyEdges(snapshot.DependencyAdjacency),
			"duration_ms", time.Since(started).Milliseconds(),
		)...,
	)
	return info, snapshot, false, nil
}

func countStateEngineAdjacencyEdges(in map[string][]string) int {
	total := 0
	for _, deps := range in {
		total += len(deps)
	}
	return total
}

func buildStateEngineModuleMembers(resources []store.StateEngineResourceInventory) map[string][]string {
	if len(resources) == 0 {
		return nil
	}
	out := make(map[string][]string)
	for _, resource := range resources {
		prefixes := modulePrefixesForResource(resource)
		for _, prefix := range prefixes {
			out[prefix] = append(out[prefix], resource.Address)
		}
	}
	for prefix := range out {
		out[prefix] = dedupeSortedStrings(out[prefix])
	}
	return out
}

func modulePrefixesForResource(resource store.StateEngineResourceInventory) []string {
	modulePath := strings.TrimSpace(resource.ModulePath)
	if modulePath == "" {
		modulePath = modulePrefixFromAddress(resource.Address)
	}
	return expandModulePathPrefixes(modulePath)
}

func modulePrefixFromAddress(addr string) string {
	addr = strings.TrimSpace(addr)
	if !strings.HasPrefix(addr, "module.") {
		return ""
	}
	parts := strings.Split(addr, ".")
	collected := make([]string, 0, len(parts))
	for i := 0; i+1 < len(parts); {
		if parts[i] != "module" {
			break
		}
		collected = append(collected, parts[i], parts[i+1])
		i += 2
		if i >= len(parts) || parts[i] != "module" {
			break
		}
	}
	return strings.Join(collected, ".")
}

func expandModulePathPrefixes(modulePath string) []string {
	modulePath = strings.TrimSpace(modulePath)
	if modulePath == "" || !strings.HasPrefix(modulePath, "module.") {
		return nil
	}
	parts := strings.Split(modulePath, ".")
	out := make([]string, 0, len(parts)/2)
	for i := 0; i+1 < len(parts); {
		if parts[i] != "module" {
			break
		}
		out = append(out, strings.Join(parts[:i+2], "."))
		i += 2
	}
	return dedupeSortedStrings(out)
}
