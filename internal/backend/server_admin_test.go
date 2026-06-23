package backend

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kilolockio/kilolock/pkg/store"
)

func TestHandleRoutingStats_GET(t *testing.T) {
	s := New((*store.Store)(nil), nil).WithRoutingStatsProvider(func() map[string]any {
		return map[string]any{"routing_cache_hits": uint64(3)}
	})
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/routing/stats", nil)
	w := httptest.NewRecorder()
	s.handleRoutingStats(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body["ok"] != true {
		t.Fatalf("ok = %#v", body["ok"])
	}
}

func TestHandleRoutingStats_MethodNotAllowed(t *testing.T) {
	s := New((*store.Store)(nil), nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/routing/stats", nil)
	w := httptest.NewRecorder()
	s.handleRoutingStats(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d", w.Code)
	}
}
