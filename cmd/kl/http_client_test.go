package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNewAPIClientFromBackend_UsesTFHTTPEnvAuth(t *testing.T) {
	dir := t.TempDir()
	tfDir := filepath.Join(dir, ".terraform")
	if err := os.MkdirAll(tfDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := `{
  "version": 3,
  "backend": {
    "type": "http",
    "config": {
      "address": "http://localhost:18080/states/big-state",
      "username": "cfg-user",
      "password": "cfg-pass"
    }
  }
}`
	if err := os.WriteFile(filepath.Join(tfDir, "terraform.tfstate"), []byte(body), 0o644); err != nil {
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
