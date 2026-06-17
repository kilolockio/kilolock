// Package config loads and validates Kilolock runtime configuration.
//
// Configuration sources, in increasing precedence:
//
//  1. Defaults (lowest)
//  2. Project-local .kl.toml found by walking up from CWD
//  3. Environment variables (KL_* and DATABASE_URL)
//
// Flags passed on the command line override Config values for
// matching keys when the subcommand explicitly wires them; the
// precedence ladder above governs everything that flows through
// Config.Load.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
)

// LoadedFromFile records the path of the .kl.toml that was
// found and applied during Load, or "" when none was loaded. Useful
// for `kl` subcommands that want to log "config: .../foo/
// .kl.toml" so operators can see which file is in effect.
//
// Stored on Config rather than returned alongside it so subcommands
// that pass Config around don't need a second value. Empty when no
// file applied; non-empty paths are absolute.
type loadedFromFile = string // type alias keeps the field on Config typed-string for clarity

// Config holds all runtime configuration for the Kilolock server and CLI.
type Config struct {
	// DatabaseURL is the PostgreSQL connection string (libpq URI form).
	// Required for serve and migrate; ignored by version.
	DatabaseURL string

	// ControlPlaneDatabaseURL is the metadata database (tenants,
	// environments, api_tokens). When empty, DatabaseURL is used.
	ControlPlaneDatabaseURL string

	// DataPlaneDatabaseURL is the default data-plane connection for
	// environments with no database_dsn. When empty, DatabaseURL is used.
	DataPlaneDatabaseURL string

	// DataPlaneAdminURL is a superuser DSN used to CREATE DATABASE for
	// new environments (optional; required for --provision).
	DataPlaneAdminURL string

	// DataPlaneInstanceURLs optionally maps instance key -> base DSN.
	// Environment variable format:
	//   KL_DATA_PLANE_DSN_<KEY>=postgres://...
	DataPlaneInstanceURLs map[string]string

	// DataPlaneInstanceAdminURLs optionally maps instance key -> admin DSN.
	// Environment variable format:
	//   KL_DATA_PLANE_ADMIN_DSN_<KEY>=postgres://...
	DataPlaneInstanceAdminURLs map[string]string

	// DataPlaneDefaultMaxConns caps routed pool size when no per-instance
	// override is set.
	DataPlaneDefaultMaxConns int

	// DataPlaneInstanceMaxConns maps instance key -> routed pool max conns.
	// Environment variable format:
	//   KL_DATA_PLANE_MAX_CONNS_<KEY>=N
	DataPlaneInstanceMaxConns map[string]int

	// DataPlaneDefaultMaxPools caps open routed pools per instance key.
	// Environment variable:
	//   KL_DATA_PLANE_MAX_POOLS
	DataPlaneDefaultMaxPools int

	// DataPlaneInstanceMaxPools maps instance key -> max open routed pools.
	// Environment variable format:
	//   KL_DATA_PLANE_MAX_POOLS_<KEY>=N
	DataPlaneInstanceMaxPools map[string]int

	// IACBinary is the default IaC CLI binary (terraform or tofu).
	IACBinary string

	// IACVersion optionally pins a desired IaC CLI version.
	IACVersion string

	// MaxEnvironmentPools caps cached pgx pools per serve process.
	MaxEnvironmentPools int

	// RoutingStatsIntervalSeconds controls how often serve logs routing cache
	// stats. Set to 0 to disable periodic logs.
	RoutingStatsIntervalSeconds int

	// RoutingCircuitFailureThreshold opens an instance circuit after this many
	// consecutive connect failures.
	RoutingCircuitFailureThreshold int

	// RoutingCircuitCooldownSeconds keeps an instance in open_circuit for this
	// duration before new connect attempts.
	RoutingCircuitCooldownSeconds int

	// EnvMigrationEnabled controls background migration of provisioned
	// environment databases on serve startup/runtime.
	EnvMigrationEnabled bool

	// EnvMigrationIntervalSeconds controls periodic background environment
	// migration cadence when EnvMigrationEnabled is true.
	EnvMigrationIntervalSeconds int

	// ListenAddr is the address the HTTP backend server binds to.
	ListenAddr string

	// TLSMode controls transport policy for HTTP listeners.
	//   off      - plain HTTP allowed
	//   required - HTTPS listener is required (cert/key must be configured)
	TLSMode string

	// TLSCertFile/TLSKeyFile configure direct HTTPS listeners for serve/control.
	TLSCertFile string
	TLSKeyFile  string

	// ProdTLSRequiredExplicit allows operators to relax the default
	// "strict transport in prod" policy for non-regulated environments.
	// When unset, resolved behavior defaults to true.
	ProdTLSRequiredExplicit *bool

	// LogLevel controls slog verbosity: "debug", "info", "warn", "error".
	LogLevel string

	// LogFormat selects "text" or "json" output for slog.
	LogFormat string

	// PortalAuthMode controls customer-portal login behavior:
	//   password       - local email/password only
	//   trusted_header - portal trusts identity headers from an upstream OIDC/auth proxy
	//   mixed          - allow local password plus any configured OIDC mode
	PortalAuthMode string

	// PortalTrustedEmailHeaders lists request headers to inspect for a
	// trusted upstream-authenticated email identity.
	PortalTrustedEmailHeaders []string

	// PortalOIDCProviderLabel is the customer-facing label for the
	// trusted-header SSO button, e.g. "Google".
	PortalOIDCProviderLabel string

	// Portal direct Google OIDC settings (used when PortalAuthMode=oidc_google
	// or mixed with Google credentials present).
	PortalOIDCGoogleClientID     string
	PortalOIDCGoogleClientSecret string
	PortalOIDCGoogleRedirectURL  string
	PortalOIDCGoogleIssuer       string
	PortalOIDCGoogleScopes       []string

	// AuthMode selects HTTP authentication: open, static, database, auto.
	//   open     — no authentication (development only)
	//   static   — single shared KL_AUTH_TOKEN (self-hosted)
	//   database — per-tenant API tokens in Postgres (multi-customer)
	//   auto     — static if AuthToken set, else database
	AuthMode string

	// AuthToken is used when AuthMode is static (or auto with token set).
	AuthToken string

	// Bootstrap* seeds a tenant + API token on serve startup (database mode).
	BootstrapTenantSlug  string
	BootstrapTenantName  string
	BootstrapTokenName   string
	BootstrapTokenSecret string

	// InitMode controls bootstrap behavior:
	//   dev  - allow env-based auto-bootstrap on serve startup
	//   prod - require explicit one-time `kl operator init`
	InitMode string

	// AutoDefaultEnvironmentExplicit allows overriding automatic default
	// environment creation behavior. When unset, resolved behavior is:
	//   dev -> true
	//   prod -> false
	AutoDefaultEnvironmentExplicit *bool

	// LoadedConfigFile is the absolute path of the .kl.toml
	// found during Load, or empty when env-only configuration was
	// used. Subcommands may surface this to the operator (e.g. for
	// the audit trail or a debug log line) so it's clear which file
	// is in effect when behavior diverges from expectation.
	LoadedConfigFile loadedFromFile
}

// Defaults returns a Config populated with reasonable development defaults.
// All fields can be overridden via environment variables.
func Defaults() Config {
	return Config{
		DatabaseURL:                    "",
		ListenAddr:                     ":8080",
		TLSMode:                        "off",
		LogLevel:                       "info",
		LogFormat:                      "text",
		PortalAuthMode:                 "password",
		PortalTrustedEmailHeaders:      []string{"X-Goog-Authenticated-User-Email", "X-Forwarded-Email", "X-Auth-Request-Email"},
		PortalOIDCProviderLabel:        "Google",
		PortalOIDCGoogleIssuer:         "https://accounts.google.com",
		PortalOIDCGoogleScopes:         []string{"openid", "email", "profile"},
		MaxEnvironmentPools:            32,
		RoutingStatsIntervalSeconds:    60,
		RoutingCircuitFailureThreshold: 2,
		RoutingCircuitCooldownSeconds:  10,
		EnvMigrationEnabled:            true,
		EnvMigrationIntervalSeconds:    300,
		IACBinary:                      "terraform",
		InitMode:                       "dev",
		DataPlaneDefaultMaxConns:       8,
		DataPlaneDefaultMaxPools:       16,
	}
}

// Load returns a Config populated from (in order of increasing
// precedence): Defaults, the nearest .kl.toml walking up
// from CWD, and environment variables.
//
// It does not validate the result; call Validate when the config
// will actually be used (i.e. not for `version`).
//
// Errors finding or reading the config file are not fatal here
// because the env path may still supply everything we need; instead
// the file-load error is squelched on the unhappy path and the env
// fallback proceeds. Callers that want to surface "file not loaded
// because it was malformed" can call FindConfigFile + LoadFile
// directly.
func Load() Config {
	c := Defaults()

	// 1. File overrides defaults.
	if cwd, err := os.Getwd(); err == nil {
		if path, err := FindConfigFile(cwd); err == nil {
			if fs, ferr := LoadFile(path); ferr == nil {
				if fs.DatabaseURL != "" {
					c.DatabaseURL = fs.DatabaseURL
				}
				// BackendAddress is read for symmetry but no
				// subcommand consumes it yet; it lives on
				// FileSettings to reserve the design space.
				c.LoadedConfigFile = path
			}
		}
	}

	// 2. Env overrides file. Env values are the most specific
	//    expression of operator intent (per-shell, ephemeral) and
	//    should always win.
	if v := strings.TrimSpace(os.Getenv("KL_DATABASE_URL")); v != "" {
		c.DatabaseURL = v
	} else if v := strings.TrimSpace(os.Getenv("DATABASE_URL")); v != "" {
		c.DatabaseURL = v
	}
	if v := strings.TrimSpace(os.Getenv("KL_LISTEN_ADDR")); v != "" {
		c.ListenAddr = v
	}
	if v := strings.TrimSpace(os.Getenv("KL_TLS_MODE")); v != "" {
		c.TLSMode = strings.ToLower(v)
	}
	if v := strings.TrimSpace(os.Getenv("KL_TLS_CERT_FILE")); v != "" {
		c.TLSCertFile = v
	}
	if v := strings.TrimSpace(os.Getenv("KL_TLS_KEY_FILE")); v != "" {
		c.TLSKeyFile = v
	}
	if v, ok := parseOptionalBoolEnv("KL_PROD_TLS_REQUIRED"); ok {
		c.ProdTLSRequiredExplicit = &v
	}
	if v := strings.TrimSpace(os.Getenv("KL_LOG_LEVEL")); v != "" {
		c.LogLevel = strings.ToLower(v)
	}
	if v := strings.TrimSpace(os.Getenv("KL_LOG_FORMAT")); v != "" {
		c.LogFormat = strings.ToLower(v)
	}
	if v := strings.TrimSpace(os.Getenv("KL_PORTAL_AUTH_MODE")); v != "" {
		c.PortalAuthMode = strings.ToLower(v)
	}
	if v := strings.TrimSpace(os.Getenv("KL_PORTAL_TRUSTED_EMAIL_HEADERS")); v != "" {
		c.PortalTrustedEmailHeaders = splitAndTrimCSV(v)
	}
	if v := strings.TrimSpace(os.Getenv("KL_PORTAL_OIDC_PROVIDER_LABEL")); v != "" {
		c.PortalOIDCProviderLabel = v
	}
	if v := strings.TrimSpace(os.Getenv("KL_PORTAL_OIDC_GOOGLE_CLIENT_ID")); v != "" {
		c.PortalOIDCGoogleClientID = v
	}
	if v := strings.TrimSpace(os.Getenv("KL_PORTAL_OIDC_GOOGLE_CLIENT_SECRET")); v != "" {
		c.PortalOIDCGoogleClientSecret = v
	}
	if v := strings.TrimSpace(os.Getenv("KL_PORTAL_OIDC_GOOGLE_REDIRECT_URL")); v != "" {
		c.PortalOIDCGoogleRedirectURL = v
	}
	if v := strings.TrimSpace(os.Getenv("KL_PORTAL_OIDC_GOOGLE_ISSUER")); v != "" {
		c.PortalOIDCGoogleIssuer = v
	}
	if v := strings.TrimSpace(os.Getenv("KL_PORTAL_OIDC_GOOGLE_SCOPES")); v != "" {
		c.PortalOIDCGoogleScopes = splitAndTrimCSV(v)
	}
	if v := strings.TrimSpace(os.Getenv("KL_AUTH_MODE")); v != "" {
		c.AuthMode = strings.ToLower(v)
	}
	if v := strings.TrimSpace(os.Getenv("KL_AUTH_TOKEN")); v != "" {
		c.AuthToken = v
	}
	if v := strings.TrimSpace(os.Getenv("KL_BOOTSTRAP_TENANT_SLUG")); v != "" {
		c.BootstrapTenantSlug = v
	}
	if v := strings.TrimSpace(os.Getenv("KL_BOOTSTRAP_TENANT_NAME")); v != "" {
		c.BootstrapTenantName = v
	}
	if v := strings.TrimSpace(os.Getenv("KL_BOOTSTRAP_TOKEN_NAME")); v != "" {
		c.BootstrapTokenName = v
	}
	if v := strings.TrimSpace(os.Getenv("KL_BOOTSTRAP_TOKEN")); v != "" {
		c.BootstrapTokenSecret = v
	}
	if v := strings.TrimSpace(os.Getenv("KL_INIT_MODE")); v != "" {
		c.InitMode = strings.ToLower(v)
	}
	if v := strings.TrimSpace(os.Getenv("KL_AUTO_DEFAULT_ENV")); v != "" {
		switch strings.ToLower(v) {
		case "1", "true", "yes", "on":
			b := true
			c.AutoDefaultEnvironmentExplicit = &b
		case "0", "false", "no", "off":
			b := false
			c.AutoDefaultEnvironmentExplicit = &b
		}
	}
	if v := strings.TrimSpace(os.Getenv("KL_CONTROL_PLANE_DATABASE_URL")); v != "" {
		c.ControlPlaneDatabaseURL = v
	}
	if v := strings.TrimSpace(os.Getenv("KL_DATA_PLANE_DATABASE_URL")); v != "" {
		c.DataPlaneDatabaseURL = v
	}
	if v := strings.TrimSpace(os.Getenv("KL_DATA_PLANE_ADMIN_URL")); v != "" {
		c.DataPlaneAdminURL = v
	}
	c.DataPlaneInstanceURLs = mapFromEnvPrefix("KL_DATA_PLANE_DSN_")
	c.DataPlaneInstanceAdminURLs = mapFromEnvPrefix("KL_DATA_PLANE_ADMIN_DSN_")
	c.DataPlaneInstanceMaxConns = intMapFromEnvPrefix("KL_DATA_PLANE_MAX_CONNS_")
	if v := strings.TrimSpace(os.Getenv("KL_DATA_PLANE_MAX_CONNS")); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			c.DataPlaneDefaultMaxConns = n
		}
	}
	c.DataPlaneInstanceMaxPools = intMapFromEnvPrefix("KL_DATA_PLANE_MAX_POOLS_")
	if v := strings.TrimSpace(os.Getenv("KL_DATA_PLANE_MAX_POOLS")); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			c.DataPlaneDefaultMaxPools = n
		}
	}
	if v := strings.TrimSpace(os.Getenv("KL_MAX_ENVIRONMENT_POOLS")); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			c.MaxEnvironmentPools = n
		}
	}
	if v := strings.TrimSpace(os.Getenv("KL_IAC_BIN")); v != "" {
		c.IACBinary = v
	}
	if v := strings.TrimSpace(os.Getenv("KL_IAC_VERSION")); v != "" {
		c.IACVersion = v
	}
	if v := strings.TrimSpace(os.Getenv("KL_ROUTING_STATS_INTERVAL_SECONDS")); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n >= 0 {
			c.RoutingStatsIntervalSeconds = n
		}
	}
	if v := strings.TrimSpace(os.Getenv("KL_ROUTING_CIRCUIT_FAILURE_THRESHOLD")); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			c.RoutingCircuitFailureThreshold = n
		}
	}
	if v := strings.TrimSpace(os.Getenv("KL_ROUTING_CIRCUIT_COOLDOWN_SECONDS")); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			c.RoutingCircuitCooldownSeconds = n
		}
	}
	if v := strings.TrimSpace(os.Getenv("KL_ENV_MIGRATION_ENABLED")); v != "" {
		switch strings.ToLower(v) {
		case "1", "true", "yes", "on":
			c.EnvMigrationEnabled = true
		case "0", "false", "no", "off":
			c.EnvMigrationEnabled = false
		}
	}
	if v := strings.TrimSpace(os.Getenv("KL_ENV_MIGRATION_INTERVAL_SECONDS")); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			c.EnvMigrationIntervalSeconds = n
		}
	}

	return c
}

// ResolvedControlPlaneURL returns the control-plane DSN.
func (c Config) ResolvedControlPlaneURL() string {
	if strings.TrimSpace(c.ControlPlaneDatabaseURL) != "" {
		return strings.TrimSpace(c.ControlPlaneDatabaseURL)
	}
	return c.DatabaseURL
}

// ResolvedDataPlaneURL returns the default data-plane DSN.
func (c Config) ResolvedDataPlaneURL() string {
	if strings.TrimSpace(c.DataPlaneDatabaseURL) != "" {
		return strings.TrimSpace(c.DataPlaneDatabaseURL)
	}
	return c.DatabaseURL
}

// ResolvedDataPlaneURLForInstance returns base DSN for a data-plane instance key.
// Empty/shared key falls back to ResolvedDataPlaneURL.
func (c Config) ResolvedDataPlaneURLForInstance(instanceKey string) string {
	k := normalizeInstanceKey(instanceKey)
	if k != "shared" {
		if v := strings.TrimSpace(c.DataPlaneInstanceURLs[k]); v != "" {
			return v
		}
	}
	return c.ResolvedDataPlaneURL()
}

// ResolvedDataPlaneAdminURLForInstance returns admin DSN for a data-plane
// instance key. Empty/shared key falls back to DataPlaneAdminURL.
func (c Config) ResolvedDataPlaneAdminURLForInstance(instanceKey string) string {
	k := normalizeInstanceKey(instanceKey)
	if k != "shared" {
		if v := strings.TrimSpace(c.DataPlaneInstanceAdminURLs[k]); v != "" {
			return v
		}
	}
	return strings.TrimSpace(c.DataPlaneAdminURL)
}

// ResolvedAuthMode returns the effective auth mode after applying auto.
func (c Config) ResolvedAuthMode() string {
	switch strings.ToLower(strings.TrimSpace(c.AuthMode)) {
	case "open", "static", "database":
		return strings.ToLower(strings.TrimSpace(c.AuthMode))
	case "auto", "":
		if strings.TrimSpace(c.AuthToken) != "" {
			return "static"
		}
		return "database"
	default:
		return "database"
	}
}

func (c Config) ResolvedInitMode() string {
	switch strings.ToLower(strings.TrimSpace(c.InitMode)) {
	case "prod":
		return "prod"
	default:
		return "dev"
	}
}

func (c Config) ResolvedAutoDefaultEnvironment() bool {
	if c.AutoDefaultEnvironmentExplicit != nil {
		return *c.AutoDefaultEnvironmentExplicit
	}
	return c.ResolvedInitMode() != "prod"
}

func (c Config) ResolvedTLSMode() string {
	switch strings.ToLower(strings.TrimSpace(c.TLSMode)) {
	case "required":
		return "required"
	default:
		return "off"
	}
}

func (c Config) ResolvedPortalAuthMode() string {
	switch strings.ToLower(strings.TrimSpace(c.PortalAuthMode)) {
	case "trusted_header":
		return "trusted_header"
	case "oidc_google":
		return "oidc_google"
	case "mixed":
		return "mixed"
	default:
		return "password"
	}
}

func (c Config) ResolvedPortalPasswordAuthEnabled() bool {
	mode := c.ResolvedPortalAuthMode()
	return mode == "password" || mode == "mixed"
}

func (c Config) ResolvedPortalOIDCEnabled() bool {
	mode := c.ResolvedPortalAuthMode()
	return mode == "trusted_header" || mode == "mixed" || mode == "oidc_google"
}

func (c Config) ResolvedPortalTrustedEmailHeaders() []string {
	if len(c.PortalTrustedEmailHeaders) == 0 {
		return []string{"X-Goog-Authenticated-User-Email", "X-Forwarded-Email", "X-Auth-Request-Email"}
	}
	out := make([]string, 0, len(c.PortalTrustedEmailHeaders))
	for _, h := range c.PortalTrustedEmailHeaders {
		h = strings.TrimSpace(h)
		if h != "" {
			out = append(out, h)
		}
	}
	if len(out) == 0 {
		return []string{"X-Goog-Authenticated-User-Email", "X-Forwarded-Email", "X-Auth-Request-Email"}
	}
	return out
}

func (c Config) ResolvedPortalOIDCProviderLabel() string {
	if v := strings.TrimSpace(c.PortalOIDCProviderLabel); v != "" {
		return v
	}
	return "Google"
}

func (c Config) ResolvedPortalOIDCTrustedHeaderEnabled() bool {
	mode := c.ResolvedPortalAuthMode()
	return mode == "trusted_header" || mode == "mixed"
}

func (c Config) ResolvedPortalOIDCGoogleEnabled() bool {
	mode := c.ResolvedPortalAuthMode()
	if mode == "oidc_google" {
		return true
	}
	if mode != "mixed" {
		return false
	}
	return c.ResolvedPortalOIDCGoogleClientID() != "" &&
		c.ResolvedPortalOIDCGoogleClientSecret() != "" &&
		c.ResolvedPortalOIDCGoogleRedirectURL() != ""
}

func (c Config) ResolvedPortalOIDCGoogleClientID() string {
	return strings.TrimSpace(c.PortalOIDCGoogleClientID)
}

func (c Config) ResolvedPortalOIDCGoogleClientSecret() string {
	return strings.TrimSpace(c.PortalOIDCGoogleClientSecret)
}

func (c Config) ResolvedPortalOIDCGoogleRedirectURL() string {
	return strings.TrimSpace(c.PortalOIDCGoogleRedirectURL)
}

func (c Config) ResolvedPortalOIDCGoogleIssuer() string {
	if v := strings.TrimSpace(c.PortalOIDCGoogleIssuer); v != "" {
		return v
	}
	return "https://accounts.google.com"
}

func (c Config) ResolvedPortalOIDCGoogleScopes() []string {
	if len(c.PortalOIDCGoogleScopes) == 0 {
		return []string{"openid", "email", "profile"}
	}
	out := make([]string, 0, len(c.PortalOIDCGoogleScopes))
	for _, scope := range c.PortalOIDCGoogleScopes {
		scope = strings.TrimSpace(scope)
		if scope != "" {
			out = append(out, scope)
		}
	}
	if len(out) == 0 {
		return []string{"openid", "email", "profile"}
	}
	return out
}

func splitAndTrimCSV(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func (c Config) ResolvedProdTLSRequired() bool {
	if c.ProdTLSRequiredExplicit != nil {
		return *c.ProdTLSRequiredExplicit
	}
	return true
}

// Validate ensures that fields required for runtime operation are present
// and well-formed. Subcommands call this only when they need the values
// (e.g. `version` skips validation).
func (c Config) Validate() error {
	if c.DatabaseURL == "" {
		return errors.New("database URL is required (set KL_DATABASE_URL, DATABASE_URL, or `database_url = \"…\"` in .kl.toml)")
	}
	if c.ListenAddr == "" {
		return errors.New("listen address is required (set KL_LISTEN_ADDR)")
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("invalid log level %q (expected debug|info|warn|error)", c.LogLevel)
	}
	switch c.LogFormat {
	case "text", "json":
	default:
		return fmt.Errorf("invalid log format %q (expected text|json)", c.LogFormat)
	}
	switch c.ResolvedAuthMode() {
	case "open", "static", "database":
	default:
		return fmt.Errorf("invalid auth mode %q (expected open|static|database|auto)", c.AuthMode)
	}
	switch strings.ToLower(strings.TrimSpace(c.InitMode)) {
	case "", "dev", "prod":
	default:
		return fmt.Errorf("invalid init mode %q (expected dev|prod)", c.InitMode)
	}
	switch c.ResolvedTLSMode() {
	case "off", "required":
	default:
		return fmt.Errorf("invalid tls mode %q (expected off|required)", c.TLSMode)
	}
	if c.ResolvedPortalOIDCGoogleEnabled() || (c.ResolvedPortalAuthMode() == "mixed" && (c.ResolvedPortalOIDCGoogleClientID() != "" || c.ResolvedPortalOIDCGoogleClientSecret() != "" || c.ResolvedPortalOIDCGoogleRedirectURL() != "")) {
		if c.ResolvedPortalOIDCGoogleClientID() == "" {
			return errors.New("portal google oidc client id is required (set KL_PORTAL_OIDC_GOOGLE_CLIENT_ID)")
		}
		if c.ResolvedPortalOIDCGoogleClientSecret() == "" {
			return errors.New("portal google oidc client secret is required (set KL_PORTAL_OIDC_GOOGLE_CLIENT_SECRET)")
		}
		if c.ResolvedPortalOIDCGoogleRedirectURL() == "" {
			return errors.New("portal google oidc redirect url is required (set KL_PORTAL_OIDC_GOOGLE_REDIRECT_URL)")
		}
	}
	if c.DataPlaneDefaultMaxConns <= 0 {
		return fmt.Errorf("invalid KL_DATA_PLANE_MAX_CONNS=%d (must be > 0)", c.DataPlaneDefaultMaxConns)
	}
	for key, n := range c.DataPlaneInstanceMaxConns {
		if n <= 0 {
			return fmt.Errorf("invalid KL_DATA_PLANE_MAX_CONNS_%s=%d (must be > 0)", strings.ToUpper(strings.ReplaceAll(key, "-", "_")), n)
		}
	}
	if c.DataPlaneDefaultMaxPools <= 0 {
		return fmt.Errorf("invalid KL_DATA_PLANE_MAX_POOLS=%d (must be > 0)", c.DataPlaneDefaultMaxPools)
	}
	for key, n := range c.DataPlaneInstanceMaxPools {
		if n <= 0 {
			return fmt.Errorf("invalid KL_DATA_PLANE_MAX_POOLS_%s=%d (must be > 0)", strings.ToUpper(strings.ReplaceAll(key, "-", "_")), n)
		}
	}
	return nil
}

// UsesStrictDBTLS reports whether DSN explicitly requests verify-full.
// Missing sslmode or weaker modes return false.
func UsesStrictDBTLS(dsn string) bool {
	mode := strings.ToLower(strings.TrimSpace(extractSSLMode(dsn)))
	return mode == "verify-full"
}

func extractSSLMode(dsn string) string {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return ""
	}
	if u, err := url.Parse(dsn); err == nil && u.Scheme != "" {
		return strings.TrimSpace(u.Query().Get("sslmode"))
	}
	// key=value form
	for _, p := range strings.Fields(dsn) {
		if !strings.HasPrefix(strings.ToLower(p), "sslmode=") {
			continue
		}
		return strings.TrimSpace(strings.TrimPrefix(p, "sslmode="))
	}
	return ""
}

func parseOptionalBoolEnv(name string) (bool, bool) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return false, false
	}
	switch strings.ToLower(raw) {
	case "1", "true", "yes", "on":
		return true, true
	case "0", "false", "no", "off":
		return false, true
	default:
		return false, false
	}
}

func mapFromEnvPrefix(prefix string) map[string]string {
	out := make(map[string]string)
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, prefix) {
			continue
		}
		idx := strings.IndexByte(kv, '=')
		if idx < 0 {
			continue
		}
		key := normalizeInstanceKey(strings.ReplaceAll(kv[len(prefix):idx], "_", "-"))
		val := strings.TrimSpace(kv[idx+1:])
		if key == "" || val == "" {
			continue
		}
		out[key] = val
	}
	return out
}

func intMapFromEnvPrefix(prefix string) map[string]int {
	out := make(map[string]int)
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, prefix) {
			continue
		}
		idx := strings.IndexByte(kv, '=')
		if idx < 0 {
			continue
		}
		key := normalizeInstanceKey(strings.ReplaceAll(kv[len(prefix):idx], "_", "-"))
		val := strings.TrimSpace(kv[idx+1:])
		if key == "" || val == "" {
			continue
		}
		var n int
		if _, err := fmt.Sscanf(val, "%d", &n); err != nil || n <= 0 {
			continue
		}
		out[key] = n
	}
	return out
}

func normalizeInstanceKey(key string) string {
	k := strings.ToLower(strings.TrimSpace(key))
	if k == "" {
		return "shared"
	}
	return k
}

// DataPlaneInstanceKeys returns sorted keys declared via env maps.
func (c Config) DataPlaneInstanceKeys() []string {
	keys := make([]string, 0, len(c.DataPlaneInstanceURLs))
	for k := range c.DataPlaneInstanceURLs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
