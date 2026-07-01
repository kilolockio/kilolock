package apply

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kilolockio/kilolock/internal/plan"
	"github.com/kilolockio/kilolock/internal/slice"
	"github.com/kilolockio/kilolock/internal/tfstate"
)

func TestDetectAppliedWriteSet(t *testing.T) {
	trunk := makeTrunk(t, "lin", 7, map[string]string{
		"null_resource.keep":   `[{"attributes":{"v":1}}]`,
		"null_resource.update": `[{"attributes":{"v":2}}]`,
		"null_resource.delete": `[{"attributes":{"v":3}}]`,
	})
	postApply := makeTrunk(t, "lin", 7, map[string]string{
		"null_resource.keep":   `[{"attributes":{"v":1}}]`,
		"null_resource.update": `[{"attributes":{"v":22}}]`,
		"null_resource.create": `[{"attributes":{"v":4}}]`,
	})

	got, deletes, err := detectAppliedWriteSet(trunk, postApply, []string{
		"null_resource.keep",
		"null_resource.update",
		"null_resource.delete",
		"null_resource.create",
	})
	if err != nil {
		t.Fatalf("detectAppliedWriteSet: %v", err)
	}
	want := []string{
		"null_resource.create",
		"null_resource.delete",
		"null_resource.update",
	}
	if len(got) != len(want) {
		t.Fatalf("applied count=%d want %d got=%v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("applied[%d]=%s want %s (all=%v)", i, got[i], want[i], got)
		}
	}
	if len(deletes) != 1 || deletes[0] != "null_resource.delete" {
		t.Fatalf("deletes=%v want [null_resource.delete]", deletes)
	}
}

func TestBuildStateEngineDeltaCommit_SelectsExactInstanceAndDeleteSet(t *testing.T) {
	postApply := &slice.TrunkState{
		Version:          4,
		TerraformVersion: "1.13.4",
		Serial:           9,
		Lineage:          "lin",
		Outputs:          json.RawMessage(`{}`),
		Resources: []slice.TrunkResource{
			{
				Mode:     "managed",
				Type:     "null_resource",
				Name:     "many",
				Provider: `provider["registry.terraform.io/hashicorp/null"]`,
				Instances: json.RawMessage(`[
					{"index_key":0,"attributes":{"v":"zero"}},
					{"index_key":1,"attributes":{"v":"one-updated"}}
				]`),
			},
		},
	}

	delta, err := buildStateEngineDeltaCommit(postApply, []string{"null_resource.many[1]"}, []string{"null_resource.many[0]"})
	if err != nil {
		t.Fatalf("buildStateEngineDeltaCommit: %v", err)
	}
	if len(delta.DeleteSet) != 1 || delta.DeleteSet[0] != "null_resource.many[0]" {
		t.Fatalf("delete_set=%v want [null_resource.many[0]]", delta.DeleteSet)
	}
	if len(delta.Resources) != 1 {
		t.Fatalf("resources len=%d want 1", len(delta.Resources))
	}
	if len(delta.Resources[0].Instances) != 1 {
		t.Fatalf("selected instances len=%d want 1", len(delta.Resources[0].Instances))
	}
	idxKind, idxValue, err := delta.Resources[0].Instances[0].DecodeIndex()
	if err != nil {
		t.Fatalf("DecodeIndex: %v", err)
	}
	if idxKind.String() != "int" || idxValue != "1" {
		t.Fatalf("selected instance index=(%s,%s) want (int,1)", idxKind.String(), idxValue)
	}
}

func TestDetectOutputDelta(t *testing.T) {
	writes, deletes, err := detectOutputDelta(
		map[string]tfstate.Output{
			"keep":   {Value: json.RawMessage(`1`), Type: json.RawMessage(`"number"`)},
			"change": {Value: json.RawMessage(`2`), Type: json.RawMessage(`"number"`)},
			"drop":   {Value: json.RawMessage(`3`), Type: json.RawMessage(`"number"`)},
		},
		map[string]tfstate.Output{
			"keep":   {Value: json.RawMessage(`1`), Type: json.RawMessage(`"number"`)},
			"change": {Value: json.RawMessage(`22`), Type: json.RawMessage(`"number"`)},
			"new":    {Value: json.RawMessage(`4`), Type: json.RawMessage(`"number"`)},
		},
	)
	if err != nil {
		t.Fatalf("detectOutputDelta: %v", err)
	}
	if len(writes) != 2 {
		t.Fatalf("writes len=%d want 2 (%v)", len(writes), writes)
	}
	if _, ok := writes["change"]; !ok {
		t.Fatalf("writes missing change: %v", writes)
	}
	if _, ok := writes["new"]; !ok {
		t.Fatalf("writes missing new: %v", writes)
	}
	if len(deletes) != 1 || deletes[0] != "drop" {
		t.Fatalf("deletes=%v want [drop]", deletes)
	}
}

func TestExactIntentFromPlan(t *testing.T) {
	intent := exactIntentFromPlan(&plan.File{
		ResourceChanges: []plan.ResourceChange{
			{Address: "null_resource.keep", Change: plan.Change{Actions: []string{"no-op"}}},
			{Address: "null_resource.create", Change: plan.Change{Actions: []string{"create"}}},
			{Address: "null_resource.update", Change: plan.Change{Actions: []string{"update"}}},
			{Address: "null_resource.delete", Change: plan.Change{Actions: []string{"delete"}}},
			{Address: "null_resource.replace", Change: plan.Change{Actions: []string{"delete", "create"}}},
			{Address: "null_resource.forget", Change: plan.Change{Actions: []string{"forget"}}},
		},
	})
	wantWrites := []string{
		"null_resource.create",
		"null_resource.delete",
		"null_resource.forget",
		"null_resource.replace",
		"null_resource.update",
	}
	wantDeletes := []string{
		"null_resource.delete",
		"null_resource.forget",
	}
	if len(intent.ExactWriteSet) != len(wantWrites) {
		t.Fatalf("write_set len=%d want %d (%v)", len(intent.ExactWriteSet), len(wantWrites), intent.ExactWriteSet)
	}
	for i := range wantWrites {
		if intent.ExactWriteSet[i] != wantWrites[i] {
			t.Fatalf("write_set[%d]=%s want %s (%v)", i, intent.ExactWriteSet[i], wantWrites[i], intent.ExactWriteSet)
		}
	}
	if len(intent.DeleteSet) != len(wantDeletes) {
		t.Fatalf("delete_set len=%d want %d (%v)", len(intent.DeleteSet), len(wantDeletes), intent.DeleteSet)
	}
	for i := range wantDeletes {
		if intent.DeleteSet[i] != wantDeletes[i] {
			t.Fatalf("delete_set[%d]=%s want %s (%v)", i, intent.DeleteSet[i], wantDeletes[i], intent.DeleteSet)
		}
	}
}

func TestValidateTrustedStateEngineIntent(t *testing.T) {
	t.Run("accepts exact match", func(t *testing.T) {
		spec := &plan.PlanSpec{WriteSet: []string{"terraform_data.a", "terraform_data.b"}}
		intent := &terraformRunIntent{
			ExactWriteSet: []string{"terraform_data.b", "terraform_data.a"},
			DeleteSet:     []string{"terraform_data.b"},
		}
		if err := validateTrustedStateEngineIntent(spec, intent); err != nil {
			t.Fatalf("validateTrustedStateEngineIntent: %v", err)
		}
	})

	t.Run("rejects nil intent", func(t *testing.T) {
		spec := &plan.PlanSpec{WriteSet: []string{"terraform_data.a"}}
		err := validateTrustedStateEngineIntent(spec, nil)
		if err == nil || !strings.Contains(err.Error(), "did not return exact intent") {
			t.Fatalf("err = %v, want exact intent failure", err)
		}
	})

	t.Run("rejects wider intent", func(t *testing.T) {
		spec := &plan.PlanSpec{WriteSet: []string{"terraform_data.a"}}
		intent := &terraformRunIntent{
			ExactWriteSet: []string{"terraform_data.a", "terraform_data.b"},
		}
		err := validateTrustedStateEngineIntent(spec, intent)
		if err == nil || !strings.Contains(err.Error(), "extra=[terraform_data.b]") {
			t.Fatalf("err = %v, want extra write_set failure", err)
		}
	})

	t.Run("rejects delete outside write set", func(t *testing.T) {
		spec := &plan.PlanSpec{WriteSet: []string{"terraform_data.a"}}
		intent := &terraformRunIntent{
			ExactWriteSet: []string{"terraform_data.a"},
			DeleteSet:     []string{"terraform_data.b"},
		}
		err := validateTrustedStateEngineIntent(spec, intent)
		if err == nil || !strings.Contains(err.Error(), "delete_set contains address outside exact write set") {
			t.Fatalf("err = %v, want delete_set failure", err)
		}
	})
}
