package backend

import (
	"testing"
	"time"

	"github.com/kilolockio/kilolock/pkg/store"
)

func TestStateEngineGraphCache_GetPutAndEvict(t *testing.T) {
	cache := newStateEngineGraphCache(2, time.Minute)
	if cache == nil {
		t.Fatal("cache = nil, want initialized cache")
	}

	first := stateEngineGraphSnapshot{
		Resources: []store.StateEngineResourceInventory{{Address: "aws_vpc.main"}},
	}
	second := stateEngineGraphSnapshot{
		Resources: []store.StateEngineResourceInventory{{Address: "aws_subnet.private"}},
	}
	third := stateEngineGraphSnapshot{
		Resources: []store.StateEngineResourceInventory{{Address: "aws_instance.web"}},
	}

	cache.put("state-1", 1, first)
	cache.put("state-1", 2, second)
	if got, ok := cache.get("state-1", 1); !ok || len(got.Resources) != 1 || got.Resources[0].Address != "aws_vpc.main" {
		t.Fatalf("first get = %+v, %v; want cached first snapshot", got, ok)
	}

	cache.put("state-1", 3, third)
	if _, ok := cache.get("state-1", 1); ok {
		t.Fatalf("oldest entry should have been evicted")
	}
	if got, ok := cache.get("state-1", 2); !ok || len(got.Resources) != 1 || got.Resources[0].Address != "aws_subnet.private" {
		t.Fatalf("second get after eviction = %+v, %v; want cached second snapshot", got, ok)
	}
	if got, ok := cache.get("state-1", 3); !ok || len(got.Resources) != 1 || got.Resources[0].Address != "aws_instance.web" {
		t.Fatalf("third get after eviction = %+v, %v; want cached third snapshot", got, ok)
	}
}

func TestStateEngineGraphCache_ExpiredEntryMisses(t *testing.T) {
	cache := newStateEngineGraphCache(1, time.Minute)
	if cache == nil {
		t.Fatal("cache = nil, want initialized cache")
	}

	key := stateEngineGraphCacheKey("state-1", 7)
	cache.entries[key] = stateEngineGraphCacheEntry{
		snapshot: stateEngineGraphSnapshot{
			Resources: []store.StateEngineResourceInventory{{Address: "aws_vpc.main"}},
		},
		expiresAt: time.Now().UTC().Add(-time.Second),
	}

	if got, ok := cache.get("state-1", 7); ok {
		t.Fatalf("expired get = %+v, true; want miss", got)
	}
	if _, ok := cache.entries[key]; ok {
		t.Fatalf("expired entry should have been removed on read")
	}
}

func TestExpandModulePathPrefixes(t *testing.T) {
	got := expandModulePathPrefixes("module.parent.module.child")
	want := []string{"module.parent", "module.parent.module.child"}
	if len(got) != len(want) {
		t.Fatalf("prefix count = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("prefixes = %v, want %v", got, want)
		}
	}
}

func TestBuildStateEngineModuleMembers(t *testing.T) {
	resources := []store.StateEngineResourceInventory{
		{Address: "module.parent.null_resource.a", ModulePath: "module.parent"},
		{Address: "module.parent.module.child.null_resource.b", ModulePath: "module.parent.module.child"},
		{Address: "null_resource.root"},
	}
	got := buildStateEngineModuleMembers(resources)

	parent := got["module.parent"]
	if len(parent) != 2 || parent[0] != "module.parent.module.child.null_resource.b" || parent[1] != "module.parent.null_resource.a" {
		t.Fatalf("module.parent members = %v", parent)
	}
	child := got["module.parent.module.child"]
	if len(child) != 1 || child[0] != "module.parent.module.child.null_resource.b" {
		t.Fatalf("module.parent.module.child members = %v", child)
	}
	if _, ok := got[""]; ok {
		t.Fatalf("unexpected root-module index entry: %v", got[""])
	}
}
