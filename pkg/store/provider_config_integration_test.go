//go:build integration

// Integration tests for the provider_configs store layer (v1.5b).
// Run with:
//
//	KL_DATABASE_URL=postgres://kl:kl@localhost:5432/kl?sslmode=disable \
//	  go test -tags=integration ./pkg/store/...

package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/kilolockio/kilolock/pkg/db"
	"github.com/kilolockio/kilolock/pkg/testdb"
)

// resetProviderConfigs truncates the provider_configs table between
// tests. The table is owned by the v1.5b tests in this file; no
// other suite touches it.
func resetProviderConfigs(t *testing.T, pool *db.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 5*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `TRUNCATE TABLE provider_configs`); err != nil {
		t.Fatalf("truncate provider_configs: %v", err)
	}
}

func TestProviderConfig_PutGetRoundTrip(t *testing.T) {
	st, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	resetProviderConfigs(t, pool)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 10*time.Second)
	defer cancel()

	want := map[string]any{
		"region":  "us-east-1",
		"profile": "dev",
		// Nested objects round-trip via JSONB; verify a non-scalar
		// attribute survives the trip without losing structure.
		"endpoints": map[string]any{
			"s3":  "https://s3.example.com",
			"ec2": "https://ec2.example.com",
		},
	}
	if err := st.PutProviderConfig(ctx, "registry.terraform.io/hashicorp/aws", "", want); err != nil {
		t.Fatalf("PutProviderConfig: %v", err)
	}

	got, err := st.GetProviderConfig(ctx, "registry.terraform.io/hashicorp/aws", "")
	if err != nil {
		t.Fatalf("GetProviderConfig: %v", err)
	}
	if got.Source != "registry.terraform.io/hashicorp/aws" {
		t.Errorf("Source: got %q", got.Source)
	}
	if got.Alias != "" {
		t.Errorf("Alias: got %q, want empty", got.Alias)
	}
	if !reflect.DeepEqual(got.Config, want) {
		t.Errorf("Config mismatch\n--- got ---\n%#v\n--- want ---\n%#v", got.Config, want)
	}
	if got.UpdatedAt.IsZero() {
		t.Error("UpdatedAt should be set")
	}
}

func TestProviderConfig_GetMissingReturnsSentinel(t *testing.T) {
	st, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	resetProviderConfigs(t, pool)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 5*time.Second)
	defer cancel()

	_, err := st.GetProviderConfig(ctx, "registry.terraform.io/hashicorp/aws", "")
	if !errors.Is(err, ErrConfigNotFound) {
		t.Fatalf("got %v, want ErrConfigNotFound", err)
	}
}

func TestProviderConfig_PutIsUpsert(t *testing.T) {
	st, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	resetProviderConfigs(t, pool)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 10*time.Second)
	defer cancel()

	if err := st.PutProviderConfig(ctx, "registry.terraform.io/hashicorp/aws", "", map[string]any{
		"region": "us-east-1",
	}); err != nil {
		t.Fatalf("first Put: %v", err)
	}
	first, err := st.GetProviderConfig(ctx, "registry.terraform.io/hashicorp/aws", "")
	if err != nil {
		t.Fatalf("first Get: %v", err)
	}

	// Postgres clock resolution is sub-microsecond; sleep briefly so
	// the second updated_at is observably later than the first.
	time.Sleep(10 * time.Millisecond)

	if err := st.PutProviderConfig(ctx, "registry.terraform.io/hashicorp/aws", "", map[string]any{
		"region": "us-west-2",
	}); err != nil {
		t.Fatalf("second Put: %v", err)
	}
	second, err := st.GetProviderConfig(ctx, "registry.terraform.io/hashicorp/aws", "")
	if err != nil {
		t.Fatalf("second Get: %v", err)
	}
	if second.Config["region"] != "us-west-2" {
		t.Errorf("region: got %v, want us-west-2", second.Config["region"])
	}
	if !second.UpdatedAt.After(first.UpdatedAt) {
		t.Errorf("updated_at should advance on upsert; got first=%v, second=%v", first.UpdatedAt, second.UpdatedAt)
	}
}

func TestProviderConfig_AliasesAreDistinct(t *testing.T) {
	st, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	resetProviderConfigs(t, pool)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 10*time.Second)
	defer cancel()

	if err := st.PutProviderConfig(ctx, "registry.terraform.io/hashicorp/aws", "", map[string]any{
		"region": "us-east-1",
	}); err != nil {
		t.Fatalf("Put default: %v", err)
	}
	if err := st.PutProviderConfig(ctx, "registry.terraform.io/hashicorp/aws", "west", map[string]any{
		"region": "us-west-2",
	}); err != nil {
		t.Fatalf("Put alias=west: %v", err)
	}

	def, err := st.GetProviderConfig(ctx, "registry.terraform.io/hashicorp/aws", "")
	if err != nil {
		t.Fatalf("Get default: %v", err)
	}
	west, err := st.GetProviderConfig(ctx, "registry.terraform.io/hashicorp/aws", "west")
	if err != nil {
		t.Fatalf("Get west: %v", err)
	}
	if def.Config["region"] != "us-east-1" {
		t.Errorf("default region: got %v", def.Config["region"])
	}
	if west.Config["region"] != "us-west-2" {
		t.Errorf("west region: got %v", west.Config["region"])
	}
}

func TestProviderConfig_DeleteIsIdempotent(t *testing.T) {
	st, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	resetProviderConfigs(t, pool)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 10*time.Second)
	defer cancel()

	if err := st.PutProviderConfig(ctx, "registry.terraform.io/hashicorp/aws", "", map[string]any{
		"region": "us-east-1",
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	n, err := st.DeleteProviderConfig(ctx, "registry.terraform.io/hashicorp/aws", "")
	if err != nil {
		t.Fatalf("first Delete: %v", err)
	}
	if n != 1 {
		t.Errorf("first Delete: got %d rows, want 1", n)
	}

	// Second delete: no row left, returns 0 rows, no error.
	n, err = st.DeleteProviderConfig(ctx, "registry.terraform.io/hashicorp/aws", "")
	if err != nil {
		t.Fatalf("second Delete: %v", err)
	}
	if n != 0 {
		t.Errorf("second Delete: got %d rows, want 0", n)
	}

	// And the row is actually gone.
	_, err = st.GetProviderConfig(ctx, "registry.terraform.io/hashicorp/aws", "")
	if !errors.Is(err, ErrConfigNotFound) {
		t.Fatalf("Get after Delete: got %v, want ErrConfigNotFound", err)
	}
}

func TestProviderConfig_ListOrdersDeterministically(t *testing.T) {
	st, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	resetProviderConfigs(t, pool)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 10*time.Second)
	defer cancel()

	rows := []struct {
		source string
		alias  string
		cfg    map[string]any
	}{
		// Inserted in deliberately not-sorted order; List should
		// return them sorted by (source, alias).
		{"registry.terraform.io/hashicorp/null", "", map[string]any{}},
		{"registry.terraform.io/hashicorp/aws", "west", map[string]any{"region": "us-west-2"}},
		{"registry.terraform.io/hashicorp/aws", "", map[string]any{"region": "us-east-1"}},
	}
	for _, r := range rows {
		if err := st.PutProviderConfig(ctx, r.source, r.alias, r.cfg); err != nil {
			t.Fatalf("Put %s[%s]: %v", r.source, r.alias, err)
		}
	}

	got, err := st.ListProviderConfigs(ctx)
	if err != nil {
		t.Fatalf("ListProviderConfigs: %v", err)
	}
	wantOrder := []struct{ source, alias string }{
		{"registry.terraform.io/hashicorp/aws", ""},
		{"registry.terraform.io/hashicorp/aws", "west"},
		{"registry.terraform.io/hashicorp/null", ""},
	}
	if len(got) != len(wantOrder) {
		t.Fatalf("len: got %d, want %d", len(got), len(wantOrder))
	}
	for i, w := range wantOrder {
		if got[i].Source != w.source || got[i].Alias != w.alias {
			t.Errorf("[%d]: got %s[%s], want %s[%s]", i, got[i].Source, got[i].Alias, w.source, w.alias)
		}
	}
}

func TestProviderConfig_RejectsNilConfig(t *testing.T) {
	st, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	resetProviderConfigs(t, pool)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 5*time.Second)
	defer cancel()

	err := st.PutProviderConfig(ctx, "registry.terraform.io/hashicorp/aws", "", nil)
	if err == nil {
		t.Fatal("expected error for nil config, got nil")
	}
}

func TestProviderConfig_AcceptsEmptyConfig(t *testing.T) {
	// null and similar config-free providers should be representable
	// with an empty (but non-nil) map. Verifies the JSONB CHECK
	// constraint accepts {} as a valid object.
	st, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	resetProviderConfigs(t, pool)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 5*time.Second)
	defer cancel()

	if err := st.PutProviderConfig(ctx, "registry.terraform.io/hashicorp/null", "", map[string]any{}); err != nil {
		t.Fatalf("Put empty: %v", err)
	}
	got, err := st.GetProviderConfig(ctx, "registry.terraform.io/hashicorp/null", "")
	if err != nil {
		t.Fatalf("Get empty: %v", err)
	}
	if len(got.Config) != 0 {
		t.Errorf("Config: got %#v, want empty map", got.Config)
	}
}
