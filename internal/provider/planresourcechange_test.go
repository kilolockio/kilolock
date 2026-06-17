package provider

import (
	"context"
	"strings"
	"testing"
)

func TestPlanResourceChange_ClosedReturnsSentinel(t *testing.T) {
	v5 := &clientV5{}
	v5.closed.Store(true)
	v6 := &clientV6{}
	v6.closed.Store(true)

	for name, c := range map[string]Client{"v5": v5, "v6": v6} {
		t.Run(name, func(t *testing.T) {
			_, _, err := c.PlanResourceChange(context.Background(), PlanResourceChangeRequest{
				TypeName: "x",
			})
			if err != ErrProviderClosed {
				t.Fatalf("got %v, want ErrProviderClosed", err)
			}
		})
	}
}

func TestPlanResourceChange_RejectsEmptyTypeName(t *testing.T) {
	for name, c := range map[string]Client{"v5": &clientV5{}, "v6": &clientV6{}} {
		t.Run(name, func(t *testing.T) {
			_, _, err := c.PlanResourceChange(context.Background(), PlanResourceChangeRequest{
				TypeName: "",
			})
			if err == nil || !strings.Contains(err.Error(), "empty TypeName") {
				t.Fatalf("got %v, want empty TypeName error", err)
			}
		})
	}
}
