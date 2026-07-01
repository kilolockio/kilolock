//go:build integration

package main

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kilolockio/kilolock/internal/backend"
	"github.com/kilolockio/kilolock/pkg/db"
	"github.com/kilolockio/kilolock/pkg/migrate"
	"github.com/kilolockio/kilolock/pkg/store"
	"github.com/kilolockio/kilolock/pkg/testdb"
)

func TestRunApply_FileScopedTrustedLaneUsesStateEngineDelta(t *testing.T) {
	runFileScopedStateEngineApplyTest(t, fileScopedApplyScenario{
		stateName: "cmd-apply-file-trusted",
		workFiles: map[string]string{
			"a.tf": terraformDataResource("a", `"new-a"`),
			"b.tf": terraformDataResource("b", `"old-b"`),
		},
		bootstrapFiles: map[string]string{
			"a.tf": terraformDataResource("a", `"old-a"`),
			"b.tf": terraformDataResource("b", `"old-b"`),
		},
		scopeFile: "a.tf",
		stdoutMustContain: []string{
			"commit mode:",
			"state-engine delta",
			"Native intent source: terraform validation replan",
			"terraform_data.a",
		},
		stdoutMustNotContain: []string{
			"terraform_data.b\n",
		},
		stateMustContain: []string{
			`"value":"new-a"`,
			`"value":"old-b"`,
		},
	})
}

func TestRunApply_FileScopedTrustedLaneKeepsConfigRequiredSupportWrites(t *testing.T) {
	runFileScopedStateEngineApplyTest(t, fileScopedApplyScenario{
		stateName: "cmd-apply-file-config-required-support",
		workFiles: map[string]string{
			"support.tf": terraformDataResource("support", `"support-created-for-leaf"`),
			"leaf.tf": `resource "terraform_data" "leaf" {
  input = terraform_data.support.output
}
`,
		},
		bootstrapFiles: map[string]string{
			"main.tf": `terraform {
  required_version = ">= 1.4.0"
}
`,
		},
		scopeFile: "leaf.tf",
		stdoutMustContain: []string{
			"commit mode:",
			"state-engine delta",
			"Native intent source: terraform validation replan",
			"terraform_data.leaf",
			"terraform_data.support",
		},
		stateMustContain: []string{
			`"value":"support-created-for-leaf"`,
			`"terraform_data.support"`,
			`"terraform_data.leaf"`,
		},
	})
}

func TestRunApply_FileScopedTrustedLaneKeepsSupportMutationAndHelperWrite(t *testing.T) {
	runFileScopedStateEngineApplyTest(t, fileScopedApplyScenario{
		stateName: "cmd-apply-file-config-required-helper",
		workFiles: map[string]string{
			"bridge.tf": terraformDataResource("bridge", `"bridge-created-for-leaf"`),
			"support.tf": `resource "terraform_data" "support" {
  input = terraform_data.bridge.output
}
`,
			"leaf.tf": `resource "terraform_data" "leaf" {
  input = terraform_data.support.output
}
`,
		},
		bootstrapFiles: map[string]string{
			"support.tf": terraformDataResource("support", `"support-before-bridge"`),
		},
		scopeFile: "leaf.tf",
		stdoutMustContain: []string{
			"commit mode:",
			"state-engine delta",
			"Native intent source: terraform validation replan",
			"terraform_data.leaf",
			"terraform_data.support",
			"terraform_data.bridge",
		},
		stateMustContain: []string{
			`"value":"bridge-created-for-leaf"`,
			`"terraform_data.support"`,
			`"terraform_data.leaf"`,
			`"terraform_data.bridge"`,
		},
	})
}

func TestRunApply_FileScopedTrustedLaneSupportsLocalModuleSelection(t *testing.T) {
	runFileScopedStateEngineApplyTest(t, fileScopedApplyScenario{
		stateName: "cmd-apply-file-module-scope",
		workFiles: map[string]string{
			"module.tf": `module "demo" {
  source = "./modules/demo"
  value  = "new-module-value"
}
`,
			"outside.tf": terraformDataResource("outside", `"outside-stays-old"`),
			"modules/demo/main.tf": `variable "value" {
  type = string
}

resource "terraform_data" "member" {
  input = var.value
}
`,
		},
		bootstrapFiles: map[string]string{
			"module.tf": `module "demo" {
  source = "./modules/demo"
  value  = "old-module-value"
}
`,
			"outside.tf": terraformDataResource("outside", `"outside-stays-old"`),
			"modules/demo/main.tf": `variable "value" {
  type = string
}

resource "terraform_data" "member" {
  input = var.value
}
`,
		},
		scopeFile: "module.tf",
		stdoutMustContain: []string{
			"commit mode:",
			"state-engine delta",
			"Native intent source: terraform validation replan",
			"module.demo.terraform_data.member",
		},
		stdoutMustNotContain: []string{
			"terraform_data.outside\n",
		},
		stateMustContain: []string{
			`"value":"new-module-value"`,
			`"value":"outside-stays-old"`,
			`"module.demo.terraform_data.member"`,
		},
	})
}

type fileScopedApplyScenario struct {
	stateName            string
	workFiles            map[string]string
	bootstrapFiles       map[string]string
	scopeFile            string
	stdoutMustContain    []string
	stdoutMustNotContain []string
	stateMustContain     []string
}

func runFileScopedStateEngineApplyTest(t *testing.T, sc fileScopedApplyScenario) {
	t.Helper()

	url := os.Getenv("KL_DATABASE_URL")
	if url == "" {
		url = os.Getenv("DATABASE_URL")
	}
	if url == "" {
		t.Skip("no KL_DATABASE_URL or DATABASE_URL set")
	}
	tfBin, err := exec.LookPath("terraform")
	if err != nil {
		t.Skipf("terraform not on PATH: %v", err)
	}

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 15*time.Second)
	defer cancel()

	pool, err := db.Open(ctx, url)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer pool.Close()
	if err := migrate.Run(ctx, pool.Pool, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	if _, err := pool.Exec(ctx, `DELETE FROM apply_runs WHERE state_id IN (SELECT id FROM states WHERE name = $1)`, sc.stateName); err != nil {
		t.Fatalf("delete apply_runs: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM states WHERE name = $1`, sc.stateName); err != nil {
		t.Fatalf("delete states: %v", err)
	}
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 5*time.Second)
		defer ccancel()
		_, _ = pool.Exec(cctx, `DELETE FROM states WHERE name = $1`, sc.stateName)
	})

	st := store.New(pool.Pool)
	srv := httptest.NewServer(backend.New(st, slog.New(slog.NewTextHandler(io.Discard, nil))).Handler())
	defer srv.Close()

	workDir := t.TempDir()
	bootDir := t.TempDir()

	backendTF := `terraform {
  backend "http" {
    address        = "` + srv.URL + `/v1/states/` + sc.stateName + `"
    lock_address   = "` + srv.URL + `/v1/states/` + sc.stateName + `"
    unlock_address = "` + srv.URL + `/v1/state-unlock/` + sc.stateName + `"
    lock_method    = "LOCK"
    unlock_method  = "POST"
  }
}
`
	writeFile(t, filepath.Join(workDir, "backend.tf"), backendTF)
	for name, body := range sc.workFiles {
		writeFile(t, filepath.Join(workDir, name), body)
	}
	for name, body := range sc.bootstrapFiles {
		writeFile(t, filepath.Join(bootDir, name), body)
	}

	runTF := func(dir string, args ...string) {
		t.Helper()
		tctx, tcancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 2*time.Minute)
		defer tcancel()
		cmd := exec.CommandContext(tctx, tfBin, args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("terraform %v in %s: %v\n%s", args, dir, err, out)
		}
	}

	runTF(bootDir, "init", "-input=false", "-no-color")
	runTF(bootDir, "apply", "-auto-approve", "-input=false", "-no-color")

	stateBytes, err := os.ReadFile(filepath.Join(bootDir, "terraform.tfstate"))
	if err != nil {
		t.Fatalf("read bootstrap state: %v", err)
	}
	wctx, wcancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 10*time.Second)
	defer wcancel()
	if err := st.WriteState(wctx, sc.stateName, "", stateBytes, "import", "test"); err != nil {
		t.Fatalf("seed initial state: %v", err)
	}

	runTF(workDir, "init", "-input=false", "-no-color")
	t.Setenv("KL_PROTOCOL", "state-engine")

	origStdout := os.Stdout
	origStderr := os.Stderr
	stdoutR, stdoutW, _ := os.Pipe()
	stderrR, stderrW, _ := os.Pipe()
	os.Stdout = stdoutW
	os.Stderr = stderrW

	rc := runApply([]string{
		"--work-dir=" + workDir,
		"--confirm-scope",
		"--no-color",
		"--file=" + sc.scopeFile,
	})

	stdoutW.Close()
	stderrW.Close()
	os.Stdout = origStdout
	os.Stderr = origStderr

	stdout, _ := io.ReadAll(stdoutR)
	stderr, _ := io.ReadAll(stderrR)
	stdoutText := string(stdout)
	stderrText := string(stderr)

	if rc != 0 {
		t.Fatalf("runApply rc=%d\n---stdout---\n%s\n---stderr---\n%s", rc, stdoutText, stderrText)
	}
	for _, want := range sc.stdoutMustContain {
		if !strings.Contains(stdoutText, want) {
			t.Fatalf("stdout missing %q\n---stdout---\n%s", want, stdoutText)
		}
	}
	for _, forbidden := range sc.stdoutMustNotContain {
		if strings.Contains(stdoutText, forbidden) {
			t.Fatalf("stdout unexpectedly contains %q\n---stdout---\n%s", forbidden, stdoutText)
		}
	}
	if !strings.Contains(stderrText, "native apply safety:") || !strings.Contains(stderrText, "proven-safe") {
		t.Fatalf("stderr missing proven-safe preflight\n---stderr---\n%s", stderrText)
	}

	raw, err := st.GetCurrentState(testdb.BackgroundTenantCtx(), sc.stateName)
	if err != nil {
		t.Fatalf("fetch current state: %v", err)
	}
	body := string(raw)
	for _, want := range sc.stateMustContain {
		if !strings.Contains(body, want) {
			t.Fatalf("current state missing %q\n%s", want, body)
		}
	}
}

func terraformDataResource(name, input string) string {
	return `resource "terraform_data" "` + name + `" {
  input = ` + input + `
}
`
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
