//go:build integration

// This file is built only with the `integration` build tag, matching
// the convention used by the rest of the repo. To run:
//
//	go test -tags=integration ./internal/provider/...
//
// The tests launch real Terraform provider binaries as child
// processes. They require `terraform` to be on PATH so the test can
// fetch a provider binary via `terraform init`. If terraform is not
// available, the tests skip cleanly.
//
// Note for macOS local runs: the default Cursor sandbox blocks
// unix-domain socket bind, which the provider's plugin handshake
// uses. Run these tests outside the sandbox.

package provider

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"testing"
	"time"
)

// providerOnDisk returns the path to a downloaded provider binary.
// It uses `terraform init` against a per-test temp directory to
// resolve and download the requested provider, then locates the
// resulting binary using the production Discover function.
//
// The temp dir is owned by t (cleaned up via t.TempDir). The
// downloaded binary inside it survives for the test's lifetime.
//
// If terraform is not on PATH, the test calls t.Skip() — provider
// RPC tests cannot run without it.
//
// Routing the discovery through Discover (rather than re-implementing
// the walk) means the integration suite also doubles as a smoke test
// for Discover against real terraform-init output: if Discover gets
// the layout wrong, every RPC test starts failing too.
func providerOnDisk(t *testing.T, provider string) string {
	t.Helper()

	if _, err := exec.LookPath("terraform"); err != nil {
		t.Skipf("terraform not on PATH (%v); skipping provider RPC test", err)
	}

	source, constraint := providerCoords(t, provider)

	dir := t.TempDir()
	tf := filepath.Join(dir, "main.tf")
	body := fmt.Sprintf(`terraform {
  required_providers {
    %s = {
      source  = %q
      version = %q
    }
  }
}
`, provider, source, constraint)
	if err := os.WriteFile(tf, []byte(body), 0o644); err != nil {
		t.Fatalf("write main.tf: %v", err)
	}

	cmd := exec.Command("terraform", "init", "-upgrade", "-no-color")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("terraform init failed: %v\n%s", err, out)
	}

	addr, err := ParseSourceAddress(source)
	if err != nil {
		t.Fatalf("ParseSourceAddress(%q): %v", source, err)
	}
	found, err := Discover(addr, DiscoveryOptions{
		SearchPaths: []string{filepath.Join(dir, ".terraform", "providers")},
	})
	if err != nil {
		t.Fatalf("Discover(%s): %v", source, err)
	}
	t.Logf("provider binary: %s (version %s)", found.Binary, found.Version)
	return found.Binary
}

func providerCoords(t *testing.T, provider string) (source, constraint string) {
	t.Helper()
	switch provider {
	case "null":
		return "hashicorp/null", "~> 3.2"
	case "time":
		return "hashicorp/time", "~> 0.13"
	case "random":
		return "hashicorp/random", "~> 3.6"
	case "tls":
		return "hashicorp/tls", "~> 4.0"
	}
	t.Fatalf("unknown test provider %q", provider)
	return "", ""
}

func TestLaunch_NullProvider_EndToEnd(t *testing.T) {
	binary := providerOnDisk(t, "null")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client, err := Launch(ctx, binary, LaunchOptions{})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	defer client.Close()

	if pv := client.ProtocolVersion(); pv != 5 && pv != 6 {
		t.Errorf("ProtocolVersion: got %d, want 5 or 6", pv)
	}

	schema, diags, err := client.GetSchema(ctx)
	if err != nil {
		t.Fatalf("GetSchema: %v", err)
	}
	if diags.HasError() {
		t.Fatalf("GetSchema diagnostics contain errors: %+v", diags)
	}
	if schema == nil {
		t.Fatal("GetSchema returned nil schema with no error")
	}

	// null provider declares exactly one resource type.
	rs, ok := schema.Resources["null_resource"]
	if !ok {
		names := make([]string, 0, len(schema.Resources))
		for n := range schema.Resources {
			names = append(names, n)
		}
		sort.Strings(names)
		t.Fatalf("null_resource missing from schema; got %v", names)
	}
	if rs.Block == nil {
		t.Fatal("null_resource schema has nil Block")
	}

	// null_resource has two attributes: id (computed string) and
	// triggers (optional map(string)). Verify both are present
	// with the expected flag combinations. Resource versions can
	// vary across provider releases; do not assert on Version.
	attrs := map[string]SchemaAttribute{}
	for _, a := range rs.Block.Attributes {
		attrs[a.Name] = a
	}
	if a, ok := attrs["id"]; !ok {
		t.Errorf("null_resource: id attribute missing")
	} else if !a.Computed {
		t.Errorf("null_resource: id should be computed, got %+v", a)
	}
	if a, ok := attrs["triggers"]; !ok {
		t.Errorf("null_resource: triggers attribute missing")
	} else if !a.Optional {
		t.Errorf("null_resource: triggers should be optional, got %+v", a)
	}

	if err := client.Stop(ctx); err != nil {
		t.Errorf("Stop: %v", err)
	}
}

func TestLaunch_NullProvider_CloseIsIdempotent(t *testing.T) {
	binary := providerOnDisk(t, "null")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client, err := Launch(ctx, binary, LaunchOptions{})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, _, err := client.GetSchema(ctx); !errors.Is(err, ErrProviderClosed) {
		t.Fatalf("GetSchema after Close: got %v, want ErrProviderClosed", err)
	}
}

// TestLaunch_MultipleProviders launches each downloadable test
// provider in sequence and verifies the basic schema RPC succeeds.
// Catches cases where one provider's quirks (negotiating an
// unexpected protocol version, returning an unusual schema shape)
// break the abstraction layer for everyone else.
func TestLaunch_MultipleProviders(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping multi-provider test in short mode")
	}
	if runtime.GOOS == "windows" {
		t.Skip("provider plugins are POSIX-launched; not exercised on Windows")
	}

	for _, p := range []string{"null", "time", "tls", "random"} {
		p := p
		t.Run(p, func(t *testing.T) {
			binary := providerOnDisk(t, p)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			client, err := Launch(ctx, binary, LaunchOptions{})
			if err != nil {
				t.Fatalf("Launch %s: %v", p, err)
			}
			defer client.Close()

			schema, diags, err := client.GetSchema(ctx)
			if err != nil {
				t.Fatalf("GetSchema %s: %v", p, err)
			}
			if diags.HasError() {
				t.Fatalf("GetSchema %s diagnostics: %+v", p, diags)
			}
			if schema == nil || len(schema.Resources) == 0 {
				t.Fatalf("GetSchema %s: expected at least one resource type", p)
			}
			t.Logf("%s: protocol=%d resources=%d data_sources=%d",
				p, client.ProtocolVersion(), len(schema.Resources), len(schema.DataSources))
		})
	}
}
