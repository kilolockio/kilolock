package plan

import (
	"strings"
	"testing"
)

func TestValidateTargetedWriteSet_AllowsExactAndModuleDescendants(t *testing.T) {
	writeSet := []string{
		"random_pet.deployment_name",
		"module.small_herd.random_id.leader",
	}
	allowed := []string{
		"random_pet.deployment_name",
		"module.small_herd",
	}
	if err := ValidateTargetedWriteSet(writeSet, allowed); err != nil {
		t.Fatalf("ValidateTargetedWriteSet: %v", err)
	}
}

func TestValidateTargetedWriteSet_RejectsUnexpectedWrites(t *testing.T) {
	writeSet := []string{
		"random_pet.deployment_name",
		"module.shadow_herd.random_id.leader",
	}
	allowed := []string{
		"module.small_herd",
	}
	err := ValidateTargetedWriteSet(writeSet, allowed)
	if err == nil {
		t.Fatal("expected violation error, got nil")
	}
	if !strings.Contains(err.Error(), "add missing --target entries or run full plan") {
		t.Fatalf("missing remediation hint in error: %v", err)
	}
}
