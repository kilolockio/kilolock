package auth

import (
	"context"
	"net/http/httptest"
	"testing"
)

func TestSingleTenantAuthenticator_AlwaysReturnsSelfHosted(t *testing.T) {
	a := SingleTenantAuthenticator{}
	req := httptest.NewRequest("GET", "/states/anything", nil)
	p, err := a.Authenticate(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.TenantID != SelfHostedTenantID {
		t.Errorf("TenantID = %q, want %q", p.TenantID, SelfHostedTenantID)
	}
	if p.Source != "http" {
		t.Errorf("Source = %q, want http", p.Source)
	}
}

func TestCLIPrincipal_TaggedAsCLI(t *testing.T) {
	p := CLIPrincipal()
	if p.TenantID != SelfHostedTenantID {
		t.Errorf("TenantID mismatch: %q", p.TenantID)
	}
	if p.Source != "cli" {
		t.Errorf("Source = %q, want cli", p.Source)
	}
}

func TestWithPrincipal_RoundTrips(t *testing.T) {
	want := Principal{TenantID: "11111111-1111-1111-1111-111111111111", Source: "test"}
	ctx := WithPrincipal(context.Background(), want)
	got, ok := FromContext(ctx)
	if !ok {
		t.Fatal("FromContext: principal not found")
	}
	if got != want {
		t.Errorf("FromContext: %+v, want %+v", got, want)
	}
}

func TestFromContext_BareContextReturnsFalse(t *testing.T) {
	_, ok := FromContext(context.Background())
	if ok {
		t.Error("FromContext on bare context returned ok=true")
	}
}

// TestMustFromContext_PanicsOnMissingPrincipal pins the
// defensive-programming contract: a store call without a
// principal in context is a programmer error, not a runtime
// "default to self-hosted" condition. The panic is what catches
// future code paths that forget to bootstrap auth.
func TestMustFromContext_PanicsOnMissingPrincipal(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("MustFromContext did not panic on bare context")
		}
	}()
	_ = MustFromContext(context.Background())
}

// TestMustFromContext_PanicsOnEmptyTenantID — equally important:
// a Principal in context with an empty TenantID is a *broken*
// auth implementation, not a self-hosted fallback. The panic
// surfaces the bug at the boundary instead of silently writing
// rows with tenant_id = '00000000-...-000000000000' (which would
// look fine until a SaaS deployment surfaces the cross-tenant
// leak).
func TestMustFromContext_PanicsOnEmptyTenantID(t *testing.T) {
	ctx := WithPrincipal(context.Background(), Principal{Source: "broken"})
	defer func() {
		if recover() == nil {
			t.Error("MustFromContext did not panic on empty TenantID")
		}
	}()
	_ = MustFromContext(ctx)
}

func TestTenantFromContext_ReturnsBareID(t *testing.T) {
	ctx := WithPrincipal(context.Background(), Principal{TenantID: "abc", Source: "test"})
	if got := TenantFromContext(ctx); got != "abc" {
		t.Errorf("got %q, want abc", got)
	}
}
