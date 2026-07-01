package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kilolockio/kilolock/pkg/store"
)

func TestNormalizeWriteStateForApplyError_MapsSerialConflict(t *testing.T) {
	err := normalizeWriteStateForApplyError(errors.New(`POST /v1/admin/state/write-apply?name=demo: 409 Conflict ({"error":"state serial conflict"})`))
	if !errors.Is(err, store.ErrSerialConflict) {
		t.Fatalf("errors.Is(err, store.ErrSerialConflict)=false; err=%v", err)
	}
}

func TestNormalizeWriteStateForApplyError_PreservesOtherErrors(t *testing.T) {
	orig := errors.New(`POST /v1/admin/state/write-apply?name=demo: 409 Conflict ({"error":"different"})`)
	err := normalizeWriteStateForApplyError(orig)
	if !errors.Is(err, orig) {
		t.Fatalf("errors.Is(err, orig)=false; err=%v", err)
	}
	if errors.Is(err, store.ErrSerialConflict) {
		t.Fatalf("unexpected serial conflict mapping: %v", err)
	}
}

func TestRemoteApplyClient_BeginApplyRun_UsesTrustedStateEnginePathWhenEnabled(t *testing.T) {
	var (
		gotPath string
		gotBody map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":              "apply-1",
			"state_id":        "st-1",
			"from_version":    "sv-1",
			"from_version_id": "sv-1",
			"source_serial":   7,
			"actor":           "tester",
			"status":          "running",
		})
	}))
	defer srv.Close()

	c := newRemoteApplyClient(&apiClient{baseURL: srv.URL, defaultStateName: "ws_x/env_y/demo"}, "ws_x/env_y/demo", true)
	_, err := c.BeginApplyRun(context.Background(), "st-1", "sv-1", "tester", 7, json.RawMessage(`{"mode":"demo"}`))
	if err != nil {
		t.Fatalf("BeginApplyRun: %v", err)
	}
	if gotPath != "/state-engine/apply-runs/begin" {
		t.Fatalf("path = %q, want %q", gotPath, "/state-engine/apply-runs/begin")
	}
	if gotBody["state"] != "ws_x/env_y/demo" {
		t.Fatalf("state field = %#v, want ws_x/env_y/demo", gotBody["state"])
	}
	if _, ok := gotBody["state_id"]; !ok {
		t.Fatalf("expected state_id in request body, got %v", gotBody)
	}
}

func TestRemoteApplyClient_BeginApplyRun_UsesAdminPathWhenStateEngineDisabled(t *testing.T) {
	var (
		gotPath string
		gotBody map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":              "apply-1",
			"state_id":        "st-1",
			"from_version_id": "sv-1",
			"source_serial":   7,
			"actor":           "tester",
			"status":          "running",
		})
	}))
	defer srv.Close()

	c := newRemoteApplyClient(&apiClient{baseURL: srv.URL, defaultStateName: "ws_x/env_y/demo"}, "ws_x/env_y/demo", false)
	_, err := c.BeginApplyRun(context.Background(), "st-1", "sv-1", "tester", 7, json.RawMessage(`{"mode":"demo"}`))
	if err != nil {
		t.Fatalf("BeginApplyRun: %v", err)
	}
	if gotPath != "/admin/apply-runs/begin" {
		t.Fatalf("path = %q, want %q", gotPath, "/admin/apply-runs/begin")
	}
	if _, ok := gotBody["state"]; ok {
		t.Fatalf("unexpected state field in admin request body: %v", gotBody)
	}
}

func TestRemoteApplyClient_AcquireReservations_UsesTrustedStateEnginePathWhenEnabled(t *testing.T) {
	var (
		gotPath string
		gotBody map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer srv.Close()

	c := newRemoteApplyClient(&apiClient{baseURL: srv.URL, defaultStateName: "ws_x/env_y/demo"}, "ws_x/env_y/demo", true)
	err := c.AcquireReservations(context.Background(), "st-1", "apply-1", "tester", []store.Reservation{
		{AddressGlob: "time_sleep.slow_a", Mode: store.ReservationWrite},
	}, 30*time.Second)
	if err != nil {
		t.Fatalf("AcquireReservations: %v", err)
	}
	if gotPath != "/state-engine/reservations/acquire" {
		t.Fatalf("path = %q, want %q", gotPath, "/state-engine/reservations/acquire")
	}
	if gotBody["state"] != "ws_x/env_y/demo" {
		t.Fatalf("state field = %#v, want ws_x/env_y/demo", gotBody["state"])
	}
}

func TestRemoteApplyClient_WriteStateEngineDeltaForApply_UsesStateEngineCommitPath(t *testing.T) {
	var (
		gotPath string
		gotBody map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer srv.Close()

	c := newRemoteApplyClient(&apiClient{baseURL: srv.URL, defaultStateName: "ws_x/env_y/demo"}, "ws_x/env_y/demo", true)
	err := c.WriteStateEngineDeltaForApply(context.Background(), "ws_x/env_y/demo", "apply-1", 9, store.StateEngineDeltaCommit{
		WriteSet: []string{"time_sleep.slow_a"},
	}, "state-engine-apply", "tester")
	if err != nil {
		t.Fatalf("WriteStateEngineDeltaForApply: %v", err)
	}
	if gotPath != "/state-engine/state/commit" {
		t.Fatalf("path = %q, want %q", gotPath, "/state-engine/state/commit")
	}
	if gotBody["mode"] != "delta" {
		t.Fatalf("mode = %#v, want delta", gotBody["mode"])
	}
	if gotBody["state"] != "ws_x/env_y/demo" {
		t.Fatalf("state field = %#v, want ws_x/env_y/demo", gotBody["state"])
	}
	if _, ok := gotBody["delta"]; !ok {
		t.Fatalf("expected delta payload in request body: %v", gotBody)
	}
}
