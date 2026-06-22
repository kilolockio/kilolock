package main

import (
	"errors"
	"testing"

	"github.com/kilolockio/kilolock/pkg/store"
)

func TestNormalizeWriteStateForApplyError_MapsSerialConflict(t *testing.T) {
	err := normalizeWriteStateForApplyError(errors.New(`POST /admin/state/write-apply?name=demo: 409 Conflict ({"error":"state serial conflict"})`))
	if !errors.Is(err, store.ErrSerialConflict) {
		t.Fatalf("errors.Is(err, store.ErrSerialConflict)=false; err=%v", err)
	}
}

func TestNormalizeWriteStateForApplyError_PreservesOtherErrors(t *testing.T) {
	orig := errors.New(`POST /admin/state/write-apply?name=demo: 409 Conflict ({"error":"different"})`)
	err := normalizeWriteStateForApplyError(orig)
	if !errors.Is(err, orig) {
		t.Fatalf("errors.Is(err, orig)=false; err=%v", err)
	}
	if errors.Is(err, store.ErrSerialConflict) {
		t.Fatalf("unexpected serial conflict mapping: %v", err)
	}
}
