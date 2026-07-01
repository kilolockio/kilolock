package main

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/kilolockio/kilolock/internal/configscope"
	"github.com/kilolockio/kilolock/internal/tfstate"
	"github.com/kilolockio/kilolock/pkg/config"
	"github.com/kilolockio/kilolock/pkg/store"
)

func TestMarshalStateEngineSliceToTerraformState_GroupsAndSortsInstances(t *testing.T) {
	raw, err := marshalStateEngineSliceToTerraformState("lineage-1", 7, []store.StateEngineResource{
		{
			ResourceSnapshot: store.ResourceSnapshot{
				Address:        `module.net.aws_subnet.private["blue"]`,
				Mode:           "managed",
				Type:           "aws_subnet",
				Name:           "private",
				Provider:       `provider["registry.terraform.io/hashicorp/aws"]`,
				ModulePath:     "module.net",
				IndexKind:      "string",
				IndexValue:     "blue",
				Attributes:     json.RawMessage(`{"id":"subnet-blue"}`),
				SensitivePaths: json.RawMessage(`[]`),
			},
			Dependencies: []string{"aws_vpc.main"},
		},
		{
			ResourceSnapshot: store.ResourceSnapshot{
				Address:        `module.net.aws_subnet.private["green"]`,
				Mode:           "managed",
				Type:           "aws_subnet",
				Name:           "private",
				Provider:       `provider["registry.terraform.io/hashicorp/aws"]`,
				ModulePath:     "module.net",
				IndexKind:      "string",
				IndexValue:     "green",
				Attributes:     json.RawMessage(`{"id":"subnet-green"}`),
				SensitivePaths: json.RawMessage(`[]`),
			},
			Dependencies: []string{"aws_vpc.main"},
		},
		{
			ResourceSnapshot: store.ResourceSnapshot{
				Address:        "aws_vpc.main",
				Mode:           "managed",
				Type:           "aws_vpc",
				Name:           "main",
				Provider:       `provider["registry.terraform.io/hashicorp/aws"]`,
				Attributes:     json.RawMessage(`{"id":"vpc-1"}`),
				SensitivePaths: json.RawMessage(`[]`),
			},
		},
	})
	if err != nil {
		t.Fatalf("marshalStateEngineSliceToTerraformState: %v", err)
	}
	state, err := tfstate.Parse(raw)
	if err != nil {
		t.Fatalf("tfstate.Parse: %v", err)
	}
	if state.Serial != 7 {
		t.Fatalf("serial=%d want 7", state.Serial)
	}
	if state.Lineage != "lineage-1" {
		t.Fatalf("lineage=%q want lineage-1", state.Lineage)
	}
	if got := len(state.Resources); got != 2 {
		t.Fatalf("resources=%d want 2", got)
	}
	if state.Resources[0].Type != "aws_vpc" {
		t.Fatalf("first resource type=%q want aws_vpc", state.Resources[0].Type)
	}
	if state.Resources[1].Type != "aws_subnet" {
		t.Fatalf("second resource type=%q want aws_subnet", state.Resources[1].Type)
	}
	if got := len(state.Resources[1].Instances); got != 2 {
		t.Fatalf("subnet instances=%d want 2", got)
	}
	_, firstIndex, err := state.Resources[1].Instances[0].DecodeIndex()
	if err != nil {
		t.Fatalf("decode first index: %v", err)
	}
	_, secondIndex, err := state.Resources[1].Instances[1].DecodeIndex()
	if err != nil {
		t.Fatalf("decode second index: %v", err)
	}
	if firstIndex != "blue" || secondIndex != "green" {
		t.Fatalf("instance ordering = [%q %q], want [blue green]", firstIndex, secondIndex)
	}
}

func TestResolvedCLIProtocol(t *testing.T) {
	tests := map[string]string{
		"":               cliProtocolTerraformHTTP,
		"http":           cliProtocolTerraformHTTP,
		"terraform-http": cliProtocolTerraformHTTP,
		"state-engine":   cliProtocolStateEngine,
		"STATE-ENGINE":   cliProtocolStateEngine,
	}
	for in, want := range tests {
		got := resolvedCLIProtocol(config.Config{Protocol: in})
		if got != want {
			t.Fatalf("protocol %q -> %q, want %q", in, got, want)
		}
	}
}

func TestShouldFallbackStateEngineScoped(t *testing.T) {
	t.Run("unsafe scope does not fallback", func(t *testing.T) {
		err := &stateEngineScopeSafetyError{
			UnknownMissing: []string{"aws_subnet.unknown"},
			Confidence:     "unsafe",
		}
		fallback, why := shouldFallbackStateEngineScoped(err)
		if fallback {
			t.Fatal("expected unsafe scope to fail closed")
		}
		if why == "" {
			t.Fatal("expected explanatory reason")
		}
		if !errors.Is(err, errStateEngineUnsafeScope) {
			t.Fatal("expected unsafe scope sentinel")
		}
	})

	t.Run("transport-ish error falls back", func(t *testing.T) {
		fallback, why := shouldFallbackStateEngineScoped(errors.New("404 not found"))
		if !fallback {
			t.Fatal("expected generic native-path failure to fallback")
		}
		if why == "" {
			t.Fatal("expected explanatory reason")
		}
	})
}

func TestStateEnginePlanModeForScopedResult(t *testing.T) {
	t.Run("native slice", func(t *testing.T) {
		got := stateEnginePlanModeForScopedResult(&stateEngineScopedSliceResult{
			DiscoveryEngine: "opentofu",
			Notes:           []string{"required config-only node preserved from support file"},
		})
		if got != "native-slice" {
			t.Fatalf("mode = %q, want native-slice", got)
		}
	})

	t.Run("native slice with discovery fallback", func(t *testing.T) {
		got := stateEnginePlanModeForScopedResult(&stateEngineScopedSliceResult{
			DiscoveryEngine: "heuristic",
			Notes:           []string{"config discovery fell back from opentofu to heuristic: parse failed"},
		})
		if got != "native-slice-with-discovery-fallback" {
			t.Fatalf("mode = %q, want native-slice-with-discovery-fallback", got)
		}
	})
}

func TestIntentHasNativeScope(t *testing.T) {
	tests := []struct {
		name   string
		intent *configscope.Intent
		want   bool
	}{
		{name: "nil", intent: nil, want: false},
		{name: "empty", intent: &configscope.Intent{}, want: false},
		{name: "planning targets", intent: &configscope.Intent{PlanningTargets: []string{"time_sleep.slow_a"}}, want: true},
		{name: "selectors only", intent: &configscope.Intent{Selectors: []configscope.Selector{{Kind: "resource_address", Value: "time_sleep.slow_a"}}}, want: true},
		{name: "write candidates only", intent: &configscope.Intent{ExplicitWriteCandidates: []string{"time_sleep.slow_a"}}, want: true},
		{name: "read candidates only", intent: &configscope.Intent{ExplicitReadCandidates: []string{"aws_vpc.main"}}, want: true},
		{name: "removed candidates only", intent: &configscope.Intent{RemovedConfigCandidates: []string{"time_sleep.deleted"}}, want: true},
	}
	for _, tt := range tests {
		if got := intentHasNativeScope(tt.intent); got != tt.want {
			t.Fatalf("%s: got %v want %v", tt.name, got, tt.want)
		}
	}
}
