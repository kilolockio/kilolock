package main

import (
	"errors"
	"strings"
	"testing"
)

func TestFormatTargetScopeViolation_IncludesActionableContext(t *testing.T) {
	err := formatTargetScopeViolation(
		errors.New("target scope violation: planned writes outside safe target closure: module.shadow_herd.random_id.leader"),
		[]string{"time_sleep.slow_a"},
		[]string{"module.shadow_herd.random_id.leader", "random_pet.deployment_name"},
	)
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	for _, want := range []string{
		"requested targets: time_sleep.slow_a",
		"planned writes (preview):",
		"suggested extra --target:",
		"module.shadow_herd",
		"random_pet.deployment_name",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("missing %q in message:\n%s", want, msg)
		}
	}
}

func TestSuggestTargetsFromWrites_DedupesAndSkipsAlreadyRequested(t *testing.T) {
	got := suggestTargetsFromWrites(
		[]string{
			"module.small_herd.random_id.leader",
			"module.small_herd.random_id.tag[0]",
			"time_sleep.slow_a",
		},
		[]string{"time_sleep.slow_a"},
	)
	if len(got) != 1 || got[0] != "module.small_herd" {
		t.Fatalf("suggestions = %v, want [module.small_herd]", got)
	}
}
