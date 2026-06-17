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

	tfplugin6 "github.com/davesade/kilolock/internal/grpcwire/tfplugin6"
)

// pluginV6 is the go-plugin Plugin shim that lets the framework
// wrap an incoming gRPC ClientConn into a tfplugin6.ProviderClient.
// It implements only the client side; the server side is the
// provider binary itself.
type pluginV6 struct{ plugin.Plugin }

func (pluginV6) GRPCServer(*plugin.GRPCBroker, *grpc.Server) error {
	return errors.New("provider: client side does not implement provider server")
}

func (pluginV6) GRPCClient(_ context.Context, _ *plugin.GRPCBroker, c *grpc.ClientConn) (interface{}, error) {
	return &clientV6{rpc: tfplugin6.NewProviderClient(c)}, nil
}

// clientV6 implements Client over a tfprotov6 gRPC connection.
type clientV6 struct {
	rpc          tfplugin6.ProviderClient
	pluginClient *plugin.Client // owns the child process; populated by Launch
	negotiated   int

	closeOnce sync.Once
	closed    atomic.Bool
}

func (c *clientV6) ProtocolVersion() int { return c.negotiated }

func (c *clientV6) Close() error {
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		if c.pluginClient != nil {
			c.pluginClient.Kill()
		}
	})
	return nil
}

func (c *clientV6) Stop(ctx context.Context) error {
	if c.closed.Load() {
		return ErrProviderClosed
	}
	resp, err := c.rpc.StopProvider(ctx, &tfplugin6.StopProvider_Request{})
	if err != nil {
		return fmt.Errorf("provider v6 Stop: %w", err)
	}
	if resp.GetError() != "" {
		return fmt.Errorf("provider v6 Stop: %s", resp.GetError())
	}
	return nil
}

func (c *clientV6) Configure(ctx context.Context, req ConfigureProviderRequest) (Diagnostics, error) {
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
	resp, err := c.rpc.ConfigureProvider(ctx, &tfplugin6.ConfigureProvider_Request{
		TerraformVersion: tfVersion,
		Config:           &tfplugin6.DynamicValue{Msgpack: req.Config},
	})
	if err != nil {
		return nil, fmt.Errorf("provider v6 Configure: %w", err)
	}
	return convertDiagnosticsV6(resp.GetDiagnostics()), nil
}

func (c *clientV6) UpgradeResourceState(ctx context.Context, req UpgradeResourceStateRequest) (*UpgradeResourceStateResponse, Diagnostics, error) {
	if c.closed.Load() {
		return nil, nil, ErrProviderClosed
	}
	if req.TypeName == "" {
		return nil, nil, errors.New("UpgradeResourceState: empty TypeName")
	}
	if len(req.RawState) == 0 {
		return nil, nil, errors.New("UpgradeResourceState: empty RawState")
	}
	wreq := &tfplugin6.UpgradeResourceState_Request{
		TypeName: req.TypeName,
		Version:  req.Version,
		RawState: &tfplugin6.RawState{Json: req.RawState},
	}
	resp, err := c.rpc.UpgradeResourceState(ctx, wreq)
	if err != nil {
		return nil, nil, fmt.Errorf("provider v6 UpgradeResourceState: %w", err)
	}
	out := &UpgradeResourceStateResponse{}
	if us := resp.GetUpgradedState(); us != nil {
		out.UpgradedState = us.GetMsgpack()
		if len(out.UpgradedState) == 0 && len(us.GetJson()) != 0 {
			return nil, convertDiagnosticsV6(resp.GetDiagnostics()),
				errors.New("provider v6 UpgradeResourceState: response uses JSON DynamicValue; only msgpack is supported")
		}
	}
	return out, convertDiagnosticsV6(resp.GetDiagnostics()), nil
}

func (c *clientV6) ReadResource(ctx context.Context, req ReadResourceRequest) (*ReadResourceResponse, Diagnostics, error) {
	if c.closed.Load() {
		return nil, nil, ErrProviderClosed
	}
	if req.TypeName == "" {
		return nil, nil, errors.New("ReadResource: empty TypeName")
	}
	if len(req.CurrentState) == 0 {
		return nil, nil, errors.New("ReadResource: empty CurrentState (encode prior state first)")
	}
	wreq := &tfplugin6.ReadResource_Request{
		TypeName:     req.TypeName,
		CurrentState: &tfplugin6.DynamicValue{Msgpack: req.CurrentState},
		Private:      req.Private,
	}
	resp, err := c.rpc.ReadResource(ctx, wreq)
	if err != nil {
		return nil, nil, fmt.Errorf("provider v6 ReadResource: %w", err)
	}
	out := &ReadResourceResponse{
		Private: resp.GetPrivate(),
	}
	if sens, err := extractReadResourceSensitiveAttributes(resp); err != nil {
		return nil, convertDiagnosticsV6(resp.GetDiagnostics()), err
	} else if sens != nil {
		out.SensitiveAttributes = sens
	}
	if ns := resp.GetNewState(); ns != nil {
		out.NewState = ns.GetMsgpack()
		if len(out.NewState) == 0 && len(ns.GetJson()) != 0 {
			return nil, convertDiagnosticsV6(resp.GetDiagnostics()),
				errors.New("provider v6 ReadResource: response uses JSON DynamicValue; only msgpack is supported in v1.3b")
		}
	}
	if d := resp.GetDeferred(); d != nil {
		out.Deferred = &Deferred{Reason: deferredReasonFromV6(d.GetReason())}
	}
	return out, convertDiagnosticsV6(resp.GetDiagnostics()), nil
}

func (c *clientV6) PlanResourceChange(ctx context.Context, req PlanResourceChangeRequest) (*PlanResourceChangeResponse, Diagnostics, error) {
	if c.closed.Load() {
		return nil, nil, ErrProviderClosed
	}
	if req.TypeName == "" {
		return nil, nil, errors.New("PlanResourceChange: empty TypeName")
	}

	wreq := &tfplugin6.PlanResourceChange_Request{
		TypeName:         req.TypeName,
		PriorState:       &tfplugin6.DynamicValue{Msgpack: req.PriorState},
		ProposedNewState: &tfplugin6.DynamicValue{Msgpack: req.ProposedNewState},
		Config:           &tfplugin6.DynamicValue{Msgpack: req.Config},
		PriorPrivate:     req.PriorPrivate,
	}
	resp, err := c.rpc.PlanResourceChange(ctx, wreq)
	if err != nil {
		return nil, nil, fmt.Errorf("provider v6 PlanResourceChange: %w", err)
	}
	out := &PlanResourceChangeResponse{
		PlannedPrivate: resp.GetPlannedPrivate(),
	}
	if ps := resp.GetPlannedState(); ps != nil {
		out.PlannedState = ps.GetMsgpack()
		if len(out.PlannedState) == 0 && len(ps.GetJson()) != 0 {
			return nil, convertDiagnosticsV6(resp.GetDiagnostics()), errors.New("provider v6 PlanResourceChange: response uses JSON DynamicValue; only msgpack is supported")
		}
	}
	return out, convertDiagnosticsV6(resp.GetDiagnostics()), nil
}

func deferredReasonFromV6(r tfplugin6.Deferred_Reason) DeferredReason {
	switch r {
	case tfplugin6.Deferred_RESOURCE_CONFIG_UNKNOWN:
		return DeferredReasonResourceConfigUnknown
	case tfplugin6.Deferred_PROVIDER_CONFIG_UNKNOWN:
		return DeferredReasonProviderConfigUnknown
	case tfplugin6.Deferred_ABSENT_PREREQ:
		return DeferredReasonAbsentPrereq
	default:
		return DeferredReasonUnknown
	}
}

func (c *clientV6) GetSchema(ctx context.Context) (*Schema, Diagnostics, error) {
	if c.closed.Load() {
		return nil, nil, ErrProviderClosed
	}
	resp, err := c.rpc.GetProviderSchema(ctx, &tfplugin6.GetProviderSchema_Request{})
	if err != nil {
		return nil, nil, fmt.Errorf("provider v6 GetProviderSchema: %w", err)
	}

	out := &Schema{
		Resources:   make(map[string]*ResourceSchema, len(resp.GetResourceSchemas())),
		DataSources: make(map[string]*ResourceSchema, len(resp.GetDataSourceSchemas())),
	}
	if resp.GetProvider() != nil {
		out.Provider = convertResourceSchemaV6(resp.GetProvider())
	}
	for name, s := range resp.GetResourceSchemas() {
		out.Resources[name] = convertResourceSchemaV6(s)
	}
	for name, s := range resp.GetDataSourceSchemas() {
		out.DataSources[name] = convertResourceSchemaV6(s)
	}
	return out, convertDiagnosticsV6(resp.GetDiagnostics()), nil
}

func convertResourceSchemaV6(s *tfplugin6.Schema) *ResourceSchema {
	if s == nil {
		return nil
	}
	return &ResourceSchema{
		Version: s.GetVersion(),
		Block:   convertBlockV6(s.GetBlock()),
	}
}

func convertBlockV6(b *tfplugin6.Schema_Block) *SchemaBlock {
	if b == nil {
		return nil
	}
	out := &SchemaBlock{
		Version:    b.GetVersion(),
		Attributes: make([]SchemaAttribute, 0, len(b.GetAttributes())),
		BlockTypes: make([]SchemaNestedBlock, 0, len(b.GetBlockTypes())),
	}
	for _, a := range b.GetAttributes() {
		out.Attributes = append(out.Attributes, convertAttributeV6(a))
	}
	for _, n := range b.GetBlockTypes() {
		out.BlockTypes = append(out.BlockTypes, convertNestedBlockV6(n))
	}
	return out
}

func convertAttributeV6(a *tfplugin6.Schema_Attribute) SchemaAttribute {
	if a == nil {
		return SchemaAttribute{}
	}
	out := SchemaAttribute{
		Name:      a.GetName(),
		Type:      json.RawMessage(a.GetType()),
		Required:  a.GetRequired(),
		Optional:  a.GetOptional(),
		Computed:  a.GetComputed(),
		Sensitive: a.GetSensitive(),
	}
	if nt := a.GetNestedType(); nt != nil {
		out.NestedType = &SchemaObject{
			Attributes: make([]SchemaAttribute, 0, len(nt.GetAttributes())),
			Nesting:    nestingFromV6Object(nt.GetNesting()),
		}
		for _, na := range nt.GetAttributes() {
			out.NestedType.Attributes = append(out.NestedType.Attributes, convertAttributeV6(na))
		}
	}
	return out
}

func convertNestedBlockV6(n *tfplugin6.Schema_NestedBlock) SchemaNestedBlock {
	if n == nil {
		return SchemaNestedBlock{}
	}
	return SchemaNestedBlock{
		TypeName: n.GetTypeName(),
		Block:    convertBlockV6(n.GetBlock()),
		Nesting:  nestingFromV6Block(n.GetNesting()),
		MinItems: int64(n.GetMinItems()),
		MaxItems: int64(n.GetMaxItems()),
	}
}

func nestingFromV6Block(n tfplugin6.Schema_NestedBlock_NestingMode) NestingMode {
	switch n {
	case tfplugin6.Schema_NestedBlock_SINGLE:
		return NestingSingle
	case tfplugin6.Schema_NestedBlock_LIST:
		return NestingList
	case tfplugin6.Schema_NestedBlock_SET:
		return NestingSet
	case tfplugin6.Schema_NestedBlock_MAP:
		return NestingMap
	case tfplugin6.Schema_NestedBlock_GROUP:
		return NestingGroup
	default:
		return NestingInvalid
	}
}

func nestingFromV6Object(n tfplugin6.Schema_Object_NestingMode) NestingMode {
	switch n {
	case tfplugin6.Schema_Object_SINGLE:
		return NestingSingle
	case tfplugin6.Schema_Object_LIST:
		return NestingList
	case tfplugin6.Schema_Object_SET:
		return NestingSet
	case tfplugin6.Schema_Object_MAP:
		return NestingMap
	default:
		return NestingInvalid
	}
}

func convertDiagnosticsV6(in []*tfplugin6.Diagnostic) Diagnostics {
	if len(in) == 0 {
		return nil
	}
	out := make(Diagnostics, 0, len(in))
	for _, d := range in {
		if d == nil {
			continue
		}
		out = append(out, Diagnostic{
			Severity: severityFromV6(d.GetSeverity()),
			Summary:  d.GetSummary(),
			Detail:   d.GetDetail(),
		})
	}
	return out
}

func severityFromV6(s tfplugin6.Diagnostic_Severity) Severity {
	switch s {
	case tfplugin6.Diagnostic_ERROR:
		return SeverityError
	case tfplugin6.Diagnostic_WARNING:
		return SeverityWarning
	default:
		return SeverityInvalid
	}
}
