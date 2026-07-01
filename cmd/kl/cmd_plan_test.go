package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kilolockio/kilolock/internal/plan"
)

// ---------------------------------------------------------------------------
// resolvePlanOutPath
// ---------------------------------------------------------------------------

func TestResolvePlanOutPath_DefaultsNextToHCL(t *testing.T) {
	got, err := resolvePlanOutPath("", "/tmp/infra")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if want := "/tmp/infra/kl-plan.json"; got != want {
		t.Errorf("default = %q, want %q", got, want)
	}
}

func TestResolvePlanOutPath_DashIsStdout(t *testing.T) {
	got, err := resolvePlanOutPath("-", "/tmp/infra")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "-" {
		t.Errorf("dash sentinel was rewritten to %q", got)
	}
}

func TestResolvePlanOutPath_AbsolutePathPassesThrough(t *testing.T) {
	got, err := resolvePlanOutPath("/var/plans/foo.json", "/tmp/infra")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "/var/plans/foo.json" {
		t.Errorf("absolute path mangled: %q", got)
	}
}

func TestResolvePlanOutPath_RelativeResolvesAgainstCwd(t *testing.T) {
	// Use a sentinel name unlikely to collide. We just verify it
	// resolves to an absolute path that ends in our filename; the
	// exact CWD prefix depends on the test runner's PWD.
	got, err := resolvePlanOutPath("plans/v2.json", "/unused")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("relative path not made absolute: %q", got)
	}
	if !strings.HasSuffix(got, filepath.Join("plans", "v2.json")) {
		t.Errorf("relative path lost suffix: %q", got)
	}
}

// ---------------------------------------------------------------------------
// countReservations
// ---------------------------------------------------------------------------

func TestCountReservations(t *testing.T) {
	rs := []plan.PlanReservation{
		{Address: "a", Mode: "write"},
		{Address: "b", Mode: "write"},
		{Address: "c", Mode: "read"},
	}
	if got := countReservations(rs, "write"); got != 2 {
		t.Errorf("writes = %d, want 2", got)
	}
	if got := countReservations(rs, "read"); got != 1 {
		t.Errorf("reads = %d, want 1", got)
	}
	if got := countReservations(rs, "other"); got != 0 {
		t.Errorf("other = %d, want 0", got)
	}
	if got := countReservations(nil, "write"); got != 0 {
		t.Errorf("nil = %d, want 0", got)
	}
}

// ---------------------------------------------------------------------------
// renderPlanSummary — assert key lines without snapshotting whitespace
// ---------------------------------------------------------------------------

func TestRenderPlanSummary_IncludesActionCountersAndAddresses(t *testing.T) {
	spec := &plan.PlanSpec{
		ConfigDir:        "/tmp/cfg",
		TerraformVersion: "1.13.4",
		PlanSummary: plan.PlanSummary{
			Create: 1, Update: 0, Delete: 2, Replace: 3, Read: 0,
			NoOp: 4, Forget: 0, Total: 10,
		},
		WriteSet: []string{"a.b", "c.d"},
		Reservations: []plan.PlanReservation{
			{Address: "a.b", Mode: "write"},
			{Address: "c.d", Mode: "write"},
			{Address: "e.f", Mode: "read"},
		},
	}
	var buf bytes.Buffer
	renderPlanSummary(&buf, spec)
	out := buf.String()
	for _, want := range []string{
		"config:", "/tmp/cfg",
		"terraform:", "1.13.4",
		"create:", "1",
		"replace:", "3",
		"no-op:", "4",
		"total:", "10",
		"reservations:", "3", "2 write", "1 read",
		"a.b", "c.d",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary missing %q\n---full---\n%s", want, out)
		}
	}
}

func TestRenderPlanSummary_ScopedNoOpLabelsOwnershipOnly(t *testing.T) {
	spec := &plan.PlanSpec{
		ConfigDir:        "/tmp/cfg",
		TerraformVersion: "1.13.4",
		ScopedFiles:      []string{"slow_a.tf"},
		PlanSummary: plan.PlanSummary{
			Create: 0, Update: 0, Delete: 0, Replace: 0, Read: 0,
			NoOp: 6, Forget: 0, Total: 6,
		},
		WriteSet: []string{
			"module.small_herd.random_id.leader",
		},
	}
	var buf bytes.Buffer
	renderPlanSummary(&buf, spec)
	out := buf.String()
	if !strings.Contains(out, "scoped writable addresses") {
		t.Fatalf("expected scoped no-op label, got:\n%s", out)
	}
	if strings.Contains(out, "scoped write set") {
		t.Fatalf("unexpected mutating scoped label in no-op summary:\n%s", out)
	}
}

// ---------------------------------------------------------------------------
// emitPlanSpec — drives the deterministic half of runPlan with a
// captured `terraform show -json` payload, verifying spec correctness
// and file/stdout output. Mirrors the v2 user flow (terraform plan →
// kl plan → kl-plan.json) without invoking terraform.
// ---------------------------------------------------------------------------

const capturedShowJSON = `{
	"format_version": "1.2",
	"terraform_version": "1.13.4",
	"resource_changes": [
		{
			"address": "random_id.web",
			"mode": "managed",
			"type": "random_id",
			"name": "web",
			"change": { "actions": ["update"] }
		},
		{
			"address": "random_id.db",
			"mode": "managed",
			"type": "random_id",
			"name": "db",
			"change": { "actions": ["no-op"] }
		}
	],
	"planned_values": {
		"root_module": {
			"resources": [
				{"address": "random_id.web", "mode": "managed", "type": "random_id", "name": "web"},
				{"address": "random_id.db",  "mode": "managed", "type": "random_id", "name": "db"}
			]
		}
	},
	"configuration": {
		"root_module": {
			"resources": []
		}
	}
}`

const capturedScopedShowJSON = `{
  "format_version": "1.2",
  "terraform_version": "1.13.4",
  "resource_changes": [
    {
      "address": "time_sleep.slow_a",
      "mode": "managed",
      "type": "time_sleep",
      "name": "slow_a",
      "change": { "actions": ["update"] }
    },
    {
      "address": "time_sleep.slow_b",
      "mode": "managed",
      "type": "time_sleep",
      "name": "slow_b",
      "change": { "actions": ["update"] }
    }
  ],
  "planned_values": {
    "root_module": {
      "resources": [
        {"address": "time_sleep.slow_a", "mode": "managed", "type": "time_sleep", "name": "slow_a"},
        {"address": "time_sleep.slow_b", "mode": "managed", "type": "time_sleep", "name": "slow_b"}
      ]
    }
  },
  "configuration": {
    "root_module": {
      "resources": [
        {
          "address": "time_sleep.slow_a",
          "mode": "managed",
          "type": "time_sleep",
          "name": "slow_a",
          "range": { "filename": "slow_a.tf" },
          "expressions": { "triggers": { "references": ["var.slow_a_version"] } }
        },
        {
          "address": "time_sleep.slow_b",
          "mode": "managed",
          "type": "time_sleep",
          "name": "slow_b",
          "range": { "filename": "slow_b.tf" },
          "expressions": { "triggers": { "references": ["var.slow_b_version"] } }
        }
      ]
    }
  }
}`

func TestEmitPlanSpec_WritesToFile(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(dir, "spec.json")

	var stderr bytes.Buffer
	rc, _, err := emitPlanSpec(planEmitInput{
		showJSON:    []byte(capturedShowJSON),
		configDir:   dir,
		outPath:     outPath,
		generatedAt: time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC),
		stdout:      &bytes.Buffer{},
		stderr:      &stderr,
	})
	if err != nil || rc != 0 {
		t.Fatalf("emitPlanSpec: rc=%d err=%v", rc, err)
	}
	if !strings.Contains(stderr.String(), "spec written to") {
		t.Errorf("stderr missing 'spec written to': %s", stderr.String())
	}

	b, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read spec file: %v", err)
	}
	spec, err := plan.UnmarshalSpec(b)
	if err != nil {
		t.Fatalf("parse written spec: %v", err)
	}
	if spec.FormatVersion != plan.CurrentSpecFormatVersion {
		t.Errorf("FormatVersion = %q", spec.FormatVersion)
	}
	if want := []string{"random_id.web"}; !equalSlices(spec.WriteSet, want) {
		t.Errorf("WriteSet = %v, want %v", spec.WriteSet, want)
	}
}

func TestEmitPlanSpec_WritesToStdout(t *testing.T) {
	var stdout, stderr bytes.Buffer
	rc, _, err := emitPlanSpec(planEmitInput{
		showJSON:    []byte(capturedShowJSON),
		configDir:   "/dummy",
		outPath:     "-",
		generatedAt: time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC),
		stdout:      &stdout,
		stderr:      &stderr,
	})
	if err != nil || rc != 0 {
		t.Fatalf("emitPlanSpec: rc=%d err=%v", rc, err)
	}
	// stderr summary still printed; stdout receives the JSON payload.
	if !strings.Contains(stderr.String(), "kl plan summary") {
		t.Errorf("stderr missing summary: %s", stderr.String())
	}
	if !strings.HasPrefix(strings.TrimSpace(stdout.String()), "{") {
		t.Errorf("stdout not JSON: %s", stdout.String()[:min(120, len(stdout.String()))])
	}
}

func TestEmitPlanSpec_RejectsInvalidJSON(t *testing.T) {
	rc, _, err := emitPlanSpec(planEmitInput{
		showJSON: []byte(`{not json`),
		outPath:  "-",
		stdout:   &bytes.Buffer{},
		stderr:   &bytes.Buffer{},
	})
	if rc == 0 || err == nil {
		t.Errorf("expected failure on bad JSON, got rc=%d err=%v", rc, err)
	}
}

func TestEmitPlanSpec_FileScopeNarrowsWriteSet(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "slow_a.tf"), []byte(`resource "time_sleep" "slow_a" {}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "slow_b.tf"), []byte(`resource "time_sleep" "slow_b" {}`), 0o644); err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(dir, "scoped.json")
	rc, _, err := emitPlanSpec(planEmitInput{
		showJSON:    []byte(capturedScopedShowJSON),
		configDir:   dir,
		scopeFiles:  []string{"slow_a.tf"},
		outPath:     outPath,
		generatedAt: time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC),
		stdout:      &bytes.Buffer{},
		stderr:      &bytes.Buffer{},
	})
	if err != nil || rc != 0 {
		t.Fatalf("emitPlanSpec: rc=%d err=%v", rc, err)
	}
	b, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read spec: %v", err)
	}
	spec, err := plan.UnmarshalSpec(b)
	if err != nil {
		t.Fatalf("unmarshal spec: %v", err)
	}
	if got, want := spec.WriteSet, []string{"time_sleep.slow_a"}; !equalSlices(got, want) {
		t.Fatalf("write_set = %v, want %v", got, want)
	}
	if got, want := spec.ScopedFiles, []string{"slow_a.tf"}; !equalSlices(got, want) {
		t.Fatalf("scoped_files = %v, want %v", got, want)
	}
	if len(spec.Reservations) != 1 || spec.Reservations[0].Address != "time_sleep.slow_a" {
		t.Fatalf("reservations = %+v", spec.Reservations)
	}
}

func TestEmitPlanSpec_PreservesStateEngineMetadata(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "slow_a.tf"), []byte(`resource "time_sleep" "slow_a" {}`), 0o644); err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(dir, "scoped-engine.json")
	rc, _, err := emitPlanSpec(planEmitInput{
		showJSON:   []byte(capturedScopedShowJSON),
		configDir:  dir,
		scopeFiles: []string{"slow_a.tf"},
		stateEngine: &plan.StateEnginePlanMetadata{
			Mode:                  "native-slice-with-discovery-fallback",
			DiscoveryEngine:       "heuristic",
			FetchAddresses:        []string{"random_pet.deployment_name"},
			ConfigRequiredNodes:   []string{"null_resource.future"},
			RemovedConfigNodes:    []string{"time_sleep.deleted"},
			MissingFromState:      []string{"null_resource.future"},
			UndeployedCandidates:  []string{"null_resource.future"},
			UnknownMissing:        []string{},
			Confidence:            "safe",
			Notes:                 []string{"required config-only node preserved from support file"},
			ResolveDurationMs:     5,
			ExpandDurationMs:      7,
			SliceFetchDurationMs:  11,
			SliceResourceCount:    2,
			GraphCacheHit:         true,
			RealizedResourceCount: 12,
			DependencyEdgeCount:   18,
			InventoryScanCount:    4,
			SliceBytes:            123,
		},
		outPath:     outPath,
		generatedAt: time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC),
		stdout:      &bytes.Buffer{},
		stderr:      &bytes.Buffer{},
	})
	if err != nil || rc != 0 {
		t.Fatalf("emitPlanSpec: rc=%d err=%v", rc, err)
	}
	b, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read spec: %v", err)
	}
	spec, err := plan.UnmarshalSpec(b)
	if err != nil {
		t.Fatalf("unmarshal spec: %v", err)
	}
	if spec.StateEngine == nil {
		t.Fatalf("expected state_engine metadata to be present")
	}
	if spec.StateEngine.Mode != "native-slice-with-discovery-fallback" {
		t.Fatalf("mode = %q, want native-slice-with-discovery-fallback", spec.StateEngine.Mode)
	}
	if spec.StateEngine.DiscoveryEngine != "heuristic" {
		t.Fatalf("discovery_engine = %q, want heuristic", spec.StateEngine.DiscoveryEngine)
	}
	if got, want := spec.StateEngine.FetchAddresses, []string{"random_pet.deployment_name"}; !equalSlices(got, want) {
		t.Fatalf("fetch_addresses = %v, want %v", got, want)
	}
	if got, want := spec.StateEngine.ConfigRequiredNodes, []string{"null_resource.future"}; !equalSlices(got, want) {
		t.Fatalf("config_required_nodes = %v, want %v", got, want)
	}
	if got, want := spec.StateEngine.RemovedConfigNodes, []string{"time_sleep.deleted"}; !equalSlices(got, want) {
		t.Fatalf("removed_config_nodes = %v, want %v", got, want)
	}
	if spec.StateEngine.SliceBytes != 123 {
		t.Fatalf("slice_bytes = %d, want 123", spec.StateEngine.SliceBytes)
	}
	if spec.StateEngine.ResolveDurationMs != 5 || spec.StateEngine.ExpandDurationMs != 7 || spec.StateEngine.SliceFetchDurationMs != 11 {
		t.Fatalf("unexpected state-engine timings: %+v", spec.StateEngine)
	}
	if spec.StateEngine.SliceResourceCount != 2 {
		t.Fatalf("slice_resource_count = %d, want 2", spec.StateEngine.SliceResourceCount)
	}
	if !spec.StateEngine.GraphCacheHit || spec.StateEngine.RealizedResourceCount != 12 || spec.StateEngine.DependencyEdgeCount != 18 || spec.StateEngine.InventoryScanCount != 4 {
		t.Fatalf("unexpected scope diagnostics: %+v", spec.StateEngine)
	}
	if got, want := spec.StateEngine.MissingFromState, []string{"null_resource.future"}; !equalSlices(got, want) {
		t.Fatalf("missing_from_state = %v, want %v", got, want)
	}
	if got, want := spec.StateEngine.UndeployedCandidates, []string{"null_resource.future"}; !equalSlices(got, want) {
		t.Fatalf("undeployed_candidates = %v, want %v", got, want)
	}
	if spec.StateEngine.Confidence != "safe" {
		t.Fatalf("confidence = %q, want safe", spec.StateEngine.Confidence)
	}
	if got, want := spec.StateEngine.Notes, []string{"required config-only node preserved from support file"}; !equalSlices(got, want) {
		t.Fatalf("notes = %v, want %v", got, want)
	}
}

func TestEmitPlanSpec_PreservesStateEngineFallbackMetadata(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "slow_a.tf"), []byte(`resource "time_sleep" "slow_a" {}`), 0o644); err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(dir, "scoped-engine-fallback.json")
	rc, _, err := emitPlanSpec(planEmitInput{
		showJSON:   []byte(capturedScopedShowJSON),
		configDir:  dir,
		scopeFiles: []string{"slow_a.tf"},
		stateEngine: &plan.StateEnginePlanMetadata{
			Mode:           "full-trunk-fallback",
			FallbackReason: "native scoped state-engine path unavailable; falling back to full trunk",
			Notes:          []string{"native scoped state-engine path unavailable; falling back to full trunk"},
			FullStateBytes: 4096,
		},
		outPath:     outPath,
		generatedAt: time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC),
		stdout:      &bytes.Buffer{},
		stderr:      &bytes.Buffer{},
	})
	if err != nil || rc != 0 {
		t.Fatalf("emitPlanSpec: rc=%d err=%v", rc, err)
	}
	b, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read spec: %v", err)
	}
	spec, err := plan.UnmarshalSpec(b)
	if err != nil {
		t.Fatalf("unmarshal spec: %v", err)
	}
	if spec.StateEngine == nil {
		t.Fatalf("expected state_engine metadata to be present")
	}
	if spec.StateEngine.Mode != "full-trunk-fallback" {
		t.Fatalf("mode = %q, want full-trunk-fallback", spec.StateEngine.Mode)
	}
	if spec.StateEngine.FallbackReason == "" {
		t.Fatal("expected fallback reason to be preserved")
	}
	if spec.StateEngine.FullStateBytes != 4096 {
		t.Fatalf("full_state_bytes = %d, want 4096", spec.StateEngine.FullStateBytes)
	}
}

func TestEmitPlanSpec_FileScopeEmptyWriteSetFails(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "not-owned.tf"), []byte(`resource "null_resource" "x" {}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := emitPlanSpec(planEmitInput{
		showJSON:    []byte(capturedScopedShowJSON),
		configDir:   dir,
		scopeFiles:  []string{"not-owned.tf"},
		outPath:     filepath.Join(dir, "bad.json"),
		generatedAt: time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC),
		stdout:      &bytes.Buffer{},
		stderr:      &bytes.Buffer{},
	})
	if err == nil || !strings.Contains(err.Error(), "empty write_set") {
		t.Fatalf("expected empty write_set error, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// argv handling — runPlan's parsing layer
// ---------------------------------------------------------------------------

// TestRunPlan_DefaultsConfigDirToCWD asserts that running
// `kl plan` with no positional argument uses the current
// working directory as the config dir. The terraform invocation
// will still fail in this test (the test runner's CWD has no
// .tf files), but the failure must come from terraform — NOT from
// usage validation — so rc != 2.
func TestRunPlan_DefaultsConfigDirToCWD(t *testing.T) {
	// Force CWD to a brand-new empty tempdir so we don't
	// accidentally run terraform plan against the test runner's
	// real working directory (which could be a source tree with
	// .tf files in it).
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
	rc := runPlan(nil)
	if rc == 2 {
		t.Errorf("no-args run should NOT return usage error (rc=2); got %d", rc)
	}
}

func TestRunPlan_NonexistentConfigDirReturnsUsageError(t *testing.T) {
	if rc := runPlan([]string{"/this/dir/does/not/exist/v2spike"}); rc != 2 {
		t.Errorf("bad dir: rc = %d, want 2", rc)
	}
}

func TestRunPlan_FileInsteadOfDirReturnsUsageError(t *testing.T) {
	f, err := os.CreateTemp("", "kl-plan-target-*.tf")
	if err != nil {
		t.Fatalf("create tmp: %v", err)
	}
	f.Close()
	defer os.Remove(f.Name())
	if rc := runPlan([]string{f.Name()}); rc != 2 {
		t.Errorf("file-not-dir: rc = %d, want 2", rc)
	}
}

// equalSlices is a copy-free pre-go1.21 slice equality check used here
// to avoid pulling reflect.DeepEqual for trivial string comparisons.
func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// varFlag + variables plumbing into the plan spec.
// ---------------------------------------------------------------------------

func TestVarFlag_ParsesNAMEVALUE_JSONEncoded(t *testing.T) {
	v := &varFlag{}
	if err := v.Set("slow_a_version=v2"); err != nil {
		t.Fatalf("first set: %v", err)
	}
	if err := v.Set("slow_b_version=v1"); err != nil {
		t.Fatalf("second set: %v", err)
	}
	if got := string(v.values["slow_a_version"]); got != `"v2"` {
		t.Errorf(`slow_a_version = %s, want "v2" (JSON-quoted)`, got)
	}
	if got := string(v.values["slow_b_version"]); got != `"v1"` {
		t.Errorf(`slow_b_version = %s, want "v1" (JSON-quoted)`, got)
	}
}

func TestVarFlag_LastWriteWins(t *testing.T) {
	v := &varFlag{}
	_ = v.Set("env=staging")
	_ = v.Set("env=prod")
	if got := string(v.values["env"]); got != `"prod"` {
		t.Errorf("env = %s, want \"prod\" (later --var must override earlier)", got)
	}
}

func TestVarFlag_AllowsEqualsInValue(t *testing.T) {
	v := &varFlag{}
	if err := v.Set("query=k=v&x=y"); err != nil {
		t.Fatalf("err: %v", err)
	}
	if got := string(v.values["query"]); got != `"k=v&x=y"` {
		t.Errorf("query = %s, want %q (JSON-quoted)", got, `"k=v&x=y"`)
	}
}

func TestVarFlag_EmbeddedQuotesEscaped(t *testing.T) {
	v := &varFlag{}
	if err := v.Set(`greeting=hello "world"`); err != nil {
		t.Fatalf("err: %v", err)
	}
	want := `"hello \"world\""`
	if got := string(v.values["greeting"]); got != want {
		t.Errorf("greeting = %s, want %s", got, want)
	}
}

func TestVarFlag_RejectsMissingEquals(t *testing.T) {
	v := &varFlag{}
	if err := v.Set("slow_a_version"); err == nil {
		t.Error("expected error on NAME with no =VALUE")
	}
}

func TestVarFlag_RejectsEmptyName(t *testing.T) {
	v := &varFlag{}
	if err := v.Set("=v1"); err == nil {
		t.Error("expected error on =VALUE with empty name")
	}
}

// capturedShowJSONWithVars mirrors capturedShowJSON but carries a
// top-level `variables` block, the shape terraform emits when any
// HCL variable was evaluated during plan. emitPlanSpec must surface
// these into spec.Variables when pinAllVars=true.
const capturedShowJSONWithVars = `{
	"format_version": "1.2",
	"terraform_version": "1.13.4",
	"variables": {
		"region":         {"value": "us-east-1"},
		"instance_count": {"value": 3},
		"tags":           {"value": {"env": "prod"}},
		"slow_a_version": {"value": "v1"}
	},
	"resource_changes": [
		{
			"address": "random_id.web",
			"mode": "managed",
			"type": "random_id",
			"name": "web",
			"change": { "actions": ["update"] }
		}
	],
	"planned_values": {
		"root_module": {
			"resources": [
				{"address": "random_id.web", "mode": "managed", "type": "random_id", "name": "web"}
			]
		}
	},
	"configuration": {"root_module": {"resources": []}}
}`

// TestEmitPlanSpec_AutoPinsTerraformObservedVariables drives the
// auto-pin path: the operator passes no --var flags, but the spec
// still records the effective input set terraform actually used.
// Explicit overrides on the same name win (slow_a_version=v2 wins
// over the v1 baked into the show JSON).
func TestEmitPlanSpec_AutoPinsTerraformObservedVariables(t *testing.T) {
	tmp := t.TempDir()
	outPath := filepath.Join(tmp, "spec.json")

	rc, _, err := emitPlanSpec(planEmitInput{
		showJSON:    []byte(capturedShowJSONWithVars),
		configDir:   "/tmp/cfg",
		outPath:     outPath,
		generatedAt: time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC),
		explicitVars: map[string]json.RawMessage{
			"slow_a_version": json.RawMessage(`"v2"`),
		},
		pinAllVars: true,
		stdout:     &bytes.Buffer{},
		stderr:     &bytes.Buffer{},
	})
	if err != nil || rc != 0 {
		t.Fatalf("emit: rc=%d err=%v", rc, err)
	}

	b, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read spec: %v", err)
	}
	spec, err := plan.UnmarshalSpec(b)
	if err != nil {
		t.Fatalf("parse spec: %v", err)
	}
	// Compare each value semantically (compact JSON) so we tolerate
	// any whitespace MarshalIndent introduces while still asserting
	// faithful type/value preservation. terraform splicing handles
	// the re-compaction at apply time (see TerraformVarArgs).
	cases := map[string]string{
		"region":         `"us-east-1"`,
		"instance_count": `3`,
		"tags":           `{"env":"prod"}`,
		"slow_a_version": `"v2"`,
	}
	for k, want := range cases {
		got := compactJSON(t, spec.Variables[k])
		if got != want {
			t.Errorf("spec.Variables[%s] = %s, want %s", k, got, want)
		}
	}
}

func compactJSON(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		t.Fatalf("compact: %v", err)
	}
	return buf.String()
}

// TestEmitPlanSpec_NoPinVars_SkipsObserved exercises the
// --no-pin-vars escape hatch: only explicit --var=NAME=VALUE
// values end up in spec.Variables. Useful when some plan-time
// values are sensitive.
func TestEmitPlanSpec_NoPinVars_SkipsObserved(t *testing.T) {
	tmp := t.TempDir()
	outPath := filepath.Join(tmp, "spec.json")

	rc, _, err := emitPlanSpec(planEmitInput{
		showJSON:    []byte(capturedShowJSONWithVars),
		configDir:   "/tmp/cfg",
		outPath:     outPath,
		generatedAt: time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC),
		explicitVars: map[string]json.RawMessage{
			"slow_a_version": json.RawMessage(`"v2"`),
		},
		pinAllVars: false,
		stdout:     &bytes.Buffer{},
		stderr:     &bytes.Buffer{},
	})
	if err != nil || rc != 0 {
		t.Fatalf("emit: rc=%d err=%v", rc, err)
	}

	b, _ := os.ReadFile(outPath)
	spec, _ := plan.UnmarshalSpec(b)
	if _, leaked := spec.Variables["region"]; leaked {
		t.Error("--no-pin-vars must NOT carry terraform-observed variables into the spec")
	}
	if got := string(spec.Variables["slow_a_version"]); got != `"v2"` {
		t.Errorf("explicit --var dropped under --no-pin-vars: %s", got)
	}
}

// TestEmitPlanSpec_PinsExplicitVariables verifies that --var=
// values land in PlanSpec.Variables (JSON-encoded), the contract
// `kl apply` relies on to replay variables.
func TestEmitPlanSpec_PinsExplicitVariables(t *testing.T) {
	tmp, err := os.MkdirTemp("", "kl-plan-vars-*")
	if err != nil {
		t.Fatalf("mkdir tmp: %v", err)
	}
	defer os.RemoveAll(tmp)
	outPath := filepath.Join(tmp, "spec.json")

	rc, _, err := emitPlanSpec(planEmitInput{
		showJSON:    []byte(capturedShowJSON),
		configDir:   "/tmp/cfg",
		outPath:     outPath,
		generatedAt: time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC),
		explicitVars: map[string]json.RawMessage{
			"slow_a_version": json.RawMessage(`"v2"`),
			"slow_b_version": json.RawMessage(`"v1"`),
		},
		pinAllVars: true,
		stdout:     &bytes.Buffer{},
		stderr:     &bytes.Buffer{},
	})
	if err != nil || rc != 0 {
		t.Fatalf("emit: rc=%d err=%v", rc, err)
	}

	b, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read spec: %v", err)
	}
	spec, err := plan.UnmarshalSpec(b)
	if err != nil {
		t.Fatalf("parse spec: %v", err)
	}
	if got := string(spec.Variables["slow_a_version"]); got != `"v2"` {
		t.Errorf("spec.Variables[slow_a_version] = %s", got)
	}
	if got := string(spec.Variables["slow_b_version"]); got != `"v1"` {
		t.Errorf("spec.Variables[slow_b_version] = %s", got)
	}
}

// TestRenderPlanSummary_ShowsVariables verifies the human-readable
// plan summary surfaces pinned variables so operators can sanity-
// check the spec before applying.
func TestRenderPlanSummary_ShowsVariables(t *testing.T) {
	spec := &plan.PlanSpec{
		ConfigDir:        "/tmp/cfg",
		TerraformVersion: "1.13.4",
		PlanSummary: plan.PlanSummary{
			Update: 1, NoOp: 0, Total: 1,
		},
		WriteSet: []string{"time_sleep.slow_a"},
		Reservations: []plan.PlanReservation{
			{Address: "time_sleep.slow_a", Mode: "write"},
		},
		Variables: map[string]json.RawMessage{
			"slow_a_version": json.RawMessage(`"v2"`),
			"slow_b_version": json.RawMessage(`"v1"`),
		},
		StateName: "big-state",
	}
	var buf bytes.Buffer
	renderPlanSummary(&buf, spec)
	out := buf.String()
	for _, want := range []string{
		"variables", "(2,",
		`slow_a_version="v2"`,
		`slow_b_version="v1"`,
		"state:", "big-state",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary missing %q\n---full---\n%s", want, out)
		}
	}
}
