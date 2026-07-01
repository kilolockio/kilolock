//go:build integration

package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/kilolockio/kilolock/internal/tfstate"
	"github.com/kilolockio/kilolock/pkg/testdb"
)

func TestWriteStateDeltaForApply_PreservesSchemaVersionInCanonicalState(t *testing.T) {
	s, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	mustResetTables(t, pool)

	initial := map[string]any{
		"version":           4,
		"terraform_version": "1.13.4",
		"serial":            1,
		"lineage":           "11111111-2222-3333-4444-555555555599",
		"outputs":           map[string]any{},
		"resources": []any{
			map[string]any{
				"mode":     "managed",
				"type":     "random_string",
				"name":     "label",
				"provider": `provider["registry.terraform.io/hashicorp/random"]`,
				"instances": []any{
					map[string]any{
						"schema_version":       2,
						"attributes":           map[string]any{"id": "seed", "result": "seed", "length": 8},
						"sensitive_attributes": []any{},
					},
				},
			},
		},
	}
	initialRaw, err := json.Marshal(initial)
	if err != nil {
		t.Fatalf("marshal initial state: %v", err)
	}

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 10*time.Second)
	defer cancel()
	if err := s.WriteState(ctx, "schema-version-test", "", initialRaw, "itest", "itest"); err != nil {
		t.Fatalf("WriteState: %v", err)
	}

	delta := map[string]any{
		"version":           4,
		"terraform_version": "1.13.4",
		"serial":            0,
		"lineage":           "11111111-2222-3333-4444-555555555599",
		"outputs":           map[string]any{},
		"resources":         initial["resources"],
	}
	deltaRaw, err := json.Marshal(delta)
	if err != nil {
		t.Fatalf("marshal delta state: %v", err)
	}

	if err := s.WriteStateDeltaForApply(ctx, "schema-version-test", "", 1, deltaRaw, "itest", "itest", []string{"random_string.label"}); err != nil {
		t.Fatalf("WriteStateDeltaForApply: %v", err)
	}

	currentRaw, err := s.GetCurrentState(ctx, "schema-version-test")
	if err != nil {
		t.Fatalf("GetCurrentState: %v", err)
	}
	parsed, err := tfstate.Parse(currentRaw)
	if err != nil {
		t.Fatalf("Parse(current): %v", err)
	}
	if len(parsed.Resources) != 1 || len(parsed.Resources[0].Instances) != 1 {
		t.Fatalf("unexpected resources shape: %#v", parsed.Resources)
	}
	if got := parsed.Resources[0].Instances[0].SchemaVersion; got != 2 {
		t.Fatalf("schema_version after canonical rebuild = %d; want 2\nraw:\n%s", got, currentRaw)
	}
}
