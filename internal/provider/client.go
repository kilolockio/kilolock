// Package provider speaks the Terraform Plugin RPC protocol
// (tfprotov5 and tfprotov6) directly to provider binaries, without
// going through the terraform or tofu CLI.
//
// It is the foundation for v1's refresh path: walk the normalized
// resource graph in Postgres, ask each resource's provider what the
// current cloud state looks like, and project the result back into
// the graph. See ADR 0005.
//
// The package presents a single protocol-version-agnostic Client
// interface; v5 and v6 providers both satisfy it. Construct one with
// Launch(). The Client multiplexes concurrent RPCs over a single
// gRPC connection per provider process.
//
// The surface grows as v1 needs each piece. ReadResource is wired
// here because it is the entire point of v1 (provider-aware refresh).
// Configure, PlanResourceChange, ApplyResourceChange, and
// ImportResourceState arrive in later commits when refresh has
// pulled them out of the abstract: e.g. ConfigureProvider only
// matters when we have a provider that demands configuration.
package provider

import (
	"context"
	"encoding/json"
	"errors"
)

// ErrProviderClosed is returned by Client methods after Close has
// completed (or after the provider process has exited on its own).
var ErrProviderClosed = errors.New("provider: client closed")

// Client is the protocol-version-agnostic view of a single running
// provider process. Each Client wraps one binary launched via
// go-plugin; calling Close terminates the process.
//
// All methods are safe to call concurrently from multiple goroutines
// on the same Client; the underlying gRPC connection multiplexes.
// Close is the only exception: it must not race with in-flight calls.
//
// Provider-returned Diagnostics flow back to the caller as a separate
// return value, not folded into err. The wire protocol distinguishes
// "an RPC failed" (err) from "an RPC succeeded with provider messages
// attached" (Diagnostics); callers that care about that distinction
// would lose it if we collapsed both into a single error.
type Client interface {
	// GetSchema fetches the provider's complete declared schema.
	// The (Schema, Diagnostics) pair is the wire response; err is
	// non-nil only for transport-level failures.
	GetSchema(ctx context.Context) (*Schema, Diagnostics, error)

	// Stop sends the provider's Stop RPC. This is the documented
	// mechanism for cancelling a long-running RPC running on
	// another goroutine — for example, interrupting a refresh when
	// the user's STS credentials are about to expire.
	//
	// Stop itself returns quickly; the in-flight RPC observes
	// the cancellation in its own goroutine. Stop is idempotent.
	Stop(ctx context.Context) error

	// ProtocolVersion reports the wire protocol negotiated during
	// Launch (5 or 6). The Client interface itself abstracts the
	// protocol version away; this is for diagnostics and tests.
	ProtocolVersion() int

	// Configure performs the one-time provider configuration step.
	// Most providers refuse all data RPCs (ReadResource,
	// PlanResourceChange, etc.) until Configure has succeeded —
	// it is where they parse credentials, validate region settings,
	// and so on. The exception is null, which accepts data RPCs
	// without prior configuration.
	//
	// Config in req must be the msgpack-encoded provider config
	// block. Callers compute this by:
	//
	//   - if Schema.Provider is non-nil:
	//       typ := BlockType(Schema.Provider.Block)
	//       config := EncodeMsgpack(typ, configAttrs)
	//
	//   - if Schema.Provider is nil (e.g. null):
	//       config := EncodeMsgpack(tftypes.Object{}, map[string]any{})
	//
	// Diagnostics with Error severity mean configuration failed;
	// subsequent RPCs will also fail until a successful Configure.
	// Warnings are informational and do not block.
	Configure(ctx context.Context, req ConfigureProviderRequest) (Diagnostics, error)

	// UpgradeResourceState runs the provider's stored-state
	// upgrade logic for a resource type whose persisted
	// schema_version is older than the live schema's Version.
	// Without this call, ReadResource against a stale-schema
	// resource will return diagnostics like "resource has invalid
	// schema version" because the provider has no way to migrate
	// the document on its own. The wire RPC accepts the raw JSON
	// bytes Terraform stored, *not* a re-encoded DynamicValue —
	// the provider may have to interpret the bytes under the older
	// (no-longer-published) schema, so they pass through
	// uninterpreted.
	//
	// RawState in req must be the JSON-shaped bytes as they live
	// in the .tfstate document (i.e. Resource.Instances[i].
	// Attributes verbatim). Version is the schema_version recorded
	// alongside those bytes.
	//
	// UpgradedState in the response is the msgpack-encoded
	// DynamicValue at the *current* schema, suitable to feed
	// straight into ReadResource as CurrentState. Decode via
	// DecodeMsgpack with the current resource block's
	// BlockType when you need the Go-typed form (the orchestrator
	// uses encodingClient to round-trip through JSON
	// transparently).
	//
	// Diagnostics with Severity Error mean the upgrade failed;
	// the caller must surface them and skip subsequent RPCs for
	// the resource. Warnings are informational and accompany a
	// usable UpgradedState.
	UpgradeResourceState(ctx context.Context, req UpgradeResourceStateRequest) (*UpgradeResourceStateResponse, Diagnostics, error)

	// ReadResource asks the provider what a single existing
	// resource currently looks like in the cloud, given its prior
	// state. This is the heart of v1's refresh path.
	//
	// CurrentState in req must be the msgpack-encoded DynamicValue
	// of the resource's prior state (produced by EncodeMsgpack with
	// the resource's BlockType). The returned NewState has the same
	// shape on the wire.
	//
	// Diagnostics with Severity Error indicate the response is not
	// usable; callers should treat the resource as un-refreshed and
	// surface the diagnostics. Warnings are informational and
	// accompany a usable NewState.
	//
	// The Deferred field of the response (v6 only) signals that
	// the provider intentionally declined to compute new state.
	// Refresh callers should treat this as "no new state available"
	// rather than an error, but v1.3b does not pretend to handle
	// the deferral protocol — we surface it via a non-nil
	// Response.Deferred and let the caller decide.
	ReadResource(ctx context.Context, req ReadResourceRequest) (*ReadResourceResponse, Diagnostics, error)

	// PlanResourceChange asks the provider to plan changes for a resource
	// given its prior state, proposed new config, and any prior private data.
	PlanResourceChange(ctx context.Context, req PlanResourceChangeRequest) (*PlanResourceChangeResponse, Diagnostics, error)

	// Close terminates the provider process and releases all
	// associated resources (gRPC connection, child stdio, etc.).
	// Subsequent method calls return ErrProviderClosed.
	//
	// Close is safe to call multiple times; subsequent calls are
	// no-ops and return nil.
	Close() error
}

// DefaultReportedTerraformVersion is the Terraform version Configure
// reports to providers when the caller leaves
// ConfigureProviderRequest.TerraformVersion empty.
//
// Some providers gate behavior on the calling Terraform version
// (typically with rules like "feature X requires Terraform >= 1.5"),
// and most parse this with a strict semver matcher that rejects
// anything not shaped like \d+\.\d+\.\d+. Reporting a recent stable
// Terraform release maximizes compatibility without misleading
// providers about API generation.
//
// This is a default, not a lie: callers wanting to identify
// Kilolock explicitly can pass any string they like (e.g.
// "1.9.8-kl"); providers that care will still version-match
// the numeric prefix.
const DefaultReportedTerraformVersion = "1.9.8"

// ConfigureProviderRequest carries the inputs to Client.Configure.
//
// ClientCapabilities is not exposed here: it signals optional
// features the client supports (deferred actions, forwards-
// compatible state changes, etc.). The default zero value
// translates to "client does not opt into anything optional",
// which is exactly what v1 wants — refresh has no need for any
// of the toggles, and opting in unintentionally could change
// provider response shapes.
type ConfigureProviderRequest struct {
	// Config is the msgpack-encoded provider configuration block.
	// See Client.Configure documentation for how to compute it.
	// Must not be empty; an empty config block must still be
	// encoded as msgpack-of-empty-object, not literally nothing.
	Config []byte

	// TerraformVersion is the version string the provider sees.
	// Empty means use DefaultReportedTerraformVersion.
	TerraformVersion string
}

// UpgradeResourceStateRequest carries the inputs to
// Client.UpgradeResourceState. The wire message also accepts a
// legacy flatmap-encoded payload (`RawState.flatmap`) used by
// Terraform 0.11-era state. We don't support flatmap input because
// Kilolock imports `.tfstate` v4+ exclusively (see internal/
// tfstate), and v4 has always been JSON-shaped. Add flatmap support
// here if a real consumer surfaces.
type UpgradeResourceStateRequest struct {
	// TypeName is the resource type, e.g. "aws_s3_bucket". Must be
	// present in the provider's schema. Must not be empty.
	TypeName string

	// Version is the schema_version recorded against the resource
	// instance in state. Zero is valid (it's the implicit default
	// for resources predating schema versioning). The provider
	// uses this to decide which upgrade ladder to run.
	Version int64

	// RawState is the JSON-shaped attributes bytes from state.
	// Pass `Resource.Instances[i].Attributes` here verbatim. Must
	// not be empty; an upgrade of a non-existent resource is a
	// caller bug, not a provider concern.
	RawState []byte
}

// UpgradeResourceStateResponse carries the response from
// Client.UpgradeResourceState. UpgradedState is the upgraded resource
// state ready to feed into ReadResource as CurrentState.
type UpgradeResourceStateResponse struct {
	// UpgradedState is the msgpack-encoded DynamicValue of the
	// resource at the current schema. Always msgpack on the wire
	// today; the response is decodable with DecodeMsgpack against
	// the current schema's BlockType.
	UpgradedState []byte
}

// ReadResourceRequest carries the inputs to Client.ReadResource. The
// shape mirrors the wire request, minus the fields v1 refresh doesn't
// need:
//
//   - provider_meta: not used until we encounter a provider that
//     declares one. Most don't.
//   - client_capabilities: signals to the provider what optional
//     features the client supports. The default zero value (which
//     translates to "nothing optional supported") is exactly what
//     v1 refresh wants — we explicitly do not opt into deferred
//     actions yet.
//   - current_identity: a recent addition tied to import-by-identity
//     workflows; refresh doesn't drive identity.
//
// Add fields here when a real consumer needs them, not before.
type ReadResourceRequest struct {
	// TypeName is the resource type, e.g. "null_resource" or
	// "aws_s3_bucket". Must be present in the provider's schema.
	TypeName string

	// CurrentState is the msgpack-encoded DynamicValue of the
	// resource's prior state, as stored in the graph and produced
	// by EncodeMsgpack. The wire also accepts JSON encoding via
	// DynamicValue.json; v1.3b only sends msgpack.
	CurrentState []byte

	// Private is the opaque per-resource bytes the provider returned
	// from the previous ReadResource (or ApplyResourceChange) call.
	// Most providers don't use this; pass it through unchanged.
	Private []byte
}

// ReadResourceResponse is Client.ReadResource's output. NewState is
// the refreshed wire value; decode it with DecodeMsgpack using the
// resource's BlockType to recover a JSON-shaped Go value suitable for
// storing back to Postgres.
type ReadResourceResponse struct {
	// NewState is the msgpack-encoded DynamicValue the provider
	// returned. May be nil when the provider could not produce new
	// state (rare but legal — see Deferred).
	NewState []byte

	// SensitiveAttributes is Terraform's state-file encoding of
	// dynamically-sensitive attribute paths discovered at runtime.
	//
	// When present, this is JSON shaped exactly like a v4 state file's
	// `instances[].sensitive_attributes` field: a list of paths, where
	// each path is a list of steps (strings for attribute names / map
	// keys; numbers for list indices).
	//
	// If absent (nil), callers should preserve whatever sensitive list
	// was already recorded in state: static schema sensitivity is
	// re-derived by Terraform from the schema on load, and not all
	// providers report dynamic sensitivity.
	SensitiveAttributes json.RawMessage

	// Private is the new opaque per-resource bytes. Persist these
	// alongside the resource state; pass them back unchanged on the
	// next call.
	Private []byte

	// Deferred is non-nil if the provider chose to defer the read.
	// v1.3b does not implement the deferral protocol; the field is
	// exposed so callers can detect the case and decide what to do
	// (typically: keep the prior state, surface a warning).
	Deferred *Deferred
}

// Deferred is the protocol-version-agnostic mirror of the wire's
// Deferred message. v6 carries a reason enum; v5 has no such
// concept. v1 callers typically only need to know "the provider
// punted"; the Reason is provided for diagnostic logging.
type Deferred struct {
	Reason DeferredReason
}

// DeferredReason mirrors the v6 enum values. Values not enumerated
// here become DeferredReasonUnknown so we never silently drop a new
// reason a provider may emit in the future.
type DeferredReason uint8

const (
	DeferredReasonUnknown DeferredReason = iota
	DeferredReasonResourceConfigUnknown
	DeferredReasonProviderConfigUnknown
	DeferredReasonAbsentPrereq
)

// String reports a short symbolic name for diagnostic output.
func (r DeferredReason) String() string {
	switch r {
	case DeferredReasonResourceConfigUnknown:
		return "resource_config_unknown"
	case DeferredReasonProviderConfigUnknown:
		return "provider_config_unknown"
	case DeferredReasonAbsentPrereq:
		return "absent_prereq"
	default:
		return "unknown"
	}
}

// Schema is the protocol-version-agnostic view of a provider's
// declared schema, as returned by GetSchema (protocol v5) or
// GetProviderSchema (protocol v6).
//
// Several fields the wire protocol carries are intentionally omitted
// from this struct until a refresh-path consumer needs them.
// Examples: provider-meta, ephemeral-resource schemas, identity
// schemas, function schemas, deferred actions, server capabilities.
// Add them when proven necessary; do not pre-populate.
type Schema struct {
	// Provider is the schema for the provider's own configuration
	// block (the `provider "aws" { ... }` block in HCL). It is nil
	// for providers that take no configuration, like null.
	Provider *ResourceSchema

	// Resources is keyed by resource type name (e.g. "aws_s3_bucket").
	Resources map[string]*ResourceSchema

	// DataSources is keyed by data source type name.
	DataSources map[string]*ResourceSchema
}

// ResourceSchema is a single resource or data source's schema.
type ResourceSchema struct {
	// Version is the schema version this provider currently
	// publishes. State persisted at a lower version must be passed
	// through UpgradeResourceState before any other RPC accepts it.
	Version int64

	// Block is the structural schema for the resource body.
	Block *SchemaBlock
}

// SchemaBlock describes the attributes and nested blocks that make
// up a resource (or provider config) body.
type SchemaBlock struct {
	// Version is duplicated from ResourceSchema.Version for the
	// special case of nested blocks that carry their own version
	// (rare; included here to mirror the wire protocol exactly).
	Version int64

	Attributes []SchemaAttribute
	BlockTypes []SchemaNestedBlock
}

// SchemaAttribute is a single named attribute on a resource body.
//
// Protocol v5 exposes types purely through Type (a JSON-encoded cty
// type descriptor). Protocol v6 adds NestedType for the
// nested-attribute model used by terraform-plugin-framework
// providers; for those attributes, Type is nil and NestedType is
// populated. Consumers should check NestedType != nil first.
type SchemaAttribute struct {
	Name string

	// Type is the cty type descriptor as JSON bytes, exactly as
	// the provider sent it. The format matches
	// tftypes.Type.MarshalJSON. Opaque to most consumers; the
	// encoder package added in a later commit will parse it.
	//
	// json.RawMessage rather than []byte so that round-tripping
	// a Schema through encoding/json keeps the cty-json inline
	// instead of base64-encoding it.
	Type json.RawMessage

	// NestedType is populated only by v6 providers using the
	// nested-attribute model. Mutually exclusive with Type.
	NestedType *SchemaObject

	Required  bool
	Optional  bool
	Computed  bool
	Sensitive bool
}

// SchemaNestedBlock is a block-typed nested element on a resource.
type SchemaNestedBlock struct {
	TypeName string
	Block    *SchemaBlock
	Nesting  NestingMode
	MinItems int64
	MaxItems int64
}

// SchemaObject is the inner shape of a NestedType-using attribute on
// a v6 provider. It mirrors SchemaBlock structurally but is never
// itself a block.
type SchemaObject struct {
	Attributes []SchemaAttribute
	Nesting    NestingMode
}

// NestingMode mirrors the wire protocol enum used by both nested
// blocks and nested attributes. The numeric values do not match the
// proto enums verbatim; do not cast between them. Use the package
// constants below.
type NestingMode uint8

const (
	NestingInvalid NestingMode = iota
	NestingSingle
	NestingList
	NestingSet
	NestingMap
	NestingGroup
)

// String reports a short symbolic name. Used in test failure
// messages, not in user-facing output.
func (n NestingMode) String() string {
	switch n {
	case NestingSingle:
		return "single"
	case NestingList:
		return "list"
	case NestingSet:
		return "set"
	case NestingMap:
		return "map"
	case NestingGroup:
		return "group"
	default:
		return "invalid"
	}
}

type PlanResourceChangeRequest struct {
	TypeName         string
	PriorState       []byte // msgpack-encoded cty.Value
	ProposedNewState []byte // msgpack-encoded cty.Value
	Config           []byte // msgpack-encoded cty.Value
	PriorPrivate     []byte
}

type PlanResourceChangeResponse struct {
	PlannedState   []byte // msgpack-encoded cty.Value
	PlannedPrivate []byte
}
