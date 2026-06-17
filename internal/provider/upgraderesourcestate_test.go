package provider

import (
	"context"
	"strings"
	"testing"
)

// These tests exercise UpgradeResourceState's pre-RPC validation
// gates (closed check, empty TypeName / RawState) for both protocol
// versions. The gates run before the gRPC client is touched, so the
// concrete clients can be zero-value here.
//
// Round-trip behavior against a real provider lives in
// upgraderesourcestate_integration_test.go.

func TestUpgradeResourceState_ClosedReturnsSentinel(t *testing.T) {
	v5 := &clientV5{}
	v5.closed.Store(true)
	v6 := &clientV6{}
	v6.closed.Store(true)

	for name, c := range map[string]Client{"v5": v5, "v6": v6} {
		t.Run(name, func(t *testing.T) {
			_, _, err := c.UpgradeResourceState(context.Background(), UpgradeResourceStateRequest{
				TypeName: "x",
				Version:  0,
				RawState: []byte(`{}`),
			})
			if err != ErrProviderClosed {
				t.Fatalf("got %v, want ErrProviderClosed", err)
			}
		})
	}
}

func TestUpgradeResourceState_RejectsEmptyTypeName(t *testing.T) {
	cases := map[string]Client{
		"v5": &clientV5{},
		"v6": &clientV6{},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, err := c.UpgradeResourceState(context.Background(), UpgradeResourceStateRequest{
				RawState: []byte(`{}`),
			})
			if err == nil || !strings.Contains(err.Error(), "empty TypeName") {
				t.Fatalf("got %v, want empty TypeName error", err)
			}
		})
	}
}

func TestUpgradeResourceState_RejectsEmptyRawState(t *testing.T) {
	cases := map[string]Client{
		"v5": &clientV5{},
		"v6": &clientV6{},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			// An upgrade with no prior state to migrate is a
			// caller bug: there's nothing for the provider to
			// upgrade from. We surface this as an error
			// rather than sending an empty payload that the
			// provider would reject opaquely.
			_, _, err := c.UpgradeResourceState(context.Background(), UpgradeResourceStateRequest{
				TypeName: "null_resource",
			})
			if err == nil || !strings.Contains(err.Error(), "empty RawState") {
				t.Fatalf("got %v, want empty RawState error", err)
			}
		})
	}
}
