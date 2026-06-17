package main

import (
	"context"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/davesade/kilolock/internal/auth"
	"github.com/davesade/kilolock/pkg/store"
)

func TestFirstNonEmptyControlActor(t *testing.T) {
	if got := firstNonEmptyControlActor("", "  ", "alice@example.com"); got != "alice@example.com" {
		t.Fatalf("got %q", got)
	}
}

func TestControlActorFromContext(t *testing.T) {
	ctx := context.WithValue(context.Background(), controlPrincipalKey{}, auth.Principal{
		Email: "token-owner@example.com",
	})
	if got := controlActorFromContext(ctx); got != "token-owner@example.com" {
		t.Fatalf("got %q", got)
	}
}

func TestValidateRetentionPurgeApply(t *testing.T) {
	if err := validateRetentionPurgeApply(false, ""); err != nil {
		t.Fatalf("dry-run should not require reason: %v", err)
	}
	if err := validateRetentionPurgeApply(true, "billing policy"); err != nil {
		t.Fatalf("apply with reason should pass: %v", err)
	}
	err := validateRetentionPurgeApply(true, " ")
	if err == nil || !strings.Contains(err.Error(), "reason is required") {
		t.Fatalf("expected missing reason error, got: %v", err)
	}
}

func TestParseListFilter(t *testing.T) {
	v := url.Values{
		"q":         []string{" ACME "},
		"lifecycle": []string{"SUSPENDED"},
		"tier":      []string{"DEDICATED_HOST"},
		"limit":     []string{"5000"},
		"offset":    []string{"12"},
	}
	f := parseListFilter(v)
	if f.Query != "acme" || f.Lifecycle != "suspended" || f.Tier != "dedicated_host" {
		t.Fatalf("unexpected filter normalization: %+v", f)
	}
	if f.Limit != 1000 || f.Offset != 12 {
		t.Fatalf("unexpected limit/offset: %+v", f)
	}
}

func TestFilterAndPaginateTenants(t *testing.T) {
	in := []store.TenantRow{
		{Slug: "acme", Name: "Acme", LifecycleStatus: store.LifecycleStatusActive},
		{Slug: "beta", Name: "Beta", LifecycleStatus: store.LifecycleStatusSuspended},
		{Slug: "gamma", Name: "Gamma", LifecycleStatus: store.LifecycleStatusSuspended},
	}
	filtered := filterTenants(in, listFilter{Query: "a", Lifecycle: "suspended"})
	if len(filtered) != 2 {
		t.Fatalf("filtered len=%d", len(filtered))
	}
	page, total := paginateAny(filtered, 1, 1)
	if total != 2 || len(page) != 1 || page[0].Slug != "gamma" {
		t.Fatalf("unexpected page total=%d page=%+v", total, page)
	}
}

func TestFilterTokens(t *testing.T) {
	in := []store.APITokenRow{
		{Name: "bootstrap", TenantSlug: "operator", EnvSlug: "default", LifecycleStatus: store.LifecycleStatusActive},
		{Name: "acme-ci", TenantSlug: "acme", EnvSlug: "prod", LifecycleStatus: store.LifecycleStatusSuspended},
	}
	out := filterTokens(in, listFilter{Query: "acme", Lifecycle: "suspended"})
	want := []store.APITokenRow{in[1]}
	if !reflect.DeepEqual(out, want) {
		t.Fatalf("out=%+v want=%+v", out, want)
	}
}

func TestValidateEnvironmentStateAccess(t *testing.T) {
	active := store.EnvironmentRow{
		TenantSlug:      "acme",
		Slug:            "prod",
		LifecycleStatus: store.LifecycleStatusActive,
	}
	if err := validateEnvironmentStateAccess(active); err != nil {
		t.Fatalf("active env should pass: %v", err)
	}
	suspended := active
	suspended.LifecycleStatus = store.LifecycleStatusSuspended
	err := validateEnvironmentStateAccess(suspended)
	if err == nil || !strings.Contains(err.Error(), "is suspended") {
		t.Fatalf("expected suspended rejection, got %v", err)
	}
}

func TestRequireGlobalScopeForIncludeInactive_NoPrincipalAllows(t *testing.T) {
	s := &server{}
	r := httptest.NewRequest("GET", "/api/tenants?include_inactive=true", nil)
	w := httptest.NewRecorder()
	if ok := s.requireGlobalScopeForIncludeInactive(w, r, "tenant.read"); !ok {
		t.Fatalf("expected allow without scoped principal")
	}
}
