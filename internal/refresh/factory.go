package refresh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/kilolockio/kilolock/internal/provider"
	"github.com/kilolockio/kilolock/pkg/store"
)

// ProductionFactory is the v1.6c implementation of ClientFactory. It
// wires together everything built in v1.0 through v1.5:
//
//	Discover  → find the provider binary on disk (v1.4)
//	Launch    → fork the binary, negotiate protocol (v1.0/v1.1)
//	GetSchema → fetch the schema if not cached (v1.2 + v1.3a codec)
//	Configure → push the persisted config block (v1.5a/b)
//	wrap      → present a JSON-in/JSON-out Client to the orchestrator
//
// One ProductionFactory instance is intended to live for the duration
// of a single Run; it does not aggressively cache state across runs
// because schema caching already happens at the database layer
// (provider_schemas) and config storage at provider_configs. Reusing
// across runs would only save the in-memory schema lookups, which
// are cheap.
type ProductionFactory struct {
	store       *store.Store
	searchPaths []string
	logger      *slog.Logger
}

// ProductionFactoryOptions captures the variability ProductionFactory
// needs at construction time. Zero value is invalid (Store must be
// set); callers should always go through NewProductionFactory.
type ProductionFactoryOptions struct {
	// Store is required. The factory writes schemas it had to fetch
	// live, and reads provider configurations the operator persisted
	// via `kl provider configure`.
	Store *store.Store

	// SearchPaths is forwarded directly to provider.Discover. When
	// empty, Discover applies its own defaults (~/.terraform.d/...
	// and per-project .terraform/providers).
	SearchPaths []string

	// Logger receives operational log lines (cache misses, configure
	// fallbacks). nil means slog.Default().
	Logger *slog.Logger
}

// NewProductionFactory validates options and returns a factory ready
// to drive Run.
func NewProductionFactory(opts ProductionFactoryOptions) (*ProductionFactory, error) {
	if opts.Store == nil {
		return nil, errors.New("ProductionFactory: Store must not be nil")
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &ProductionFactory{
		store:       opts.Store,
		searchPaths: opts.SearchPaths,
		logger:      logger,
	}, nil
}

// Open implements ClientFactory. The contract requires that on a
// non-nil error the factory leak no resources: every successfully
// launched provider process must be terminated before Open returns.
// The unhappy paths below are written to honor that.
func (f *ProductionFactory) Open(ctx context.Context, source provider.SourceAddress, alias string) (*OpenedClient, error) {
	disco, err := provider.Discover(source, provider.DiscoveryOptions{
		SearchPaths: f.searchPaths,
	})
	if err != nil {
		return nil, fmt.Errorf("discover %s: %w", source.String(), err)
	}

	rawClient, err := provider.Launch(ctx, disco.Binary, provider.LaunchOptions{})
	if err != nil {
		return nil, fmt.Errorf("launch %s (%s): %w", source.String(), disco.Version, err)
	}

	// From here on, any error must Close rawClient before returning.
	schema, err := f.resolveSchema(ctx, source, disco.Version, rawClient)
	if err != nil {
		_ = rawClient.Close()
		return nil, err
	}

	if err := f.configure(ctx, source, alias, schema, rawClient); err != nil {
		_ = rawClient.Close()
		return nil, err
	}

	wrapped := &encodingClient{inner: rawClient, schema: schema}
	return &OpenedClient{
		Client:          wrapped,
		Source:          source,
		Alias:           alias,
		Version:         disco.Version,
		ProtocolVersion: rawClient.ProtocolVersion(),
		Schema:          schema,
	}, nil
}

// resolveSchema returns the provider's schema, preferring the
// persisted cache (provider_schemas) over a live RPC. On a cache
// miss the live result is written back so subsequent runs in any
// process get the cache hit.
//
// A failed cache write is logged but not fatal — we still have the
// schema in memory for this run. A failed cache read is fatal because
// it implies the database is broken in a way that will keep biting.
func (f *ProductionFactory) resolveSchema(
	ctx context.Context,
	source provider.SourceAddress,
	version string,
	client provider.Client,
) (*provider.Schema, error) {
	entry, err := f.store.GetProviderSchema(ctx, source.String(), version)
	if err == nil {
		return entry.Schema, nil
	}
	if !errors.Is(err, store.ErrSchemaNotCached) {
		return nil, fmt.Errorf("schema cache lookup %s@%s: %w", source.String(), version, err)
	}

	f.logger.Debug("provider schema cache miss; fetching live",
		"source", source.String(), "version", version)
	live, diags, err := client.GetSchema(ctx)
	if err != nil {
		return nil, fmt.Errorf("GetSchema %s: %w", source.String(), err)
	}
	if diags.HasError() {
		return nil, fmt.Errorf("GetSchema %s: %s", source.String(), joinDiagnostics(diags.Errors()))
	}

	if err := f.store.PutProviderSchema(ctx, source.String(), version, client.ProtocolVersion(), live); err != nil {
		f.logger.Warn("schema cache write failed; continuing with in-memory schema",
			"source", source.String(), "version", version, "err", err)
	}
	return live, nil
}

// configure runs the provider's Configure RPC with the persisted
// configuration for (source, alias), or an empty configuration if
// none is stored. Empty-config-as-default is intentional:
//
//   - null and similar configless providers Just Work without a
//     provider_configs row.
//   - Cloud providers with environment-variable credentials
//     (AWS_PROFILE, GOOGLE_APPLICATION_CREDENTIALS, ARM_CLIENT_ID,
//     etc.) work as long as the env is exported, since
//     provider.Launch passes os.Environ() through to the child.
//   - Providers that strictly require HCL-level configuration will
//     return Error diagnostics from Configure, which Open surfaces
//     as a launch failure. Operator action: run
//     `kl provider configure ...`.
func (f *ProductionFactory) configure(
	ctx context.Context,
	source provider.SourceAddress,
	alias string,
	schema *provider.Schema,
	client provider.Client,
) error {
	cfg := map[string]any{}
	entry, err := f.store.GetProviderConfig(ctx, source.String(), alias)
	switch {
	case err == nil:
		cfg = entry.Config
	case errors.Is(err, store.ErrConfigNotFound):
		f.logger.Debug("no persisted provider config; using empty config",
			"source", source.String(), "alias", alias)
	default:
		return fmt.Errorf("provider config lookup %s[%s]: %w", source.String(), alias, err)
	}

	cfgType, err := providerConfigType(schema)
	if err != nil {
		return fmt.Errorf("derive provider config type for %s: %w", source.String(), err)
	}
	encoded, err := provider.EncodeMsgpack(cfgType, cfg)
	if err != nil {
		return fmt.Errorf("encode provider config %s[%s]: %w", source.String(), alias, err)
	}

	diags, err := client.Configure(ctx, provider.ConfigureProviderRequest{Config: encoded})
	if err != nil {
		return fmt.Errorf("Configure %s: %w", source.String(), err)
	}
	if diags.HasError() {
		return fmt.Errorf("Configure %s[%s]: %s",
			source.String(), alias, joinDiagnostics(diags.Errors()))
	}
	return nil
}

// providerConfigType returns the tftypes.Type for the provider's
// configuration block, or an empty object type for providers that
// declare no config (the schema's Provider field is nil — null is
// the canonical example).
func providerConfigType(schema *provider.Schema) (tftypes.Type, error) {
	if schema == nil || schema.Provider == nil || schema.Provider.Block == nil {
		return tftypes.Object{AttributeTypes: map[string]tftypes.Type{}}, nil
	}
	return provider.BlockType(schema.Provider.Block)
}

// encodingClient wraps a provider.Client so callers can pass and
// receive resource state as JSON instead of msgpack DynamicValue
// bytes. It is the bridge between the orchestrator (which mutates
// state in JSON form, mirroring tfstate's on-disk shape) and the
// wire (which speaks msgpack-encoded cty values).
//
// Round-trip: caller sends JSON → encodingClient looks up the
// resource's BlockType from the cached schema → EncodeMsgpack into
// the request → real Client RPC → DecodeMsgpack the response →
// json.Marshal back into JSON for the caller.
//
// Methods other than ReadResource pass through unchanged. Configure
// is left to the factory because the config block's BlockType is
// known at factory time, not per call.
type encodingClient struct {
	inner  provider.Client
	schema *provider.Schema
}

// Compile-time interface conformance.
var _ provider.Client = (*encodingClient)(nil)

func (c *encodingClient) ReadResource(
	ctx context.Context,
	req provider.ReadResourceRequest,
) (*provider.ReadResourceResponse, provider.Diagnostics, error) {
	rs, ok := c.schema.Resources[req.TypeName]
	if !ok {
		return nil, nil, fmt.Errorf("type %q not declared by provider schema", req.TypeName)
	}
	if rs.Block == nil {
		return nil, nil, fmt.Errorf("type %q has nil schema block", req.TypeName)
	}
	blockType, err := provider.BlockType(rs.Block)
	if err != nil {
		return nil, nil, fmt.Errorf("compute block type for %q: %w", req.TypeName, err)
	}

	priorAttrs, err := decodeJSONAttrs(req.CurrentState)
	if err != nil {
		return nil, nil, fmt.Errorf("parse prior state JSON for %q: %w", req.TypeName, err)
	}
	encoded, err := provider.EncodeMsgpack(blockType, priorAttrs)
	if err != nil {
		return nil, nil, fmt.Errorf("encode prior state for %q: %w", req.TypeName, err)
	}

	resp, diags, err := c.inner.ReadResource(ctx, provider.ReadResourceRequest{
		TypeName:     req.TypeName,
		CurrentState: encoded,
		Private:      req.Private,
	})
	if err != nil {
		return nil, diags, err
	}
	if resp == nil {
		return nil, diags, nil
	}

	// Provider may return no NewState (rare; usually paired with a
	// Deferred response or an error diagnostic). Pass that through
	// without trying to decode.
	if len(resp.NewState) == 0 {
		return resp, diags, nil
	}

	decoded, err := provider.DecodeMsgpack(blockType, resp.NewState)
	if err != nil {
		return nil, diags, fmt.Errorf("decode new state for %q: %w", req.TypeName, err)
	}
	jsonBytes, err := json.Marshal(decoded)
	if err != nil {
		return nil, diags, fmt.Errorf("re-encode new state JSON for %q: %w", req.TypeName, err)
	}
	return &provider.ReadResourceResponse{
		NewState:            jsonBytes,
		SensitiveAttributes: resp.SensitiveAttributes,
		Private:             resp.Private,
		Deferred:            resp.Deferred,
	}, diags, nil
}

// UpgradeResourceState is asymmetric in encoding: the *request* sends
// raw JSON bytes verbatim (the provider must interpret them under
// whatever older schema produced them, and we have no way to know
// that shape from the current schema), but the *response* is a
// msgpack DynamicValue at the current schema. So we skip the inbound
// re-encode and only decode the outbound payload back to JSON for the
// orchestrator.
//
// This contract matches the wire spec exactly — RawState on the
// request is a `bytes json` field, not a DynamicValue. The
// orchestrator sends stored Attributes JSON straight through.
func (c *encodingClient) UpgradeResourceState(
	ctx context.Context,
	req provider.UpgradeResourceStateRequest,
) (*provider.UpgradeResourceStateResponse, provider.Diagnostics, error) {
	rs, ok := c.schema.Resources[req.TypeName]
	if !ok {
		return nil, nil, fmt.Errorf("type %q not declared by provider schema", req.TypeName)
	}
	if rs.Block == nil {
		return nil, nil, fmt.Errorf("type %q has nil schema block", req.TypeName)
	}
	blockType, err := provider.BlockType(rs.Block)
	if err != nil {
		return nil, nil, fmt.Errorf("compute block type for %q: %w", req.TypeName, err)
	}

	resp, diags, err := c.inner.UpgradeResourceState(ctx, req)
	if err != nil {
		return nil, diags, err
	}
	if resp == nil || len(resp.UpgradedState) == 0 {
		return resp, diags, nil
	}

	decoded, err := provider.DecodeMsgpack(blockType, resp.UpgradedState)
	if err != nil {
		return nil, diags, fmt.Errorf("decode upgraded state for %q: %w", req.TypeName, err)
	}
	jsonBytes, err := json.Marshal(decoded)
	if err != nil {
		return nil, diags, fmt.Errorf("re-encode upgraded state JSON for %q: %w", req.TypeName, err)
	}
	return &provider.UpgradeResourceStateResponse{UpgradedState: jsonBytes}, diags, nil
}

func (c *encodingClient) PlanResourceChange(ctx context.Context, req provider.PlanResourceChangeRequest) (*provider.PlanResourceChangeResponse, provider.Diagnostics, error) {
	return c.inner.PlanResourceChange(ctx, req)
}

func (c *encodingClient) GetSchema(ctx context.Context) (*provider.Schema, provider.Diagnostics, error) {
	return c.inner.GetSchema(ctx)
}

func (c *encodingClient) Configure(ctx context.Context, req provider.ConfigureProviderRequest) (provider.Diagnostics, error) {
	return c.inner.Configure(ctx, req)
}

func (c *encodingClient) Stop(ctx context.Context) error { return c.inner.Stop(ctx) }
func (c *encodingClient) ProtocolVersion() int           { return c.inner.ProtocolVersion() }
func (c *encodingClient) Close() error                   { return c.inner.Close() }

// decodeJSONAttrs is the read-side of the orchestrator's
// JSON-shaped attribute representation. An empty input decodes to
// an empty (not nil) map so encoders that depend on a non-nil
// container don't get surprised; tftypes.NewValue accepts both, but
// being explicit avoids subtle behavior drift if the codec evolves.
func decodeJSONAttrs(raw []byte) (map[string]any, error) {
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	// "null" attributes happen for the resource-not-yet-seen case
	// (rare in refresh; common in import workflows). Treat them as
	// "no attributes recorded" rather than as a JSON null.
	if string(raw) == "null" {
		return map[string]any{}, nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	if m == nil {
		return map[string]any{}, nil
	}
	return m, nil
}
