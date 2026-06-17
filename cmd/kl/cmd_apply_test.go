package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kilolockio/kilolock/internal/apply"
	"github.com/kilolockio/kilolock/internal/plan"
	"github.com/kilolockio/kilolock/pkg/store"
)

// ---------------------------------------------------------------------------
// resolveApplyWorkDir
// ---------------------------------------------------------------------------

func TestResolveApplyWorkDir_FlagWinsWhenSet(t *testing.T) {
	dir := t.TempDir()
	got, err := resolveApplyWorkDir(dir, "/spec/path/plan.json", nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	abs, _ := filepath.Abs(dir)
	if got != abs {
		t.Errorf("got %q, want %q", got, abs)
	}
}

func TestResolveApplyWorkDir_FlagMustExist(t *testing.T) {
	_, err := resolveApplyWorkDir("/definitely-not-a-real-dir", "", nil)
	if err == nil {
		t.Errorf("expected error for missing --work-dir, got nil")
	}
}

func TestResolveApplyWorkDir_FallsBackToSpecConfigDir(t *testing.T) {
	dir := t.TempDir()
	spec := &plan.PlanSpec{ConfigDir: dir}
	got, err := resolveApplyWorkDir("", "/tmp/plan.json", spec)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	abs, _ := filepath.Abs(dir)
	if got != abs {
		t.Errorf("got %q, want %q", got, abs)
	}
}

// When the spec's recorded ConfigDir doesn't exist (e.g. spec was
// produced on a CI runner with a different layout), we fall
// through to the spec-path's directory rather than failing.
func TestResolveApplyWorkDir_SpecConfigDirGoneFallsBackToSpecPathDir(t *testing.T) {
	specDir := t.TempDir()
	specPath := filepath.Join(specDir, "spec.json")
	if err := os.WriteFile(specPath, []byte(`{}`), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	spec := &plan.PlanSpec{ConfigDir: "/nope/this/is/gone"}
	got, err := resolveApplyWorkDir("", specPath, spec)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	abs, _ := filepath.Abs(specDir)
	if got != abs {
		t.Errorf("got %q, want %q", got, abs)
	}
}

func TestResolveApplyWorkDir_AllMissingErrors(t *testing.T) {
	_, err := resolveApplyWorkDir("", "", nil)
	if err == nil {
		t.Errorf("expected error when nothing resolves")
	}
}

// ---------------------------------------------------------------------------
// renderApplyResult
// ---------------------------------------------------------------------------

func TestRenderApplyResult_SuccessShowsCommittedSerial(t *testing.T) {
	res := &apply.Result{
		ApplyID:          "ap-1",
		StateName:        "infra",
		StateID:          "st-1",
		SourceSerial:     7,
		CommittedSerial:  8,
		NewVersionID:     "sv-2",
		ResourcesPlanned: 3,
		ResourcesApplied: 2,
		AppliedAddresses: []string{"aws_instance.web", "aws_security_group.web"},
		StartedAt:        time.Date(2026, 5, 14, 13, 0, 0, 0, time.UTC),
		FinishedAt:       time.Date(2026, 5, 14, 13, 0, 2, 0, time.UTC),
		TempDir:          "/tmp/apply-x",
	}
	var buf bytes.Buffer
	renderApplyResult(&buf, res, nil)
	got := buf.String()

	for _, want := range []string{
		"apply succeeded",
		"apply id:", "ap-1",
		"state:", "infra",
		"source serial:", "7",
		"committed serial:", "8",
		"new version:", "sv-2",
		"resources planned:", "3",
		"resources applied:", "2",
		"temp dir:", "/tmp/apply-x",
		"duration:", "2s",
		"Applied:",
		"aws_instance.web",
		"aws_security_group.web",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q\nfull output:\n%s", want, got)
		}
	}
}

func TestRenderApplyResult_FailureSurfacesError(t *testing.T) {
	res := &apply.Result{
		ApplyID:          "ap-2",
		StateName:        "infra",
		SourceSerial:     5,
		ResourcesPlanned: 4,
		ResourcesApplied: 0,
		StartedAt:        time.Now().Add(-1 * time.Second),
		FinishedAt:       time.Now(),
	}
	var buf bytes.Buffer
	renderApplyResult(&buf, res, errors.New("acquire reservations: write/write conflict"))
	got := buf.String()
	for _, want := range []string{
		"apply FAILED",
		"Error:",
		"write/write conflict",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q\nfull output:\n%s", want, got)
		}
	}
	if strings.Contains(got, "committed serial") {
		t.Errorf("failure output should not advertise a committed serial; got:\n%s", got)
	}
}

func TestRenderApplyResult_NilSafe(t *testing.T) {
	var buf bytes.Buffer
	renderApplyResult(&buf, nil, errors.New("any error"))
	if buf.Len() != 0 {
		t.Errorf("nil result should produce no output, got: %q", buf.String())
	}
}

// ---------------------------------------------------------------------------
// Default-path ergonomics: --plan-spec defaults to ./kl-plan.json
// and --state defaults to spec.state_name.
// ---------------------------------------------------------------------------

// TestRunApply_NoPlanSpec_ReportsHelpfulMessage covers the
// ergonomics-defaults path: when --plan-spec is omitted the
// command looks for ./kl-plan.json in the CWD. If that
// file doesn't exist, the operator must get a clear hint
// ("run `kl plan` first?") rather than a cryptic open(2)
// error or unrelated config validation.
func TestRunApply_NoPlanSpec_ReportsHelpfulMessage(t *testing.T) {
	orig, _ := os.Getwd()
	tmp := t.TempDir()
	if err := os.Chdir(tmp); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer func() {
		if err := os.Chdir(orig); err != nil {
			t.Fatalf("restore chdir: %v", err)
		}
	}()

	// runApply writes directly to os.Stderr; redirect for capture.
	origStderr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w
	rc := runApply(nil)
	w.Close()
	os.Stderr = origStderr
	out, _ := io.ReadAll(r)

	if rc != 2 {
		t.Errorf("rc = %d, want 2 (usage error)", rc)
	}
	for _, want := range []string{"kl-plan.json", "kl plan"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("stderr missing %q\n---full---\n%s", want, string(out))
		}
	}
}

// TestRunApply_NoStateAndNoSpecStateName_RejectsWithUsage covers
// the second half of the default path: --state is omitted AND the
// spec carries no state_name (e.g. the plan was generated outside
// a terraform-init'ed directory). We refuse to guess; the operator
// gets a clear usage error before any DB work happens.
func TestRunApply_NoStateAndNoSpecStateName_RejectsWithUsage(t *testing.T) {
	dir := t.TempDir()
	specPath := filepath.Join(dir, "kl-plan.json")
	specJSON := `{"format_version":"1","generated_at":"2026-05-14T12:00:00Z","config_dir":"` + dir +
		`","plan_summary":{"create":0,"update":0,"delete":0,"replace":0,"read":0,"no_op":0,"forget":0,"total":0},` +
		`"write_set":[],"read_set":[],"hcl_footprint":[],"reservations":[]}`
	if err := os.WriteFile(specPath, []byte(specJSON), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}

	origStderr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w
	rc := runApply([]string{"--plan-spec=" + specPath})
	w.Close()
	os.Stderr = origStderr
	out, _ := io.ReadAll(r)

	if rc != 2 {
		t.Errorf("rc = %d, want 2 (usage error)", rc)
	}
	if !strings.Contains(string(out), "--state is required") {
		t.Errorf("stderr missing --state error\n---full---\n%s", string(out))
	}
}

// TestDefaultPlanSpecPath_IsRelativeToCWD pins the relative-path
// semantics of the default. It MUST stay relative so the engineer
// flow (`cd module/; kl plan; kl apply`) resolves
// to the spec just written in that directory rather than some
// absolute path baked in at build time.
func TestDefaultPlanSpecPath_IsRelativeToCWD(t *testing.T) {
	got := defaultPlanSpecPath()
	if filepath.IsAbs(got) {
		t.Errorf("default = %q; expected a relative path", got)
	}
	if filepath.Base(got) != "kl-plan.json" {
		t.Errorf("default basename = %q; expected kl-plan.json", filepath.Base(got))
	}
}

func TestDiscoverApplyStateName_PrefersBackendOverStaleSpec(t *testing.T) {
	dir := t.TempDir()
	terraformDir := filepath.Join(dir, ".terraform")
	if err := os.MkdirAll(terraformDir, 0o755); err != nil {
		t.Fatalf("mkdir .terraform: %v", err)
	}
	const initState = `{
  "version": 3,
  "terraform_version": "1.13.4",
  "backend": {
    "type": "http",
    "config": {
      "address": "http://localhost:8080/states/ws_0fb018ee0c37/env_bba69410e14b/blarg"
    }
  }
}`
	if err := os.WriteFile(filepath.Join(terraformDir, "terraform.tfstate"), []byte(initState), 0o644); err != nil {
		t.Fatalf("write init state: %v", err)
	}

	spec := &plan.PlanSpec{
		ConfigDir: dir,
		StateName: "blarg",
	}
	got := discoverApplyStateName("", "", spec)
	want := "ws_0fb018ee0c37/env_bba69410e14b/blarg"
	if got != want {
		t.Fatalf("discoverApplyStateName() = %q, want %q", got, want)
	}
}

func TestDiscoverApplyStateName_FallsBackToSpecStateName(t *testing.T) {
	spec := &plan.PlanSpec{StateName: "blarg"}
	if got := discoverApplyStateName("", "", spec); got != "blarg" {
		t.Fatalf("discoverApplyStateName() = %q, want %q", got, "blarg")
	}
}

func TestPlanBackendHTTPAuth_PrefersEnv(t *testing.T) {
	t.Setenv("TF_HTTP_USERNAME", "env-user")
	t.Setenv("TF_HTTP_PASSWORD", "env-pass")
	gotUser, gotPass := planBackendHTTPAuth(&plan.BackendInfo{
		Username: "file-user",
		Password: "file-pass",
	})
	if gotUser != "env-user" || gotPass != "env-pass" {
		t.Fatalf("planBackendHTTPAuth() = (%q,%q), want (%q,%q)", gotUser, gotPass, "env-user", "env-pass")
	}
}

func TestPlanBackendHTTPAuth_FallsBackToBackendInfo(t *testing.T) {
	gotUser, gotPass := planBackendHTTPAuth(&plan.BackendInfo{
		Username: "file-user",
		Password: "file-pass",
	})
	if gotUser != "file-user" || gotPass != "file-pass" {
		t.Fatalf("planBackendHTTPAuth() = (%q,%q), want (%q,%q)", gotUser, gotPass, "file-user", "file-pass")
	}
}

func TestRequireUnsafeTargetConfirmation(t *testing.T) {
	if err := requireUnsafeTargetConfirmation(nil, false, false); err != nil {
		t.Fatalf("unexpected err for empty targets: %v", err)
	}
	if err := requireUnsafeTargetConfirmation([]string{"time_sleep.slow_a"}, true, false); err != nil {
		t.Fatalf("unexpected err when unsafe gate is acknowledged: %v", err)
	}
	if err := requireUnsafeTargetConfirmation([]string{"time_sleep.slow_a"}, false, true); err != nil {
		t.Fatalf("unexpected err for dry-run target apply: %v", err)
	}
	err := requireUnsafeTargetConfirmation([]string{"time_sleep.slow_a"}, false, false)
	if err == nil {
		t.Fatal("expected error when --target is used without --allow-unsafe-target")
	}
	if !strings.Contains(err.Error(), "--allow-unsafe-target") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRenderApplyPreflight(t *testing.T) {
	spec := &plan.PlanSpec{
		StateName: "big-state",
		ConfigDir: "/tmp/repo",
		WriteSet:  []string{"time_sleep.slow_a"},
		ReadSet:   []string{"time_sleep.slow_b"},
		Reservations: []plan.PlanReservation{
			{Mode: "write", Address: "time_sleep.slow_a"},
		},
		PlanSummary: plan.PlanSummary{
			Create: 1,
			NoOp:   2,
			Total:  3,
		},
	}
	var buf bytes.Buffer
	renderApplyPreflight(&buf, spec, "targeted", []string{"time_sleep.slow_a"}, true, nil)
	got := buf.String()
	for _, want := range []string{
		"kl apply preflight",
		"mode:",
		"targeted",
		"state:",
		"big-state",
		"reservations:",
		"write preview:",
		"selectors:",
		"DRY-RUN",
		"rerun:",
		"--confirm-scope",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("preflight missing %q\nfull:\n%s", want, got)
		}
	}
}

func TestTargetedPreflightWarnings(t *testing.T) {
	spec := &plan.PlanSpec{
		WriteSet: make([]string, 12),
		Reservations: []plan.PlanReservation{
			{}, {}, {}, {}, {}, {}, {}, {}, {}, {}, {}, {},
		},
	}
	got := targetedPreflightWarnings(spec, []string{"time_sleep.slow_a"})
	if len(got) == 0 {
		t.Fatalf("expected fanout warnings, got none")
	}
}

func TestEnforceTargetPreflightStrict(t *testing.T) {
	if err := enforceTargetPreflightStrict(false, []string{"fanout high"}); err != nil {
		t.Fatalf("non-strict should not fail: %v", err)
	}
	if err := enforceTargetPreflightStrict(true, nil); err != nil {
		t.Fatalf("strict with no warnings should not fail: %v", err)
	}
	err := enforceTargetPreflightStrict(true, []string{"fanout high"})
	if err == nil {
		t.Fatal("expected strict preflight failure")
	}
	if !strings.Contains(err.Error(), "strict target preflight rejected") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestEnforceCoexistenceStrict(t *testing.T) {
	if err := enforceCoexistenceStrict(false, "big-state", []store.StatusLock{{LockID: "l1", Who: "alice"}}); err != nil {
		t.Fatalf("non-strict should not fail: %v", err)
	}
	if err := enforceCoexistenceStrict(true, "big-state", nil); err != nil {
		t.Fatalf("strict with no locks should not fail: %v", err)
	}
	err := enforceCoexistenceStrict(true, "big-state", []store.StatusLock{{LockID: "l1", Who: "alice"}})
	if err == nil {
		t.Fatal("expected strict coexistence failure")
	}
	if !strings.Contains(err.Error(), "strict coexistence rejected apply") || !strings.Contains(err.Error(), "big-state") {
		t.Fatalf("unexpected single-lock error: %v", err)
	}

	err = enforceCoexistenceStrict(true, "big-state", []store.StatusLock{
		{LockID: "l1", Who: "alice"},
		{LockID: "l2", Who: "bob"},
	})
	if err == nil {
		t.Fatal("expected strict coexistence failure for multiple locks")
	}
	if !strings.Contains(err.Error(), "2 active vanilla Terraform whole-state locks") {
		t.Fatalf("unexpected multi-lock error: %v", err)
	}
}

func TestEnforceCoexistenceStrict_StatePolicyEquivalent(t *testing.T) {
	err := enforceCoexistenceStrict(true, "big-state", []store.StatusLock{{LockID: "l1", Who: "alice"}})
	if err == nil || !strings.Contains(err.Error(), "strict coexistence rejected apply") {
		t.Fatalf("expected strict rejection, got %v", err)
	}
}

func TestRequireScopedApplyConfirmation(t *testing.T) {
	spec := &plan.PlanSpec{
		PlanSummary: plan.PlanSummary{Update: 1, Total: 1},
	}
	if err := requireScopedApplyConfirmation(spec, "file-scoped", []string{"slow_a.tf"}, true, false); err != nil {
		t.Fatalf("confirmed scope should pass: %v", err)
	}
	if err := requireScopedApplyConfirmation(spec, "file-scoped", []string{"slow_a.tf"}, false, true); err != nil {
		t.Fatalf("dry-run should pass: %v", err)
	}
	err := requireScopedApplyConfirmation(spec, "file-scoped", []string{"slow_a.tf"}, false, false)
	if err == nil {
		t.Fatal("expected error when scope is not confirmed")
	}
	if !strings.Contains(err.Error(), "--confirm-scope") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRequireDestructiveScopedConfirmation_IncludesRerunHint(t *testing.T) {
	spec := &plan.PlanSpec{
		PlanSummary: plan.PlanSummary{Delete: 1, Total: 1},
	}
	err := requireDestructiveScopedConfirmation(spec, []string{"slow_a.tf"}, false, false)
	if err == nil {
		t.Fatal("expected error when destructive scoped apply is not acknowledged")
	}
	if !strings.Contains(err.Error(), "--allow-destructive-scoped") || !strings.Contains(err.Error(), "--confirm-scope") {
		t.Fatalf("unexpected error: %v", err)
	}
}
