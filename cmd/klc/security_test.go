package main

import (
	"testing"

	"github.com/kilolockio/kilolock/pkg/config"
	"github.com/kilolockio/kilolock/pkg/store"
)

func TestValidateControlServeSecurityConfig(t *testing.T) {
	cfg := config.Config{
		InitMode:                 "prod",
		TLSMode:                  "required",
		TLSCertFile:              "/tmp/cert.pem",
		TLSKeyFile:               "/tmp/key.pem",
		DatabaseURL:              "postgres://u:p@db/kl?sslmode=verify-full",
		ControlPlaneDatabaseURL:  "postgres://u:p@db/meta?sslmode=verify-full",
		DataPlaneDefaultMaxConns: 8,
		DataPlaneDefaultMaxPools: 16,
	}
	if err := validateControlServeSecurityConfig(cfg, ""); err == nil {
		t.Fatal("expected error when control token missing in prod")
	}
	if err := validateControlServeSecurityConfig(cfg, "token"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateControlServeSecurityConfig_RejectsWeakTransportProd(t *testing.T) {
	cfg := config.Config{
		InitMode:                 "prod",
		TLSMode:                  "off",
		DatabaseURL:              "postgres://u:p@db/kl?sslmode=require",
		ControlPlaneDatabaseURL:  "postgres://u:p@db/meta?sslmode=require",
		DataPlaneDefaultMaxConns: 8,
		DataPlaneDefaultMaxPools: 16,
	}
	if err := validateControlServeSecurityConfig(cfg, "token"); err == nil {
		t.Fatal("expected error for non-required TLS mode in prod")
	}

	cfg.TLSMode = "required"
	cfg.TLSCertFile = "/tmp/cert.pem"
	cfg.TLSKeyFile = "/tmp/key.pem"
	if err := validateControlServeSecurityConfig(cfg, "token"); err == nil {
		t.Fatal("expected error for non-verify-full control-plane DSN")
	}
}

func TestValidateControlServeSecurityConfig_AllowsNonTLSProdWhenPolicyDisabled(t *testing.T) {
	allow := false
	cfg := config.Config{
		InitMode:                 "prod",
		TLSMode:                  "off",
		ProdTLSRequiredExplicit:  &allow,
		DatabaseURL:              "postgres://u:p@db/kl?sslmode=disable",
		ControlPlaneDatabaseURL:  "postgres://u:p@db/meta?sslmode=disable",
		DataPlaneDefaultMaxConns: 8,
		DataPlaneDefaultMaxPools: 16,
	}
	if err := validateControlServeSecurityConfig(cfg, "token"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRequiredPermissionForRoute(t *testing.T) {
	cases := []struct {
		method string
		path   string
		perm   string
	}{
		{"GET", "/tenants", "tenant.read"},
		{"POST", "/tenants", "tenant.create"},
		{"POST", "/tenants/lifecycle", "tenant.lifecycle.update"},
		{"POST", "/tenants/delete", "tenant.lifecycle.update"},
		{"GET", "/tenants/acme/environments", "environment.read"},
		{"POST", "/tenants/acme/environments", "environment.create"},
		{"POST", "/tenants/acme/environments/rename", "environment.create"},
		{"POST", "/tenants/acme/environments/lifecycle", "environment.lifecycle.update"},
		{"POST", "/tenants/acme/environments/delete", "environment.lifecycle.update"},
		{"GET", "/tenants/acme/tokens", "token.read"},
		{"POST", "/tenants/acme/tokens", "token.create"},
		{"POST", "/tokens/lifecycle", "token.lifecycle.update"},
		{"POST", "/states/acme/prod/delete", "state.delete"},
		{"POST", "/states/acme/prod/destroy", "state.delete"},
		{"POST", "/retention/purge", "retention.purge"},
		{"GET", "/rbac/grants", "rbac.manage"},
		{"GET", "/operators/tokens", "rbac.manage"},
		{"GET", "/platform/iac-versions", "rbac.manage"},
		{"POST", "/rbac/grants", "rbac.manage"},
		{"POST", "/operators/tokens", "rbac.manage"},
		{"POST", "/rbac/grants/revoke", "rbac.manage"},
		{"GET", "/states/acme/prod", "tenant.read"},
		{"POST", "/states/acme/prod/config", "state.config.update"},
		{"GET", "/ownership-transfers", "tenant.read"},
		{"POST", "/ownership-transfers", "environment.transfer.update"},
		{"POST", "/ownership-transfers/abc/accept", "environment.transfer.update"},
		{"POST", "/ownership-transfers/abc/reject", "environment.transfer.update"},
		{"POST", "/ownership-transfers/abc/cancel", "environment.transfer.update"},
	}
	for _, tc := range cases {
		got, ok := requiredPermissionForRoute(tc.method, tc.path)
		if !ok {
			t.Fatalf("%s %s unexpectedly unguarded", tc.method, tc.path)
		}
		if got != tc.perm {
			t.Fatalf("%s %s permission=%q want=%q", tc.method, tc.path, got, tc.perm)
		}
	}
	if _, ok := requiredPermissionForRoute("GET", "/nope"); ok {
		t.Fatal("unexpected permission mapping for unknown route")
	}
}

func TestFilterStatesForEnvironment(t *testing.T) {
	rows := []store.StateInfo{
		{Name: "ws_aaa/env_prod/app"},
		{Name: "ws_aaa/env_dev/app"},
		{Name: "ws_bbb/env_prod/app"},
		{Name: "just-a-name"},
	}
	got := filterStatesForEnvironment(rows, store.EnvironmentRow{WorkspaceID: "ws_aaa", EnvPublicID: "env_prod"})
	if len(got) != 1 {
		t.Fatalf("filtered states len=%d want 1", len(got))
	}
	if got[0].Name != "ws_aaa/env_prod/app" {
		t.Fatalf("filtered state name=%q want ws_aaa/env_prod/app", got[0].Name)
	}
}
