package configscope

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/kilolockio/kilolock/internal/plan"
)

func TestDiscoverForFiles_BuildsStableIntent(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "slow_a.tf"), []byte(`
module "small_herd" {
  source = "./modules/herd"
  prefix = "${random_pet.deployment_name.id}-small"
}

resource "time_sleep" "slow_a" {}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	scope, err := plan.NormalizeFileScope(dir, []string{"slow_a.tf"})
	if err != nil {
		t.Fatalf("NormalizeFileScope: %v", err)
	}
	got, err := DiscoverForFiles(dir, scope)
	if err != nil {
		t.Fatalf("DiscoverForFiles: %v", err)
	}
	if len(got.Selectors) == 0 {
		t.Fatalf("selectors empty")
	}
	if !hasSelector(got.Selectors, "module_prefix", "module.small_herd") {
		t.Fatalf("selectors=%v missing module.small_herd", got.Selectors)
	}
	if !hasSelector(got.Selectors, "resource_address", "time_sleep.slow_a") {
		t.Fatalf("selectors=%v missing time_sleep.slow_a", got.Selectors)
	}
	if !contains(got.ExplicitWriteCandidates, "time_sleep.slow_a") {
		t.Fatalf("write candidates=%v", got.ExplicitWriteCandidates)
	}
	if !contains(got.UndeployedConfigCandidates, "random_pet.deployment_name") {
		t.Fatalf("undeployed candidates=%v", got.UndeployedConfigCandidates)
	}
	if !contains(got.ExplicitReadCandidates, "random_pet.deployment_name") {
		t.Fatalf("read candidates=%v", got.ExplicitReadCandidates)
	}
	if len(got.RemovedConfigCandidates) != 0 {
		t.Fatalf("removed candidates=%v want empty", got.RemovedConfigCandidates)
	}
}

func TestDiscoverForTargets_DedupesAndKeepsWriteCandidates(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(`
resource "null_resource" "x" {
  triggers = {
    dep = data.aws_ami.ubuntu.id
  }
}

data "aws_ami" "ubuntu" {}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := DiscoverForTargets(dir, []string{"null_resource.x", "null_resource.x"})
	if err != nil {
		t.Fatalf("DiscoverForTargets: %v", err)
	}
	if count(got.Selectors, "resource_address", "null_resource.x") != 1 {
		t.Fatalf("selectors=%v", got.Selectors)
	}
	if !contains(got.ExplicitWriteCandidates, "null_resource.x") {
		t.Fatalf("write candidates=%v", got.ExplicitWriteCandidates)
	}
	if !contains(got.ExplicitReadCandidates, "data.aws_ami.ubuntu") {
		t.Fatalf("read candidates=%v", got.ExplicitReadCandidates)
	}
	if !hasConfigNode(got.ConfigNodes, "data.aws_ami.ubuntu") {
		t.Fatalf("config nodes=%v missing data.aws_ami.ubuntu", got.ConfigNodes)
	}
}

func TestSelectedEngine_DefaultsToHeuristic(t *testing.T) {
	t.Setenv(EnvDiscoveryEngine, "")
	engine, err := selectedEngine()
	if err != nil {
		t.Fatalf("selectedEngine: %v", err)
	}
	if engine.Name() != EngineAuto {
		t.Fatalf("engine=%q want %q", engine.Name(), EngineAuto)
	}
}

func TestSelectedEngine_OpenTofuReservedButUnavailable(t *testing.T) {
	t.Setenv(EnvDiscoveryEngine, EngineOpenTofu)
	engine, err := selectedEngine()
	if err != nil {
		t.Fatalf("selectedEngine: %v", err)
	}
	if engine.Name() != EngineOpenTofu {
		t.Fatalf("engine=%q want %q", engine.Name(), EngineOpenTofu)
	}
}

func TestDiscoverForFiles_AutoFallsBackToHeuristicWhenOpenTofuParseFails(t *testing.T) {
	t.Setenv(EnvDiscoveryEngine, "")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "broken.tf"), []byte(`
resource "null_resource" "x" {
  triggers = {
    dep = local.
  }
}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	scope, err := plan.NormalizeFileScope(dir, []string{"broken.tf"})
	if err != nil {
		t.Fatalf("NormalizeFileScope: %v", err)
	}
	got, err := DiscoverForFiles(dir, scope)
	if err != nil {
		t.Fatalf("DiscoverForFiles: %v", err)
	}
	if got.DiscoveryEngine != EngineHeuristic {
		t.Fatalf("discovery engine=%q want %q", got.DiscoveryEngine, EngineHeuristic)
	}
	if !contains(got.ExplicitWriteCandidates, "null_resource.x") {
		t.Fatalf("write candidates=%v", got.ExplicitWriteCandidates)
	}
	if len(got.DiscoveryNotes) == 0 {
		t.Fatalf("expected fallback note, got none")
	}
	if got.DiscoveryNotes[0] == "" {
		t.Fatalf("fallback note is empty")
	}
}

func TestSelectedEngine_RejectsUnknownEngine(t *testing.T) {
	t.Setenv(EnvDiscoveryEngine, "mystery")
	_, err := selectedEngine()
	if !errors.Is(err, ErrUnsupportedEngine) {
		t.Fatalf("selectedEngine error=%v want ErrUnsupportedEngine", err)
	}
}

func TestOpenTofuEngine_DiscoverForTargets_UsesHCLReferences(t *testing.T) {
	t.Setenv(EnvDiscoveryEngine, EngineOpenTofu)
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
	got, err := DiscoverForTargets(dir, []string{"null_resource.x"})
	if err != nil {
		t.Fatalf("DiscoverForTargets: %v", err)
	}
	if !contains(got.ExplicitWriteCandidates, "null_resource.x") {
		t.Fatalf("write candidates=%v", got.ExplicitWriteCandidates)
	}
	if !contains(got.ExplicitReadCandidates, "random_pet.seed") {
		t.Fatalf("read candidates=%v", got.ExplicitReadCandidates)
	}
	if !contains(got.UndeployedConfigCandidates, "random_pet.seed") {
		t.Fatalf("undeployed candidates=%v", got.UndeployedConfigCandidates)
	}
	if !hasConfigNode(got.ConfigNodes, "random_pet.seed") {
		t.Fatalf("config nodes=%v missing random_pet.seed", got.ConfigNodes)
	}
}

func TestBuildIntent_IncludesConfigNodesForExplicitReadCandidates(t *testing.T) {
	intent := buildIntent(
		[]string{"null_resource.app"},
		map[string]struct{}{"null_resource.app": {}},
		nil,
		[]string{"null_resource.app", "null_resource.support"},
		map[string][]string{
			"null_resource.app":     {"null_resource.support"},
			"null_resource.support": {"random_pet.seed"},
			"random_pet.seed":       {},
		},
	)
	if !contains(intent.ExplicitReadCandidates, "null_resource.support") {
		t.Fatalf("read candidates=%v", intent.ExplicitReadCandidates)
	}
	if !hasConfigNode(intent.ConfigNodes, "null_resource.support") {
		t.Fatalf("config nodes=%v missing null_resource.support", intent.ConfigNodes)
	}
}

func TestBuildIntent_IncludesRemovedConfigCandidates(t *testing.T) {
	intent := buildIntent(
		[]string{"time_sleep.deleted"},
		map[string]struct{}{"time_sleep.deleted": {}},
		nil,
		[]string{"time_sleep.deleted"},
		map[string][]string{},
	)
	if !contains(intent.RemovedConfigCandidates, "time_sleep.deleted") {
		t.Fatalf("removed candidates=%v", intent.RemovedConfigCandidates)
	}
}

func TestBuildIntent_PrefersExplicitRemovedResourcesEvenWhenGraphStillContainsAddress(t *testing.T) {
	intent := buildIntent(
		[]string{"null_resource.state_engine_removed_demo"},
		map[string]struct{}{"null_resource.state_engine_removed_demo": {}},
		map[string]struct{}{"null_resource.state_engine_removed_demo": {}},
		[]string{"null_resource.state_engine_removed_demo"},
		map[string][]string{
			"null_resource.state_engine_removed_demo": {},
		},
	)
	if !contains(intent.RemovedConfigCandidates, "null_resource.state_engine_removed_demo") {
		t.Fatalf("removed candidates=%v", intent.RemovedConfigCandidates)
	}
}

func hasSelector(in []Selector, kind, value string) bool {
	for _, s := range in {
		if s.Kind == kind && s.Value == value {
			return true
		}
	}
	return false
}

func count(in []Selector, kind, value string) int {
	var n int
	for _, s := range in {
		if s.Kind == kind && s.Value == value {
			n++
		}
	}
	return n
}

func contains(in []string, want string) bool {
	for _, s := range in {
		if s == want {
			return true
		}
	}
	return false
}

func hasConfigNode(in []ConfigNode, want string) bool {
	for _, node := range in {
		if node.Address == want {
			return true
		}
	}
	return false
}
