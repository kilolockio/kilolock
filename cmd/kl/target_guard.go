package main

import (
	"fmt"
	"os"
	"strconv"

	"github.com/davesade/kilolock/internal/plan"
)

const (
	envTargetMaxWrites       = "KL_TARGET_MAX_WRITES"
	envTargetMaxReservations = "KL_TARGET_MAX_RESERVATIONS"
)

func targetGuardViolation(spec *plan.PlanSpec, targets []string) error {
	if spec == nil || len(targets) == 0 {
		return nil
	}
	maxWrites, err := readPositiveIntEnv(envTargetMaxWrites)
	if err != nil {
		return err
	}
	maxReservations, err := readPositiveIntEnv(envTargetMaxReservations)
	if err != nil {
		return err
	}
	if maxWrites > 0 && len(spec.WriteSet) > maxWrites {
		return fmt.Errorf("target scope guard: write_set=%d exceeds %s=%d", len(spec.WriteSet), envTargetMaxWrites, maxWrites)
	}
	if maxReservations > 0 && len(spec.Reservations) > maxReservations {
		return fmt.Errorf("target scope guard: reservations=%d exceeds %s=%d", len(spec.Reservations), envTargetMaxReservations, maxReservations)
	}
	return nil
}

func readPositiveIntEnv(key string) (int, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s must be a non-negative integer", key)
	}
	return n, nil
}
