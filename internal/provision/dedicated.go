package provision

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/davesade/kilolock/pkg/store"
)

// GCPDedicatedConfig configures Terraform-based dedicated Cloud SQL provisioning.
type GCPDedicatedConfig struct {
	ProjectID    string
	Region       string
	ModulePath   string
	StateBaseDir string
	SQLTier      string
	// DSNForm selects socket (Cloud Run) or tcp (cutover tooling).
	DSNForm string
	// IACBinary is terraform/tofu binary path/name used for module apply.
	IACBinary string
}

type tfOutputValues struct {
	connectionName    string
	databaseURLSocket string
	databaseURLTCP    string
}

// ProvisionDedicatedHost creates a dedicated Cloud SQL instance via Terraform,
// migrates the environment database, optionally copies data, and returns the
// new DSN and connection name.
func ProvisionDedicatedHost(
	ctx context.Context,
	cfg GCPDedicatedConfig,
	env store.EnvironmentRow,
	tenant store.TenantRow,
	logger *slog.Logger,
) (connectionName, dsn string, err error) {
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.StateBaseDir == "" {
		cfg.StateBaseDir = os.TempDir()
	}
	if cfg.Region == "" {
		cfg.Region = "europe-west1"
	}
	if cfg.SQLTier == "" {
		cfg.SQLTier = "db-f1-micro"
	}
	if err := cfg.validate(); err != nil {
		return "", "", err
	}
	if cfg.IACBinary == "" {
		cfg.IACBinary = "terraform"
	}
	instanceName := gcpInstanceName(env.TenantSlug, env.Slug)
	stateDir := filepath.Join(cfg.StateBaseDir, env.ID)
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return "", "", err
	}

	out, err := runTerraformApply(ctx, cfg, stateDir, instanceName, env.DatabaseName, logger)
	if err != nil {
		return "", "", err
	}

	connectionName = out.connectionName
	switch strings.ToLower(cfg.DSNForm) {
	case "socket":
		dsn = out.databaseURLSocket
	default:
		if out.databaseURLTCP != "" {
			dsn = out.databaseURLTCP
		} else {
			dsn = out.databaseURLSocket
		}
	}
	if dsn == "" {
		return "", "", fmt.Errorf("terraform produced empty database URL")
	}

	if err := MigrateEnvironment(ctx, dsn, logger); err != nil {
		return connectionName, "", fmt.Errorf("migrate dedicated database: %w", err)
	}

	poolDSN := dsn
	// sync tenant uses isolated store
	if err := syncTenantOnDSN(ctx, poolDSN, tenant); err != nil {
		return connectionName, "", fmt.Errorf("sync tenant: %w", err)
	}

	if src := strings.TrimSpace(env.SourceDatabaseDSN); src != "" {
		logger.Info("copying environment data", "from", "source_database_dsn", "to", env.DatabaseName)
		if err := CopyDatabase(ctx, src, dsn); err != nil {
			return connectionName, "", fmt.Errorf("copy database: %w", err)
		}
	}

	return connectionName, dsn, nil
}

func (c GCPDedicatedConfig) validate() error {
	if strings.TrimSpace(c.ProjectID) == "" {
		return fmt.Errorf("GCP project id is required (KL_GCP_PROJECT_ID)")
	}
	if strings.TrimSpace(c.ModulePath) == "" {
		return fmt.Errorf("terraform module path is required (KL_DEDICATED_TF_MODULE)")
	}
	if _, err := os.Stat(c.ModulePath); err != nil {
		return fmt.Errorf("terraform module path: %w", err)
	}
	return nil
}

func runTerraformApply(
	ctx context.Context,
	cfg GCPDedicatedConfig,
	stateDir, instanceName, databaseName string,
	logger *slog.Logger,
) (*tfOutputValues, error) {
	if err := exec.CommandContext(ctx, cfg.IACBinary, "-chdir="+cfg.ModulePath, "init", "-input=false").Run(); err != nil {
		return nil, fmt.Errorf("terraform init: %w", err)
	}

	args := []string{
		"-chdir=" + cfg.ModulePath,
		"apply",
		"-auto-approve",
		"-input=false",
		"-no-color",
		"-state=" + filepath.Join(stateDir, "terraform.tfstate"),
		"-var=project_id=" + cfg.ProjectID,
		"-var=region=" + cfg.Region,
		"-var=instance_name=" + instanceName,
		"-var=database_name=" + databaseName,
		"-var=tier=" + cfg.SQLTier,
	}
	logger.Info("terraform apply", "instance", instanceName, "database", databaseName)
	cmd := exec.CommandContext(ctx, cfg.IACBinary, args...)
	cmd.Env = append(os.Environ(), "GOOGLE_PROJECT="+cfg.ProjectID)
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("terraform apply: %w: %s", err, strings.TrimSpace(string(out)))
	}

	outCmd := exec.CommandContext(ctx, cfg.IACBinary,
		"-chdir="+cfg.ModulePath,
		"output", "-json",
		"-state="+filepath.Join(stateDir, "terraform.tfstate"),
	)
	raw, err := outCmd.Output()
	if err != nil {
		return nil, fmt.Errorf("terraform output: %w", err)
	}
	var parsed map[string]struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("parse terraform output: %w", err)
	}
	get := func(k string) string {
		if v, ok := parsed[k]; ok {
			return v.Value
		}
		return ""
	}
	return &tfOutputValues{
		connectionName:    get("connection_name"),
		databaseURLSocket: get("database_url_socket"),
		databaseURLTCP:    get("database_url_tcp"),
	}, nil
}

func gcpInstanceName(tenantSlug, envSlug string) string {
	s := strings.ToLower("ig-" + tenantSlug + "-" + envSlug)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		case r == '_':
			b.WriteByte('-')
		}
	}
	name := b.String()
	if len(name) > 90 {
		name = name[:90]
	}
	if name == "" {
		name = "ig-env"
	}
	return name
}

// LoadGCPDedicatedConfigFromEnv reads operator configuration from the environment.
func LoadGCPDedicatedConfigFromEnv() GCPDedicatedConfig {
	return GCPDedicatedConfig{
		ProjectID:    strings.TrimSpace(os.Getenv("KL_GCP_PROJECT_ID")),
		Region:       strings.TrimSpace(os.Getenv("KL_GCP_REGION")),
		ModulePath:   strings.TrimSpace(os.Getenv("KL_DEDICATED_TF_MODULE")),
		StateBaseDir: strings.TrimSpace(os.Getenv("KL_TERRAFORM_STATE_DIR")),
		SQLTier:      strings.TrimSpace(os.Getenv("KL_DEDICATED_SQL_TIER")),
		DSNForm:      strings.TrimSpace(os.Getenv("KL_DEDICATED_DSN_FORM")),
		IACBinary:    strings.TrimSpace(os.Getenv("KL_IAC_BIN")),
	}
}
