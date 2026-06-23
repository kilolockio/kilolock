package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kilolockio/kilolock/pkg/config"
)

func TestWithAuth_AllowsPortalServiceToken(t *testing.T) {
	t.Setenv("KL_PORTAL_SERVICE_TOKEN", "portal-secret")
	s := newServer(nil, config.Defaults(), slog.New(slog.NewTextHandler(io.Discard, nil)), "control-token")

	h := s.withAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/api/anything", nil)
	req.Header.Set("X-Kl-Portal-Service-Token", "portal-secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", w.Code)
	}
}

func TestWithAuth_RejectsMissingAuth(t *testing.T) {
	t.Setenv("KL_PORTAL_SERVICE_TOKEN", "portal-secret")
	s := newServer(nil, config.Defaults(), slog.New(slog.NewTextHandler(io.Discard, nil)), "control-token")

	h := s.withAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/api/anything", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d want 401", w.Code)
	}
}
