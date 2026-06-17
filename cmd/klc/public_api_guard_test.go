package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kilolockio/kilolock/pkg/config"
)

func TestPublicAPIGuard_ProdDefaultsToHidden(t *testing.T) {
	t.Setenv("KL_CONTROL_PUBLIC_API", "")
	s := newServer(nil, config.Config{InitMode: "prod"}, slog.New(slog.NewTextHandler(io.Discard, nil)), "token")
	h := s.withPublicAPIGuard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))

	req := httptest.NewRequest(http.MethodGet, "/api/tenants", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status=%d want 404", w.Code)
	}
}

func TestPublicAPIGuard_AllowsPortalServiceToken(t *testing.T) {
	t.Setenv("KL_CONTROL_PUBLIC_API", "")
	t.Setenv("KL_PORTAL_SERVICE_TOKEN", "portal")
	s := newServer(nil, config.Config{InitMode: "prod"}, slog.New(slog.NewTextHandler(io.Discard, nil)), "token")
	h := s.withPublicAPIGuard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))

	req := httptest.NewRequest(http.MethodGet, "/api/tenants", nil)
	req.Header.Set("X-Kl-Portal-Service-Token", "portal")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", w.Code)
	}
}

func TestPublicAPIGuard_AllowsBearerTokenWhenProdHidden(t *testing.T) {
	t.Setenv("KL_CONTROL_PUBLIC_API", "")
	s := newServer(nil, config.Config{InitMode: "prod"}, slog.New(slog.NewTextHandler(io.Discard, nil)), "token")
	h := s.withPublicAPIGuard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))

	req := httptest.NewRequest(http.MethodGet, "/api/tenants", nil)
	req.Header.Set("Authorization", "Bearer token")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", w.Code)
	}
}

func TestPublicAPIGuard_EnabledExposesAPI(t *testing.T) {
	t.Setenv("KL_CONTROL_PUBLIC_API", "enabled")
	s := newServer(nil, config.Config{InitMode: "prod"}, slog.New(slog.NewTextHandler(io.Discard, nil)), "token")
	h := s.withPublicAPIGuard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))

	req := httptest.NewRequest(http.MethodGet, "/api/tenants", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", w.Code)
	}
}
