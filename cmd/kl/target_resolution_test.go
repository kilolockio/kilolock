package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveStateTarget_UsesKLStateURLWhenNoPositional(t *testing.T) {
	t.Setenv("KL_STATE_URL", "https://api.kilolock.cloud/v1/states/ws_123/env_456/demo")

	target, discovered, err := resolveStateTarget("", ".")
	if err != nil {
		t.Fatalf("resolveStateTarget: %v", err)
	}
	if !discovered {
		t.Fatalf("discovered = false, want true")
	}
	if target.StateName != "ws_123/env_456/demo" {
		t.Fatalf("state name = %q", target.StateName)
	}
	if target.BaseURL != "https://api.kilolock.cloud/v1" {
		t.Fatalf("base URL = %q", target.BaseURL)
	}
}

func TestResolveStateTarget_PositionalURLWins(t *testing.T) {
	t.Setenv("KL_STATE_URL", "https://api.kilolock.cloud/v1/states/ws_env/ignored")

	target, discovered, err := resolveStateTarget("https://other.example/v1/states/ws_999/env_888/demo", ".")
	if err != nil {
		t.Fatalf("resolveStateTarget: %v", err)
	}
	if discovered {
		t.Fatalf("discovered = true, want false")
	}
	if target.StateName != "ws_999/env_888/demo" {
		t.Fatalf("state name = %q", target.StateName)
	}
	if target.BaseURL != "https://other.example/v1" {
		t.Fatalf("base URL = %q", target.BaseURL)
	}
}

func TestResolveStateTarget_FallsBackToBackend(t *testing.T) {
	dir := t.TempDir()
	body := `
terraform {
  backend "http" {
    address  = "https://api.kilolock.cloud/v1/states/ws_123/env_456/demo"
    username = "cfg-user"
    password = "cfg-pass"
  }
}
`
	if err := os.WriteFile(filepath.Join(dir, "backend.tf"), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	target, discovered, err := resolveStateTarget("", dir)
	if err != nil {
		t.Fatalf("resolveStateTarget: %v", err)
	}
	if !discovered {
		t.Fatalf("discovered = false, want true")
	}
	if target.StateName != "ws_123/env_456/demo" {
		t.Fatalf("state name = %q", target.StateName)
	}
	if target.BaseURL != "https://api.kilolock.cloud/v1" {
		t.Fatalf("base URL = %q", target.BaseURL)
	}
	if target.Username != "cfg-user" || target.Password != "cfg-pass" {
		t.Fatalf("backend auth = %q/%q", target.Username, target.Password)
	}
}
