package store

import (
	"strings"
	"testing"
)

func TestSanitizeLifecycleAudit(t *testing.T) {
	actor, reason := sanitizeLifecycleAudit("  alice@example.com  ", "  non-payment  ")
	if actor != "alice@example.com" {
		t.Fatalf("actor=%q", actor)
	}
	if reason != "non-payment" {
		t.Fatalf("reason=%q", reason)
	}
}

func TestValidateLifecycleTransitionAudit(t *testing.T) {
	if err := validateLifecycleTransitionAudit(LifecycleStatusActive, ""); err != nil {
		t.Fatalf("active should not require reason: %v", err)
	}
	if err := validateLifecycleTransitionAudit(LifecycleStatusSuspended, "maintenance"); err != nil {
		t.Fatalf("suspended with reason should pass: %v", err)
	}
	if err := validateLifecycleTransitionAudit(LifecycleStatusArchived, "retention"); err != nil {
		t.Fatalf("archived with reason should pass: %v", err)
	}
	err := validateLifecycleTransitionAudit(LifecycleStatusSuspended, "  ")
	if err == nil || !strings.Contains(err.Error(), "reason is required") {
		t.Fatalf("expected missing-reason error, got %v", err)
	}
}
