//go:build integration

package provider

import (
	"context"
	"reflect"
	"testing"
	"time"
)

// TestPlanResourceChange_NullProvider_NoOp exercises the happy path
// against the null provider for a resource that requires no changes.
//
// We use a no-op plan because it guarantees that the provider will
// return a fully known state. If we simulated a change, the null
// provider would flag the "id" attribute as UnknownValue{}, which
// is valid and correctly parsed, but makes deep equality testing
// in Go slightly more complex for a basic end-to-end transport check.
func TestPlanResourceChange_NullProvider_NoOp(t *testing.T) {
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

	cfgType := providerConfigType(t, schema)
	cfgEncoded, err := EncodeMsgpack(cfgType, map[string]any{})
	if err != nil {
		t.Fatalf("EncodeMsgpack(config): %v", err)
	}
	if _, err := client.Configure(ctx, ConfigureProviderRequest{Config: cfgEncoded}); err != nil {
		t.Fatalf("Configure: %v", err)
	}

	rs, ok := schema.Resources["null_resource"]
	if !ok {
		t.Fatal("null_resource missing from schema")
	}
	blockType, err := BlockType(rs.Block)
	if err != nil {
		t.Fatalf("BlockType: %v", err)
	}

	prior := map[string]any{
		"id": "old-id",
		"triggers": map[string]any{
			"version": "1",
		},
	}
	proposed := map[string]any{
		"id": "old-id",
		"triggers": map[string]any{
			"version": "1",
		},
	}
	configVal := map[string]any{
		"id": nil,
		"triggers": map[string]any{
			"version": "1",
		},
	}

	priorEncoded, _ := EncodeMsgpack(blockType, prior)
	proposedEncoded, _ := EncodeMsgpack(blockType, proposed)
	configEncoded, _ := EncodeMsgpack(blockType, configVal)

	resp, diags, err := client.PlanResourceChange(ctx, PlanResourceChangeRequest{
		TypeName:         "null_resource",
		PriorState:       priorEncoded,
		ProposedNewState: proposedEncoded,
		Config:           configEncoded,
	})
	if err != nil {
		t.Fatalf("PlanResourceChange: %v", err)
	}
	if diags.HasError() {
		t.Fatalf("PlanResourceChange diagnostics: %+v", diags)
	}
	if len(resp.PlannedState) == 0 {
		t.Fatal("PlanResourceChange returned empty PlannedState")
	}

	decoded, err := DecodeMsgpack(blockType, resp.PlannedState)
	if err != nil {
		t.Fatalf("DecodeMsgpack: %v", err)
	}
	got, ok := decoded.(map[string]any)
	if !ok {
		t.Fatalf("decoded PlannedState: got %T", decoded)
	}

	if got["id"] != "old-id" {
		t.Errorf("id: got %v, want old-id", got["id"])
	}
	if !reflect.DeepEqual(got["triggers"], map[string]any{"version": "1"}) {
		t.Errorf("triggers: got %#v", got["triggers"])
	}
}
