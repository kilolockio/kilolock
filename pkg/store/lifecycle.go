package store

import (
	"fmt"
	"strings"
)

type LifecycleStatus string

const (
	LifecycleStatusActive    LifecycleStatus = "active"
	LifecycleStatusSuspended LifecycleStatus = "suspended"
	LifecycleStatusArchived  LifecycleStatus = "archived"
)

func ParseLifecycleStatus(raw string) (LifecycleStatus, error) {
	s := LifecycleStatus(strings.ToLower(strings.TrimSpace(raw)))
	switch s {
	case LifecycleStatusActive, LifecycleStatusSuspended, LifecycleStatusArchived:
		return s, nil
	default:
		return "", fmt.Errorf("invalid lifecycle status %q (expected active|suspended|archived)", raw)
	}
}
