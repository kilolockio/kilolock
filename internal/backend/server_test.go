package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kilolockio/kilolock/pkg/auth"
	"github.com/kilolockio/kilolock/pkg/store"
)

// These tests exercise the routing, method dispatch, and request validation
// layer of the Server. They use the real *store.Store wired against a real
// pgxpool, but only via the helper in server_integration_test.go (build tag
// gated). The tests here only check pre-store behavior — input parsing,
// method allow-list, JSON shape — and use a Server with a nil store. Any
// path that would call into the store is structured to fail input
// validation first.

func newTestServer() *Server {
	return New(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestHealth(t *testing.T) {
	srv := newTestServer()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body not json: %v (%s)", err, w.Body.String())
	}
	if body["status"] != "ok" {
		t.Errorf("body.status = %v, want ok", body["status"])
	}
}

func TestMethodNotAllowed(t *testing.T) {
	srv := newTestServer()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/v1/states/example", nil)
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", w.Code)
	}
	allow := w.Header().Get("Allow")
	for _, want := range []string{"GET", "POST", "DELETE", "LOCK", "UNLOCK"} {
		if !strings.Contains(allow, want) {
			t.Errorf("Allow header %q missing %s", allow, want)
		}
	}
}

func TestPost_BadJSON(t *testing.T) {
	srv := newTestServer()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/states/example",
		bytes.NewBufferString("this is not json"))
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestPost_BadJSON_MultiSegmentStateName(t *testing.T) {
	srv := newTestServer()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/states/vnv/prod/my_awesome_project",
		bytes.NewBufferString("not json"))
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

type staticAuth struct{ p auth.Principal }

func (s staticAuth) Authenticate(_ *http.Request) (auth.Principal, error) { return s.p, nil }

func TestStatePathMustMatchAuthenticatedEnvironment(t *testing.T) {
	srv := newTestServer().WithAuthenticator(staticAuth{p: auth.Principal{
		TenantID:            "tenant-1",
		WorkspaceID:         "ws_abc123",
		TenantSlug:          "balik",
		EnvironmentID:       "env-db-1",
		EnvironmentPublicID: "env_def456",
		EnvironmentSlug:     "dust",
		Source:              "test",
	}})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/states/balik/gra/my-awesome-project", nil)
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body=%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "wrong workspace and/or environment") {
		t.Fatalf("body = %q, want mismatch error", w.Body.String())
	}
}

func TestStatePathAllowsAuthenticatedEnvironmentPrefix(t *testing.T) {
	srv := newTestServer().WithAuthenticator(staticAuth{p: auth.Principal{
		TenantID:            "tenant-1",
		WorkspaceID:         "ws_abc123",
		TenantSlug:          "balik",
		EnvironmentID:       "env-db-1",
		EnvironmentPublicID: "env_def456",
		EnvironmentSlug:     "dust",
		Source:              "test",
	}}).WithStoreResolver(func(_ context.Context) (*store.Store, error) {
		return nil, fmt.Errorf("store unavailable")
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/states/ws_abc123/env_def456/my-awesome-project", nil)
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 once path validation passes (body=%s)", w.Code, w.Body.String())
	}
}

func TestLock_MissingID(t *testing.T) {
	srv := newTestServer()
	w := httptest.NewRecorder()
	req := httptest.NewRequest("LOCK", "/v1/states/example",
		bytes.NewBufferString(`{"Operation":"OperationTypePlan"}`))
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestUnlock_BadJSON(t *testing.T) {
	// Any non-empty non-JSON body is rejected with 400. Empty and
	// empty-ID payloads now dispatch to the force-release path; that
	// behavior is covered in the integration tests because it
	// requires a real store.
	srv := newTestServer()
	w := httptest.NewRecorder()
	req := httptest.NewRequest("UNLOCK", "/v1/states/example",
		bytes.NewBufferString("not json at all"))
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestActorFromRequest_PrefersBasicAuth(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.SetBasicAuth("alice", "secret")
	req.Header.Set("User-Agent", "Terraform/1.13")
	got := actorFromRequest(req)
	if got != "alice" {
		t.Errorf("actor = %q, want alice", got)
	}
}

func TestActorFromRequest_FallsBackToUA(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("User-Agent", "Terraform/1.13.4 (+https://www.terraform.io)")
	got := actorFromRequest(req)
	if got != "Terraform/1.13.4" {
		t.Errorf("actor = %q, want Terraform/1.13.4", got)
	}
}
