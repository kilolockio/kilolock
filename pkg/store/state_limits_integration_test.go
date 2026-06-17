//go:build integration

package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/davesade/kilolock/internal/auth"
	"github.com/davesade/kilolock/internal/testdb"
)

func TestWriteState_EnforcesMaxStateResources(t *testing.T) {
	s, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	mustResetTables(t, pool)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 10*time.Second)
	defer cancel()

	// Set a tiny soft cap on the self-hosted tenant for this test.
	if _, err := s.pool.Exec(ctx, `UPDATE tenants SET max_state_resources = 1 WHERE id = $1`, auth.SelfHostedTenantID); err != nil {
		t.Fatalf("set max_state_resources: %v", err)
	}

	raw, err := json.Marshal(map[string]any{
		"version":           4,
		"terraform_version": "1.13.4",
		"serial":            1,
		"lineage":           "11111111-2222-3333-4444-555555555555",
		"outputs":           map[string]any{},
		"resources": []any{
			map[string]any{
				"mode":     "managed",
				"type":     "null_resource",
				"name":     "x",
				"provider": "provider[\"registry.terraform.io/hashicorp/null\"]",
				"instances": []any{
					map[string]any{"schema_version": 0, "attributes": map[string]any{"id": "n1"}},
					map[string]any{"schema_version": 0, "attributes": map[string]any{"id": "n2"}},
					map[string]any{"schema_version": 0, "attributes": map[string]any{"id": "n3"}},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	err = s.WriteState(ctx, "limit-test", "", raw, "itest", "itest")
	if !errors.Is(err, ErrEntitlementExceeded) {
		t.Fatalf("got %v, want ErrEntitlementExceeded", err)
	}
	if err == nil || !strings.Contains(err.Error(), "state quota exceeded") {
		t.Fatalf("error = %v, want state quota wording", err)
	}
}

func TestWriteState_IgnoresDataResourcesForQuotaCounting(t *testing.T) {
	s, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	mustResetTables(t, pool)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 10*time.Second)
	defer cancel()

	if _, err := s.pool.Exec(ctx, `UPDATE tenants SET max_state_resources = 1, max_environment_resources = 1 WHERE id = $1`, auth.SelfHostedTenantID); err != nil {
		t.Fatalf("set resource limits: %v", err)
	}

	raw, err := json.Marshal(map[string]any{
		"version":           4,
		"terraform_version": "1.13.4",
		"serial":            1,
		"lineage":           "11111111-2222-3333-4444-555555555556",
		"outputs":           map[string]any{},
		"resources": []any{
			map[string]any{
				"mode":      "managed",
				"type":      "null_resource",
				"name":      "kept",
				"provider":  "provider[\"registry.terraform.io/hashicorp/null\"]",
				"instances": []any{map[string]any{"schema_version": 0, "attributes": map[string]any{"id": "m1"}}},
			},
			map[string]any{
				"mode":      "data",
				"type":      "terraform_remote_state",
				"name":      "ignored",
				"provider":  "provider[\"terraform.io/builtin/terraform\"]",
				"instances": []any{map[string]any{"schema_version": 0, "attributes": map[string]any{"id": "d1"}}},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := s.WriteState(ctx, "data-ignored", "", raw, "itest", "itest"); err != nil {
		t.Fatalf("write state with data resource under hard limit: %v", err)
	}
}

func TestWriteState_AllowsExactHardStateLimit(t *testing.T) {
	s, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	mustResetTables(t, pool)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 10*time.Second)
	defer cancel()

	if _, err := s.pool.Exec(ctx, `UPDATE tenants SET max_state_resources = 100, max_environment_resources = 10000 WHERE id = $1`, auth.SelfHostedTenantID); err != nil {
		t.Fatalf("set resource limits: %v", err)
	}

	instances := make([]any, 0, 150)
	for i := 0; i < 150; i++ {
		instances = append(instances, map[string]any{
			"schema_version": 0,
			"index_key":      i,
			"attributes":     map[string]any{"id": i},
		})
	}
	raw, err := json.Marshal(map[string]any{
		"version":           4,
		"terraform_version": "1.13.4",
		"serial":            1,
		"lineage":           "11111111-2222-3333-4444-555555555557",
		"outputs":           map[string]any{},
		"resources": []any{
			map[string]any{
				"mode":      "managed",
				"type":      "null_resource",
				"name":      "limit",
				"provider":  "provider[\"registry.terraform.io/hashicorp/null\"]",
				"instances": instances,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := s.WriteState(ctx, "exact-hard-limit", "", raw, "itest", "itest"); err != nil {
		t.Fatalf("write state exactly at hard limit: %v", err)
	}
}

func TestWriteState_DeduplicatesDuplicateManagedAddressesForQuotaCounting(t *testing.T) {
	s, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	mustResetTables(t, pool)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 10*time.Second)
	defer cancel()

	if _, err := s.pool.Exec(ctx, `UPDATE tenants SET max_state_resources = 1, max_environment_resources = 10000 WHERE id = $1`, auth.SelfHostedTenantID); err != nil {
		t.Fatalf("set resource limits: %v", err)
	}

	raw, err := json.Marshal(map[string]any{
		"version":           4,
		"terraform_version": "1.13.4",
		"serial":            1,
		"lineage":           "11111111-2222-3333-4444-555555555558",
		"outputs":           map[string]any{},
		"resources": []any{
			map[string]any{
				"mode":     "managed",
				"type":     "null_resource",
				"name":     "x",
				"provider": "provider[\"registry.terraform.io/hashicorp/null\"]",
				"instances": []any{
					map[string]any{"schema_version": 0, "index_key": 0, "attributes": map[string]any{"id": "current"}},
					map[string]any{"schema_version": 0, "index_key": 0, "status": "deposed", "attributes": map[string]any{"id": "deposed-copy"}},
					map[string]any{"schema_version": 0, "index_key": 1, "attributes": map[string]any{"id": "other"}},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := s.WriteState(ctx, "duplicate-addresses", "", raw, "itest", "itest"); err != nil {
		t.Fatalf("write state with duplicate managed addresses at hard limit: %v", err)
	}
}

func TestWriteState_EnforcesMaxEnvironmentResources(t *testing.T) {
	s, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	mustResetTables(t, pool)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 10*time.Second)
	defer cancel()

	if _, err := s.pool.Exec(ctx, `UPDATE tenants SET max_state_resources = 10000, max_environment_resources = 2 WHERE id = $1`, auth.SelfHostedTenantID); err != nil {
		t.Fatalf("set resource limits: %v", err)
	}

	rawOne, err := json.Marshal(map[string]any{
		"version":           4,
		"terraform_version": "1.13.4",
		"serial":            1,
		"lineage":           "11111111-2222-3333-4444-555555555551",
		"outputs":           map[string]any{},
		"resources": []any{
			map[string]any{
				"mode":      "managed",
				"type":      "null_resource",
				"name":      "x",
				"provider":  "provider[\"registry.terraform.io/hashicorp/null\"]",
				"instances": []any{map[string]any{"schema_version": 0, "attributes": map[string]any{"id": "n1"}}},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	rawTwo, err := json.Marshal(map[string]any{
		"version":           4,
		"terraform_version": "1.13.4",
		"serial":            1,
		"lineage":           "11111111-2222-3333-4444-555555555552",
		"outputs":           map[string]any{},
		"resources": []any{
			map[string]any{
				"mode":      "managed",
				"type":      "null_resource",
				"name":      "y",
				"provider":  "provider[\"registry.terraform.io/hashicorp/null\"]",
				"instances": []any{map[string]any{"schema_version": 0, "attributes": map[string]any{"id": "n2"}}},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	rawThree, err := json.Marshal(map[string]any{
		"version":           4,
		"terraform_version": "1.13.4",
		"serial":            1,
		"lineage":           "11111111-2222-3333-4444-555555555553",
		"outputs":           map[string]any{},
		"resources": []any{
			map[string]any{
				"mode":      "managed",
				"type":      "null_resource",
				"name":      "z",
				"provider":  "provider[\"registry.terraform.io/hashicorp/null\"]",
				"instances": []any{map[string]any{"schema_version": 0, "attributes": map[string]any{"id": "n3"}}},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	rawFour, err := json.Marshal(map[string]any{
		"version":           4,
		"terraform_version": "1.13.4",
		"serial":            1,
		"lineage":           "11111111-2222-3333-4444-555555555554",
		"outputs":           map[string]any{},
		"resources": []any{
			map[string]any{
				"mode":      "managed",
				"type":      "null_resource",
				"name":      "w",
				"provider":  "provider[\"registry.terraform.io/hashicorp/null\"]",
				"instances": []any{map[string]any{"schema_version": 0, "attributes": map[string]any{"id": "n4"}}},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := s.WriteState(ctx, "env-limit-a", "", rawOne, "itest", "itest"); err != nil {
		t.Fatalf("write first state: %v", err)
	}
	if err := s.WriteState(ctx, "env-limit-b", "", rawTwo, "itest", "itest"); err != nil {
		t.Fatalf("write second state: %v", err)
	}

	if err := s.WriteState(ctx, "env-limit-c", "", rawThree, "itest", "itest"); err != nil {
		t.Fatalf("write third state under hard limit: %v", err)
	}

	err = s.WriteState(ctx, "env-limit-d", "", rawFour, "itest", "itest")
	if !errors.Is(err, ErrEntitlementExceeded) {
		t.Fatalf("got %v, want ErrEntitlementExceeded", err)
	}
	if err == nil || !strings.Contains(err.Error(), "environment quota exceeded") {
		t.Fatalf("error = %v, want environment quota wording", err)
	}
}

func TestWriteState_EnvironmentQuotaIsScopedToCurrentEnvironment(t *testing.T) {
	s, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	mustResetTables(t, pool)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 10*time.Second)
	defer cancel()

	if _, err := s.pool.Exec(ctx, `UPDATE tenants SET max_state_resources = 10000, max_environment_resources = 500 WHERE id = $1`, auth.SelfHostedTenantID); err != nil {
		t.Fatalf("set resource limits: %v", err)
	}

	makeState := func(lineage string, instances int) []byte {
		raw, err := json.Marshal(map[string]any{
			"version":           4,
			"terraform_version": "1.13.4",
			"serial":            1,
			"lineage":           lineage,
			"outputs":           map[string]any{},
			"resources": []any{
				map[string]any{
					"mode":     "managed",
					"type":     "null_resource",
					"name":     "x",
					"provider": "provider[\"registry.terraform.io/hashicorp/null\"]",
					"instances": func() []any {
						out := make([]any, 0, instances)
						for i := 0; i < instances; i++ {
							out = append(out, map[string]any{"schema_version": 0, "attributes": map[string]any{"id": i}})
						}
						return out
					}(),
				},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}

	if err := s.WriteState(ctx, "ws_alpha/env_one/a", "", makeState("11111111-2222-3333-4444-555555555571", 400), "itest", "itest"); err != nil {
		t.Fatalf("seed first environment: %v", err)
	}

	if err := s.WriteState(ctx, "ws_alpha/env_two/b", "", makeState("11111111-2222-3333-4444-555555555572", 200), "itest", "itest"); err != nil {
		t.Fatalf("write second environment under own limit: %v", err)
	}
}

func TestWriteState_EnvironmentQuotaIgnoresArchivedStates(t *testing.T) {
	s, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	mustResetTables(t, pool)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 10*time.Second)
	defer cancel()

	if _, err := s.pool.Exec(ctx, `UPDATE tenants SET max_state_resources = 10000, max_environment_resources = 500 WHERE id = $1`, auth.SelfHostedTenantID); err != nil {
		t.Fatalf("set resource limits: %v", err)
	}

	makeState := func(lineage string, instances int) []byte {
		raw, err := json.Marshal(map[string]any{
			"version":           4,
			"terraform_version": "1.13.4",
			"serial":            1,
			"lineage":           lineage,
			"outputs":           map[string]any{},
			"resources": []any{
				map[string]any{
					"mode":     "managed",
					"type":     "null_resource",
					"name":     "x",
					"provider": "provider[\"registry.terraform.io/hashicorp/null\"]",
					"instances": func() []any {
						out := make([]any, 0, instances)
						for i := 0; i < instances; i++ {
							out = append(out, map[string]any{"schema_version": 0, "attributes": map[string]any{"id": i}})
						}
						return out
					}(),
				},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}

	archivedState := "ws_alpha/env_one/old"
	if err := s.WriteState(ctx, archivedState, "", makeState("11111111-2222-3333-4444-555555555581", 400), "itest", "itest"); err != nil {
		t.Fatalf("seed archived state candidate: %v", err)
	}
	if err := s.SetStateLifecycleStatusAudit(ctx, archivedState, LifecycleStatusArchived, "itest", "archive for quota reuse"); err != nil {
		t.Fatalf("archive state: %v", err)
	}

	if err := s.WriteState(ctx, "ws_alpha/env_one/new", "", makeState("11111111-2222-3333-4444-555555555582", 200), "itest", "itest"); err != nil {
		t.Fatalf("write new state after archived state should ignore archived resources: %v", err)
	}
}

func TestWriteState_EnvironmentQuotaCheck_AllowsUpdatingSameStateBelowHardLimit(t *testing.T) {
	s, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	mustResetTables(t, pool)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 10*time.Second)
	defer cancel()

	if _, err := s.pool.Exec(ctx, `UPDATE tenants SET max_state_resources = 10000, max_environment_resources = 15000 WHERE id = $1`, auth.SelfHostedTenantID); err != nil {
		t.Fatalf("set resource limits: %v", err)
	}

	rawStart, err := json.Marshal(map[string]any{
		"version":           4,
		"terraform_version": "1.13.4",
		"serial":            1,
		"lineage":           "11111111-2222-3333-4444-555555555561",
		"outputs":           map[string]any{},
		"resources": []any{
			map[string]any{
				"mode":     "managed",
				"type":     "null_resource",
				"name":     "x",
				"provider": "provider[\"registry.terraform.io/hashicorp/null\"]",
				"instances": []any{
					map[string]any{"schema_version": 0, "attributes": map[string]any{"id": "n1"}},
					map[string]any{"schema_version": 0, "attributes": map[string]any{"id": "n2"}},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	rawGrow, err := json.Marshal(map[string]any{
		"version":           4,
		"terraform_version": "1.13.4",
		"serial":            2,
		"lineage":           "11111111-2222-3333-4444-555555555561",
		"outputs":           map[string]any{},
		"resources": []any{
			map[string]any{
				"mode":     "managed",
				"type":     "null_resource",
				"name":     "x",
				"provider": "provider[\"registry.terraform.io/hashicorp/null\"]",
				"instances": []any{
					map[string]any{"schema_version": 0, "attributes": map[string]any{"id": "n1"}},
					map[string]any{"schema_version": 0, "attributes": map[string]any{"id": "n2"}},
					map[string]any{"schema_version": 0, "attributes": map[string]any{"id": "n3"}},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := s.WriteState(ctx, "env-same-state", "", rawStart, "itest", "itest"); err != nil {
		t.Fatalf("write initial state: %v", err)
	}
	if err := s.WriteState(ctx, "env-same-state", "", rawGrow, "itest", "itest"); err != nil {
		t.Fatalf("grow same state below hard limit: %v", err)
	}
}
