package provider

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// --- ParseSourceAddress -----------------------------------------------------

func TestParseSourceAddress_Forms(t *testing.T) {
	cases := []struct {
		in   string
		want SourceAddress
	}{
		{"null", SourceAddress{Hostname: "registry.terraform.io", Namespace: "hashicorp", Name: "null"}},
		{"hashicorp/aws", SourceAddress{Hostname: "registry.terraform.io", Namespace: "hashicorp", Name: "aws"}},
		{"registry.terraform.io/hashicorp/null", SourceAddress{Hostname: "registry.terraform.io", Namespace: "hashicorp", Name: "null"}},
		{"  hashicorp/null  ", SourceAddress{Hostname: "registry.terraform.io", Namespace: "hashicorp", Name: "null"}},
		{"private.example.com/team/widget", SourceAddress{Hostname: "private.example.com", Namespace: "team", Name: "widget"}},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseSourceAddress(tc.in)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestParseSourceAddress_Rejects(t *testing.T) {
	cases := map[string]string{
		"empty":           "",
		"whitespace":      "   ",
		"leading slash":   "/null",
		"trailing slash":  "null/",
		"double slash":    "hashicorp//null",
		"four parts":      "a.b/c/d/e",
		"hostname no dot": "noregistry/hashicorp/null",
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseSourceAddress(in)
			if err == nil {
				t.Fatalf("expected error for %q, got nil", in)
			}
		})
	}
}

// --- compareVersions --------------------------------------------------------

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.0.0", "1.0.0", 0},
		{"1.0.0", "1.0.1", -1},
		{"3.10.0", "3.2.0", 1},
		{"3.2.0", "3.10.0", -1},
		{"2.0", "2.0.0", 0}, // implicit-zero trailing segment
		{"2.0.1", "2.0", 1}, // shorter loses if a deeper segment differs
		{"1.0.0", "1.0.0-rc1", 1},
		{"1.0.0-rc1", "1.0.0-rc2", -1},
		{"v1.0.0", "1.0.0", 0}, // tolerate leading 'v'
	}
	for _, tc := range cases {
		got := compareVersions(tc.a, tc.b)
		if got != tc.want {
			t.Errorf("compareVersions(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

// --- Discover ---------------------------------------------------------------

// fakeLayout builds a synthetic provider tree under root and returns
// the touched binary path. The layout mirrors what `terraform init`
// produces:
//
//	<root>/<host>/<ns>/<name>/<version>/<platform>/terraform-provider-<name>_v<version>_x5
//
// The binary is written with executable bits set so Discover's mode
// check passes.
func fakeLayout(t *testing.T, root, host, ns, name, version, platform string) string {
	t.Helper()
	dir := filepath.Join(root, host, ns, name, version, platform)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	bin := filepath.Join(dir, "terraform-provider-"+name+"_v"+version+"_x5")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return bin
}

func TestDiscover_HappyPath(t *testing.T) {
	root := t.TempDir()
	want := fakeLayout(t, root, "registry.terraform.io", "hashicorp", "null", "3.2.4", "linux_amd64")

	got, err := Discover(SourceAddress{
		Hostname: "registry.terraform.io", Namespace: "hashicorp", Name: "null",
	}, DiscoveryOptions{
		SearchPaths: []string{root},
		Platform:    "linux_amd64",
	})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if got.Binary != want {
		t.Errorf("Binary: got %s, want %s", got.Binary, want)
	}
	if got.Version != "3.2.4" {
		t.Errorf("Version: got %s, want 3.2.4", got.Version)
	}
	if got.Platform != "linux_amd64" {
		t.Errorf("Platform: got %s, want linux_amd64", got.Platform)
	}
}

func TestDiscover_PicksHighestVersion(t *testing.T) {
	root := t.TempDir()
	fakeLayout(t, root, "registry.terraform.io", "hashicorp", "null", "3.2.0", "linux_amd64")
	winner := fakeLayout(t, root, "registry.terraform.io", "hashicorp", "null", "3.10.1", "linux_amd64")
	fakeLayout(t, root, "registry.terraform.io", "hashicorp", "null", "3.2.4", "linux_amd64")

	got, err := Discover(SourceAddress{
		Hostname: "registry.terraform.io", Namespace: "hashicorp", Name: "null",
	}, DiscoveryOptions{
		SearchPaths: []string{root},
		Platform:    "linux_amd64",
	})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if got.Binary != winner {
		t.Errorf("got %s, want %s (highest version)", got.Binary, winner)
	}
}

func TestDiscover_VersionPinExact(t *testing.T) {
	root := t.TempDir()
	fakeLayout(t, root, "registry.terraform.io", "hashicorp", "null", "3.2.0", "linux_amd64")
	pinned := fakeLayout(t, root, "registry.terraform.io", "hashicorp", "null", "3.2.4", "linux_amd64")
	fakeLayout(t, root, "registry.terraform.io", "hashicorp", "null", "3.10.0", "linux_amd64")

	got, err := Discover(SourceAddress{
		Hostname: "registry.terraform.io", Namespace: "hashicorp", Name: "null",
	}, DiscoveryOptions{
		SearchPaths: []string{root},
		Platform:    "linux_amd64",
		Version:     "3.2.4",
	})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if got.Binary != pinned {
		t.Errorf("got %s, want %s (pinned 3.2.4)", got.Binary, pinned)
	}
}

func TestDiscover_VersionPinMissing(t *testing.T) {
	root := t.TempDir()
	fakeLayout(t, root, "registry.terraform.io", "hashicorp", "null", "3.2.0", "linux_amd64")

	_, err := Discover(SourceAddress{
		Hostname: "registry.terraform.io", Namespace: "hashicorp", Name: "null",
	}, DiscoveryOptions{
		SearchPaths: []string{root},
		Platform:    "linux_amd64",
		Version:     "99.99.99",
	})
	if !errors.Is(err, ErrProviderNotFound) {
		t.Fatalf("got %v, want ErrProviderNotFound", err)
	}
}

func TestDiscover_PlatformMismatch(t *testing.T) {
	root := t.TempDir()
	fakeLayout(t, root, "registry.terraform.io", "hashicorp", "null", "3.2.0", "linux_amd64")

	_, err := Discover(SourceAddress{
		Hostname: "registry.terraform.io", Namespace: "hashicorp", Name: "null",
	}, DiscoveryOptions{
		SearchPaths: []string{root},
		Platform:    "windows_arm64",
	})
	if !errors.Is(err, ErrProviderNotFound) {
		t.Fatalf("got %v, want ErrProviderNotFound", err)
	}
}

func TestDiscover_SearchPathOrder(t *testing.T) {
	// Two roots — both have the provider, second one is newer.
	// First should win regardless of version comparison, because
	// SearchPaths are tried in order (matches Terraform's
	// "workspace first, cache second" precedence).
	first := t.TempDir()
	second := t.TempDir()
	winner := fakeLayout(t, first, "registry.terraform.io", "hashicorp", "null", "3.2.0", "linux_amd64")
	fakeLayout(t, second, "registry.terraform.io", "hashicorp", "null", "99.99.99", "linux_amd64")

	got, err := Discover(SourceAddress{
		Hostname: "registry.terraform.io", Namespace: "hashicorp", Name: "null",
	}, DiscoveryOptions{
		SearchPaths: []string{first, second},
		Platform:    "linux_amd64",
	})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if got.Binary != winner {
		t.Errorf("got %s, want %s (first search path wins)", got.Binary, winner)
	}
}

func TestDiscover_FallsBackToLaterPaths(t *testing.T) {
	first := t.TempDir() // empty
	second := t.TempDir()
	want := fakeLayout(t, second, "registry.terraform.io", "hashicorp", "null", "3.2.0", "linux_amd64")

	got, err := Discover(SourceAddress{
		Hostname: "registry.terraform.io", Namespace: "hashicorp", Name: "null",
	}, DiscoveryOptions{
		SearchPaths: []string{first, second},
		Platform:    "linux_amd64",
	})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if got.Binary != want {
		t.Errorf("got %s, want %s", got.Binary, want)
	}
}

func TestDiscover_NoSearchPaths(t *testing.T) {
	_, err := Discover(SourceAddress{
		Hostname: "registry.terraform.io", Namespace: "hashicorp", Name: "null",
	}, DiscoveryOptions{})
	if err == nil || !strings.Contains(err.Error(), "no search paths") {
		t.Fatalf("got %v, want 'no search paths' error", err)
	}
}

func TestDiscover_NotFound_MentionsTriedPaths(t *testing.T) {
	root := t.TempDir()
	_, err := Discover(SourceAddress{
		Hostname: "registry.terraform.io", Namespace: "hashicorp", Name: "null",
	}, DiscoveryOptions{
		SearchPaths: []string{root},
		Platform:    "linux_amd64",
	})
	if !errors.Is(err, ErrProviderNotFound) {
		t.Fatalf("got %v, want ErrProviderNotFound", err)
	}
	// The error should mention the subtree we tried, so a user
	// debugging "why didn't my provider load" can see exactly
	// where Discover looked.
	if !strings.Contains(err.Error(), "hashicorp/null") {
		t.Errorf("error message lacks subtree path: %v", err)
	}
}

func TestDiscover_AmbiguousBinaryIsAnError(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "registry.terraform.io", "hashicorp", "null", "3.2.0", "linux_amd64")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	for _, name := range []string{
		"terraform-provider-null_v3.2.0_x5",
		"terraform-provider-null_v3.2.0_x6",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh"), 0o755); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}

	_, err := Discover(SourceAddress{
		Hostname: "registry.terraform.io", Namespace: "hashicorp", Name: "null",
	}, DiscoveryOptions{
		SearchPaths: []string{root},
		Platform:    "linux_amd64",
	})
	if err == nil || !strings.Contains(err.Error(), "multiple") {
		t.Fatalf("got %v, want ambiguous-binary error", err)
	}
	// Surfacing this specifically (not as ErrProviderNotFound) is
	// the contract: ambiguity is a config bug, not a missing
	// install. Make sure callers can distinguish.
	if errors.Is(err, ErrProviderNotFound) {
		t.Errorf("ambiguous binary leaked as ErrProviderNotFound: %v", err)
	}
}

func TestDiscover_NonExecutableFileSkipped(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "registry.terraform.io", "hashicorp", "null", "3.2.0", "linux_amd64")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	// File present but not executable — should be ignored.
	if err := os.WriteFile(filepath.Join(dir, "terraform-provider-null_v3.2.0_x5"), []byte("script"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := Discover(SourceAddress{
		Hostname: "registry.terraform.io", Namespace: "hashicorp", Name: "null",
	}, DiscoveryOptions{
		SearchPaths: []string{root},
		Platform:    "linux_amd64",
	})
	if err == nil {
		t.Fatal("expected error for non-executable binary")
	}
}

func TestDiscover_DefaultPlatformIsCurrent(t *testing.T) {
	root := t.TempDir()
	platform := runtime.GOOS + "_" + runtime.GOARCH
	want := fakeLayout(t, root, "registry.terraform.io", "hashicorp", "null", "3.2.0", platform)

	got, err := Discover(SourceAddress{
		Hostname: "registry.terraform.io", Namespace: "hashicorp", Name: "null",
	}, DiscoveryOptions{
		SearchPaths: []string{root},
		// Platform left empty — should default to current platform.
	})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if got.Binary != want {
		t.Errorf("got %s, want %s", got.Binary, want)
	}
}
