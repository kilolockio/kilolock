package main

import (
	"testing"

	"github.com/kilolockio/kilolock/internal/plan"
)

func TestQuotaPlanDeltaFromSummary(t *testing.T) {
	summary := plan.PlanSummary{
		Create: 5,
		Delete: 2,
		Forget: 1,
	}
	if got, want := quotaPlanDeltaFromSummary(summary), 2; got != want {
		t.Fatalf("quotaPlanDeltaFromSummary() = %d, want %d", got, want)
	}
}
