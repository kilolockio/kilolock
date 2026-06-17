//go:build integration

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/davesade/kilolock/internal/auth"
	"github.com/davesade/kilolock/internal/testdb"
)

func TestMoveEnvironmentStateNamespace_RehomesSharedStateToTargetWorkspace(t *testing.T) {
	s, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	mustResetTables(t, pool)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 20*time.Second)
	defer cancel()

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	sourceSlug := "source-" + suffix
	targetSlug := "target-" + suffix
	if _, err := s.CreateTenant(ctx, sourceSlug, "Source"); err != nil {
		t.Fatalf("create source tenant: %v", err)
	}
	targetTenant, err := s.CreateTenant(ctx, targetSlug, "Target")
	if err != nil {
		t.Fatalf("create target tenant: %v", err)
	}
	sourceEnv, err := s.CreateEnvironment(ctx, sourceSlug, "prod", EnvironmentTierSharedHost, "")
	if err != nil {
		t.Fatalf("create source env: %v", err)
	}

	_, secret, err := s.CreateAPIToken(ctx, sourceSlug, "prod", "ci")
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	sourcePrincipal, err := s.AuthenticateAPIToken(ctx, secret, sourceSlug)
	if err != nil {
		t.Fatalf("authenticate token: %v", err)
	}
	oldStateName := sourcePrincipal.WorkspaceID + "/" + sourcePrincipal.EnvironmentPublicID + "/my-awesome-project"
	raw, err := json.Marshal(map[string]any{
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
	if err := s.WriteState(auth.WithPrincipal(ctx, sourcePrincipal), oldStateName, "", raw, "itest", "itest"); err != nil {
		t.Fatalf("write state: %v", err)
	}

	if err := s.MoveEnvironmentStateNamespace(ctx, sourceEnv.TenantID, targetTenant.ID, sourceEnv.WorkspaceID, targetTenant.WorkspaceID, sourceEnv.EnvPublicID); err != nil {
		t.Fatalf("move environment state namespace: %v", err)
	}

	if _, err := s.GetCurrentState(auth.WithPrincipal(ctx, sourcePrincipal), oldStateName); !errors.Is(err, ErrStateNotFound) {
		t.Fatalf("old state lookup err=%v, want ErrStateNotFound", err)
	}

	targetCtx := auth.WithPrincipal(ctx, auth.Principal{
		TenantID:            targetTenant.ID,
		WorkspaceID:         targetTenant.WorkspaceID,
		TenantSlug:          targetTenant.Slug,
		EnvironmentID:       sourceEnv.ID,
		EnvironmentPublicID: sourceEnv.EnvPublicID,
		EnvironmentSlug:     sourceEnv.Slug,
	})
	newStateName := targetTenant.WorkspaceID + "/" + sourceEnv.EnvPublicID + "/my-awesome-project"
	got, err := s.GetCurrentState(targetCtx, newStateName)
	if err != nil {
		t.Fatalf("new state lookup: %v", err)
	}
	if string(got) == "" {
		t.Fatal("new state raw is empty")
	}

	var stateTenantID string
	if err := pool.QueryRow(ctx, `SELECT tenant_id::text FROM states WHERE name = $1`, newStateName).Scan(&stateTenantID); err != nil {
		t.Fatalf("query moved state tenant_id: %v", err)
	}
	if stateTenantID != targetTenant.ID {
		t.Fatalf("moved state tenant_id=%q want %q", stateTenantID, targetTenant.ID)
	}

	var resourceTenantID string
	if err := pool.QueryRow(ctx, `
SELECT r.tenant_id::text
FROM resources r
JOIN states s ON s.id = r.state_id
WHERE s.name = $1
LIMIT 1`, newStateName).Scan(&resourceTenantID); err != nil {
		t.Fatalf("query moved resource tenant_id: %v", err)
	}
	if resourceTenantID != targetTenant.ID {
		t.Fatalf("moved resource tenant_id=%q want %q", resourceTenantID, targetTenant.ID)
	}

	targetPrincipal, err := s.AuthenticateAPIToken(ctx, secret, targetTenant.WorkspaceID)
	if err != nil {
		t.Fatalf("authenticate transferred token against target workspace: %v", err)
	}
	if targetPrincipal.WorkspaceID != targetTenant.WorkspaceID {
		t.Fatalf("token workspace_id=%q want %q", targetPrincipal.WorkspaceID, targetTenant.WorkspaceID)
	}
	if targetPrincipal.EnvironmentPublicID != sourceEnv.EnvPublicID {
		t.Fatalf("token env_public_id=%q want %q", targetPrincipal.EnvironmentPublicID, sourceEnv.EnvPublicID)
	}

	newProjectState := targetPrincipal.WorkspaceID + "/" + targetPrincipal.EnvironmentPublicID + "/brand-new-state"
	newRaw, err := json.Marshal(map[string]any{
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
	if err := s.WriteState(auth.WithPrincipal(ctx, targetPrincipal), newProjectState, "", newRaw, "itest", "itest"); err != nil {
		t.Fatalf("write brand-new state with transferred token: %v", err)
	}
	if _, err := s.GetCurrentState(auth.WithPrincipal(ctx, targetPrincipal), newProjectState); err != nil {
		t.Fatalf("read brand-new state with transferred token: %v", err)
	}
}
