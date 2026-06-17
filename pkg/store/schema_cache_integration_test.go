//go:build integration

// Run with:
//
//	KL_DATABASE_URL=postgres://kl:kl@localhost:5432/kl?sslmode=disable \
//	  go test -tags=integration ./pkg/store/...

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/davesade/kilolock/internal/db"
	"github.com/davesade/kilolock/internal/provider"
	"github.com/davesade/kilolock/internal/testdb"
)

// jsonEqual reports whether two JSON-encoded byte slices represent
// the same value, ignoring insignificant whitespace and the byte-
// level differences Postgres JSONB introduces (e.g. inserting a
// space after `,` and `:` in arrays and objects on re-serialization).
//
// Used by schema cache tests because SchemaAttribute.Type is a
// json.RawMessage: byte-level equality is meaningless for it after
// a JSONB round-trip, but semantic equality is exactly what we
// want to verify.
func jsonEqual(a, b []byte) (bool, error) {
	var ax, bx any
	if err := json.Unmarshal(a, &ax); err != nil {
		return false, err
	}
	if err := json.Unmarshal(b, &bx); err != nil {
		return false, err
	}
	return reflect.DeepEqual(ax, bx), nil
}

// resetSchemaCache wipes the provider_schemas table. Tests in this
// file own the table; truncating before each test keeps them
// independent regardless of execution order.
func resetSchemaCache(t *testing.T, pool *db.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 5*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `TRUNCATE TABLE provider_schemas`); err != nil {
		t.Fatalf("truncate provider_schemas: %v", err)
	}
}

// sampleSchema builds a Schema that exercises the fields the cache
// needs to round-trip: top-level resources, data sources, the
// provider block, primitive attributes with cty-json types, the v6
// NestedType path, and a nested block. Hand-built so the test
// asserts on exact contents without depending on a running provider.
func sampleSchema() *provider.Schema {
	stringType := json.RawMessage(`"string"`)
	mapStringType := json.RawMessage(`["map","string"]`)

	return &provider.Schema{
		Provider: &provider.ResourceSchema{
			Version: 0,
			Block: &provider.SchemaBlock{
				Attributes: []provider.SchemaAttribute{
					{Name: "region", Type: stringType, Required: true},
				},
			},
		},
		Resources: map[string]*provider.ResourceSchema{
			"sample_resource": {
				Version: 2,
				Block: &provider.SchemaBlock{
					Version: 2,
					Attributes: []provider.SchemaAttribute{
						{Name: "id", Type: stringType, Computed: true},
						{Name: "triggers", Type: mapStringType, Optional: true},
						{
							Name: "nested",
							NestedType: &provider.SchemaObject{
								Nesting: provider.NestingSingle,
								Attributes: []provider.SchemaAttribute{
									{Name: "inner", Type: stringType, Optional: true},
								},
							},
						},
					},
					BlockTypes: []provider.SchemaNestedBlock{
						{
							TypeName: "timeouts",
							Nesting:  provider.NestingSingle,
							Block: &provider.SchemaBlock{
								Attributes: []provider.SchemaAttribute{
									{Name: "create", Type: stringType, Optional: true},
								},
							},
						},
					},
				},
			},
		},
		DataSources: map[string]*provider.ResourceSchema{
			"sample_data": {
				Version: 0,
				Block: &provider.SchemaBlock{
					Attributes: []provider.SchemaAttribute{
						{Name: "result", Type: stringType, Computed: true},
					},
				},
			},
		},
	}
}

func TestSchemaCache_PutGetRoundTrip(t *testing.T) {
	s, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	resetSchemaCache(t, pool)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 5*time.Second)
	defer cancel()

	in := sampleSchema()
	const source = "registry.terraform.io/hashicorp/sample"
	const version = "1.2.3"

	if err := s.PutProviderSchema(ctx, source, version, 6, in); err != nil {
		t.Fatalf("PutProviderSchema: %v", err)
	}

	got, err := s.GetProviderSchema(ctx, source, version)
	if err != nil {
		t.Fatalf("GetProviderSchema: %v", err)
	}

	if got.Source != source || got.Version != version {
		t.Errorf("key mismatch: got (%s, %s)", got.Source, got.Version)
	}
	if got.ProtocolVersion != 6 {
		t.Errorf("ProtocolVersion: got %d, want 6", got.ProtocolVersion)
	}
	if got.FetchedAt.IsZero() {
		t.Error("FetchedAt should be set by the DB default")
	}
	if time.Since(got.FetchedAt) > time.Minute {
		t.Errorf("FetchedAt is suspiciously old: %v", got.FetchedAt)
	}
	// Semantic JSON equality, not byte equality: Postgres JSONB
	// normalizes whitespace inside arrays/objects on re-serialization,
	// so the SchemaAttribute.Type RawMessage will not be byte-equal
	// after the round-trip even when the cty-json descriptor is the
	// same value. The schema is canonical iff the marshaled JSON
	// representations are semantically equal.
	inJSON, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal in: %v", err)
	}
	gotJSON, err := json.Marshal(got.Schema)
	if err != nil {
		t.Fatalf("marshal got: %v", err)
	}
	eq, err := jsonEqual(inJSON, gotJSON)
	if err != nil {
		t.Fatalf("jsonEqual: %v", err)
	}
	if !eq {
		ib, _ := json.MarshalIndent(in, "", "  ")
		gb, _ := json.MarshalIndent(got.Schema, "", "  ")
		t.Fatalf("schema round-trip mismatch\n--- in ---\n%s\n--- got ---\n%s", ib, gb)
	}
}

func TestSchemaCache_PutIsUpsert(t *testing.T) {
	s, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	resetSchemaCache(t, pool)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 5*time.Second)
	defer cancel()

	const source = "registry.terraform.io/hashicorp/sample"
	const version = "1.0.0"

	// First Put: small schema, protocol 5.
	first := &provider.Schema{
		Resources: map[string]*provider.ResourceSchema{
			"sample_a": {Version: 0, Block: &provider.SchemaBlock{}},
		},
	}
	if err := s.PutProviderSchema(ctx, source, version, 5, first); err != nil {
		t.Fatalf("first Put: %v", err)
	}

	got1, err := s.GetProviderSchema(ctx, source, version)
	if err != nil {
		t.Fatalf("Get after first Put: %v", err)
	}
	if got1.ProtocolVersion != 5 || len(got1.Schema.Resources) != 1 {
		t.Fatalf("first Put state unexpected: protocol=%d resources=%d",
			got1.ProtocolVersion, len(got1.Schema.Resources))
	}
	firstFetched := got1.FetchedAt

	// Same row, different content + protocol; sleep a tick so the
	// FetchedAt comparison is reliable on systems with coarse clocks.
	time.Sleep(20 * time.Millisecond)

	second := &provider.Schema{
		Resources: map[string]*provider.ResourceSchema{
			"sample_a": {Version: 0, Block: &provider.SchemaBlock{}},
			"sample_b": {Version: 0, Block: &provider.SchemaBlock{}},
			"sample_c": {Version: 1, Block: &provider.SchemaBlock{}},
		},
	}
	if err := s.PutProviderSchema(ctx, source, version, 6, second); err != nil {
		t.Fatalf("second Put: %v", err)
	}

	got2, err := s.GetProviderSchema(ctx, source, version)
	if err != nil {
		t.Fatalf("Get after second Put: %v", err)
	}
	if got2.ProtocolVersion != 6 {
		t.Errorf("ProtocolVersion after upsert: got %d, want 6", got2.ProtocolVersion)
	}
	if len(got2.Schema.Resources) != 3 {
		t.Errorf("resources after upsert: got %d, want 3", len(got2.Schema.Resources))
	}
	if !got2.FetchedAt.After(firstFetched) {
		t.Errorf("FetchedAt should advance on upsert: first=%v second=%v", firstFetched, got2.FetchedAt)
	}
}

func TestSchemaCache_GetMissingReturnsSentinel(t *testing.T) {
	s, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	resetSchemaCache(t, pool)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 5*time.Second)
	defer cancel()

	got, err := s.GetProviderSchema(ctx, "registry.terraform.io/never/cached", "0.0.0")
	if got != nil {
		t.Errorf("expected nil entry, got %+v", got)
	}
	if !errors.Is(err, ErrSchemaNotCached) {
		t.Fatalf("expected ErrSchemaNotCached, got %v", err)
	}
}

func TestSchemaCache_InvalidateRemoves(t *testing.T) {
	s, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	resetSchemaCache(t, pool)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 5*time.Second)
	defer cancel()

	const source = "registry.terraform.io/hashicorp/sample"
	const version = "9.9.9"
	if err := s.PutProviderSchema(ctx, source, version, 6, sampleSchema()); err != nil {
		t.Fatalf("Put: %v", err)
	}

	n, err := s.InvalidateProviderSchema(ctx, source, version)
	if err != nil {
		t.Fatalf("Invalidate: %v", err)
	}
	if n != 1 {
		t.Errorf("Invalidate: got %d rows deleted, want 1", n)
	}

	if _, err := s.GetProviderSchema(ctx, source, version); !errors.Is(err, ErrSchemaNotCached) {
		t.Errorf("expected ErrSchemaNotCached after invalidate, got %v", err)
	}

	// Idempotent: second invalidate returns 0, no error.
	n, err = s.InvalidateProviderSchema(ctx, source, version)
	if err != nil {
		t.Fatalf("second Invalidate: %v", err)
	}
	if n != 0 {
		t.Errorf("second Invalidate: got %d rows deleted, want 0", n)
	}
}

func TestSchemaCache_RejectsBadProtocolVersion(t *testing.T) {
	s, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	resetSchemaCache(t, pool)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 5*time.Second)
	defer cancel()

	err := s.PutProviderSchema(ctx, "x", "1", 99, &provider.Schema{})
	if err == nil {
		t.Fatal("expected error for bogus protocol version, got nil")
	}
}

func TestSchemaCache_JSONIsInlineNotBase64(t *testing.T) {
	// Catches regressions where SchemaAttribute.Type accidentally
	// reverts from json.RawMessage to []byte, which would silently
	// switch the on-disk cty-json type to a base64 string. Schemas
	// would still round-trip, but the JSONB would be ugly and
	// twice the size.
	s, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	resetSchemaCache(t, pool)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 5*time.Second)
	defer cancel()

	const source = "registry.terraform.io/hashicorp/sample"
	const version = "0.1.0"
	if err := s.PutProviderSchema(ctx, source, version, 6, sampleSchema()); err != nil {
		t.Fatalf("Put: %v", err)
	}

	var raw []byte
	err := pool.QueryRow(ctx,
		`SELECT schema_jsonb::text FROM provider_schemas WHERE provider_source = $1 AND provider_version = $2`,
		source, version,
	).Scan(&raw)
	if err != nil {
		t.Fatalf("raw select: %v", err)
	}

	// The cty-json descriptor for "string" should appear verbatim
	// in the JSONB (with surrounding quotes from the SchemaAttribute
	// containing it). Postgres jsonb adds a space after the colon
	// on re-serialization, so we match `"Type": "string"`. If Type
	// fell back to []byte, the same value would appear as base64
	// ("InN0cmluZyI=" for "\"string\"") instead.
	if !bytes.Contains(raw, []byte(`"Type": "string"`)) {
		t.Errorf("cty-json type not inlined in JSONB; found:\n%s", raw)
	}
	if bytes.Contains(raw, []byte("InN0cmluZyI=")) {
		t.Errorf("cty-json type appears base64-encoded in JSONB; found:\n%s", raw)
	}
}
