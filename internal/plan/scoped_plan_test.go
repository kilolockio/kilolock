package plan

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kilolockio/kilolock/internal/slice"
)

func TestSelectedRootResourceAddresses(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "slow_a.tf"), []byte(`
resource "time_sleep" "slow_a" {}
resource "null_resource" "marker" {}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := SelectedRootResourceAddresses(dir, []string{"slow_a.tf"})
	if err != nil {
		t.Fatalf("SelectedRootResourceAddresses: %v", err)
	}
	want := []string{"null_resource.marker", "time_sleep.slow_a"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestSelectedRootResourceAddresses_ModuleBlockBecomesModuleTarget(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "mod.tf"), []byte(`
module "db" {
  source = "./modules/db"
}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := SelectedRootResourceAddresses(dir, []string{"mod.tf"})
	if err != nil {
		t.Fatalf("unexpected error for module target: %v", err)
	}
	if len(got) != 1 || got[0] != "module.db" {
		t.Fatalf("got %v, want [module.db]", got)
	}
}

func TestAnalyzeSelectedFiles_CollectsLocalModuleSource(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "mod.tf"), []byte(`
module "db" {
  source = "./modules/db"
}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	selected, err := AnalyzeSelectedFiles(dir, []string{"mod.tf"})
	if err != nil {
		t.Fatalf("AnalyzeSelectedFiles: %v", err)
	}
	if len(selected.ModulePrefixes) != 1 || selected.ModulePrefixes[0] != "module.db" {
		t.Fatalf("module prefixes = %v", selected.ModulePrefixes)
	}
	if len(selected.LocalModulePaths) != 1 || !strings.HasSuffix(selected.LocalModulePaths[0], filepath.Join("modules", "db")) {
		t.Fatalf("module paths = %v", selected.LocalModulePaths)
	}
}

func TestAnalyzeSelectedFiles_CollectsDependencyAddressesForSlice(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "slow_a.tf"), []byte(`
module "small_herd" {
  source = "./modules/herd"
  prefix = "${random_pet.deployment_name.id}-small"
}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	selected, err := AnalyzeSelectedFiles(dir, []string{"slow_a.tf"})
	if err != nil {
		t.Fatalf("AnalyzeSelectedFiles: %v", err)
	}
	want := map[string]struct{}{
		"module.small_herd":          {},
		"random_pet.deployment_name": {},
	}
	for _, got := range selected.SliceAddresses {
		delete(want, got)
	}
	if len(want) != 0 {
		t.Fatalf("missing expected slice addresses: %+v (got=%v)", want, selected.SliceAddresses)
	}
}

func TestAnalyzeSelectedFiles_RemovedBlockAddsTargetAndSliceAddr(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "del.tf"), []byte(`
removed {
  from = time_sleep.slow_a
}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	selected, err := AnalyzeSelectedFiles(dir, []string{"del.tf"})
	if err != nil {
		t.Fatalf("AnalyzeSelectedFiles: %v", err)
	}
	if len(selected.Targets) != 1 || selected.Targets[0] != "time_sleep.slow_a" {
		t.Fatalf("targets = %v", selected.Targets)
	}
	if _, ok := selected.RemovedResources["time_sleep.slow_a"]; !ok {
		t.Fatalf("removed resources missing time_sleep.slow_a: %v", selected.RemovedResources)
	}
	found := false
	for _, a := range selected.SliceAddresses {
		if a == "time_sleep.slow_a" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("slice_addresses missing time_sleep.slow_a: %v", selected.SliceAddresses)
	}
}

func TestAnalyzeSelectedFiles_UsesOwnershipCacheForDeletedResources(t *testing.T) {
	dir := t.TempDir()
	// Initial config: resource exists in the file.
	if err := os.WriteFile(filepath.Join(dir, "slow_a.tf"), []byte(`
resource "time_sleep" "keep" {}
resource "time_sleep" "deleted" {}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	initial := &File{
		Configuration: Configuration{
			RootModule: ConfigModule{
				Resources: []ConfigResource{
					{Address: "time_sleep.keep", DeclRange: &SourceRange{Filename: filepath.Join(dir, "slow_a.tf")}},
					{Address: "time_sleep.deleted", DeclRange: &SourceRange{Filename: filepath.Join(dir, "slow_a.tf")}},
				},
			},
		},
	}
	if err := UpdateOwnershipCache(dir, initial); err != nil {
		t.Fatalf("UpdateOwnershipCache(initial): %v", err)
	}

	// Now delete the resource block from the file.
	if err := os.WriteFile(filepath.Join(dir, "slow_a.tf"), []byte(`
resource "time_sleep" "keep" {}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	afterDelete := &File{
		Configuration: Configuration{
			RootModule: ConfigModule{
				Resources: []ConfigResource{
					{Address: "time_sleep.keep", DeclRange: &SourceRange{Filename: filepath.Join(dir, "slow_a.tf")}},
				},
			},
		},
	}
	// Cache merge should keep the deleted address.
	if err := UpdateOwnershipCache(dir, afterDelete); err != nil {
		t.Fatalf("UpdateOwnershipCache(afterDelete): %v", err)
	}

	selected, err := AnalyzeSelectedFiles(dir, []string{"slow_a.tf"})
	if err != nil {
		t.Fatalf("AnalyzeSelectedFiles: %v", err)
	}
	if len(selected.Targets) != 1 || selected.Targets[0] != "time_sleep.keep" {
		t.Fatalf("targets = %v", selected.Targets)
	}
	if _, ok := selected.RootResources["time_sleep.deleted"]; !ok {
		t.Fatalf("expected RootResources to include deleted address")
	}
	found := false
	for _, a := range selected.SliceAddresses {
		if a == "time_sleep.deleted" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("slice_addresses missing time_sleep.deleted: %v", selected.SliceAddresses)
	}
}

func TestAnalyzeSelectedFiles_MovedBlockAddsFromAndToTargets(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "move.tf"), []byte(`
moved {
  from = time_sleep.old
  to   = time_sleep.new
}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	selected, err := AnalyzeSelectedFiles(dir, []string{"move.tf"})
	if err != nil {
		t.Fatalf("AnalyzeSelectedFiles: %v", err)
	}
	want := map[string]struct{}{
		"time_sleep.old": {},
		"time_sleep.new": {},
	}
	for _, a := range selected.Targets {
		delete(want, a)
	}
	if len(want) != 0 {
		t.Fatalf("missing targets: %v (got=%v)", want, selected.Targets)
	}
}

func TestExpandTargetSliceAddresses_ModuleIncludesReferencedRootResource(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(`
resource "random_pet" "deployment_name" {}

module "small_herd" {
  source = "./modules/herd"
  prefix = "${random_pet.deployment_name.id}-small"
}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ExpandTargetSliceAddresses(dir, []string{"module.small_herd"})
	if err != nil {
		t.Fatalf("ExpandTargetSliceAddresses: %v", err)
	}
	want := map[string]struct{}{
		"module.small_herd":          {},
		"random_pet.deployment_name": {},
	}
	for _, a := range got {
		delete(want, a)
	}
	if len(want) != 0 {
		t.Fatalf("missing expected addresses: %v (got=%v)", want, got)
	}
}

func TestExpandTargetSliceAddresses_IncludesDataReferences(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(`
data "random_pet" "seed" {}
resource "null_resource" "x" {
  triggers = {
    v = data.random_pet.seed.id
  }
}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ExpandTargetSliceAddresses(dir, []string{"null_resource.x"})
	if err != nil {
		t.Fatalf("ExpandTargetSliceAddresses: %v", err)
	}
	want := map[string]struct{}{
		"null_resource.x":      {},
		"data.random_pet.seed": {},
	}
	for _, a := range got {
		delete(want, a)
	}
	if len(want) != 0 {
		t.Fatalf("missing expected addresses: %v (got=%v)", want, got)
	}
}

func TestExpandTargetSliceAddresses_IncludesLocalTransitiveRefs(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(`
resource "random_pet" "seed" {}
locals {
  suffix = random_pet.seed.id
}
resource "null_resource" "x" {
  triggers = {
    v = local.suffix
  }
}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ExpandTargetSliceAddresses(dir, []string{"null_resource.x"})
	if err != nil {
		t.Fatalf("ExpandTargetSliceAddresses: %v", err)
	}
	want := map[string]struct{}{
		"null_resource.x": {},
		"random_pet.seed": {},
	}
	for _, a := range got {
		delete(want, a)
	}
	if len(want) != 0 {
		t.Fatalf("missing expected addresses: %v (got=%v)", want, got)
	}
}

func TestExpandTargetSliceAddresses_IncludesCrossFileDataRefs(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "data.tf"), []byte(`
data "random_pet" "seed" {}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(`
resource "null_resource" "x" {
  triggers = {
    v = data.random_pet.seed.id
  }
}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ExpandTargetSliceAddresses(dir, []string{"null_resource.x"})
	if err != nil {
		t.Fatalf("ExpandTargetSliceAddresses: %v", err)
	}
	want := map[string]struct{}{
		"null_resource.x":      {},
		"data.random_pet.seed": {},
	}
	for _, a := range got {
		delete(want, a)
	}
	if len(want) != 0 {
		t.Fatalf("missing expected addresses: %v (got=%v)", want, got)
	}
}

func TestExpandTargetSliceAddresses_IncludesCrossFileLocalRefs(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "locals.tf"), []byte(`
locals {
  suffix = random_pet.seed.id
}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "deps.tf"), []byte(`
resource "random_pet" "seed" {}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(`
resource "null_resource" "x" {
  triggers = {
    v = local.suffix
  }
}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ExpandTargetSliceAddresses(dir, []string{"null_resource.x"})
	if err != nil {
		t.Fatalf("ExpandTargetSliceAddresses: %v", err)
	}
	want := map[string]struct{}{
		"null_resource.x": {},
		"random_pet.seed": {},
	}
	for _, a := range got {
		delete(want, a)
	}
	if len(want) != 0 {
		t.Fatalf("missing expected addresses: %v (got=%v)", want, got)
	}
}

func TestExpandScopedSliceAddresses_IncludesTransitiveLocalDataDeps(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "locals.tf"), []byte(`
locals {
  seed = data.random_pet.seed.id
}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "data.tf"), []byte(`
data "random_pet" "seed" {}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "slow_a.tf"), []byte(`
module "small_herd" {
  source = "./modules/herd"
  prefix = local.seed
}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	selected, err := AnalyzeSelectedFiles(dir, []string{"slow_a.tf"})
	if err != nil {
		t.Fatalf("AnalyzeSelectedFiles: %v", err)
	}
	got, err := expandScopedSliceAddresses(dir, selected)
	if err != nil {
		t.Fatalf("expandScopedSliceAddresses: %v", err)
	}
	want := map[string]struct{}{
		"module.small_herd":    {},
		"data.random_pet.seed": {},
	}
	for _, a := range got {
		delete(want, a)
	}
	if len(want) != 0 {
		t.Fatalf("missing expected addresses: %v (got=%v)", want, got)
	}
}

func TestExpandScopedSliceAddresses_NilSelectedFails(t *testing.T) {
	_, err := expandScopedSliceAddresses(t.TempDir(), nil)
	if err == nil {
		t.Fatal("expected error for nil selected scope")
	}
}

func TestFilterSupportBlocks_KeepsResourcesDataAndStripsOutputs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.tf")
	if err := os.WriteFile(path, []byte(`
terraform {
  required_version = ">= 1.5.0"
}

variable "slow_a_version" {
  type = string
}

locals {
  slow_duration = "30s"
}

resource "time_sleep" "slow_a" {
  create_duration = local.slow_duration
}

data "time_static" "now" {}

module "herd" {
  source = "./modules/herd"
}

output "slow_a" {
  value = time_sleep.slow_a.id
}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := filterSupportBlocks(path, nil)
	if err != nil {
		t.Fatalf("filterSupportBlocks: %v", err)
	}
	s := string(got)
	for _, want := range []string{
		"terraform {",
		`variable "slow_a_version"`,
		"locals {",
		`resource "time_sleep" "slow_a"`,
		`data "time_static" "now"`,
		`module "herd"`,
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("filtered support missing %q:\n%s", want, s)
		}
	}
	for _, forbidden := range []string{`output "slow_a"`} {
		if strings.Contains(s, forbidden) {
			t.Fatalf("filtered support unexpectedly kept %q:\n%s", forbidden, s)
		}
	}
}

func TestBuildScopedStateSlice_KeepsModulePrefixResources(t *testing.T) {
	trunk := &slice.TrunkState{
		Version: 4,
		Resources: []slice.TrunkResource{
			{Module: "module.db", Mode: "managed", Type: "random_id", Name: "leader"},
			{Module: "module.db", Mode: "managed", Type: "random_string", Name: "label"},
			{Mode: "managed", Type: "time_sleep", Name: "slow_a"},
		},
	}
	got := buildScopedStateSlice(trunk, []string{"module.db"})
	if len(got.Resources) != 2 {
		t.Fatalf("kept %d resources, want 2", len(got.Resources))
	}
	for _, r := range got.Resources {
		if !strings.HasPrefix(r.Address(), "module.db.") {
			t.Fatalf("unexpected resource kept: %s", r.Address())
		}
	}
}

func TestBuildScopedWorkspace_ReusesProjectTerraformDir(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "slow_a.tf"), []byte(`resource "time_sleep" "slow_a" {}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(src, ".terraform", "modules"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, ".terraform", "modules", "modules.json"), []byte(`{"Modules":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	scope := &FileScope{Relative: []string{"slow_a.tf"}}
	analyzed := &SelectedScope{}
	if err := buildScopedWorkspace(src, dst, scope, analyzed, nil); err != nil {
		t.Fatalf("buildScopedWorkspace: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, ".terraform", "modules", "modules.json")); err != nil {
		t.Fatalf("expected copied .terraform cache, stat failed: %v", err)
	}
}

func TestBuildScopedWorkspace_CopiesLocalModulesFromSupportFiles(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "slow_a.tf"), []byte(`resource "time_sleep" "slow_a" {}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "main.tf"), []byte(`
module "primary_herd" {
  source = "./modules/herd"
}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(src, "modules", "herd"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "modules", "herd", "main.tf"), []byte(`resource "random_id" "leader" { byte_length = 4 }`), 0o644); err != nil {
		t.Fatal(err)
	}
	scope := &FileScope{Relative: []string{"slow_a.tf"}}
	analyzed := &SelectedScope{} // selected file itself has no module blocks
	if err := buildScopedWorkspace(src, dst, scope, analyzed, nil); err != nil {
		t.Fatalf("buildScopedWorkspace: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "modules", "herd", "main.tf")); err != nil {
		t.Fatalf("expected copied local module from support file, stat failed: %v", err)
	}
}

func TestBuildScopedWorkspace_KeepsConfigRequiredNodesFromSupportFiles(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "slow_a.tf"), []byte(`resource "time_sleep" "slow_a" {}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "network.tf"), []byte(`
resource "aws_vpc" "main" {
  cidr_block = "10.0.0.0/16"
}

resource "aws_subnet" "future" {
  vpc_id = aws_vpc.main.id
}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	scope := &FileScope{Relative: []string{"slow_a.tf"}}
	analyzed := &SelectedScope{}
	if err := buildScopedWorkspace(src, dst, scope, analyzed, []string{"aws_subnet.future"}); err != nil {
		t.Fatalf("buildScopedWorkspace: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dst, "network.tf"))
	if err != nil {
		t.Fatalf("read kept support file: %v", err)
	}
	got := string(b)
	if !strings.Contains(got, `resource "aws_subnet" "future"`) {
		t.Fatalf("support file missing required config node:\n%s", got)
	}
	if !strings.Contains(got, `resource "aws_vpc" "main"`) {
		t.Fatalf("support file should still keep normal support blocks:\n%s", got)
	}
}

func TestBuildTargetWorkspace_CopiesLocalModules(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "modules", "herd"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "main.tf"), []byte(`
module "small_herd" {
  source = "./modules/herd"
}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "modules", "herd", "main.tf"), []byte(`resource "random_id" "leader" { byte_length = 4 }`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(src, ".terraform", "modules"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, ".terraform", "modules", "modules.json"), []byte(`{"Modules":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := buildTargetWorkspace(src, dst); err != nil {
		t.Fatalf("buildTargetWorkspace: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "modules", "herd", "main.tf")); err != nil {
		t.Fatalf("expected copied local module, stat failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, ".terraform", "modules", "modules.json")); err != nil {
		t.Fatalf("expected copied .terraform modules metadata, stat failed: %v", err)
	}
}

func TestBuildTargetWorkspace_StripsBackendBlocksFromAnyTFFile(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "main.tf"), []byte(`
terraform {
  required_version = ">= 1.5.0"
  backend "s3" {
    bucket = "x"
  }
}
resource "null_resource" "x" {}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := buildTargetWorkspace(src, dst); err != nil {
		t.Fatalf("buildTargetWorkspace: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dst, "main.tf"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	if strings.Contains(got, `backend "s3"`) {
		t.Fatalf("backend block should be stripped from copied tf file, got:\n%s", got)
	}
	if !strings.Contains(got, `required_version = ">= 1.5.0"`) {
		t.Fatalf("required_version should be preserved, got:\n%s", got)
	}
}

func TestBuildScopedWorkspace_StripsBackendBlocksFromSelectedTF(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "slow_a.tf"), []byte(`
terraform {
  required_providers {
    random = {
      source = "hashicorp/random"
    }
  }
  backend "http" {}
}
resource "time_sleep" "slow_a" {}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	scope := &FileScope{Relative: []string{"slow_a.tf"}}
	analyzed := &SelectedScope{}
	if err := buildScopedWorkspace(src, dst, scope, analyzed, nil); err != nil {
		t.Fatalf("buildScopedWorkspace: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dst, "slow_a.tf"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	if strings.Contains(got, `backend "http"`) {
		t.Fatalf("backend block should be stripped from selected tf file, got:\n%s", got)
	}
	if !strings.Contains(got, `required_providers`) {
		t.Fatalf("required_providers should be preserved, got:\n%s", got)
	}
}
