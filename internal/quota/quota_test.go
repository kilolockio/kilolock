package quota

import (
	"context"
	"testing"
)

// TestUnlimited_AllChecksPass is the only meaningful unit test
// for the self-hosted default implementation. It exists to pin
// the contract that swapping in a non-Unlimited implementation
// is the only way to start rejecting writes — there is no
// global "deny everything" sentinel waiting to be triggered.
func TestUnlimited_AllChecksPass(t *testing.T) {
	q := Unlimited{}
	ctx := context.Background()
	tenant := "00000000-0000-0000-0000-000000000000"
	state := "11111111-1111-1111-1111-111111111111"

	if err := q.CheckStateCount(ctx, tenant); err != nil {
		t.Errorf("CheckStateCount: %v", err)
	}
	if err := q.CheckStateVersion(ctx, tenant, state); err != nil {
		t.Errorf("CheckStateVersion: %v", err)
	}
	if err := q.CheckStorageBytes(ctx, tenant, 1_000_000); err != nil {
		t.Errorf("CheckStorageBytes(positive): %v", err)
	}
	if err := q.CheckStorageBytes(ctx, tenant, -42); err != nil {
		t.Errorf("CheckStorageBytes(negative): %v", err)
	}
}

// TestUnlimited_SatisfiesInterface keeps the type-assertion
// honest. If a future commit adds a method to Quota and forgets
// to implement it on Unlimited, this test breaks the build.
func TestUnlimited_SatisfiesInterface(t *testing.T) {
	var _ Quota = Unlimited{}
}
