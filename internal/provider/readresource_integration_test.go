//go:build integration

// Integration tests for Client.ReadResource. These prove the end-to-
// end v1 refresh path against real provider binaries: schema fetch →
// type derivation → state encode → wire RPC → state decode. See the
// header on launch_integration_test.go for run instructions and the
// macOS sandbox caveat.

package provider

import (
	"context"
	"reflect"
	"testing"
	"time"
)

// TestReadResource_NullProvider_EchoesState is the canonical happy
// path for v1.3b. null_resource is the simplest possible provider:
// its ReadResource implementation just returns the state it was given
// (with id set), because there is no cloud to refresh against. That
// makes it the ideal target for proving the wire transport, the
// encoder, and the decoder all line up.
//
// What this test verifies, beyond what unit tests can:
//
//   - A schema-derived BlockType can drive EncodeMsgpack to produce
//     msgpack bytes the real provider accepts. (Unit tests use
//     hand-built tftypes types; this test uses ones we computed
//     from the schema null returned.)
//
//   - The provider's wire response, when decoded with the same
//     BlockType, round-trips structurally back to what we sent.
//
//   - Diagnostics flow through correctly when there are none.
func TestReadResource_NullProvider_EchoesState(t *testing.T) {
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

	// Build a prior state that looks like what a Terraform apply
	// would have stored: id is the synthetic identifier null
	// generates, triggers is the user-supplied map.
	prior := map[string]any{
		"id": "test-id-12345",
		"triggers": map[string]any{
			"version": "1",
			"purpose": "integration-test",
		},
	}
	encoded, err := EncodeMsgpack(blockType, prior)
	if err != nil {
		t.Fatalf("EncodeMsgpack: %v", err)
	}

	resp, diags, err := client.ReadResource(ctx, ReadResourceRequest{
		TypeName:     "null_resource",
		CurrentState: encoded,
	})
	if err != nil {
		t.Fatalf("ReadResource: %v", err)
	}
	if diags.HasError() {
		t.Fatalf("ReadResource diagnostics: %+v", diags)
	}
	if resp == nil {
		t.Fatal("ReadResource returned nil response with no error")
	}
	if resp.Deferred != nil {
		t.Fatalf("unexpected Deferred=%+v", resp.Deferred)
	}
	if len(resp.NewState) == 0 {
		t.Fatal("ReadResource returned empty NewState")
	}

	decoded, err := DecodeMsgpack(blockType, resp.NewState)
	if err != nil {
		t.Fatalf("DecodeMsgpack: %v", err)
	}

	got, ok := decoded.(map[string]any)
	if !ok {
		t.Fatalf("decoded NewState: got %T, want map[string]any", decoded)
	}
	// null_resource's ReadResource implementation returns the prior
	// state unchanged. Equality is exact for the id (string) and
	// for the triggers map.
	if got["id"] != prior["id"] {
		t.Errorf("id: got %v, want %v", got["id"], prior["id"])
	}
	if !reflect.DeepEqual(got["triggers"], prior["triggers"]) {
		t.Errorf("triggers: got %#v, want %#v", got["triggers"], prior["triggers"])
	}
}

// TestReadResource_NullProvider_RoundTripsNullId verifies the second
// behavior null_resource implements: when the prior state has a null
// id, the provider treats the resource as gone and returns a null
// state. This is what refresh looks like when an out-of-band delete
// has happened — the provider tells us the resource no longer exists.
func TestReadResource_NullProvider_RoundTripsNullId(t *testing.T) {
	binary := providerOnDisk(t, "null")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client, err := Launch(ctx, binary, LaunchOptions{})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	defer client.Close()

	schema, _, err := client.GetSchema(ctx)
	if err != nil {
		t.Fatalf("GetSchema: %v", err)
	}
	blockType, err := BlockType(schema.Resources["null_resource"].Block)
	if err != nil {
		t.Fatalf("BlockType: %v", err)
	}

	encoded, err := EncodeMsgpack(blockType, map[string]any{
		"id":       nil,
		"triggers": nil,
	})
	if err != nil {
		t.Fatalf("EncodeMsgpack: %v", err)
	}

	resp, diags, err := client.ReadResource(ctx, ReadResourceRequest{
		TypeName:     "null_resource",
		CurrentState: encoded,
	})
	if err != nil {
		t.Fatalf("ReadResource: %v", err)
	}
	if diags.HasError() {
		t.Fatalf("ReadResource diagnostics: %+v", diags)
	}

	decoded, err := DecodeMsgpack(blockType, resp.NewState)
	if err != nil {
		t.Fatalf("DecodeMsgpack: %v", err)
	}
	// Two valid shapes for "resource is gone": the provider may
	// return null for the whole block, or an object with null id.
	// Both convey the same fact; accept either.
	switch m := decoded.(type) {
	case nil:
	case map[string]any:
		if m["id"] != nil {
			t.Errorf("expected null id, got %#v", m["id"])
		}
	default:
		t.Fatalf("unexpected decoded shape: %T %#v", decoded, decoded)
	}
}

// TestReadResource_UnknownType verifies that the diagnostics channel
// actually carries provider errors back to the caller. Asking the
// null provider about a non-existent resource type is one of the
// few error paths null actually exercises.
func TestReadResource_UnknownType(t *testing.T) {
	binary := providerOnDisk(t, "null")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client, err := Launch(ctx, binary, LaunchOptions{})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	defer client.Close()

	// Encode a synthetic minimal state — content doesn't matter,
	// the provider rejects on TypeName before looking at it.
	encoded := []byte{0xC0} // msgpack: nil

	_, diags, err := client.ReadResource(ctx, ReadResourceRequest{
		TypeName:     "definitely_not_a_real_resource_type",
		CurrentState: encoded,
	})
	// Providers may either return a transport-level error, or
	// (more commonly) succeed at the RPC level and surface the
	// rejection through Diagnostics. Either signals "bad type" to
	// our caller; assert that at least one of them fired.
	if err == nil && !diags.HasError() {
		t.Fatalf("expected error or diagnostics for unknown type; got err=nil, diags=%+v", diags)
	}
}

// TestReadResource_AfterCloseReturnsErrProviderClosed mirrors the
// existing GetSchema test: every Client method should reject calls
// after Close with ErrProviderClosed rather than panicking inside
// the wrapped gRPC client.
func TestReadResource_AfterCloseReturnsErrProviderClosed(t *testing.T) {
	binary := providerOnDisk(t, "null")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client, err := Launch(ctx, binary, LaunchOptions{})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	_, _, err = client.ReadResource(ctx, ReadResourceRequest{
		TypeName:     "null_resource",
		CurrentState: []byte{0xC0},
	})
	if err != ErrProviderClosed {
		t.Fatalf("got %v, want ErrProviderClosed", err)
	}
}
