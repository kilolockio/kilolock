package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAPIClientDoJSON_SetsKilolockStateHeader(t *testing.T) {
	var gotHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("X-Kilolock-State-Name")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer srv.Close()

	c := &apiClient{baseURL: srv.URL}
	var out map[string]any
	if err := c.postJSON(context.Background(), "/admin/quota/check", "ws_x/env_y/demo", map[string]any{"x": 1}, &out); err != nil {
		t.Fatalf("postJSON: %v", err)
	}
	if gotHeader != "ws_x/env_y/demo" {
		t.Fatalf("X-Kilolock-State-Name = %q, want %q", gotHeader, "ws_x/env_y/demo")
	}
}

func TestAPIClientDoJSON_UsesDefaultKilolockStateHeader(t *testing.T) {
	var gotHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("X-Kilolock-State-Name")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer srv.Close()

	c := &apiClient{baseURL: srv.URL, defaultStateName: "ws_x/env_y/demo"}
	var out map[string]any
	if err := c.postJSON(context.Background(), "/admin/query", "", map[string]any{"sql": "select 1"}, &out); err != nil {
		t.Fatalf("postJSON: %v", err)
	}
	if gotHeader != "ws_x/env_y/demo" {
		t.Fatalf("X-Kilolock-State-Name = %q, want %q", gotHeader, "ws_x/env_y/demo")
	}
}
