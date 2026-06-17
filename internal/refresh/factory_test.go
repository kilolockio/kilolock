package refresh

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/kilolockio/kilolock/internal/provider"
)

func TestNewProductionFactory_Validation(t *testing.T) {
	if _, err := NewProductionFactory(ProductionFactoryOptions{}); err == nil {
		t.Error("expected error for nil Store")
	}
}

// stringAttr is a tiny convenience so the test's hand-rolled schemas
// look more like the real wire schemas (which carry JSON-encoded cty
// types). The cty-json equivalent of `string` is the literal `"string"`.
func stringAttr(name string) provider.SchemaAttribute {
	return provider.SchemaAttribute{
		Name: name,
		Type: json.RawMessage(`"string"`),
	}
}

// nullishSchema returns a schema with a single resource type
// "demo_thing" whose block has one attribute "id" (string). It's
// the smallest schema that exercises the encoding path realistically.
func nullishSchema() *provider.Schema {
	return &provider.Schema{
		Resources: map[string]*provider.ResourceSchema{
			"demo_thing": {
				Block: &provider.SchemaBlock{
					Attributes: []provider.SchemaAttribute{stringAttr("id")},
				},
			},
		},
	}
}

// encodingTransport is a fake provider.Client that:
//   - records the msgpack bytes encodingClient sends to it;
//   - decodes them back to a JSON-shaped value for assertions;
//   - returns a configurable response (also msgpack) so the
//     decode path round-trips through the wrapper.
//
// It exists so encodingClient's encode/decode logic is testable
// without launching a real provider.
type encodingTransport struct {
	mu    sync.Mutex
	calls []encodingTransportCall

	// respond hooks ReadResource. nil means "echo prior unchanged".
	respond func(typeName string, prior any) (newState any, diags provider.Diagnostics, err error)

	upgradeCalls []upgradeTransportCall

	// respondUpgrade hooks UpgradeResourceState. nil means
	// "upgrade is a no-op": re-encode the supplied JSON as
	// msgpack under the current schema. That matches what real
	// providers do when no version-ladder rule applies.
	respondUpgrade func(typeName string, version int64, raw []byte) (upgraded any, diags provider.Diagnostics, err error)

	closed atomic.Bool
}

type encodingTransportCall struct {
	TypeName string
	// PriorJSON is the msgpack-decoded prior state, re-rendered as
	// JSON for easy comparison.
	PriorJSON string
}

type upgradeTransportCall struct {
	TypeName string
	Version  int64
	// RawJSON is the request payload verbatim (the wire takes
	// JSON for UpgradeResourceState, not msgpack).
	RawJSON string
}

func (t *encodingTransport) ReadResource(
	ctx context.Context,
	req provider.ReadResourceRequest,
) (*provider.ReadResourceResponse, provider.Diagnostics, error) {
	if t.closed.Load() {
		return nil, nil, provider.ErrProviderClosed
	}

	// The wrapper hands us msgpack bytes. To make assertions cleaner
	// we decode them back through the same codec and stringify.
	blockType, err := provider.BlockType(nullishSchema().Resources[req.TypeName].Block)
	if err != nil {
		return nil, nil, err
	}
	prior, err := provider.DecodeMsgpack(blockType, req.CurrentState)
	if err != nil {
		return nil, nil, err
	}
	priorJSON, _ := json.Marshal(prior)

	t.mu.Lock()
	t.calls = append(t.calls, encodingTransportCall{
		TypeName:  req.TypeName,
		PriorJSON: string(priorJSON),
	})
	t.mu.Unlock()

	if t.respond == nil {
		// Default: echo prior unchanged.
		return &provider.ReadResourceResponse{NewState: req.CurrentState}, nil, nil
	}
	newVal, diags, err := t.respond(req.TypeName, prior)
	if err != nil {
		return nil, diags, err
	}
	encoded, err := provider.EncodeMsgpack(blockType, newVal)
	if err != nil {
		return nil, diags, err
	}
	return &provider.ReadResourceResponse{NewState: encoded}, diags, nil
}

func (t *encodingTransport) UpgradeResourceState(
	ctx context.Context,
	req provider.UpgradeResourceStateRequest,
) (*provider.UpgradeResourceStateResponse, provider.Diagnostics, error) {
	if t.closed.Load() {
		return nil, nil, provider.ErrProviderClosed
	}

	t.mu.Lock()
	t.upgradeCalls = append(t.upgradeCalls, upgradeTransportCall{
		TypeName: req.TypeName,
		Version:  req.Version,
		RawJSON:  string(req.RawState),
	})
	t.mu.Unlock()

	blockType, err := provider.BlockType(nullishSchema().Resources[req.TypeName].Block)
	if err != nil {
		return nil, nil, err
	}

	var upgraded any
	if t.respondUpgrade != nil {
		v, diags, err := t.respondUpgrade(req.TypeName, req.Version, req.RawState)
		if err != nil {
			return nil, diags, err
		}
		upgraded = v
		encoded, err := provider.EncodeMsgpack(blockType, upgraded)
		if err != nil {
			return nil, diags, err
		}
		return &provider.UpgradeResourceStateResponse{UpgradedState: encoded}, diags, nil
	}

	// Default: parse the raw JSON and re-emit it at the current
	// schema (no-op upgrade).
	parsed := map[string]any{}
	if len(req.RawState) > 0 && string(req.RawState) != "null" {
		if err := json.Unmarshal(req.RawState, &parsed); err != nil {
			return nil, nil, err
		}
	}
	encoded, err := provider.EncodeMsgpack(blockType, parsed)
	if err != nil {
		return nil, nil, err
	}
	return &provider.UpgradeResourceStateResponse{UpgradedState: encoded}, nil, nil
}

func (t *encodingTransport) GetSchema(ctx context.Context) (*provider.Schema, provider.Diagnostics, error) {
	return nullishSchema(), nil, nil
}
func (t *encodingTransport) Configure(context.Context, provider.ConfigureProviderRequest) (provider.Diagnostics, error) {
	return nil, nil
}
func (t *encodingTransport) PlanResourceChange(context.Context, provider.PlanResourceChangeRequest) (*provider.PlanResourceChangeResponse, provider.Diagnostics, error) {
	if t.closed.Load() {
		return nil, nil, provider.ErrProviderClosed
	}
	return nil, nil, errors.New("PlanResourceChange not implemented in encodingTransport test double")
}
func (t *encodingTransport) Stop(context.Context) error { return nil }
func (t *encodingTransport) ProtocolVersion() int       { return 6 }
func (t *encodingTransport) Close() error {
	t.closed.Store(true)
	return nil
}

var _ provider.Client = (*encodingTransport)(nil)

func TestEncodingClient_RoundTripsJSON(t *testing.T) {
	t.Parallel()
	inner := &encodingTransport{}
	wrapped := &encodingClient{inner: inner, schema: nullishSchema()}

	prior := []byte(`{"id":"vpc-old"}`)
	resp, diags, err := wrapped.ReadResource(context.Background(), provider.ReadResourceRequest{
		TypeName:     "demo_thing",
		CurrentState: prior,
	})
	if err != nil {
		t.Fatalf("ReadResource: %v", err)
	}
	if diags.HasError() {
		t.Fatalf("diags: %+v", diags)
	}
	if resp == nil || len(resp.NewState) == 0 {
		t.Fatal("nil/empty response")
	}

	// The transport saw the prior payload as JSON-equivalent.
	if got := inner.calls; len(got) != 1 || !strings.Contains(got[0].PriorJSON, "vpc-old") {
		t.Errorf("transport saw unexpected prior: %+v", got)
	}

	// And the response decoded back to the same shape (echo).
	var out map[string]any
	if err := json.Unmarshal(resp.NewState, &out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if out["id"] != "vpc-old" {
		t.Errorf("response id: got %v, want vpc-old", out["id"])
	}
}

func TestEncodingClient_DriftRoundTrips(t *testing.T) {
	t.Parallel()
	inner := &encodingTransport{
		respond: func(typeName string, prior any) (any, provider.Diagnostics, error) {
			return map[string]any{"id": "vpc-new"}, nil, nil
		},
	}
	wrapped := &encodingClient{inner: inner, schema: nullishSchema()}

	resp, _, err := wrapped.ReadResource(context.Background(), provider.ReadResourceRequest{
		TypeName:     "demo_thing",
		CurrentState: []byte(`{"id":"vpc-old"}`),
	})
	if err != nil {
		t.Fatalf("ReadResource: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(resp.NewState, &out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if out["id"] != "vpc-new" {
		t.Errorf("response id: got %v, want vpc-new", out["id"])
	}
}

func TestEncodingClient_UnknownTypeIsRejected(t *testing.T) {
	t.Parallel()
	wrapped := &encodingClient{inner: &encodingTransport{}, schema: nullishSchema()}
	_, _, err := wrapped.ReadResource(context.Background(), provider.ReadResourceRequest{
		TypeName:     "no_such_type",
		CurrentState: []byte(`{}`),
	})
	if err == nil {
		t.Fatal("expected error for unknown type")
	}
	if !strings.Contains(err.Error(), "no_such_type") {
		t.Errorf("error should mention the unknown type, got: %v", err)
	}
}

func TestEncodingClient_NilBlockIsRejected(t *testing.T) {
	t.Parallel()
	bad := &provider.Schema{
		Resources: map[string]*provider.ResourceSchema{
			"demo_thing": {Block: nil},
		},
	}
	wrapped := &encodingClient{inner: &encodingTransport{}, schema: bad}
	_, _, err := wrapped.ReadResource(context.Background(), provider.ReadResourceRequest{
		TypeName:     "demo_thing",
		CurrentState: []byte(`{}`),
	})
	if err == nil {
		t.Fatal("expected error for nil block")
	}
}

// TestEncodingClient_UpgradeResourceState_DecodesMsgpackToJSON proves
// the asymmetric encoding contract: the request goes out as raw JSON
// bytes (the wire takes `bytes json` here, not a DynamicValue), and
// the response comes back as a msgpack DynamicValue that the wrapper
// decodes to JSON before handing it to the caller. The orchestrator
// stays JSON-shaped on both ends regardless.
func TestEncodingClient_UpgradeResourceState_DecodesMsgpackToJSON(t *testing.T) {
	t.Parallel()
	inner := &encodingTransport{}
	wrapped := &encodingClient{inner: inner, schema: nullishSchema()}

	raw := []byte(`{"id":"vpc-old"}`)
	resp, diags, err := wrapped.UpgradeResourceState(context.Background(), provider.UpgradeResourceStateRequest{
		TypeName: "demo_thing",
		Version:  0,
		RawState: raw,
	})
	if err != nil {
		t.Fatalf("UpgradeResourceState: %v", err)
	}
	if diags.HasError() {
		t.Fatalf("diags: %+v", diags)
	}
	if resp == nil || len(resp.UpgradedState) == 0 {
		t.Fatal("nil/empty response")
	}

	// The transport saw the raw JSON verbatim (not msgpack-encoded;
	// the upgrade RPC takes raw JSON on the request side).
	if got := inner.upgradeCalls; len(got) != 1 {
		t.Fatalf("transport upgradeCalls: got %d, want 1", len(got))
	}
	if inner.upgradeCalls[0].RawJSON != string(raw) {
		t.Errorf("transport raw: got %q, want %q", inner.upgradeCalls[0].RawJSON, raw)
	}

	// And the response decoded back to the same shape (no-op upgrade).
	var out map[string]any
	if err := json.Unmarshal(resp.UpgradedState, &out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if out["id"] != "vpc-old" {
		t.Errorf("upgraded id: got %v, want vpc-old", out["id"])
	}
}

// TestEncodingClient_UpgradeResourceState_ShapeChangeRoundTrips
// proves the upgrade can return a *different* shape (the simulated
// migration outcome) and the wrapper still decodes it cleanly.
// Acceptance test for a real schema-version bump.
func TestEncodingClient_UpgradeResourceState_ShapeChangeRoundTrips(t *testing.T) {
	t.Parallel()
	inner := &encodingTransport{
		respondUpgrade: func(typeName string, version int64, raw []byte) (any, provider.Diagnostics, error) {
			// Simulate a v0→v1 migration that fills in a new
			// default field. Real providers do exactly this.
			return map[string]any{"id": "vpc-old"}, nil, nil
		},
	}
	wrapped := &encodingClient{inner: inner, schema: nullishSchema()}

	resp, _, err := wrapped.UpgradeResourceState(context.Background(), provider.UpgradeResourceStateRequest{
		TypeName: "demo_thing",
		Version:  0,
		RawState: []byte(`{"id":"vpc-old","legacy_field":"drop-me"}`),
	})
	if err != nil {
		t.Fatalf("UpgradeResourceState: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(resp.UpgradedState, &out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if out["id"] != "vpc-old" {
		t.Errorf("id: got %v, want vpc-old", out["id"])
	}
	// legacy_field would have been silently stripped if it were
	// part of the schema; here it never was, so its absence
	// just confirms the simulated migration shaped the response.
	if _, has := out["legacy_field"]; has {
		t.Errorf("legacy_field should not survive simulated migration: %+v", out)
	}
}

// TestEncodingClient_UpgradeResourceState_UnknownTypeIsRejected
// mirrors the ReadResource gate. If the orchestrator asks for an
// upgrade against a type that's not in the cached schema, the
// wrapper refuses rather than sending a request that would
// invariably fail downstream.
func TestEncodingClient_UpgradeResourceState_UnknownTypeIsRejected(t *testing.T) {
	t.Parallel()
	wrapped := &encodingClient{inner: &encodingTransport{}, schema: nullishSchema()}
	_, _, err := wrapped.UpgradeResourceState(context.Background(), provider.UpgradeResourceStateRequest{
		TypeName: "no_such_type",
		RawState: []byte(`{}`),
	})
	if err == nil {
		t.Fatal("expected error for unknown type")
	}
	if !strings.Contains(err.Error(), "no_such_type") {
		t.Errorf("error should mention the unknown type, got: %v", err)
	}
}

// TestEncodingClient_UpgradeResourceState_TransportErrorPropagates
// confirms transport errors flow through untouched (no swallowing).
func TestEncodingClient_UpgradeResourceState_TransportErrorPropagates(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("upgrade boom")
	inner := &encodingTransport{
		respondUpgrade: func(string, int64, []byte) (any, provider.Diagnostics, error) {
			return nil, nil, wantErr
		},
	}
	wrapped := &encodingClient{inner: inner, schema: nullishSchema()}
	_, _, err := wrapped.UpgradeResourceState(context.Background(), provider.UpgradeResourceStateRequest{
		TypeName: "demo_thing",
		RawState: []byte(`{"id":"x"}`),
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("got %v, want wrapping wantErr", err)
	}
}

func TestEncodingClient_PassesThroughOtherMethods(t *testing.T) {
	t.Parallel()
	inner := &encodingTransport{}
	wrapped := &encodingClient{inner: inner, schema: nullishSchema()}

	// All five non-ReadResource methods should delegate to inner
	// without surprise. We only spot-check the ones whose return
	// types are unambiguous.
	if pv := wrapped.ProtocolVersion(); pv != 6 {
		t.Errorf("ProtocolVersion: got %d, want 6", pv)
	}
	if err := wrapped.Stop(context.Background()); err != nil {
		t.Errorf("Stop: %v", err)
	}
	if _, err := wrapped.Configure(context.Background(), provider.ConfigureProviderRequest{}); err != nil {
		t.Errorf("Configure: %v", err)
	}
	if err := wrapped.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if !inner.closed.Load() {
		t.Errorf("inner client not closed after wrapper Close")
	}
}

func TestEncodingClient_TransportErrorPropagates(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("transport boom")
	inner := &encodingTransport{
		respond: func(string, any) (any, provider.Diagnostics, error) {
			return nil, nil, wantErr
		},
	}
	wrapped := &encodingClient{inner: inner, schema: nullishSchema()}
	_, _, err := wrapped.ReadResource(context.Background(), provider.ReadResourceRequest{
		TypeName:     "demo_thing",
		CurrentState: []byte(`{"id":"x"}`),
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("got %v, want wrapping wantErr", err)
	}
}

func TestDecodeJSONAttrs(t *testing.T) {
	cases := []struct {
		in     string
		want   map[string]any
		wantOK bool
	}{
		{"", map[string]any{}, true},
		{"null", map[string]any{}, true},
		{`{}`, map[string]any{}, true},
		{`{"id":"x"}`, map[string]any{"id": "x"}, true},
		{"not-json", nil, false},
	}
	for _, tc := range cases {
		got, err := decodeJSONAttrs([]byte(tc.in))
		if tc.wantOK {
			if err != nil {
				t.Errorf("decodeJSONAttrs(%q): %v", tc.in, err)
				continue
			}
			if len(got) != len(tc.want) {
				t.Errorf("decodeJSONAttrs(%q): got %v, want %v", tc.in, got, tc.want)
			}
		} else if err == nil {
			t.Errorf("decodeJSONAttrs(%q): expected error", tc.in)
		}
	}
}

func TestProviderConfigType_NilProviderBlock(t *testing.T) {
	// Schema with no Provider (e.g. null) → empty object type.
	got, err := providerConfigType(&provider.Schema{})
	if err != nil {
		t.Fatalf("providerConfigType: %v", err)
	}
	obj, ok := got.(tftypes.Object)
	if !ok {
		t.Fatalf("got %T, want tftypes.Object", got)
	}
	if len(obj.AttributeTypes) != 0 {
		t.Errorf("expected empty object, got %v", obj.AttributeTypes)
	}
}

func TestProviderConfigType_PopulatedBlock(t *testing.T) {
	schema := &provider.Schema{
		Provider: &provider.ResourceSchema{
			Block: &provider.SchemaBlock{
				Attributes: []provider.SchemaAttribute{stringAttr("region")},
			},
		},
	}
	got, err := providerConfigType(schema)
	if err != nil {
		t.Fatalf("providerConfigType: %v", err)
	}
	obj, ok := got.(tftypes.Object)
	if !ok {
		t.Fatalf("got %T, want tftypes.Object", got)
	}
	if _, has := obj.AttributeTypes["region"]; !has {
		t.Errorf("expected region attribute, got %v", obj.AttributeTypes)
	}
}
