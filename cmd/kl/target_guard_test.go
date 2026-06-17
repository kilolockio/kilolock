package main

import (
	"os"
	"strings"
	"testing"

	"github.com/kilolockio/kilolock/internal/plan"
)

func TestTargetGuardViolation(t *testing.T) {
	t.Setenv(envTargetMaxWrites, "1")
	spec := &plan.PlanSpec{
		WriteSet: []string{"a", "b"},
	}
	err := targetGuardViolation(spec, []string{"a"})
	if err == nil {
		t.Fatal("expected target guard violation")
	}
	if !strings.Contains(err.Error(), envTargetMaxWrites) {
		t.Fatalf("unexpected err: %v", err)
	}
}

func TestTargetGuardViolation_NoLimits(t *testing.T) {
	t.Setenv(envTargetMaxWrites, "")
	t.Setenv(envTargetMaxReservations, "")
	spec := &plan.PlanSpec{
		WriteSet:     []string{"a", "b", "c"},
		Reservations: []plan.PlanReservation{{}, {}, {}},
	}
	if err := targetGuardViolation(spec, []string{"a"}); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
}

func TestReadPositiveIntEnv_Invalid(t *testing.T) {
	t.Setenv(envTargetMaxWrites, "abc")
	_, err := readPositiveIntEnv(envTargetMaxWrites)
	if err == nil {
		t.Fatal("expected parse error")
	}
}

func TestReadPositiveIntEnv_Negative(t *testing.T) {
	t.Setenv(envTargetMaxWrites, "-1")
	_, err := readPositiveIntEnv(envTargetMaxWrites)
	if err == nil {
		t.Fatal("expected negative value error")
	}
}

func TestReadPositiveIntEnv_Value(t *testing.T) {
	t.Setenv(envTargetMaxWrites, "7")
	got, err := readPositiveIntEnv(envTargetMaxWrites)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != 7 {
		t.Fatalf("got %d want 7", got)
	}
}

func TestTargetGuardViolation_NoTargets(t *testing.T) {
	t.Setenv(envTargetMaxWrites, "0")
	if err := targetGuardViolation(&plan.PlanSpec{}, nil); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
}

func TestTargetGuardViolation_Reservations(t *testing.T) {
	t.Setenv(envTargetMaxReservations, "1")
	spec := &plan.PlanSpec{
		Reservations: []plan.PlanReservation{{}, {}},
	}
	err := targetGuardViolation(spec, []string{"a"})
	if err == nil {
		t.Fatal("expected reservation guard violation")
	}
	if !strings.Contains(err.Error(), envTargetMaxReservations) {
		t.Fatalf("unexpected err: %v", err)
	}
}

func TestReadPositiveIntEnv_Empty(t *testing.T) {
	_ = os.Unsetenv(envTargetMaxWrites)
	got, err := readPositiveIntEnv(envTargetMaxWrites)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != 0 {
		t.Fatalf("got %d want 0", got)
	}
}
