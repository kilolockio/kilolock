package main

import (
	"testing"

	"github.com/kilolockio/kilolock/pkg/config"
)

func TestValidateServeSecurityConfig_TLSProd(t *testing.T) {
	cfg := config.Config{
		InitMode:                 "prod",
		AuthMode:                 "database",
		TLSMode:                  "required",
		TLSCertFile:              "/tmp/cert.pem",
		TLSKeyFile:               "/tmp/key.pem",
		DatabaseURL:              "postgres://u:p@db/kl?sslmode=verify-full",
		ControlPlaneDatabaseURL:  "postgres://u:p@db/meta?sslmode=verify-full",
		DataPlaneDatabaseURL:     "postgres://u:p@db/data?sslmode=verify-full",
		DataPlaneAdminURL:        "postgres://u:p@db/admin?sslmode=verify-full",
		DataPlaneDefaultMaxConns: 8,
		DataPlaneDefaultMaxPools: 16,
	}
	if err := validateServeSecurityConfig(cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateServeSecurityConfig_RejectsNonTLSProd(t *testing.T) {
	cfg := config.Config{
		InitMode:                 "prod",
		AuthMode:                 "database",
		TLSMode:                  "off",
		DatabaseURL:              "postgres://u:p@db/kl?sslmode=verify-full",
		DataPlaneDefaultMaxConns: 8,
		DataPlaneDefaultMaxPools: 16,
	}
	if err := validateServeSecurityConfig(cfg); err == nil {
		t.Fatal("expected error")
	}
}

func TestValidateServeSecurityConfig_RejectsWeakDBTLSProd(t *testing.T) {
	cfg := config.Config{
		InitMode:                 "prod",
		AuthMode:                 "database",
		TLSMode:                  "required",
		TLSCertFile:              "/tmp/cert.pem",
		TLSKeyFile:               "/tmp/key.pem",
		DatabaseURL:              "postgres://u:p@db/kl?sslmode=require",
		DataPlaneDefaultMaxConns: 8,
		DataPlaneDefaultMaxPools: 16,
	}
	if err := validateServeSecurityConfig(cfg); err == nil {
		t.Fatal("expected error")
	}
}

func TestValidateServeSecurityConfig_AllowsNonTLSProdWhenPolicyDisabled(t *testing.T) {
	allow := false
	cfg := config.Config{
		InitMode:                 "prod",
		AuthMode:                 "database",
		TLSMode:                  "off",
		ProdTLSRequiredExplicit:  &allow,
		DatabaseURL:              "postgres://u:p@db/kl?sslmode=disable",
		ControlPlaneDatabaseURL:  "postgres://u:p@db/meta?sslmode=disable",
		DataPlaneDatabaseURL:     "postgres://u:p@db/data?sslmode=disable",
		DataPlaneAdminURL:        "postgres://u:p@db/admin?sslmode=disable",
		DataPlaneDefaultMaxConns: 8,
		DataPlaneDefaultMaxPools: 16,
	}
	if err := validateServeSecurityConfig(cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
