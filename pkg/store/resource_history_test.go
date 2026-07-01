package store

import (
	"encoding/json"
	"testing"

	"github.com/kilolockio/kilolock/internal/tfstate"
)

func TestPatchStateResource_ReplacesHistoricalInstance(t *testing.T) {
	current := mustState(t, map[string]string{
		"aws_instance.web": "i-new",
		"aws_instance.db":  "i-db",
	})
	target := mustState(t, map[string]string{
		"aws_instance.web": "i-old",
	})

	currentLoc, err := findResourceInstance(current, "aws_instance.web")
	if err != nil {
		t.Fatalf("find current: %v", err)
	}
	targetLoc, err := findResourceInstance(target, "aws_instance.web")
	if err != nil {
		t.Fatalf("find target: %v", err)
	}
	got, err := patchStateResource(current, currentLoc, targetLoc)
	if err != nil {
		t.Fatalf("patchStateResource: %v", err)
	}

	assertStateHasID(t, got, "aws_instance.web", "i-old")
	assertStateHasID(t, got, "aws_instance.db", "i-db")
}

func TestPatchStateResource_RemovesCurrentAddressWhenTargetMissing(t *testing.T) {
	current := mustState(t, map[string]string{
		"aws_instance.web": "i-new",
		"aws_instance.db":  "i-db",
	})
	currentLoc, err := findResourceInstance(current, "aws_instance.web")
	if err != nil {
		t.Fatalf("find current: %v", err)
	}
	got, err := patchStateResource(current, currentLoc, nil)
	if err != nil {
		t.Fatalf("patchStateResource: %v", err)
	}
	if loc, err := findResourceInstance(got, "aws_instance.web"); err != nil {
		t.Fatalf("find removed: %v", err)
	} else if loc != nil {
		t.Fatalf("aws_instance.web still present after removal")
	}
	assertStateHasID(t, got, "aws_instance.db", "i-db")
}

func TestClassifyResourceReplay_NoOpWhenInstancesEqual(t *testing.T) {
	state := mustState(t, map[string]string{"aws_instance.web": "i-same"})
	loc, err := findResourceInstance(state, "aws_instance.web")
	if err != nil {
		t.Fatalf("find instance: %v", err)
	}
	if got := classifyResourceReplay(true, true, loc, loc); got != "no-op" {
		t.Fatalf("classifyResourceReplay = %q, want no-op", got)
	}
}

func mustState(t *testing.T, resources map[string]string) *tfstate.State {
	t.Helper()
	items := make([]any, 0, len(resources))
	for address, id := range resources {
		resource, _, err := tfstate.ParseInstanceAddress(address)
		if err != nil {
			t.Fatalf("ParseInstanceAddress(%q): %v", address, err)
		}
		items = append(items, map[string]any{
			"module":   resource.Module,
			"mode":     resource.Mode,
			"type":     resource.Type,
			"name":     resource.Name,
			"provider": `provider["registry.terraform.io/hashicorp/aws"]`,
			"instances": []any{
				map[string]any{
					"attributes":           map[string]any{"id": id},
					"sensitive_attributes": []any{},
				},
			},
		})
	}
	raw, err := json.Marshal(map[string]any{
		"version":           4,
		"terraform_version": "1.13.4",
		"serial":            1,
		"lineage":           "11111111-2222-3333-4444-555555555555",
		"outputs":           map[string]any{},
		"resources":         items,
	})
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	state, err := tfstate.Parse(raw)
	if err != nil {
		t.Fatalf("parse state: %v", err)
	}
	return state
}

func assertStateHasID(t *testing.T, st *tfstate.State, address, want string) {
	t.Helper()
	loc, err := findResourceInstance(st, address)
	if err != nil {
		t.Fatalf("find %s: %v", address, err)
	}
	if loc == nil {
		t.Fatalf("address %s not found", address)
	}
	var attrs map[string]any
	if err := json.Unmarshal(loc.Instance.Attributes, &attrs); err != nil {
		t.Fatalf("unmarshal attrs for %s: %v", address, err)
	}
	if got, _ := attrs["id"].(string); got != want {
		t.Fatalf("%s id=%q want %q", address, got, want)
	}
}
