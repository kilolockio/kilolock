//go:build integration && cloud

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/davesade/kilolock/internal/config"
	"github.com/davesade/kilolock/internal/testdb"
	"github.com/davesade/kilolock/pkg/store"
)

func TestPublicSignup_CreatesTenantDefaultEnvAndToken(t *testing.T) {
	dsn := os.Getenv("KL_DATABASE_URL")
	if dsn == "" {
		t.Skip("KL_DATABASE_URL is required for integration test")
	}
	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 30*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer pool.Close()
	st := store.New(pool)

	s := newServer(
		st,
		config.Defaults(),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		"static-super-token",
	)
	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	t.Setenv("KL_PUBLIC_SIGNUP_MODE", "trusted_header")

	body, _ := json.Marshal(map[string]any{
		"email":       "acme-signup@example.com",
		"tenant_slug": "acme-signup-" + time.Now().UTC().Format("20060102150405"),
		"tenant_name": "Acme Signup",
		"token_name":  "first",
	})
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/public/signup", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Goog-Authenticated-User-Email", "accounts.google.com:acme-signup@example.com")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(res.Body)
		t.Fatalf("status=%d want 201 body=%s", res.StatusCode, string(b))
	}

	// Verify tenant has starter entitlements applied.
	tenantSlug := "acme-signup-" + time.Now().UTC().Format("20060102150405")
	// Read back slug from request body so we don't race the time tick.
	var reqBody map[string]any
	_ = json.Unmarshal(body, &reqBody)
	if s, ok := reqBody["tenant_slug"].(string); ok && s != "" {
		tenantSlug = s
	}
	row, err := st.GetTenantBySlug(ctx, tenantSlug)
	if err != nil {
		t.Fatalf("GetTenantBySlug: %v", err)
	}
	if row.BillingPlan != "starter" || row.MaxEnvironments != 1 || row.MaxStateResources != 100 || row.MaxEnvironmentResources != 500 {
		t.Fatalf("unexpected entitlements: plan=%s max_env=%d max_state_resources=%d max_environment_resources=%d",
			row.BillingPlan, row.MaxEnvironments, row.MaxStateResources, row.MaxEnvironmentResources)
	}
	if row.StripeCustomerID != "" || row.StripeSubID != "" || row.StripeSubStatus != "" {
		t.Fatalf("unexpected stripe fields on signup tenant: customer=%q sub=%q status=%q", row.StripeCustomerID, row.StripeSubID, row.StripeSubStatus)
	}
}
