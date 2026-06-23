package config

import (
	"testing"
)

func TestDefaults(t *testing.T) {
	c := Defaults()
	if c.ListenAddr != ":8080" {
		t.Errorf("ListenAddr = %q, want :8080", c.ListenAddr)
	}
	if c.LogLevel != "info" {
		t.Errorf("LogLevel = %q, want info", c.LogLevel)
	}
	if c.LogFormat != "text" {
		t.Errorf("LogFormat = %q, want text", c.LogFormat)
	}
	if c.RoutingStatsIntervalSeconds != 60 {
		t.Errorf("RoutingStatsIntervalSeconds = %d, want 60", c.RoutingStatsIntervalSeconds)
	}
	if c.RoutingCircuitFailureThreshold != 2 {
		t.Errorf("RoutingCircuitFailureThreshold = %d, want 2", c.RoutingCircuitFailureThreshold)
	}
	if c.RoutingCircuitCooldownSeconds != 10 {
		t.Errorf("RoutingCircuitCooldownSeconds = %d, want 10", c.RoutingCircuitCooldownSeconds)
	}
	if !c.EnvMigrationEnabled {
		t.Errorf("EnvMigrationEnabled = false, want true")
	}
	if c.EnvMigrationIntervalSeconds != 300 {
		t.Errorf("EnvMigrationIntervalSeconds = %d, want 300", c.EnvMigrationIntervalSeconds)
	}
	if c.DataPlaneDefaultMaxConns != 8 {
		t.Errorf("DataPlaneDefaultMaxConns = %d, want 8", c.DataPlaneDefaultMaxConns)
	}
	if c.DataPlaneDefaultMaxPools != 16 {
		t.Errorf("DataPlaneDefaultMaxPools = %d, want 16", c.DataPlaneDefaultMaxPools)
	}
}

func TestLoad_PrefersKilolockPrefix(t *testing.T) {
	t.Setenv("KL_DATABASE_URL", "postgres://from-kl/db")
	t.Setenv("DATABASE_URL", "postgres://from-generic/db")

	c := Load()

	if c.DatabaseURL != "postgres://from-kl/db" {
		t.Errorf("DatabaseURL = %q, want postgres://from-kl/db", c.DatabaseURL)
	}
}

func TestLoad_FallsBackToDatabaseURL(t *testing.T) {
	t.Setenv("KL_DATABASE_URL", "")
	t.Setenv("DATABASE_URL", "postgres://from-generic/db")

	c := Load()

	if c.DatabaseURL != "postgres://from-generic/db" {
		t.Errorf("DatabaseURL = %q, want postgres://from-generic/db", c.DatabaseURL)
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		c       Config
		wantErr bool
	}{
		{
			name: "valid",
			c: Config{
				DatabaseURL:              "postgres://x/db",
				ListenAddr:               ":8080",
				LogLevel:                 "info",
				LogFormat:                "text",
				DataPlaneDefaultMaxConns: 8,
				DataPlaneDefaultMaxPools: 16,
			},
			wantErr: false,
		},
		{
			name:    "missing database url",
			c:       Config{ListenAddr: ":8080", LogLevel: "info", LogFormat: "text"},
			wantErr: true,
		},
		{
			name: "missing listen addr",
			c: Config{
				DatabaseURL: "postgres://x/db",
				LogLevel:    "info",
				LogFormat:   "text",
			},
			wantErr: true,
		},
		{
			name: "bad log level",
			c: Config{
				DatabaseURL: "postgres://x/db",
				ListenAddr:  ":8080",
				LogLevel:    "trace",
				LogFormat:   "text",
			},
			wantErr: true,
		},
		{
			name: "bad log format",
			c: Config{
				DatabaseURL: "postgres://x/db",
				ListenAddr:  ":8080",
				LogLevel:    "info",
				LogFormat:   "xml",
			},
			wantErr: true,
		},
		{
			name: "oidc google missing redirect config",
			c: Config{
				DatabaseURL:                  "postgres://x/db",
				ListenAddr:                   ":8080",
				LogLevel:                     "info",
				LogFormat:                    "text",
				PortalAuthMode:               "oidc_google",
				PortalOIDCGoogleClientID:     "client",
				PortalOIDCGoogleClientSecret: "secret",
				DataPlaneDefaultMaxConns:     8,
				DataPlaneDefaultMaxPools:     16,
			},
			wantErr: true,
		},
		{
			name: "mixed google missing redirect config",
			c: Config{
				DatabaseURL:                  "postgres://x/db",
				ListenAddr:                   ":8080",
				LogLevel:                     "info",
				LogFormat:                    "text",
				PortalAuthMode:               "mixed",
				PortalOIDCGoogleClientID:     "client",
				PortalOIDCGoogleClientSecret: "secret",
				DataPlaneDefaultMaxConns:     8,
				DataPlaneDefaultMaxPools:     16,
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.c.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestPortalAuthResolution_MixedWithGoogleKeepsPasswordAndGoogle(t *testing.T) {
	c := Config{
		PortalAuthMode:               "mixed",
		PortalOIDCGoogleClientID:     "client",
		PortalOIDCGoogleClientSecret: "secret",
		PortalOIDCGoogleRedirectURL:  "http://localhost:8091/v1/portal/api/oidc/callback",
	}

	if !c.ResolvedPortalPasswordAuthEnabled() {
		t.Fatalf("password auth should stay enabled in mixed mode")
	}
	if !c.ResolvedPortalOIDCEnabled() {
		t.Fatalf("oidc should stay enabled in mixed mode")
	}
	if !c.ResolvedPortalOIDCGoogleEnabled() {
		t.Fatalf("google oidc should be enabled in mixed mode when credentials are present")
	}
}

func TestResolvedDataPlaneURLForInstance(t *testing.T) {
	c := Config{
		DatabaseURL: "postgres://default/db",
		DataPlaneInstanceURLs: map[string]string{
			"premium": "postgres://premium/db",
		},
	}
	if got := c.ResolvedDataPlaneURLForInstance("premium"); got != "postgres://premium/db" {
		t.Fatalf("premium => %q", got)
	}
	if got := c.ResolvedDataPlaneURLForInstance("shared"); got != "postgres://default/db" {
		t.Fatalf("shared => %q", got)
	}
}

func TestResolvedDataPlaneAdminURLForInstance(t *testing.T) {
	c := Config{
		DataPlaneAdminURL: "postgres://admin/default",
		DataPlaneInstanceAdminURLs: map[string]string{
			"premium": "postgres://admin/premium",
		},
	}
	if got := c.ResolvedDataPlaneAdminURLForInstance("premium"); got != "postgres://admin/premium" {
		t.Fatalf("premium => %q", got)
	}
	if got := c.ResolvedDataPlaneAdminURLForInstance("shared"); got != "postgres://admin/default" {
		t.Fatalf("shared => %q", got)
	}
}

func TestLoad_RoutingStatsIntervalFromEnv(t *testing.T) {
	t.Setenv("KL_DATABASE_URL", "postgres://from-kl/db")
	t.Setenv("KL_ROUTING_STATS_INTERVAL_SECONDS", "15")
	c := Load()
	if c.RoutingStatsIntervalSeconds != 15 {
		t.Fatalf("RoutingStatsIntervalSeconds = %d, want 15", c.RoutingStatsIntervalSeconds)
	}
}

func TestLoad_RoutingCircuitFromEnv(t *testing.T) {
	t.Setenv("KL_DATABASE_URL", "postgres://from-kl/db")
	t.Setenv("KL_ROUTING_CIRCUIT_FAILURE_THRESHOLD", "5")
	t.Setenv("KL_ROUTING_CIRCUIT_COOLDOWN_SECONDS", "30")
	c := Load()
	if c.RoutingCircuitFailureThreshold != 5 {
		t.Fatalf("RoutingCircuitFailureThreshold = %d, want 5", c.RoutingCircuitFailureThreshold)
	}
	if c.RoutingCircuitCooldownSeconds != 30 {
		t.Fatalf("RoutingCircuitCooldownSeconds = %d, want 30", c.RoutingCircuitCooldownSeconds)
	}
}

func TestLoad_DataPlaneMaxConns(t *testing.T) {
	t.Setenv("KL_DATABASE_URL", "postgres://from-kl/db")
	t.Setenv("KL_DATA_PLANE_MAX_CONNS", "20")
	t.Setenv("KL_DATA_PLANE_MAX_CONNS_PREMIUM", "40")
	c := Load()
	if c.DataPlaneDefaultMaxConns != 20 {
		t.Fatalf("DataPlaneDefaultMaxConns = %d, want 20", c.DataPlaneDefaultMaxConns)
	}
	if c.DataPlaneInstanceMaxConns["premium"] != 40 {
		t.Fatalf("premium max conns = %d, want 40", c.DataPlaneInstanceMaxConns["premium"])
	}
}

func TestLoad_DataPlaneMaxPools(t *testing.T) {
	t.Setenv("KL_DATABASE_URL", "postgres://from-kl/db")
	t.Setenv("KL_DATA_PLANE_MAX_POOLS", "12")
	t.Setenv("KL_DATA_PLANE_MAX_POOLS_PREMIUM", "30")
	c := Load()
	if c.DataPlaneDefaultMaxPools != 12 {
		t.Fatalf("DataPlaneDefaultMaxPools = %d, want 12", c.DataPlaneDefaultMaxPools)
	}
	if c.DataPlaneInstanceMaxPools["premium"] != 30 {
		t.Fatalf("premium max pools = %d, want 30", c.DataPlaneInstanceMaxPools["premium"])
	}
}

func TestLoad_IACFromEnv(t *testing.T) {
	t.Setenv("KL_DATABASE_URL", "postgres://from-kl/db")
	t.Setenv("KL_IAC_BIN", "tofu")
	t.Setenv("KL_IAC_VERSION", "1.8.2")
	c := Load()
	if c.IACBinary != "tofu" {
		t.Fatalf("IACBinary = %q", c.IACBinary)
	}
	if c.IACVersion != "1.8.2" {
		t.Fatalf("IACVersion = %q", c.IACVersion)
	}
}

func TestLoad_EnvMigrationFromEnv(t *testing.T) {
	t.Setenv("KL_DATABASE_URL", "postgres://from-kl/db")
	t.Setenv("KL_ENV_MIGRATION_ENABLED", "false")
	t.Setenv("KL_ENV_MIGRATION_INTERVAL_SECONDS", "42")
	c := Load()
	if c.EnvMigrationEnabled {
		t.Fatalf("EnvMigrationEnabled = true, want false")
	}
	if c.EnvMigrationIntervalSeconds != 42 {
		t.Fatalf("EnvMigrationIntervalSeconds = %d, want 42", c.EnvMigrationIntervalSeconds)
	}
}

func TestLoad_TLSFromEnv(t *testing.T) {
	t.Setenv("KL_DATABASE_URL", "postgres://from-kl/db")
	t.Setenv("KL_TLS_MODE", "required")
	t.Setenv("KL_TLS_CERT_FILE", "/tmp/cert.pem")
	t.Setenv("KL_TLS_KEY_FILE", "/tmp/key.pem")
	c := Load()
	if c.ResolvedTLSMode() != "required" {
		t.Fatalf("ResolvedTLSMode = %q, want required", c.ResolvedTLSMode())
	}
	if c.TLSCertFile != "/tmp/cert.pem" || c.TLSKeyFile != "/tmp/key.pem" {
		t.Fatalf("unexpected tls files cert=%q key=%q", c.TLSCertFile, c.TLSKeyFile)
	}
}

func TestLoad_ProdTLSRequiredFromEnv(t *testing.T) {
	t.Setenv("KL_DATABASE_URL", "postgres://from-kl/db")
	t.Setenv("KL_PROD_TLS_REQUIRED", "false")
	c := Load()
	if c.ResolvedProdTLSRequired() {
		t.Fatal("ResolvedProdTLSRequired = true, want false")
	}
}

func TestUsesStrictDBTLS(t *testing.T) {
	cases := []struct {
		dsn  string
		want bool
	}{
		{"postgres://u:p@db/kl?sslmode=verify-full", true},
		{"postgres://u:p@db/kl?sslmode=require", false},
		{"host=db dbname=kl sslmode=verify-full", true},
		{"host=db dbname=kl sslmode=disable", false},
		{"postgres://u:p@db/kl", false},
	}
	for _, tc := range cases {
		if got := UsesStrictDBTLS(tc.dsn); got != tc.want {
			t.Fatalf("UsesStrictDBTLS(%q)=%v want %v", tc.dsn, got, tc.want)
		}
	}
}
