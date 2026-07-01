package apply

import (
	"testing"

	"github.com/kilolockio/kilolock/internal/plan"
)

func TestEffectiveSliceFootprint_IncludesStateEngineDeletedAndFetchAddresses(t *testing.T) {
	spec := &plan.PlanSpec{
		HCLFootprint: []string{"terraform_data.keep"},
		StateEngine: &plan.StateEnginePlanMetadata{
			FetchAddresses:      []string{"terraform_data.dep"},
			ConfigRequiredNodes: []string{"null_resource.support"},
			RemovedConfigNodes:  []string{"terraform_data.deleted"},
		},
	}
	got := effectiveSliceFootprint(spec)
	for _, addr := range []string{
		"terraform_data.keep",
		"terraform_data.dep",
		"null_resource.support",
		"terraform_data.deleted",
	} {
		if _, ok := got[addr]; !ok {
			t.Fatalf("effective footprint missing %s: %v", addr, got)
		}
	}
}
