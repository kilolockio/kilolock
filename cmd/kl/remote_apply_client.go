package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/davesade/kilolock/pkg/store"
)

type remoteApplyClient struct {
	api       *apiClient
	stateName string
}

func newRemoteApplyClient(api *apiClient, stateName string) *remoteApplyClient {
	return &remoteApplyClient{api: api, stateName: stateName}
}

func (c *remoteApplyClient) GetCurrentStateInfo(ctx context.Context, name string) (*store.CurrentStateInfo, error) {
	return c.getStateInfo(ctx, name, false)
}

func (c *remoteApplyClient) EnsureCurrentStateInfo(ctx context.Context, name string) (*store.CurrentStateInfo, error) {
	return c.getStateInfo(ctx, name, true)
}

func (c *remoteApplyClient) getStateInfo(ctx context.Context, name string, ensure bool) (*store.CurrentStateInfo, error) {
	var out struct {
		StateID   string `json:"state_id"`
		VersionID string `json:"version_id"`
		Serial    int64  `json:"serial"`
		RawState  string `json:"raw_state"`
	}
	path := "/admin/state/current?name=" + url.QueryEscape(name)
	if ensure {
		path += "&ensure_genesis=true"
	}
	if err := c.api.doJSON(ctx, "GET", path, name, nil, &out); err != nil {
		return nil, err
	}
	return &store.CurrentStateInfo{StateID: out.StateID, VersionID: out.VersionID, Serial: out.Serial, Raw: []byte(out.RawState)}, nil
}

func (c *remoteApplyClient) GetStateRawAtSerial(ctx context.Context, name string, serial int64) ([]byte, error) {
	var out struct {
		RawState string `json:"raw_state"`
	}
	path := "/admin/state/raw?name=" + url.QueryEscape(name) + "&serial=" + strconv.FormatInt(serial, 10)
	if err := c.api.doJSON(ctx, "GET", path, name, nil, &out); err != nil {
		return nil, err
	}
	return []byte(out.RawState), nil
}

func (c *remoteApplyClient) BeginApplyRun(ctx context.Context, stateID, fromVersionID, actor string, sourceSerial int64, info json.RawMessage) (*store.ApplyRun, error) {
	in := map[string]any{"state_id": stateID, "from_version_id": fromVersionID, "actor": actor, "source_serial": sourceSerial, "info": info}
	var out store.ApplyRun
	if err := c.api.postJSON(ctx, "/admin/apply-runs/begin", c.stateName, in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *remoteApplyClient) GetApplyRunStatus(ctx context.Context, id string) (store.ApplyRunStatus, error) {
	var out struct {
		Status store.ApplyRunStatus `json:"status"`
	}
	if err := c.api.doJSON(ctx, "GET", "/admin/apply-runs/"+url.PathEscape(id)+"/status", c.stateName, nil, &out); err != nil {
		return "", err
	}
	return out.Status, nil
}

func (c *remoteApplyClient) FinishApplyRun(ctx context.Context, id string, in store.FinishApplyRunInput) error {
	return c.api.postJSON(ctx, "/admin/apply-runs/"+url.PathEscape(id)+"/finish", c.stateName, in, nil)
}

func (c *remoteApplyClient) AbortApplyRun(ctx context.Context, id, reason string) error {
	return c.api.postJSON(ctx, "/admin/apply-runs/"+url.PathEscape(id)+"/abort", c.stateName, map[string]any{"reason": reason}, nil)
}

func (c *remoteApplyClient) AcquireReservations(ctx context.Context, stateID, applyID, actor string, want []store.Reservation, lease time.Duration) error {
	var out struct {
		Error     string                    `json:"error"`
		Conflicts []store.ActiveReservation `json:"conflicts"`
	}
	in := map[string]any{
		"state_id":      stateID,
		"apply_id":      applyID,
		"actor":         actor,
		"want":          want,
		"lease_seconds": int(lease / time.Second),
	}
	err := c.api.postJSON(ctx, "/admin/reservations/acquire", c.stateName, in, &out)
	if err == nil {
		return nil
	}
	if out.Error != "" || len(out.Conflicts) > 0 {
		return &store.ReservationConflictError{StateID: stateID, Conflicts: out.Conflicts}
	}
	return err
}

func (c *remoteApplyClient) RenewReservations(ctx context.Context, applyID string, lease time.Duration) (int, error) {
	var out struct {
		Rows int `json:"rows"`
	}
	err := c.api.postJSON(ctx, "/admin/reservations/"+url.PathEscape(applyID)+"/renew", c.stateName, map[string]any{"lease_seconds": int(lease / time.Second)}, &out)
	return out.Rows, err
}

func (c *remoteApplyClient) ReleaseReservations(ctx context.Context, applyID string) error {
	return c.api.postJSON(ctx, "/admin/reservations/"+url.PathEscape(applyID)+"/release", c.stateName, map[string]any{}, nil)
}

func (c *remoteApplyClient) WriteStateForApply(ctx context.Context, name string, rawState []byte, source, actor string) error {
	return c.api.postJSON(ctx, "/admin/state/write-apply?name="+url.QueryEscape(name), name, map[string]any{
		"raw_state": string(rawState),
		"source":    source,
		"actor":     actor,
	}, nil)
}

func (c *remoteApplyClient) LookupStateVersionID(ctx context.Context, stateID string, serial int64) (string, error) {
	var out struct {
		VersionID string `json:"version_id"`
	}
	path := "/admin/state/version-id?state_id=" + url.QueryEscape(stateID) + "&serial=" + strconv.FormatInt(serial, 10)
	if err := c.api.doJSON(ctx, "GET", path, c.stateName, nil, &out); err != nil {
		return "", err
	}
	if out.VersionID == "" {
		return "", fmt.Errorf("state version id missing in response")
	}
	return out.VersionID, nil
}
