package provider

import (
	"context"
	"strings"
	"testing"
)

// These tests exercise ReadResource paths that don't require a live
// provider: the closed-check and the request-validation gates. The
// concrete clients are zero-value constructable for this purpose
// because both gates run before the gRPC client is touched.

func TestReadResource_ClosedReturnsSentinel(t *testing.T) {
	v5 := &clientV5{}
	v5.closed.Store(true)
	v6 := &clientV6{}
	v6.closed.Store(true)

	for name, c := range map[string]Client{"v5": v5, "v6": v6} {
		t.Run(name, func(t *testing.T) {
			_, _, err := c.ReadResource(context.Background(), ReadResourceRequest{
				TypeName:     "x",
				CurrentState: []byte{0xC0},
			})
			if err != ErrProviderClosed {
				t.Fatalf("got %v, want ErrProviderClosed", err)
			}
		})
	}
}

func TestReadResource_RejectsEmptyTypeName(t *testing.T) {
	cases := map[string]Client{
		"v5": &clientV5{},
		"v6": &clientV6{},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, err := c.ReadResource(context.Background(), ReadResourceRequest{
				CurrentState: []byte{0xC0},
			})
			if err == nil || !strings.Contains(err.Error(), "empty TypeName") {
				t.Fatalf("got %v, want empty TypeName error", err)
			}
		})
	}
}

func TestReadResource_RejectsEmptyCurrentState(t *testing.T) {
	cases := map[string]Client{
		"v5": &clientV5{},
		"v6": &clientV6{},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, err := c.ReadResource(context.Background(), ReadResourceRequest{
				TypeName: "null_resource",
			})
			if err == nil || !strings.Contains(err.Error(), "empty CurrentState") {
				t.Fatalf("got %v, want empty CurrentState error", err)
			}
		})
	}
}

func TestDeferredReason_String(t *testing.T) {
	cases := map[DeferredReason]string{
		DeferredReasonUnknown:               "unknown",
		DeferredReasonResourceConfigUnknown: "resource_config_unknown",
		DeferredReasonProviderConfigUnknown: "provider_config_unknown",
		DeferredReasonAbsentPrereq:          "absent_prereq",
		DeferredReason(99):                  "unknown",
	}
	for r, want := range cases {
		if got := r.String(); got != want {
			t.Errorf("DeferredReason(%d).String() = %q, want %q", r, got, want)
		}
	}
}
