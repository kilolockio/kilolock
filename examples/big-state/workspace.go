package orchestrator

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/kilolockio/kilolock/internal/plan"
	"github.com/kilolockio/kilolock/internal/slice"
)

// Workspace represents an isolated temporary directory for running
// sliced Terraform plans and applies without touching the trunk state.
type Workspace struct {
	Dir string
}

// NewWorkspace creates a new temporary directory and copies the
// Terraform configuration from sourceDir into it. It explicitly
// ignores any existing `backend.tf` so it can inject a local backend.
func NewWorkspace(sourceDir string) (*Workspace, error) {
	tmpDir, err := os.MkdirTemp("", "kl-workspace-*")
	if err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}

	entries, err := os.ReadDir(sourceDir)
	if err != nil {
		os.RemoveAll(tmpDir)
		return nil, fmt.Errorf("read source dir: %w", err)
	}

	for _, entry := range entries {
		// Skip hidden directories like .terraform or .git
		if entry.IsDir() && strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		// Skip backend.tf to avoid conflicts with our local override
		if entry.Name() == "backend.tf" {
			continue
		}

		srcPath := filepath.Join(sourceDir, entry.Name())
		dstPath := filepath.Join(tmpDir, entry.Name())

		if entry.IsDir() {
			if err := copyDir(srcPath, dstPath); err != nil {
				os.RemoveAll(tmpDir)
				return nil, err
			}
		} else {
			if err := copyFile(srcPath, dstPath); err != nil {
				os.RemoveAll(tmpDir)
				return nil, err
			}
		}
	}

	// Inject a local backend so Terraform reads from our slice
	// instead of attempting to reach out to the HTTP backend.
	backendConfig := `terraform { backend "local" { path = "terraform.tfstate" } }`
	if err := os.WriteFile(filepath.Join(tmpDir, "kl_backend_override.tf"), []byte(backendConfig), 0644); err != nil {
		os.RemoveAll(tmpDir)
		return nil, fmt.Errorf("write local backend config: %w", err)
	}

	// Also copy the lock file if it exists. This is critical. Without the lock
	// file, `terraform init` in the temp dir will re-resolve dependencies from
	// scratch. If multiple applies run in parallel, they will race to write to
	// the shared provider cache, causing corruption and checksum errors. With
	// the lock file present, `init` becomes a fast, read-only operation
	// against the cache.
	lockFileSrc := filepath.Join(sourceDir, ".terraform.lock.hcl")
	if _, err := os.Stat(lockFileSrc); err == nil {
		if err := copyFile(lockFileSrc, filepath.Join(tmpDir, ".terraform.lock.hcl")); err != nil {
			return nil, fmt.Errorf("copy lock file: %w", err)
		}
	}

	return &Workspace{Dir: tmpDir}, nil
}

// WriteSlice writes the sliced state database out to the workspace as a file.
func (w *Workspace) WriteSlice(state *slice.TrunkState) error {
	b, err := slice.MarshalTrunkState(state)
	if err != nil {
		return fmt.Errorf("marshal slice: %w", err)
	}
	if err := os.WriteFile(filepath.Join(w.Dir, "terraform.tfstate"), b, 0644); err != nil {
		return fmt.Errorf("write slice state: %w", err)
	}
	return nil
}

// Init runs `terraform init` in the workspace to prepare providers.
func (w *Workspace) Init(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "terraform", "init", "-input=false", "-no-color")
	cmd.Dir = w.Dir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("terraform init failed: %v\n%s", err, out)
	}
	return nil
}

// Plan runs `terraform plan` using the requested targets. By targeting
// only the specific resources requested, Terraform skips checking
// the other 49,997 resources described in the HCL files.
func (w *Workspace) Plan(ctx context.Context, targets []string) error {
	args := []string{"plan", "-input=false", "-out=plan.tfplan", "-lock=false"}
	for _, target := range targets {
		args = append(args, "-target="+target)
	}

	cmd := exec.CommandContext(ctx, "terraform", args...)
	cmd.Dir = w.Dir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("terraform plan failed: %v\n%s", err, out)
	}
	return nil
}

// Show reads the generated plan.tfplan and returns the parsed JSON structure.
func (w *Workspace) Show(ctx context.Context) (*plan.File, error) {
	out, err := plan.RunTerraformShow(ctx, "terraform", w.Dir, "plan.tfplan")
	if err != nil {
		return nil, err
	}
	return plan.ParseShowJSONBytes(out)
}

// Cleanup completely removes the temporary directory.
func (w *Workspace) Cleanup() error {
	return os.RemoveAll(w.Dir)
}

// copyFile handles basic file copying.
func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0644)
}

// copyDir recursively copies directories.
func copyDir(src, dst string) error {
	if err := os.MkdirAll(dst, 0755); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		srcPath := filepath.Join(src, entry.Name())
		dstPath := filepath.Join(dst, entry.Name())
		if entry.IsDir() {
			if err := copyDir(srcPath, dstPath); err != nil {
				return err
			}
		} else {
			if err := copyFile(srcPath, dstPath); err != nil {
				return err
			}
		}
	}
	return nil
}
