package main

import (
	"strings"
	"testing"

	"github.com/davesade/kilolock/internal/plan"
)

func TestFormatFileScopeEmptyWriteSet_IncludesOwnerHints(t *testing.T) {
	parsed, err := plan.ParseShowJSONBytes([]byte(capturedScopedShowJSON))
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	spec := plan.BuildSpec(parsed, plan.SpecBuildInput{
		ConfigDir: t.TempDir(),
	})
	scope := &plan.FileScope{Relative: []string{"not-owned.tf"}}

	msg := formatFileScopeEmptyWriteSet(parsed, spec, scope).Error()
	for _, want := range []string{
		"selected files: not-owned.tf",
		"planned mutating addresses are owned by: slow_a.tf, slow_b.tf",
		"hint: include the owning file(s)",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("missing %q in diagnostic:\n%s", want, msg)
		}
	}
}
