package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kilolockio/kilolock/pkg/config"
)

func TestRequirePermission_AllowsPortalServiceTokenForOwnershipTransferUpdate(t *testing.T) {
	t.Setenv("KL_PORTAL_SERVICE_TOKEN", "portal-secret")
	s := newServer(nil, config.Defaults(), slog.New(slog.NewTextHandler(io.Discard, nil)), "control-token")

	req := httptest.NewRequest(http.MethodPost, "/v1/api/ownership-transfers/example/accept", nil)
	req.Header.Set("X-Kl-Portal-Service-Token", "portal-secret")
	w := httptest.NewRecorder()

	if ok := s.requirePermission(w, req, "environment.transfer.update", "*", ""); !ok {
		t.Fatalf("portal service token should be allowed for ownership transfer updates; status=%d body=%s", w.Code, w.Body.String())
	}
}
