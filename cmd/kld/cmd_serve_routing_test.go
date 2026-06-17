package main

import (
	"context"
	"testing"

	"github.com/kilolockio/kilolock/pkg/config"
	"github.com/kilolockio/kilolock/pkg/store"
)

type fakeEnvLister struct {
	envs []store.EnvironmentRow
	err  error
}

func (f fakeEnvLister) ListAllEnvironments(context.Context) ([]store.EnvironmentRow, error) {
	return f.envs, f.err
}

func TestValidateEnvironmentRoutingConfig(t *testing.T) {
	cfg := config.Config{
		DataPlaneInstanceURLs: map[string]string{
			"premium": "postgres://premium",
		},
		DataPlaneInstanceAdminURLs: map[string]string{
			"premium": "postgres://premium-admin",
		},
	}
	envs := []store.EnvironmentRow{
		{TenantSlug: "acme", Slug: "prod", DatabaseInstanceKey: "premium"},
		{TenantSlug: "acme", Slug: "staging", DatabaseInstanceKey: "shared"},
	}
	if err := validateEnvironmentRoutingConfig(context.Background(), fakeEnvLister{envs: envs}, cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateEnvironmentRoutingConfigMissingKey(t *testing.T) {
	cfg := config.Config{}
	envs := []store.EnvironmentRow{
		{TenantSlug: "acme", Slug: "prod", DatabaseInstanceKey: "premium"},
	}
	if err := validateEnvironmentRoutingConfig(context.Background(), fakeEnvLister{envs: envs}, cfg); err == nil {
		t.Fatal("expected error")
	}
}
