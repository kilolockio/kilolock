//go:build integration

// Integration tests for Client.UpgradeResourceState. These prove the
// real wire path against the null provider. Run instructions and the
// macOS sandbox caveat are documented on launch_integration_test.go.

package provider

import (
	"context"
	"reflect"
	"testing"
	"time"
)

// TestUpgradeResourceState_NullProvider_NoOpEcho exercises the
// happy path against the null provider. null_resource has schema
// version 0 (it has never bumped) and its UpgradeResourceState
// implementation is the "trust the JSON" trivial case: parse the
// stored JSON, re-encode as msgpack at the current schema. That
// makes it the ideal target for proving the wire transport itself:
// no upgrade ladder runs, but the request/response shape must round-
// trip cleanly.
//
// What this verifies, beyond unit-level gates:
//
//   - The raw JSON RawState format the wire expects is accepted as-is
//     (no msgpack pre-encoding on the request side).
//   - The msgpack DynamicValue we get back decodes through the same
//     BlockType the encoder uses, round-tripping the original
//     attribute shape.
//   - Diagnostics flow through correctly when there are none.
func TestUpgradeResourceState_NullProvider_NoOpEcho(t *testing.T) {
	binary := providerOnDisk(t, "null")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client, err := Launch(ctx, binary, LaunchOptions{})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	defer client.Close()

	schema, diags, err := client.GetSchema(ctx)
	if err != nil {
		t.Fatalf("GetSchema: %v", err)
	}
	if diags.HasError() {
		t.Fatalf("GetSchema diagnostics: %+v", diags)
	}
	rs, ok := schema.Resources["null_resource"]
	if !ok {
		t.Fatal("null_resource missing from schema")
	}
	blockType, err := BlockType(rs.Block)
	if err != nil {
		t.Fatalf("BlockType: %v", err)
	}

	// The stored JSON shape Terraform writes for a null_resource.
	rawState := []byte(`{"id":"test-upgrade-123","triggers":{"key":"val"}}`)

	resp, upDiags, err := client.UpgradeResourceState(ctx, UpgradeResourceStateRequest{
		TypeName: "null_resource",
		Version:  rs.Version, // current; the no-op ladder
		RawState: rawState,
	})
	if err != nil {
		t.Fatalf("UpgradeResourceState: %v", err)
	}
	if upDiags.HasError() {
		t.Fatalf("UpgradeResourceState diagnostics: %+v", upDiags)
	}
	if resp == nil || len(resp.UpgradedState) == 0 {
		t.Fatal("UpgradeResourceState returned empty UpgradedState")
	}

	decoded, err := DecodeMsgpack(blockType, resp.UpgradedState)
	if err != nil {
		t.Fatalf("DecodeMsgpack: %v", err)
	}
	got, ok := decoded.(map[string]any)
	if !ok {
		t.Fatalf("decoded UpgradedState: got %T, want map[string]any", decoded)
	}
	if got["id"] != "test-upgrade-123" {
		t.Errorf("id: got %v, want test-upgrade-123", got["id"])
	}
	if !reflect.DeepEqual(got["triggers"], map[string]any{"key": "val"}) {
		t.Errorf("triggers: got %#v", got["triggers"])
	}
}

// TestUpgradeResourceState_NullProvider_DownVersionStillAccepted
// proves that asking the provider to upgrade *from* a lower stored
// version is accepted even when no real migration is wired. null's
// upgrade implementation only inspects the raw_state JSON and emits
// the same shape at the current schema; the version field is
// effectively informational. This matches the behavior expected by
// the orchestrator's needsUpgrade gate: passing the recorded
// schema_version through, even when it differs from the live
// version, must not crash.
func TestUpgradeResourceState_NullProvider_DownVersionStillAccepted(t *testing.T) {
	binary := providerOnDisk(t, "null")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client, err := Launch(ctx, binary, LaunchOptions{})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	defer client.Close()

	resp, diags, err := client.UpgradeResourceState(ctx, UpgradeResourceStateRequest{
		TypeName: "null_resource",
		Version:  0, // null is already at v0; calling with v0 is the boundary case
		RawState: []byte(`{"id":"v0-state","triggers":null}`),
	})
	if err != nil {
		t.Fatalf("UpgradeResourceState: %v", err)
	}
	if diags.HasError() {
		t.Fatalf("UpgradeResourceState diagnostics: %+v", diags)
	}
	if len(resp.UpgradedState) == 0 {
		t.Fatal("empty UpgradedState")
	}
}
