package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	plugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"

	tfplugin5 "github.com/kilolockio/kilolock/internal/grpcwire/tfplugin5"
)

// pluginV5 is the go-plugin Plugin shim for protocol v5. Mirror of
// pluginV6 — see that file for the rationale.
type pluginV5 struct{ plugin.Plugin }

func (pluginV5) GRPCServer(*plugin.GRPCBroker, *grpc.Server) error {
	return errors.New("provider: client side does not implement provider server")
}

func (pluginV5) GRPCClient(_ context.Context, _ *plugin.GRPCBroker, c *grpc.ClientConn) (interface{}, error) {
	return &clientV5{rpc: tfplugin5.NewProviderClient(c)}, nil
}

// clientV5 implements Client over a tfprotov5 gRPC connection.
type clientV5 struct {
	rpc          tfplugin5.ProviderClient
	pluginClient *plugin.Client
	negotiated   int

	closeOnce sync.Once
	closed    atomic.Bool
}

func (c *clientV5) ProtocolVersion() int { return c.negotiated }

func (c *clientV5) Close() error {
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		if c.pluginClient != nil {
			c.pluginClient.Kill()
		}
	})
	return nil
}

func (c *clientV5) Stop(ctx context.Context) error {
	if c.closed.Load() {
		return ErrProviderClosed
	}
	resp, err := c.rpc.Stop(ctx, &tfplugin5.Stop_Request{})
	if err != nil {
		return fmt.Errorf("provider v5 Stop: %w", err)
	}
	if resp.GetError() != "" {
		return fmt.Errorf("provider v5 Stop: %s", resp.GetError())
	}
	return nil
}

func (c *clientV5) Configure(ctx context.Context, req ConfigureProviderRequest) (Diagnostics, error) {
	if c.closed.Load() {
		return nil, ErrProviderClosed
	}
	if len(req.Config) == 0 {
		return nil, errors.New("Configure: empty Config (encode an empty object for unconfigured providers)")
	}
	tfVersion := req.TerraformVersion
	if tfVersion == "" {
		tfVersion = DefaultReportedTerraformVersion
	}
	resp, err := c.rpc.Configure(ctx, &tfplugin5.Configure_Request{
		TerraformVersion: tfVersion,
		Config:           &tfplugin5.DynamicValue{Msgpack: req.Config},
	})
	if err != nil {
		return nil, fmt.Errorf("provider v5 Configure: %w", err)
	}
	return convertDiagnosticsV5(resp.GetDiagnostics()), nil
}

func (c *clientV5) UpgradeResourceState(ctx context.Context, req UpgradeResourceStateRequest) (*UpgradeResourceStateResponse, Diagnostics, error) {
	if c.closed.Load() {
		return nil, nil, ErrProviderClosed
	}
	if req.TypeName == "" {
		return nil, nil, errors.New("UpgradeResourceState: empty TypeName")
	}
	if len(req.RawState) == 0 {
		return nil, nil, errors.New("UpgradeResourceState: empty RawState")
	}
	wreq := &tfplugin5.UpgradeResourceState_Request{
		TypeName: req.TypeName,
		Version:  req.Version,
		RawState: &tfplugin5.RawState{Json: req.RawState},
	}
	resp, err := c.rpc.UpgradeResourceState(ctx, wreq)
	if err != nil {
		return nil, nil, fmt.Errorf("provider v5 UpgradeResourceState: %w", err)
	}
	out := &UpgradeResourceStateResponse{}
	if us := resp.GetUpgradedState(); us != nil {
		out.UpgradedState = us.GetMsgpack()
		// Same JSON-vs-msgpack guard ReadResource has: we do not
		// support the JSON encoding today. If the provider sent
		// JSON, surface it explicitly instead of silently
		// returning empty bytes (which would then trip
		// ReadResource's "empty CurrentState" check with a less
		// helpful error).
		if len(out.UpgradedState) == 0 && len(us.GetJson()) != 0 {
			return nil, convertDiagnosticsV5(resp.GetDiagnostics()),
				errors.New("provider v5 UpgradeResourceState: response uses JSON DynamicValue; only msgpack is supported")
		}
	}
	return out, convertDiagnosticsV5(resp.GetDiagnostics()), nil
}

func (c *clientV5) ReadResource(ctx context.Context, req ReadResourceRequest) (*ReadResourceResponse, Diagnostics, error) {
	if c.closed.Load() {
		return nil, nil, ErrProviderClosed
	}
	if req.TypeName == "" {
		return nil, nil, errors.New("ReadResource: empty TypeName")
	}
	if len(req.CurrentState) == 0 {
		return nil, nil, errors.New("ReadResource: empty CurrentState (encode prior state first)")
	}
	wreq := &tfplugin5.ReadResource_Request{
		TypeName:     req.TypeName,
		CurrentState: &tfplugin5.DynamicValue{Msgpack: req.CurrentState},
		Private:      req.Private,
	}
	resp, err := c.rpc.ReadResource(ctx, wreq)
	if err != nil {
		return nil, nil, fmt.Errorf("provider v5 ReadResource: %w", err)
	}
	out := &ReadResourceResponse{
		Private: resp.GetPrivate(),
	}
	if ns := resp.GetNewState(); ns != nil {
		// Providers can return either msgpack or json; v1.3b only
		// understands msgpack. Surface the JSON-only case explicitly
		// rather than silently returning empty NewState.
		out.NewState = ns.GetMsgpack()
		if len(out.NewState) == 0 && len(ns.GetJson()) != 0 {
			return nil, convertDiagnosticsV5(resp.GetDiagnostics()),
				errors.New("provider v5 ReadResource: response uses JSON DynamicValue; only msgpack is supported in v1.3b")
		}
	}
	if d := resp.GetDeferred(); d != nil {
		out.Deferred = &Deferred{Reason: deferredReasonFromV5(d.GetReason())}
	}
	return out, convertDiagnosticsV5(resp.GetDiagnostics()), nil
}

func (c *clientV5) PlanResourceChange(ctx context.Context, req PlanResourceChangeRequest) (*PlanResourceChangeResponse, Diagnostics, error) {
	if c.closed.Load() {
		return nil, nil, ErrProviderClosed
	}
	if req.TypeName == "" {
		return nil, nil, errors.New("PlanResourceChange: empty TypeName")
	}

	wreq := &tfplugin5.PlanResourceChange_Request{
		TypeName:         req.TypeName,
		PriorState:       &tfplugin5.DynamicValue{Msgpack: req.PriorState},
		ProposedNewState: &tfplugin5.DynamicValue{Msgpack: req.ProposedNewState},
		Config:           &tfplugin5.DynamicValue{Msgpack: req.Config},
		PriorPrivate:     req.PriorPrivate,
	}
	resp, err := c.rpc.PlanResourceChange(ctx, wreq)
	if err != nil {
		return nil, nil, fmt.Errorf("provider v5 PlanResourceChange: %w", err)
	}
	out := &PlanResourceChangeResponse{
		PlannedPrivate: resp.GetPlannedPrivate(),
	}
	if ps := resp.GetPlannedState(); ps != nil {
		out.PlannedState = ps.GetMsgpack()
		if len(out.PlannedState) == 0 && len(ps.GetJson()) != 0 {
			return nil, convertDiagnosticsV5(resp.GetDiagnostics()), errors.New("provider v5 PlanResourceChange: response uses JSON DynamicValue; only msgpack is supported")
		}
	}
	return out, convertDiagnosticsV5(resp.GetDiagnostics()), nil
}

func deferredReasonFromV5(r tfplugin5.Deferred_Reason) DeferredReason {
	switch r {
	case tfplugin5.Deferred_RESOURCE_CONFIG_UNKNOWN:
		return DeferredReasonResourceConfigUnknown
	case tfplugin5.Deferred_PROVIDER_CONFIG_UNKNOWN:
		return DeferredReasonProviderConfigUnknown
	case tfplugin5.Deferred_ABSENT_PREREQ:
		return DeferredReasonAbsentPrereq
	default:
		return DeferredReasonUnknown
	}
}

func (c *clientV5) GetSchema(ctx context.Context) (*Schema, Diagnostics, error) {
	if c.closed.Load() {
		return nil, nil, ErrProviderClosed
	}
	// In tfprotov5 the wire method is named GetSchema but it still
	// takes a GetProviderSchema_Request (historical artifact from
	// the v5 → v6 rename).
	resp, err := c.rpc.GetSchema(ctx, &tfplugin5.GetProviderSchema_Request{})
	if err != nil {
		return nil, nil, fmt.Errorf("provider v5 GetSchema: %w", err)
	}

	out := &Schema{
		Resources:   make(map[string]*ResourceSchema, len(resp.GetResourceSchemas())),
		DataSources: make(map[string]*ResourceSchema, len(resp.GetDataSourceSchemas())),
	}
	if resp.GetProvider() != nil {
		out.Provider = convertResourceSchemaV5(resp.GetProvider())
	}
	for name, s := range resp.GetResourceSchemas() {
		out.Resources[name] = convertResourceSchemaV5(s)
	}
	for name, s := range resp.GetDataSourceSchemas() {
		out.DataSources[name] = convertResourceSchemaV5(s)
	}
	return out, convertDiagnosticsV5(resp.GetDiagnostics()), nil
}

func convertResourceSchemaV5(s *tfplugin5.Schema) *ResourceSchema {
	if s == nil {
		return nil
	}
	return &ResourceSchema{
		Version: s.GetVersion(),
		Block:   convertBlockV5(s.GetBlock()),
	}
}

func convertBlockV5(b *tfplugin5.Schema_Block) *SchemaBlock {
	if b == nil {
		return nil
	}
	out := &SchemaBlock{
		Version:    b.GetVersion(),
		Attributes: make([]SchemaAttribute, 0, len(b.GetAttributes())),
		BlockTypes: make([]SchemaNestedBlock, 0, len(b.GetBlockTypes())),
	}
	for _, a := range b.GetAttributes() {
		out.Attributes = append(out.Attributes, convertAttributeV5(a))
	}
	for _, n := range b.GetBlockTypes() {
		out.BlockTypes = append(out.BlockTypes, convertNestedBlockV5(n))
	}
	return out
}

func convertAttributeV5(a *tfplugin5.Schema_Attribute) SchemaAttribute {
	if a == nil {
		return SchemaAttribute{}
	}
	// tfprotov5 has no NestedType — nested attributes are a v6
	// addition. All complex types come through as cty-json on Type.
	return SchemaAttribute{
		Name:      a.GetName(),
		Type:      json.RawMessage(a.GetType()),
		Required:  a.GetRequired(),
		Optional:  a.GetOptional(),
		Computed:  a.GetComputed(),
		Sensitive: a.GetSensitive(),
	}
}

func convertNestedBlockV5(n *tfplugin5.Schema_NestedBlock) SchemaNestedBlock {
	if n == nil {
		return SchemaNestedBlock{}
	}
	return SchemaNestedBlock{
		TypeName: n.GetTypeName(),
		Block:    convertBlockV5(n.GetBlock()),
		Nesting:  nestingFromV5Block(n.GetNesting()),
		MinItems: int64(n.GetMinItems()),
		MaxItems: int64(n.GetMaxItems()),
	}
}

func nestingFromV5Block(n tfplugin5.Schema_NestedBlock_NestingMode) NestingMode {
	switch n {
	case tfplugin5.Schema_NestedBlock_SINGLE:
		return NestingSingle
	case tfplugin5.Schema_NestedBlock_LIST:
		return NestingList
	case tfplugin5.Schema_NestedBlock_SET:
		return NestingSet
	case tfplugin5.Schema_NestedBlock_MAP:
		return NestingMap
	case tfplugin5.Schema_NestedBlock_GROUP:
		return NestingGroup
	default:
		return NestingInvalid
	}
}

func convertDiagnosticsV5(in []*tfplugin5.Diagnostic) Diagnostics {
	if len(in) == 0 {
		return nil
	}
	out := make(Diagnostics, 0, len(in))
	for _, d := range in {
		if d == nil {
			continue
		}
		out = append(out, Diagnostic{
			Severity: severityFromV5(d.GetSeverity()),
			Summary:  d.GetSummary(),
			Detail:   d.GetDetail(),
		})
	}
	return out
}

func severityFromV5(s tfplugin5.Diagnostic_Severity) Severity {
	switch s {
	case tfplugin5.Diagnostic_ERROR:
		return SeverityError
	case tfplugin5.Diagnostic_WARNING:
		return SeverityWarning
	default:
		return SeverityInvalid
	}
}
