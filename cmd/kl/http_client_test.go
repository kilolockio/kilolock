package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNewAPIClientFromBackend_UsesTFHTTPEnvAuth(t *testing.T) {
	dir := t.TempDir()
	body := `
terraform {
  backend "http" {
    address  = "http://localhost:18080/v1/states/big-state"
    username = "cfg-user"
    password = "cfg-pass"
  }
}
`
	if err := os.WriteFile(filepath.Join(dir, "backend.tf"), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	t.Setenv("TF_HTTP_USERNAME", "env-user")
	t.Setenv("TF_HTTP_PASSWORD", "env-pass")
	c, err := newAPIClientFromBackend(dir)
	if err != nil {
		t.Fatalf("newAPIClientFromBackend: %v", err)
	}
	if c.username != "env-user" {
		t.Fatalf("username=%q want env-user", c.username)
	}
	if c.password != "env-pass" {
		t.Fatalf("password=%q want env-pass", c.password)
	}
}

func TestNewAPIClientFromBackend_UsesTFHTTPAddressOverride(t *testing.T) {
	dir := t.TempDir()
	body := `
terraform {
  backend "http" {
    address = "http://localhost:18080/v1/states/old-state"
  }
}
`
	if err := os.WriteFile(filepath.Join(dir, "backend.tf"), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	t.Setenv("TF_HTTP_ADDRESS", "https://api.kilolock.cloud/v1/states/ws_123/env_456/demo")
	c, err := newAPIClientFromBackend(dir)
	if err != nil {
		t.Fatalf("newAPIClientFromBackend: %v", err)
	}
	if c.baseURL != "https://api.kilolock.cloud/v1" {
		t.Fatalf("baseURL=%q want https://api.kilolock.cloud/v1", c.baseURL)
	}
	if c.defaultStateName != "ws_123/env_456/demo" {
		t.Fatalf("defaultStateName=%q want ws_123/env_456/demo", c.defaultStateName)
	}
}
