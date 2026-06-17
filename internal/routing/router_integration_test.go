//go:build integration

package routing

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/kilolockio/kilolock/pkg/auth"
	"github.com/kilolockio/kilolock/pkg/db"
	"github.com/kilolockio/kilolock/pkg/migrate"
	"github.com/kilolockio/kilolock/pkg/provision"
	"github.com/kilolockio/kilolock/pkg/store"
	"github.com/kilolockio/kilolock/pkg/testdb"
)

func TestRouter_SeparateEnvironmentDatabase(t *testing.T) {
	adminURL := testdb.DataPlaneAdminURL()
	baseURL := testdb.DataPlaneBaseURL()
	if adminURL == "" || baseURL == "" {
		t.Skip("set KL_DATABASE_URL for routing integration test")
	}

	url := baseURL
	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 2*time.Minute)
	defer cancel()

	cpPool, err := db.Open(ctx, url)
	if err != nil {
		t.Fatalf("open control plane: %v", err)
	}
	defer cpPool.Close()
	if err := migrate.Run(ctx, cpPool.Pool, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	control := store.New(cpPool.Pool)
	tenant, err := control.GetTenantBySlug(ctx, "route-a")
	if err != nil {
		tenant, err = control.CreateTenant(ctx, "route-a", "Route A")
		if err != nil {
			t.Fatalf("tenant: %v", err)
		}
	}
	env, err := control.GetEnvironmentByTenantSlug(ctx, tenant.Slug, "prod")
	if err != nil {
		env, err = control.CreateEnvironment(ctx, tenant.Slug, "prod", store.EnvironmentTierSharedHost, "shared")
		if err != nil {
			t.Fatalf("env: %v", err)
		}
	}
	if err := provision.CreateDatabase(ctx, adminURL, env.DatabaseName); err != nil {
		t.Fatalf("create db: %v", err)
	}
	envDSN, err := provision.DSNForDatabase(baseURL, env.DatabaseName)
	if err != nil {
		t.Fatalf("dsn: %v", err)
	}
	envPool, err := db.Open(ctx, envDSN)
	if err != nil {
		t.Fatalf("open env: %v", err)
	}
	defer envPool.Close()
	if err := migrate.Run(ctx, envPool.Pool, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("migrate env: %v", err)
	}
	if err := control.SetEnvironmentDSN(ctx, env.ID, envDSN); err != nil {
		t.Fatalf("set dsn: %v", err)
	}
	_, secret, err := control.CreateAPIToken(ctx, tenant.Slug, "prod", fmt.Sprintf("ci-%d", time.Now().UnixNano()))
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	p, err := control.AuthenticateAPIToken(ctx, secret, tenant.Slug)
	if err != nil {
		t.Fatalf("auth: %v", err)
	}

	router := NewRouter(cpPool.Pool, control, NewPoolCache(4))
	defer router.Close()

	reqCtx := auth.WithPrincipal(ctx, p)
	st, err := router.StoreFor(reqCtx)
	if err != nil {
		t.Fatalf("router: %v", err)
	}

	stateName := "isolated-state"
	tf := []byte(`{"version":4,"terraform_version":"1.13.4","serial":1,"lineage":"bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb","outputs":{},"resources":[]}`)
	if err := st.WriteState(reqCtx, stateName, "", tf, "test", "actor"); err != nil {
		t.Fatalf("write: %v", err)
	}

	unified := store.New(cpPool.Pool)
	_, err = unified.GetCurrentState(auth.WithPrincipal(ctx, p), stateName)
	if !errors.Is(err, store.ErrStateNotFound) {
		t.Fatalf("unified db should not see routed state: %v", err)
	}
}
