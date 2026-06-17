package plan

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestNormalizeFileScope(t *testing.T) {
	scope, err := NormalizeFileScope("/repo/examples/big-state", []string{
		"slow_b.tf",
		"./slow_a.tf",
		"slow_b.tf",
	})
	if err != nil {
		t.Fatalf("NormalizeFileScope: %v", err)
	}
	want := []string{"slow_a.tf", "slow_b.tf"}
	if !reflect.DeepEqual(scope.Relative, want) {
		t.Fatalf("scope = %v, want %v", scope.Relative, want)
	}
}

func TestNormalizeFileScope_RejectsOutsideConfigDir(t *testing.T) {
	_, err := NormalizeFileScope("/repo/examples/big-state", []string{"../other.tf"})
	if err == nil {
		t.Fatal("expected error for path outside config dir")
	}
}

func TestApplyFileScope_NarrowsWriteSetAndRecomputesReadSet(t *testing.T) {
	cfgDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cfgDir, "slow_a.tf"), []byte(`resource "time_sleep" "slow_a" {}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "slow_b.tf"), []byte(`resource "time_sleep" "slow_b" {}`), 0o644); err != nil {
		t.Fatal(err)
	}
	f := &File{
		Configuration: Configuration{
			RootModule: ConfigModule{
				Resources: []ConfigResource{
					{
						Address:   "time_sleep.slow_a",
						DeclRange: &SourceRange{Filename: cfgDir + "/slow_a.tf"},
						Expressions: map[string]any{
							"triggers": map[string]any{
								"references": []any{"var.slow_a_version"},
							},
						},
					},
					{
						Address:   "time_sleep.slow_b",
						DeclRange: &SourceRange{Filename: cfgDir + "/slow_b.tf"},
						Expressions: map[string]any{
							"triggers": map[string]any{
								"references": []any{"var.slow_b_version"},
							},
						},
					},
				},
			},
		},
	}
	spec := &PlanSpec{
		ConfigDir: cfgDir,
		WriteSet:  []string{"time_sleep.slow_a", "time_sleep.slow_b"},
		ReadSet:   []string{"time_sleep.slow_a", "time_sleep.slow_b"},
	}
	scope := &FileScope{Relative: []string{"slow_a.tf"}}
	got, err := ApplyFileScope(f, spec, scope)
	if err != nil {
		t.Fatalf("ApplyFileScope: %v", err)
	}
	if !reflect.DeepEqual(got.WriteSet, []string{"time_sleep.slow_a"}) {
		t.Fatalf("write_set = %v", got.WriteSet)
	}
	if !reflect.DeepEqual(got.ReadSet, []string{"time_sleep.slow_a"}) {
		t.Fatalf("read_set = %v", got.ReadSet)
	}
	if len(got.Reservations) != 1 || got.Reservations[0].Address != "time_sleep.slow_a" || got.Reservations[0].Mode != "write" {
		t.Fatalf("reservations = %+v", got.Reservations)
	}
	if !reflect.DeepEqual(got.ScopedFiles, []string{"slow_a.tf"}) {
		t.Fatalf("scoped_files = %v", got.ScopedFiles)
	}
}

func TestApplyFileScope_FallbackToFileParseWhenRangeMissing(t *testing.T) {
	cfgDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cfgDir, "slow_a.tf"), []byte(`resource "time_sleep" "slow_a" {}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "slow_b.tf"), []byte(`resource "time_sleep" "slow_b" {}`), 0o644); err != nil {
		t.Fatal(err)
	}
	f := &File{
		Configuration: Configuration{
			RootModule: ConfigModule{
				Resources: []ConfigResource{
					{Address: "time_sleep.slow_a"},
					{Address: "time_sleep.slow_b"},
				},
			},
		},
	}
	spec := &PlanSpec{
		ConfigDir: cfgDir,
		WriteSet:  []string{"time_sleep.slow_a", "time_sleep.slow_b"},
	}
	got, err := ApplyFileScope(f, spec, &FileScope{Relative: []string{"slow_a.tf"}})
	if err != nil {
		t.Fatalf("ApplyFileScope: %v", err)
	}
	if !reflect.DeepEqual(got.WriteSet, []string{"time_sleep.slow_a"}) {
		t.Fatalf("write_set = %v", got.WriteSet)
	}
}

func TestApplyFileScope_ModuleFileKeepsModuleWrites(t *testing.T) {
	cfgDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cfgDir, "modules", "db"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "db.tf"), []byte(`
module "db" {
  source = "./modules/db"
}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	f := &File{
		Configuration: Configuration{
			RootModule: ConfigModule{
				ModuleCalls: map[string]ModuleCall{
					"db": {Module: ConfigModule{}},
				},
			},
		},
	}
	spec := &PlanSpec{
		ConfigDir: cfgDir,
		WriteSet:  []string{"module.db.random_id.leader", "time_sleep.slow_a"},
	}
	got, err := ApplyFileScope(f, spec, &FileScope{Relative: []string{"db.tf"}})
	if err != nil {
		t.Fatalf("ApplyFileScope: %v", err)
	}
	if !reflect.DeepEqual(got.WriteSet, []string{"module.db.random_id.leader"}) {
		t.Fatalf("write_set = %v", got.WriteSet)
	}
}

func TestApplyFileScope_ModuleRemoveIsAttributedByOwnershipCache(t *testing.T) {
	cfgDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cfgDir, "modules", "db"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "db.tf"), []byte(`
module "db" {
  source = "./modules/db"
}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	// Seed ownership: module call existed in db.tf.
	initial := &File{
		Configuration: Configuration{
			RootModule: ConfigModule{
				ModuleCalls: map[string]ModuleCall{
					"db": {
						Module:    ConfigModule{},
						DeclRange: &SourceRange{Filename: filepath.Join(cfgDir, "db.tf")},
					},
				},
			},
		},
	}
	if err := UpdateOwnershipCache(cfgDir, initial); err != nil {
		t.Fatalf("UpdateOwnershipCache(initial): %v", err)
	}

	// Now pretend db module call was removed from config, but terraform planned
	// a delete under module.db.*. We should still keep those deletes when
	// scoping to db.tf, via cached ownership.
	f := &File{
		ResourceChanges: []ResourceChange{
			{Address: "module.db.random_id.leader", Change: Change{Actions: []string{"delete"}}},
		},
		Configuration: Configuration{
			RootModule: ConfigModule{
				ModuleCalls: map[string]ModuleCall{},
			},
		},
	}
	spec := &PlanSpec{
		ConfigDir: cfgDir,
		WriteSet:  []string{"module.db.random_id.leader"},
		ReadSet:   []string{"module.db.random_id.leader"},
	}
	got, err := ApplyFileScope(f, spec, &FileScope{Relative: []string{"db.tf"}})
	if err != nil {
		t.Fatalf("ApplyFileScope: %v", err)
	}
	if !reflect.DeepEqual(got.WriteSet, []string{"module.db.random_id.leader"}) {
		t.Fatalf("write_set = %v", got.WriteSet)
	}
}

func TestApplyFileScope_RootRemoveIsAttributedByOwnershipCache(t *testing.T) {
	cfgDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cfgDir, "slow_a.tf"), []byte(`
resource "time_sleep" "deleted_demo" {}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	initial := &File{
		Configuration: Configuration{
			RootModule: ConfigModule{
				Resources: []ConfigResource{
					{
						Address:   "time_sleep.deleted_demo",
						DeclRange: &SourceRange{Filename: filepath.Join(cfgDir, "slow_a.tf")},
					},
				},
			},
		},
	}
	if err := UpdateOwnershipCache(cfgDir, initial); err != nil {
		t.Fatalf("UpdateOwnershipCache(initial): %v", err)
	}

	f := &File{
		ResourceChanges: []ResourceChange{
			{Address: "time_sleep.deleted_demo", Change: Change{Actions: []string{"delete"}}},
		},
		Configuration: Configuration{
			RootModule: ConfigModule{
				Resources: nil,
			},
		},
	}
	spec := &PlanSpec{
		ConfigDir: cfgDir,
		WriteSet:  []string{"time_sleep.deleted_demo"},
		ReadSet:   []string{"time_sleep.deleted_demo"},
	}
	got, err := ApplyFileScope(f, spec, &FileScope{Relative: []string{"slow_a.tf"}})
	if err != nil {
		t.Fatalf("ApplyFileScope: %v", err)
	}
	if !reflect.DeepEqual(got.WriteSet, []string{"time_sleep.deleted_demo"}) {
		t.Fatalf("write_set = %v", got.WriteSet)
	}
}

func TestApplyFileScope_FailsClosedOnDeleteWithoutOwnership(t *testing.T) {
	cfgDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cfgDir, "slow_a.tf"), []byte(`
resource "time_sleep" "keep" {}
`), 0o644); err != nil {
		t.Fatal(err)
	}

	f := &File{
		ResourceChanges: []ResourceChange{
			{Address: "time_sleep.keep", Change: Change{Actions: []string{"update"}}},
			{Address: "time_sleep.deleted", Change: Change{Actions: []string{"delete"}}},
		},
		Configuration: Configuration{
			RootModule: ConfigModule{
				Resources: []ConfigResource{
					{
						Address:   "time_sleep.keep",
						DeclRange: &SourceRange{Filename: cfgDir + "/slow_a.tf"},
					},
				},
			},
		},
	}
	spec := &PlanSpec{
		ConfigDir: cfgDir,
		WriteSet:  []string{"time_sleep.keep", "time_sleep.deleted"},
		ReadSet:   []string{"time_sleep.keep", "time_sleep.deleted"},
	}
	_, err := ApplyFileScope(f, spec, &FileScope{Relative: []string{"slow_a.tf"}})
	if err == nil {
		t.Fatal("expected error for delete with unknown ownership")
	}
	if !strings.Contains(err.Error(), "delete/forget") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestApplyFileScope_AllowsDeleteWhenRemovedBlockSelected(t *testing.T) {
	cfgDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cfgDir, "slow_a.tf"), []byte(`
resource "time_sleep" "keep" {}
removed {
  from = time_sleep.deleted
}
`), 0o644); err != nil {
		t.Fatal(err)
	}

	f := &File{
		ResourceChanges: []ResourceChange{
			{Address: "time_sleep.keep", Change: Change{Actions: []string{"update"}}},
			{Address: "time_sleep.deleted", Change: Change{Actions: []string{"delete"}}},
		},
		Configuration: Configuration{
			RootModule: ConfigModule{
				Resources: []ConfigResource{
					{
						Address:   "time_sleep.keep",
						DeclRange: &SourceRange{Filename: cfgDir + "/slow_a.tf"},
					},
				},
			},
		},
	}
	spec := &PlanSpec{
		ConfigDir: cfgDir,
		WriteSet:  []string{"time_sleep.keep", "time_sleep.deleted"},
		ReadSet:   []string{"time_sleep.keep", "time_sleep.deleted"},
	}
	got, err := ApplyFileScope(f, spec, &FileScope{Relative: []string{"slow_a.tf"}})
	if err != nil {
		t.Fatalf("ApplyFileScope: %v", err)
	}
	if !reflect.DeepEqual(got.WriteSet, []string{"time_sleep.deleted", "time_sleep.keep"}) {
		t.Fatalf("write_set = %v", got.WriteSet)
	}
}

func TestApplyFileScope_AllowsModuleDeleteWhenRemovedBlockSelected(t *testing.T) {
	cfgDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cfgDir, "db.tf"), []byte(`
removed {
  from = module.db
}
`), 0o644); err != nil {
		t.Fatal(err)
	}

	f := &File{
		ResourceChanges: []ResourceChange{
			{Address: "module.db.random_id.leader", Change: Change{Actions: []string{"delete"}}},
		},
		Configuration: Configuration{
			RootModule: ConfigModule{
				ModuleCalls: map[string]ModuleCall{},
			},
		},
	}
	spec := &PlanSpec{
		ConfigDir: cfgDir,
		WriteSet:  []string{"module.db.random_id.leader"},
		ReadSet:   []string{"module.db.random_id.leader"},
	}
	got, err := ApplyFileScope(f, spec, &FileScope{Relative: []string{"db.tf"}})
	if err != nil {
		t.Fatalf("ApplyFileScope: %v", err)
	}
	if !reflect.DeepEqual(got.WriteSet, []string{"module.db.random_id.leader"}) {
		t.Fatalf("write_set = %v", got.WriteSet)
	}
}
