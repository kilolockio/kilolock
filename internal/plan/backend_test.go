package plan

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kilolockio/kilolock/internal/tfstate"
)

// ---------------------------------------------------------------------------
// stateNameFromAddress
// ---------------------------------------------------------------------------

func TestStateNameFromAddress_HappyPaths(t *testing.T) {
	cases := []struct {
		addr string
		want string
	}{
		{"http://localhost:8080/states/big-state", "big-state"},
		{"http://localhost:8080/states/ws_0fb018ee0c37/env_bba69410e14b/blarg", "ws_0fb018ee0c37/env_bba69410e14b/blarg"},
		{"http://localhost:8080/states/ws_0fb018ee0c37/env_bba69410e14b/blarg?x=1", "ws_0fb018ee0c37/env_bba69410e14b/blarg"},
		{"http://kl.example/states/prod/", "prod"},
		{"http://kl.example/states/prod?x=1", "prod"},
		{"https://kl.example/states/prod-big-1", "prod-big-1"},
		{"http://kl.example/with/longer/path/segments/foo", "foo"},
	}
	for _, c := range cases {
		t.Run(c.addr, func(t *testing.T) {
			got, err := stateNameFromAddress(c.addr)
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestStateNameFromAddress_Rejects(t *testing.T) {
	cases := []string{
		"http://kl.example",
		"http://kl.example/",
		"",
	}
	for _, c := range cases {
		t.Run(c, func(t *testing.T) {
			if _, err := stateNameFromAddress(c); err == nil {
				t.Errorf("expected error on %q, got none", c)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// DiscoverBackend — drives the JSON parser end-to-end against
// captured `.terraform/terraform.tfstate` fixtures.
// ---------------------------------------------------------------------------

const initStateHTTP = `{
  "version": 3,
  "terraform_version": "1.13.4",
  "backend": {
    "type": "http",
    "config": {
      "address": "http://localhost:8080/states/big-state",
      "lock_address": "http://localhost:8080/states/big-state",
      "unlock_address": "http://localhost:8080/states/big-state"
    },
    "hash": 1351920819
  }
}`

const initStateHTTPHierarchical = `{
  "version": 3,
  "terraform_version": "1.13.4",
  "backend": {
    "type": "http",
    "config": {
      "address": "http://localhost:8080/states/ws_0fb018ee0c37/env_bba69410e14b/blarg",
      "lock_address": "http://localhost:8080/states/ws_0fb018ee0c37/env_bba69410e14b/blarg",
      "unlock_address": "http://localhost:8080/states/ws_0fb018ee0c37/env_bba69410e14b/blarg"
    },
    "hash": 1351920819
  }
}`

const initStateHTTPAuth = `{
  "version": 3,
  "terraform_version": "1.13.4",
  "backend": {
    "type": "http",
    "config": {
      "address": "http://localhost:8080/states/big-state",
      "username": "tenant-a",
      "password": "secret-a"
    },
    "hash": 1351920819
  }
}`

const initStateS3 = `{
  "version": 3,
  "backend": {
    "type": "s3",
    "config": {
      "bucket": "my-tf-state",
      "key":    "prod/terraform.tfstate"
    }
  }
}`

const initStateNoBackend = `{
  "version": 3
}`

func TestDiscoverBackend_HTTP(t *testing.T) {
	dir := writeInitFixture(t, initStateHTTP)
	got, err := DiscoverBackend(dir)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got.Type != "http" {
		t.Errorf("Type = %q", got.Type)
	}
	if got.Address != "http://localhost:8080/states/big-state" {
		t.Errorf("Address = %q", got.Address)
	}
	if got.StateName != "big-state" {
		t.Errorf("StateName = %q", got.StateName)
	}
	if got.Username != "" || got.Password != "" {
		t.Errorf("unexpected auth fields: user=%q pass=%q", got.Username, got.Password)
	}
}

func TestDiscoverBackend_HTTP_HierarchicalStateName(t *testing.T) {
	dir := writeInitFixture(t, initStateHTTPHierarchical)
	got, err := DiscoverBackend(dir)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got.StateName != "ws_0fb018ee0c37/env_bba69410e14b/blarg" {
		t.Errorf("StateName = %q", got.StateName)
	}
}

func TestDiscoverBackend_HTTP_WithAuth(t *testing.T) {
	dir := writeInitFixture(t, initStateHTTPAuth)
	got, err := DiscoverBackend(dir)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got.Username != "tenant-a" {
		t.Errorf("Username = %q", got.Username)
	}
	if got.Password != "secret-a" {
		t.Errorf("Password = %q", got.Password)
	}
}

func TestDiscoverBackend_UnsupportedType(t *testing.T) {
	dir := writeInitFixture(t, initStateS3)
	_, err := DiscoverBackend(dir)
	if !errors.Is(err, ErrUnsupportedBackend) {
		t.Errorf("got %v, want ErrUnsupportedBackend", err)
	}
}

func TestDiscoverBackend_NoBackendField(t *testing.T) {
	dir := writeInitFixture(t, initStateNoBackend)
	_, err := DiscoverBackend(dir)
	if !errors.Is(err, ErrNoBackendConfigured) {
		t.Errorf("got %v, want ErrNoBackendConfigured", err)
	}
}

func TestDiscoverBackend_MissingInitFile(t *testing.T) {
	dir := t.TempDir() // no .terraform/ written
	_, err := DiscoverBackend(dir)
	if !errors.Is(err, ErrNoBackendConfigured) {
		t.Errorf("got %v, want ErrNoBackendConfigured", err)
	}
}

func TestFetchCurrentStateFromBackend_BasicAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != "u1" || p != "p1" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"version":4}`))
	}))
	defer srv.Close()

	raw, err := FetchCurrentStateFromBackend(context.Background(), &BackendInfo{
		Address:  srv.URL + "/states/x",
		Username: "u1",
		Password: "p1",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if string(raw) != `{"version":4}` {
		t.Fatalf("raw=%q", string(raw))
	}
}

func TestFetchCurrentStateFromBackend_UsesAddressCredentials(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != "url-user" || p != "url-pass" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"version":4,"lineage":"x"}`))
	}))
	defer srv.Close()

	addr := strings.Replace(srv.URL, "http://", "http://url-user:url-pass@", 1) + "/states/x"
	raw, err := FetchCurrentStateFromBackend(context.Background(), &BackendInfo{
		Address: addr,
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if string(raw) != `{"version":4,"lineage":"x"}` {
		t.Fatalf("raw=%q", string(raw))
	}
}

func TestFetchCurrentStateFromBackend_UsesTFHTTPEnvAuth(t *testing.T) {
	t.Setenv("TF_HTTP_USERNAME", "env-user")
	t.Setenv("TF_HTTP_PASSWORD", "env-pass")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != "env-user" || p != "env-pass" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"version":4}`))
	}))
	defer srv.Close()

	raw, err := FetchCurrentStateFromBackend(context.Background(), &BackendInfo{
		Address:  srv.URL + "/states/x",
		Username: "cfg-user",
		Password: "cfg-pass",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if string(raw) != `{"version":4}` {
		t.Fatalf("raw=%q", string(raw))
	}
}

func TestFetchCurrentStateFromBackend_404ReturnsEmptyState(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	raw, err := FetchCurrentStateFromBackend(context.Background(), &BackendInfo{
		Address: srv.URL + "/states/missing",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	st, err := tfstate.Parse(raw)
	if err != nil {
		t.Fatalf("parse empty state: %v", err)
	}
	if st.Serial != 0 {
		t.Fatalf("serial=%d want 0", st.Serial)
	}
	if len(st.Resources) != 0 {
		t.Fatalf("resources=%d want 0", len(st.Resources))
	}
}

func writeInitFixture(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	tfDir := filepath.Join(dir, ".terraform")
	if err := os.MkdirAll(tfDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tfDir, "terraform.tfstate"), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return dir
}
