package provider

import (
	"context"
	"strings"
	"testing"
)

// These tests cover Configure paths that don't need a running
// provider: the closed-check, empty-config rejection, and the
// TerraformVersion default. The closed-check path requires
// constructing a clientV{5,6} with closed=true and never touching
// c.rpc, which works because both gates run before any gRPC call.

func TestConfigure_ClosedReturnsSentinel(t *testing.T) {
	v5 := &clientV5{}
	v5.closed.Store(true)
	v6 := &clientV6{}
	v6.closed.Store(true)

	for name, c := range map[string]Client{"v5": v5, "v6": v6} {
		t.Run(name, func(t *testing.T) {
			_, err := c.Configure(context.Background(), ConfigureProviderRequest{
				Config: []byte{0xC0},
			})
			if err != ErrProviderClosed {
				t.Fatalf("got %v, want ErrProviderClosed", err)
			}
		})
	}
}

func TestConfigure_RejectsEmptyConfig(t *testing.T) {
	for name, c := range map[string]Client{"v5": &clientV5{}, "v6": &clientV6{}} {
		t.Run(name, func(t *testing.T) {
			_, err := c.Configure(context.Background(), ConfigureProviderRequest{})
			if err == nil || !strings.Contains(err.Error(), "empty Config") {
				t.Fatalf("got %v, want 'empty Config' error", err)
			}
		})
	}
}

// TestDefaultReportedTerraformVersion is a regression check: the
// constant should always be shaped like a real semver triple, because
// providers parse it strictly and reject anything that doesn't look
// like \d+\.\d+\.\d+. If someone changes the constant carelessly, the
// integration suite would silently start failing in a way that's
// hard to attribute. This test fails loud.
func TestDefaultReportedTerraformVersion_LooksLikeSemver(t *testing.T) {
	parts := strings.Split(DefaultReportedTerraformVersion, ".")
	if len(parts) != 3 {
		t.Fatalf("expected 3 dotted parts, got %q", DefaultReportedTerraformVersion)
	}
	for i, p := range parts {
		if p == "" {
			t.Fatalf("part %d is empty: %q", i, DefaultReportedTerraformVersion)
		}
		for _, r := range p {
			if r < '0' || r > '9' {
				t.Fatalf("part %d (%q) is not numeric: %q", i, p, DefaultReportedTerraformVersion)
			}
		}
	}
}
