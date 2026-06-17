package backend

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/davesade/kilolock/internal/auth"
)

func TestHandler_StaticTokenAuth(t *testing.T) {
	// Store is nil — we only exercise auth middleware before handlers run.
	// handleState will panic if reached without store; healthz does not.
	s := New(nil, nil).WithAuthenticator(auth.NewStaticTokenAuthenticator("test-secret"))

	t.Run("healthz without auth", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
	})

	t.Run("state without auth returns 401", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/states/x", nil)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		var body errResp
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if body.Error != "unauthenticated" {
			t.Fatalf("body = %+v", body)
		}
	})

}
