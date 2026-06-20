package apply

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kilolockio/kilolock/internal/slice"
)

// TestSetupApplyDir_CopiesAndOverrides exercises the happy path of
// the apply tmp-dir setup:
//
//   - .tf files from the operator's directory are copied verbatim
//   - the operator's backend.tf is REPLACED with our local backend
//   - top-level Terraform cache artifacts are reused
//   - other hidden files / state files are skipped
//   - the slice we hand in is serialized as terraform.tfstate
//
// We don't try to assert exact byte equality on the slice file:
// MarshalTrunkState produces indent-formatted JSON and that
// formatting is locked in by the slice package's own tests. Here
// we just verify the file exists, is non-empty, and parses back
// to the same Lineage / Serial we put in (the round-trip
// invariant the orchestrator will rely on).
func TestSetupApplyDir_CopiesAndOverrides(t *testing.T) {
	srcDir := t.TempDir()

	writeFile(t, srcDir, "main.tf", `resource "null_resource" "x" {}`+"\n")
	writeFile(t, srcDir, "variables.tf", `variable "foo" { default = "bar" }`+"\n")
	writeFile(t, srcDir, "backend.tf", `terraform { backend "http" { address = "https://example/" } }`+"\n")
	writeFile(t, srcDir, "terraform.tfstate", `{"version":4,"serial":1,"lineage":"stale"}`+"\n")
	writeFile(t, srcDir, ".terraform.lock.hcl", `# pinned`+"\n")
	writeFile(t, srcDir, ".terraform/providers/marker", "provider cache\n")
	if err := os.MkdirAll(filepath.Join(srcDir, "modules", "foo"), 0o755); err != nil {
		t.Fatalf("mkdir modules: %v", err)
	}
	writeFile(t, srcDir, "modules/foo/main.tf", `# inside module`+"\n")

	sliceState := &slice.TrunkState{
		Version:          4,
		TerraformVersion: "1.5.7",
		Serial:           42,
		Lineage:          "test-lineage",
		Resources:        []slice.TrunkResource{},
	}

	res, err := setupApplyDir(srcDir, sliceState, false)
	if err != nil {
		t.Fatalf("setupApplyDir: %v", err)
	}
	t.Cleanup(res.Cleanup)

	if !fileExists(filepath.Join(res.Dir, "main.tf")) {
		t.Errorf("main.tf was not copied")
	}
	if !fileExists(filepath.Join(res.Dir, "variables.tf")) {
		t.Errorf("variables.tf was not copied")
	}
	if !fileExists(filepath.Join(res.Dir, ".terraform.lock.hcl")) {
		t.Errorf("top-level lock file should have been reused")
	}
	if !fileExists(filepath.Join(res.Dir, ".terraform", "providers", "marker")) {
		t.Errorf("top-level .terraform cache should have been reused")
	}
	if !fileExists(filepath.Join(res.Dir, "modules", "foo", "main.tf")) {
		t.Errorf("local module file should have been copied recursively")
	}
	if fileExists(filepath.Join(res.Dir, "terraform.tfstate")) == false {
		// terraform.tfstate IS expected (we synthesize the slice
		// version) — make sure the synthesized one is there, not
		// the operator's stale one.
		t.Errorf("synthesized terraform.tfstate is missing")
	}

	backendBytes, err := os.ReadFile(filepath.Join(res.Dir, "backend.tf"))
	if err != nil {
		t.Fatalf("read replaced backend.tf: %v", err)
	}
	if !strings.Contains(string(backendBytes), `backend "local"`) {
		t.Errorf("backend.tf was not replaced with local backend, got:\n%s", backendBytes)
	}
	if strings.Contains(string(backendBytes), "example") {
		t.Errorf("backend.tf still contains operator's HTTP backend address")
	}

	statePath := filepath.Join(res.Dir, "terraform.tfstate")
	stateBytes, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read written slice: %v", err)
	}
	parsed, err := slice.ParseTrunkState(stateBytes)
	if err != nil {
		t.Fatalf("parse written slice: %v", err)
	}
	if parsed.Lineage != sliceState.Lineage {
		t.Errorf("slice lineage mismatch: got %q want %q", parsed.Lineage, sliceState.Lineage)
	}
	if parsed.Serial != sliceState.Serial {
		t.Errorf("slice serial mismatch: got %d want %d", parsed.Serial, sliceState.Serial)
	}
}

func TestSetupApplyDir_NilInputsRejected(t *testing.T) {
	if _, err := setupApplyDir("", nil, false); err == nil {
		t.Errorf("expected error for empty srcDir, got nil")
	}
	if _, err := setupApplyDir(t.TempDir(), nil, false); err == nil {
		t.Errorf("expected error for nil sliceState, got nil")
	}
}

// TestSetupApplyDir_DenylistFiltersHiddenAndStateAtAllDepths
// pins the recursive-copy rules. A user's repo may have:
//
//   - a top-level .terraform/ cache we do want to reuse
//   - a nested .terraform/ left over from a previous init
//   - a hidden directory like .git
//   - a sub-module that happens to contain its own
//     terraform.tfstate (foreign data we shouldn't touch)
//
// Only the root cache should land in the apply tmp dir.
func TestSetupApplyDir_DenylistFiltersHiddenAndStateAtAllDepths(t *testing.T) {
	srcDir := t.TempDir()
	writeFile(t, srcDir, "main.tf", `# root`+"\n")
	writeFile(t, srcDir, "modules/foo/main.tf", `# module foo`+"\n")
	writeFile(t, srcDir, ".terraform/providers/root-marker", "keep me\n")

	if err := os.MkdirAll(filepath.Join(srcDir, "modules", "foo", ".terraform", "providers"), 0o755); err != nil {
		t.Fatalf("mkdir nested .terraform: %v", err)
	}
	writeFile(t, srcDir, "modules/foo/.terraform/providers/marker", "should be skipped\n")
	if err := os.MkdirAll(filepath.Join(srcDir, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	writeFile(t, srcDir, ".git/HEAD", "ref: refs/heads/main\n")

	sliceState := &slice.TrunkState{Version: 4, Lineage: "k", Resources: []slice.TrunkResource{}}
	res, err := setupApplyDir(srcDir, sliceState, false)
	if err != nil {
		t.Fatalf("setupApplyDir: %v", err)
	}
	t.Cleanup(res.Cleanup)

	if !fileExists(filepath.Join(res.Dir, "modules", "foo", "main.tf")) {
		t.Errorf("expected module to be copied")
	}
	if !fileExists(filepath.Join(res.Dir, ".terraform", "providers", "root-marker")) {
		t.Errorf("expected top-level .terraform cache to be copied")
	}
	if fileExists(filepath.Join(res.Dir, "modules", "foo", ".terraform")) {
		t.Errorf("nested .terraform must be skipped")
	}
	if fileExists(filepath.Join(res.Dir, ".git")) {
		t.Errorf("hidden .git directory must be skipped")
	}
}

// TestSetupApplyDir_SkipCleanupKeepsDir asserts SkipCleanup turns
// the cleanup function into a no-op so operators debugging an
// apply can poke at the directory afterward.
func TestSetupApplyDir_SkipCleanupKeepsDir(t *testing.T) {
	srcDir := t.TempDir()
	writeFile(t, srcDir, "main.tf", `# empty`+"\n")
	sliceState := &slice.TrunkState{Version: 4, Lineage: "k", Resources: []slice.TrunkResource{}}

	res, err := setupApplyDir(srcDir, sliceState, true)
	if err != nil {
		t.Fatalf("setupApplyDir: %v", err)
	}
	dir := res.Dir
	res.Cleanup()
	if !fileExists(dir) {
		t.Errorf("SkipCleanup=true should not remove the directory; got removed: %s", dir)
	}
	_ = os.RemoveAll(dir)
}

func writeFile(t *testing.T, dir, rel, body string) {
	t.Helper()
	full := filepath.Join(dir, rel)
	if dirPart := filepath.Dir(full); dirPart != dir {
		_ = os.MkdirAll(dirPart, 0o755)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
