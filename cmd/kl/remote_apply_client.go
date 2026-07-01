package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/kilolockio/kilolock/pkg/store"
)

type remoteApplyClient struct {
	api            *apiClient
	stateName      string
	useStateEngine bool
}

func newRemoteApplyClient(api *apiClient, stateName string, useStateEngine bool) *remoteApplyClient {
	return &remoteApplyClient{api: api, stateName: stateName, useStateEngine: useStateEngine}
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
	path := "/admin/apply-runs/begin"
	if c.useStateEngine {
		in["state"] = c.stateName
		path = "/state-engine/apply-runs/begin"
	}
	if err := c.api.postJSON(ctx, path, c.stateName, in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *remoteApplyClient) GetApplyRunStatus(ctx context.Context, id string) (store.ApplyRunStatus, error) {
	var out struct {
		Status store.ApplyRunStatus `json:"status"`
	}
	path := "/admin/apply-runs/" + url.PathEscape(id) + "/status"
	if c.useStateEngine {
		path = "/state-engine/apply-runs/" + url.PathEscape(id) + "/status"
	}
	if err := c.api.doJSON(ctx, "GET", path, c.stateName, nil, &out); err != nil {
		return "", err
	}
	return out.Status, nil
}

func (c *remoteApplyClient) FinishApplyRun(ctx context.Context, id string, in store.FinishApplyRunInput) error {
	path := "/admin/apply-runs/" + url.PathEscape(id) + "/finish"
	if c.useStateEngine {
		path = "/state-engine/apply-runs/" + url.PathEscape(id) + "/finish"
	}
	return c.api.postJSON(ctx, path, c.stateName, in, nil)
}

func (c *remoteApplyClient) AbortApplyRun(ctx context.Context, id, reason string) error {
	path := "/admin/apply-runs/" + url.PathEscape(id) + "/abort"
	if c.useStateEngine {
		path = "/state-engine/apply-runs/" + url.PathEscape(id) + "/abort"
	}
	return c.api.postJSON(ctx, path, c.stateName, map[string]any{"reason": reason}, nil)
}

func (c *remoteApplyClient) AcquireStateEngineLock(ctx context.Context, name, applyID, holder string, scopeSummary []string) (store.LockInfo, error) {
	var out struct {
		Error  string         `json:"error"`
		LockID string         `json:"lock_id"`
		Lock   store.LockInfo `json:"lock"`
	}
	err := c.api.postJSON(ctx, "/state-engine/terraform-lock/acquire", c.stateName, map[string]any{
		"state":         name,
		"apply_id":      applyID,
		"holder":        holder,
		"scope_summary": scopeSummary,
	}, &out)
	if err == nil {
		return out.Lock, nil
	}
	if out.Error != "" || out.Lock.ID != "" {
		return out.Lock, store.ErrAlreadyLocked
	}
	return store.LockInfo{}, err
}

func (c *remoteApplyClient) ReleaseStateEngineLock(ctx context.Context, name, applyID, actor string) error {
	return c.api.postJSON(ctx, "/state-engine/terraform-lock/release", c.stateName, map[string]any{
		"state":    name,
		"apply_id": applyID,
		"actor":    actor,
	}, nil)
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
		"holder":        actor,
		"want":          want,
		"lease_seconds": int(lease / time.Second),
	}
	path := "/admin/reservations/acquire"
	if c.useStateEngine {
		in["state"] = c.stateName
		path = "/state-engine/reservations/acquire"
	}
	err := c.api.postJSON(ctx, path, c.stateName, in, &out)
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
		Rows    int `json:"rows"`
		Renewed int `json:"renewed"`
	}
	path := "/admin/reservations/" + url.PathEscape(applyID) + "/renew"
	if c.useStateEngine {
		path = "/state-engine/reservations/" + url.PathEscape(applyID) + "/renew"
	}
	err := c.api.postJSON(ctx, path, c.stateName, map[string]any{"lease_seconds": int(lease / time.Second)}, &out)
	if out.Renewed > 0 {
		return out.Renewed, err
	}
	return out.Rows, err
}

func (c *remoteApplyClient) ReleaseReservations(ctx context.Context, applyID string) error {
	path := "/admin/reservations/" + url.PathEscape(applyID) + "/release"
	if c.useStateEngine {
		path = "/state-engine/reservations/" + url.PathEscape(applyID) + "/release"
	}
	return c.api.postJSON(ctx, path, c.stateName, map[string]any{}, nil)
}

func (c *remoteApplyClient) WriteStateForApply(ctx context.Context, name, applyID string, baseSerial int64, rawState []byte, source, actor string) error {
	err := c.api.postJSON(ctx, "/admin/state/write-apply?name="+url.QueryEscape(name), name, map[string]any{
		"apply_id":    applyID,
		"base_serial": baseSerial,
		"raw_state":   string(rawState),
		"source":      source,
		"actor":       actor,
	}, nil)
	return normalizeWriteStateForApplyError(err)
}

func (c *remoteApplyClient) WriteStateEngineDeltaForApply(ctx context.Context, name, applyID string, baseSerial int64, delta store.StateEngineDeltaCommit, source, actor string) error {
	err := c.api.postJSON(ctx, "/state-engine/state/commit", c.stateName, map[string]any{
		"state":       name,
		"apply_id":    applyID,
		"base_serial": baseSerial,
		"mode":        "delta",
		"delta":       delta,
		"source":      source,
		"actor":       actor,
	}, nil)
	return normalizeWriteStateForApplyError(err)
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

func normalizeWriteStateForApplyError(err error) error {
	if err == nil {
		return nil
	}
	msg := strings.ToLower(strings.TrimSpace(err.Error()))
	if strings.Contains(msg, "409 conflict") && strings.Contains(msg, "state serial conflict") {
		return errors.Join(store.ErrSerialConflict, err)
	}
	return err
}
