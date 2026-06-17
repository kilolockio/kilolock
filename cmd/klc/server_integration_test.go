//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/davesade/kilolock/internal/config"
	"github.com/davesade/kilolock/internal/migrate"
	"github.com/davesade/kilolock/internal/testdb"
	"github.com/davesade/kilolock/pkg/store"
)

func seedControlAPIStateRaw(serial int64) []byte {
	return []byte(`{
  "version": 4,
  "terraform_version": "1.9.8",
  "serial": ` + strconv.FormatInt(serial, 10) + `,
  "lineage": "11111111-1111-1111-1111-111111111111",
  "outputs": {},
  "resources": []
}`)
}

func doAuthReq(t *testing.T, baseURL, token, method, path string, body []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, baseURL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return res
}

func openControlIntegrationStore(t *testing.T, ctx context.Context, dsn string) (*pgxpool.Pool, *store.Store) {
	t.Helper()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := migrate.Run(ctx, pool, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		pool.Close()
		t.Fatalf("migrate db: %v", err)
	}
	return pool, store.New(pool)
}

func TestControlAPI_ScopeEnforcedOnCriticalRoutes(t *testing.T) {
	dsn := os.Getenv("KL_DATABASE_URL")
	if dsn == "" {
		t.Skip("KL_DATABASE_URL is required for integration test")
	}
	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 30*time.Second)
	defer cancel()
	pool, st := openControlIntegrationStore(t, ctx, dsn)
	defer pool.Close()

	suffix := time.Now().UTC().Format("20060102150405.000000000")
	acme := "acme-" + suffix
	beta := "beta-" + suffix
	if _, err := st.CreateTenant(ctx, acme, "Acme"); err != nil {
		t.Fatalf("create tenant acme: %v", err)
	}
	if _, err := st.CreateTenant(ctx, beta, "Beta"); err != nil {
		t.Fatalf("create tenant beta: %v", err)
	}

	adminRow, adminSecret, err := st.CreateAPIToken(ctx, acme, "default", "tenant-admin")
	if err != nil {
		t.Fatalf("create tenant-admin token: %v", err)
	}
	if err := st.EnsurePrincipalRole(ctx, "api_token", adminRow.ID, "tenant_admin", "tenant", acme, "itest"); err != nil {
		t.Fatalf("grant tenant_admin: %v", err)
	}

	betaTokenRow, _, err := st.CreateAPIToken(ctx, beta, "default", "victim")
	if err != nil {
		t.Fatalf("create beta token: %v", err)
	}

	s := newServer(
		st,
		config.Defaults(),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		"static-super-token",
	)
	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	// 1) Tenant-scoped admin cannot mutate lifecycle of token in a different tenant.
	{
		body, _ := json.Marshal(map[string]any{
			"token_id": betaTokenRow.ID,
			"status":   "suspended",
			"reason":   "itest scope check",
		})
		res := doAuthReq(t, ts.URL, adminSecret, http.MethodPost, "/api/tokens/lifecycle", body)
		defer res.Body.Close()
		if res.StatusCode != http.StatusForbidden {
			t.Fatalf("tokens/lifecycle cross-tenant status=%d want 403", res.StatusCode)
		}
	}

	// 1b) Tenant-scoped admin can mutate lifecycle of token in same tenant.
	{
		acmeTokenRow, _, err := st.CreateAPIToken(ctx, acme, "default", "same-tenant")
		if err != nil {
			t.Fatalf("create acme token: %v", err)
		}
		body, _ := json.Marshal(map[string]any{
			"token_id": acmeTokenRow.ID,
			"status":   "suspended",
			"reason":   "itest positive path",
		})
		res := doAuthReq(t, ts.URL, adminSecret, http.MethodPost, "/api/tokens/lifecycle", body)
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("tokens/lifecycle same-tenant status=%d want 200", res.StatusCode)
		}
	}

	// 2) Tenant-scoped admin cannot list states for a different tenant env.
	{
		res := doAuthReq(t, ts.URL, adminSecret, http.MethodGet, "/api/states/"+beta+"/default", nil)
		defer res.Body.Close()
		if res.StatusCode != http.StatusForbidden {
			t.Fatalf("states cross-tenant status=%d want 403", res.StatusCode)
		}
	}

	// 3) Tenant-scoped admin cannot lifecycle-mutate a different tenant.
	{
		body, _ := json.Marshal(map[string]any{
			"slug":   beta,
			"status": "suspended",
			"reason": "itest scope check",
		})
		res := doAuthReq(t, ts.URL, adminSecret, http.MethodPost, "/api/tenants/lifecycle", body)
		defer res.Body.Close()
		if res.StatusCode != http.StatusForbidden {
			t.Fatalf("tenant lifecycle cross-tenant status=%d want 403", res.StatusCode)
		}
	}

	// 3b) Tenant-scoped admin can lifecycle-mutate its own tenant.
	{
		body, _ := json.Marshal(map[string]any{
			"slug":   acme,
			"status": "suspended",
			"reason": "itest positive path",
		})
		res := doAuthReq(t, ts.URL, adminSecret, http.MethodPost, "/api/tenants/lifecycle", body)
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("tenant lifecycle same-tenant status=%d want 200", res.StatusCode)
		}
	}

	// 4) Tenant-scoped admin can read its own tenant row, but not others.
	{
		res := doAuthReq(t, ts.URL, adminSecret, http.MethodGet, "/api/tenants/"+acme, nil)
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("tenant get same-tenant status=%d want 200", res.StatusCode)
		}
	}
	{
		res := doAuthReq(t, ts.URL, adminSecret, http.MethodGet, "/api/tenants/"+beta, nil)
		defer res.Body.Close()
		if res.StatusCode != http.StatusForbidden {
			t.Fatalf("tenant get cross-tenant status=%d want 403", res.StatusCode)
		}
	}
}

func TestControlAPI_AuthorizationSweep(t *testing.T) {
	dsn := os.Getenv("KL_DATABASE_URL")
	if dsn == "" {
		t.Skip("KL_DATABASE_URL is required for integration test")
	}
	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 30*time.Second)
	defer cancel()
	pool, st := openControlIntegrationStore(t, ctx, dsn)
	defer pool.Close()

	suffix := strings.ToLower(time.Now().UTC().Format("20060102150405.000000000"))
	acme := "acme-" + suffix
	beta := "beta-" + suffix
	if _, err := st.CreateTenant(ctx, acme, "Acme"); err != nil {
		t.Fatalf("create tenant acme: %v", err)
	}
	if _, err := st.CreateTenant(ctx, beta, "Beta"); err != nil {
		t.Fatalf("create tenant beta: %v", err)
	}
	if _, err := st.CreateEnvironment(ctx, acme, "prod", store.EnvironmentTierSharedHost, ""); err != nil {
		t.Fatalf("create acme/prod: %v", err)
	}
	if _, err := st.CreateEnvironment(ctx, beta, "prod", store.EnvironmentTierSharedHost, ""); err != nil {
		t.Fatalf("create beta/prod: %v", err)
	}
	acmeProd, err := st.GetEnvironmentByTenantSlug(ctx, acme, "prod")
	if err != nil {
		t.Fatalf("get acme/prod: %v", err)
	}
	if err := st.SetEnvironmentDSN(ctx, acmeProd.ID, dsn); err != nil {
		t.Fatalf("set acme/prod dsn: %v", err)
	}
	if err := store.NewIsolated(pool).WriteState(ctx, "qtest", "", seedControlAPIStateRaw(1), "itest", "itest"); err != nil {
		t.Fatalf("seed qtest state: %v", err)
	}

	platformRow, platformSecret, err := st.CreateAPIToken(ctx, acme, "default", "platform")
	if err != nil {
		t.Fatalf("create platform token: %v", err)
	}
	tenantRow, tenantSecret, err := st.CreateAPIToken(ctx, acme, "default", "tenant-admin")
	if err != nil {
		t.Fatalf("create tenant-admin token: %v", err)
	}
	readonlyRow, readonlySecret, err := st.CreateAPIToken(ctx, acme, "default", "readonly")
	if err != nil {
		t.Fatalf("create readonly token: %v", err)
	}
	provisionerRow, provisionerSecret, err := st.CreateAPIToken(ctx, acme, "default", "provisioner")
	if err != nil {
		t.Fatalf("create provisioner token: %v", err)
	}
	if err := st.EnsurePrincipalRole(ctx, "api_token", platformRow.ID, "platform_admin", "global", "", "itest"); err != nil {
		t.Fatalf("grant platform_admin: %v", err)
	}
	if err := st.EnsurePrincipalRole(ctx, "api_token", tenantRow.ID, "tenant_admin", "tenant", acme, "itest"); err != nil {
		t.Fatalf("grant tenant_admin: %v", err)
	}
	if err := st.EnsurePrincipalRole(ctx, "api_token", readonlyRow.ID, "support_readonly", "tenant", acme, "itest"); err != nil {
		t.Fatalf("grant support_readonly: %v", err)
	}
	if err := st.EnsurePrincipalRole(ctx, "api_token", provisionerRow.ID, "provisioner", "tenant", acme, "itest"); err != nil {
		t.Fatalf("grant provisioner: %v", err)
	}

	s := newServer(
		st,
		config.Defaults(),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		"static-super-token",
	)
	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	acmeTokenRow, _, err := st.CreateAPIToken(ctx, acme, "default", "for-lifecycle")
	if err != nil {
		t.Fatalf("create token for lifecycle: %v", err)
	}
	betaTokenRow, _, err := st.CreateAPIToken(ctx, beta, "default", "for-lifecycle")
	if err != nil {
		t.Fatalf("create beta token for lifecycle: %v", err)
	}

	cases := []struct {
		name       string
		token      string
		method     string
		path       string
		body       string
		wantStatus int
	}{
		{"platform list tenants", platformSecret, http.MethodGet, "/api/tenants", "", http.StatusOK},
		{"tenant-admin list tenants denied", tenantSecret, http.MethodGet, "/api/tenants", "", http.StatusForbidden},
		{"readonly list envs same tenant", readonlySecret, http.MethodGet, "/api/tenants/" + acme + "/environments", "", http.StatusOK},
		{"readonly list envs cross tenant denied", readonlySecret, http.MethodGet, "/api/tenants/" + beta + "/environments", "", http.StatusForbidden},
		{"tenant-admin create env same tenant", tenantSecret, http.MethodPost, "/api/tenants/" + acme + "/environments", `{"slug":"qa","tier":"shared"}`, http.StatusCreated},
		{"tenant-admin create env cross tenant denied", tenantSecret, http.MethodPost, "/api/tenants/" + beta + "/environments", `{"slug":"qa","tier":"shared"}`, http.StatusForbidden},
		{"provisioner create env denied", provisionerSecret, http.MethodPost, "/api/tenants/" + acme + "/environments", `{"slug":"prov-denied","tier":"shared"}`, http.StatusForbidden},
		{"tenant-admin list tokens same tenant", tenantSecret, http.MethodGet, "/api/tenants/" + acme + "/tokens", "", http.StatusOK},
		{"tenant-admin list tokens cross tenant denied", tenantSecret, http.MethodGet, "/api/tenants/" + beta + "/tokens", "", http.StatusForbidden},
		{"readonly create token denied", readonlySecret, http.MethodPost, "/api/tenants/" + acme + "/tokens", `{"environment":"default","name":"x"}`, http.StatusForbidden},
		{"tenant-admin create token same tenant", tenantSecret, http.MethodPost, "/api/tenants/" + acme + "/tokens", `{"environment":"default","name":"new-token"}`, http.StatusCreated},
		{"tenant-admin token lifecycle same tenant", tenantSecret, http.MethodPost, "/api/tokens/lifecycle", `{"token_id":"` + acmeTokenRow.ID + `","status":"suspended","reason":"itest"}`, http.StatusOK},
		{"tenant-admin token delete same tenant", tenantSecret, http.MethodPost, "/api/tokens/lifecycle", `{"token_id":"` + acmeTokenRow.ID + `","status":"archived","reason":"itest"}`, http.StatusOK},
		{"tenant-admin token lifecycle cross tenant denied", tenantSecret, http.MethodPost, "/api/tokens/lifecycle", `{"token_id":"` + betaTokenRow.ID + `","status":"suspended","reason":"itest"}`, http.StatusForbidden},
		{"tenant-admin state config same tenant env", tenantSecret, http.MethodPost, "/api/states/" + acme + "/prod/config", `{"state":"qtest","coexistence_mode":"strict"}`, http.StatusOK},
		{"readonly state config denied", readonlySecret, http.MethodPost, "/api/states/" + acme + "/prod/config", `{"state":"qtest","coexistence_mode":"strict"}`, http.StatusForbidden},
		{"tenant-admin state config cross tenant denied", tenantSecret, http.MethodPost, "/api/states/" + beta + "/prod/config", `{"state":"qtest","coexistence_mode":"strict"}`, http.StatusForbidden},
		{"readonly states same tenant env", readonlySecret, http.MethodGet, "/api/states/" + acme + "/prod", "", http.StatusOK},
		{"readonly states cross tenant env denied", readonlySecret, http.MethodGet, "/api/states/" + beta + "/prod", "", http.StatusForbidden},
		{"provisioner states read denied", provisionerSecret, http.MethodGet, "/api/states/" + acme + "/prod", "", http.StatusForbidden},
		{"tenant-admin state delete same tenant env", tenantSecret, http.MethodPost, "/api/states/" + acme + "/prod/delete", `{"state":"qtest","reason":"itest"}`, http.StatusOK},
		{"tenant-admin state delete cross tenant denied", tenantSecret, http.MethodPost, "/api/states/" + beta + "/prod/delete", `{"state":"qtest","reason":"itest"}`, http.StatusForbidden},
		{"platform rbac grants list", platformSecret, http.MethodGet, "/api/rbac/grants", "", http.StatusOK},
		{"tenant-admin rbac grants list denied", tenantSecret, http.MethodGet, "/api/rbac/grants", "", http.StatusForbidden},
		{"platform iac versions", platformSecret, http.MethodGet, "/api/platform/iac-versions", "", http.StatusOK},
		{"tenant-admin iac versions denied", tenantSecret, http.MethodGet, "/api/platform/iac-versions", "", http.StatusForbidden},
		{"platform retention purge dry-run", platformSecret, http.MethodPost, "/api/retention/purge", `{"older_than_hours":24,"tenant":"` + acme + `","reason":"itest","apply":false}`, http.StatusOK},
		{"tenant-admin retention purge denied", tenantSecret, http.MethodPost, "/api/retention/purge", `{"older_than_hours":24,"tenant":"` + acme + `","reason":"itest","apply":false}`, http.StatusForbidden},
		{"tenant-admin include_inactive tokens denied", tenantSecret, http.MethodGet, "/api/tenants/" + acme + "/tokens?include_inactive=true", "", http.StatusForbidden},
		{"tenant-admin include_inactive envs denied", tenantSecret, http.MethodGet, "/api/tenants/" + acme + "/environments?include_inactive=true", "", http.StatusForbidden},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var body []byte
			if tc.body != "" {
				body = []byte(tc.body)
			}
			res := doAuthReq(t, ts.URL, tc.token, tc.method, tc.path, body)
			defer res.Body.Close()
			if res.StatusCode != tc.wantStatus {
				raw, _ := io.ReadAll(res.Body)
				t.Fatalf("%s %s status=%d want=%d body=%s", tc.method, tc.path, res.StatusCode, tc.wantStatus, string(raw))
			}
		})
	}

	if _, err := st.GetAPITokenByID(ctx, acmeTokenRow.ID); !errors.Is(err, store.ErrAPITokenNotFound) {
		t.Fatalf("token delete err=%v want ErrAPITokenNotFound", err)
	}
	if _, err := store.NewIsolated(pool).GetCurrentState(ctx, "qtest"); !errors.Is(err, store.ErrStateNotFound) {
		t.Fatalf("qtest delete err=%v want ErrStateNotFound", err)
	}
	if status, err := store.NewIsolated(pool).GetStateLifecycleStatus(ctx, "qtest"); err != nil {
		t.Fatalf("qtest lifecycle status err=%v", err)
	} else if status != store.LifecycleStatusArchived {
		t.Fatalf("qtest lifecycle status=%q want %q", status, store.LifecycleStatusArchived)
	}
}

func TestControlAPI_OwnershipTransfersByOperator(t *testing.T) {
	dsn := os.Getenv("KL_DATABASE_URL")
	if dsn == "" {
		t.Skip("KL_DATABASE_URL is required for integration test")
	}
	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 30*time.Second)
	defer cancel()
	pool, st := openControlIntegrationStore(t, ctx, dsn)
	defer pool.Close()

	suffix := strings.ToLower(strings.ReplaceAll(time.Now().UTC().Format("20060102150405.000000000"), ".", "-"))
	source := "source-" + suffix
	target := "target-" + suffix
	if _, err := st.CreateTenant(ctx, source, "Source"); err != nil {
		t.Fatalf("create source tenant: %v", err)
	}
	if _, err := st.CreateTenant(ctx, target, "Target"); err != nil {
		t.Fatalf("create target tenant: %v", err)
	}
	if _, err := st.CreateEnvironment(ctx, source, "prod", store.EnvironmentTierSharedHost, ""); err != nil {
		t.Fatalf("create env: %v", err)
	}

	s := newServer(
		st,
		config.Defaults(),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		"static-super-token",
	)
	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	body, _ := json.Marshal(map[string]any{
		"source_tenant":      source,
		"environment_slug":   "prod",
		"target_tenant_slug": target,
		"actor":              "operator@example.com",
		"reason":             "customer requested move",
	})
	res := doAuthReq(t, ts.URL, "static-super-token", http.MethodPost, "/api/ownership-transfers", body)
	defer res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(res.Body)
		t.Fatalf("create transfer status=%d want 201 body=%s", res.StatusCode, string(b))
	}
	var created struct {
		Proposal store.OwnershipTransferProposal `json:"proposal"`
	}
	raw, _ := io.ReadAll(res.Body)
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatalf("decode created transfer: %v (%s)", err, string(raw))
	}

	listRes := doAuthReq(t, ts.URL, "static-super-token", http.MethodGet, "/api/ownership-transfers?tenant="+target+"&status=pending", nil)
	defer listRes.Body.Close()
	if listRes.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(listRes.Body)
		t.Fatalf("list transfers status=%d want 200 body=%s", listRes.StatusCode, string(b))
	}

	resolveBody, _ := json.Marshal(map[string]any{"actor": "operator@example.com", "target_new_slug": "prod-moved"})
	acceptRes := doAuthReq(t, ts.URL, "static-super-token", http.MethodPost, "/api/ownership-transfers/"+created.Proposal.ID+"/accept", resolveBody)
	defer acceptRes.Body.Close()
	if acceptRes.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(acceptRes.Body)
		t.Fatalf("accept transfer status=%d want 200 body=%s", acceptRes.StatusCode, string(b))
	}

	if _, err := st.GetEnvironmentByTenantSlug(ctx, source, "prod"); err == nil {
		t.Fatalf("environment still under source tenant after operator acceptance")
	}
	env, err := st.GetEnvironmentByTenantSlug(ctx, target, "prod-moved")
	if err != nil {
		t.Fatalf("get moved env: %v", err)
	}
	if env.TenantSlug != target {
		t.Fatalf("moved env tenant=%q want %q", env.TenantSlug, target)
	}
}
