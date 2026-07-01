package store

import (
	"testing"

	"github.com/kilolockio/kilolock/internal/tfstate"
)

func TestParseInstanceAddress_RoundTripsModuleAndIndex(t *testing.T) {
	resource, instance, err := tfstate.ParseInstanceAddress(`module.edge.module.db.aws_instance.web["blue"]`)
	if err != nil {
		t.Fatalf("ParseInstanceAddress: %v", err)
	}
	got, err := tfstate.InstanceAddress(resource, instance)
	if err != nil {
		t.Fatalf("InstanceAddress: %v", err)
	}
	if got != `module.edge.module.db.aws_instance.web["blue"]` {
		t.Fatalf("round-trip=%q", got)
	}
}

func TestParseInstanceAddress_IntIndex(t *testing.T) {
	resource, instance, err := tfstate.ParseInstanceAddress(`data.aws_ami.ubuntu[2]`)
	if err != nil {
		t.Fatalf("ParseInstanceAddress: %v", err)
	}
	if resource.Mode != "data" || resource.Type != "aws_ami" || resource.Name != "ubuntu" {
		t.Fatalf("unexpected resource: %+v", resource)
	}
	got, err := tfstate.InstanceAddress(resource, instance)
	if err != nil {
		t.Fatalf("InstanceAddress: %v", err)
	}
	if got != `data.aws_ami.ubuntu[2]` {
		t.Fatalf("round-trip=%q", got)
	}
}

func TestPatchMoveStateResource_MovesAddressAndRewritesDependents(t *testing.T) {
	state := mustState(t, map[string]string{
		"aws_instance.web": "i-web",
		"aws_instance.db":  "i-db",
	})
	dbLoc, err := findResourceInstance(state, "aws_instance.db")
	if err != nil {
		t.Fatalf("find db: %v", err)
	}
	dbLoc.Instance.Dependencies = []string{"aws_instance.web"}
	resource := state.Resources[dbLoc.ResourceIndex]
	resource.Instances[dbLoc.InstanceIndex] = dbLoc.Instance
	state.Resources[dbLoc.ResourceIndex] = resource

	webLoc, err := findResourceInstance(state, "aws_instance.web")
	if err != nil {
		t.Fatalf("find web: %v", err)
	}
	got, err := patchMoveStateResource(state, webLoc, "module.edge.aws_instance.web")
	if err != nil {
		t.Fatalf("patchMoveStateResource: %v", err)
	}
	if loc, err := findResourceInstance(got, "aws_instance.web"); err != nil {
		t.Fatalf("find old address: %v", err)
	} else if loc != nil {
		t.Fatalf("old address still present")
	}
	assertStateHasID(t, got, "module.edge.aws_instance.web", "i-web")
	dbLoc, err = findResourceInstance(got, "aws_instance.db")
	if err != nil {
		t.Fatalf("find db after move: %v", err)
	}
	if len(dbLoc.Instance.Dependencies) != 1 || dbLoc.Instance.Dependencies[0] != "module.edge.aws_instance.web" {
		t.Fatalf("dependencies=%v", dbLoc.Instance.Dependencies)
	}
}

func TestPatchMoveStateResource_RejectsExistingDestination(t *testing.T) {
	state := mustState(t, map[string]string{
		"aws_instance.web":             "i-web",
		"module.edge.aws_instance.web": "i-other",
	})
	webLoc, err := findResourceInstance(state, "aws_instance.web")
	if err != nil {
		t.Fatalf("find web: %v", err)
	}
	if _, err := patchMoveStateResource(state, webLoc, "module.edge.aws_instance.web"); err == nil {
		t.Fatalf("expected destination conflict error")
	}
}

func TestRewriteDependencies_NoPanicOnEmpty(t *testing.T) {
	state := mustState(t, map[string]string{"aws_instance.web": "i-web"})
	rewriteDependencies(state, "", "aws_instance.other")
}
