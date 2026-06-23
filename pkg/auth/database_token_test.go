package auth

import (
	"context"
	"net/http/httptest"
	"testing"
)

func TestTokenAuthenticator_PassesStateNameToLookup(t *testing.T) {
	var gotSecret, gotSlug, gotState string
	a := NewTokenAuthenticator(func(_ context.Context, secret, tenantSlug, stateName string) (Principal, error) {
		gotSecret, gotSlug, gotState = secret, tenantSlug, stateName
		return Principal{TenantID: "t1"}, nil
	})
	req := httptest.NewRequest("GET", "/states/ws_abc/env_def/project", nil)
	req.SetBasicAuth("ws_abc", "klp_secret")
	_, err := a.Authenticate(req)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if gotSecret != "klp_secret" || gotSlug != "ws_abc" || gotState != "ws_abc/env_def/project" {
		t.Fatalf("lookup args secret=%q slug=%q state=%q", gotSecret, gotSlug, gotState)
	}
}

func TestTokenAuthenticator_PassesStateNameToLookup_ForStateUnlockAlias(t *testing.T) {
	var gotSecret, gotSlug, gotState string
	a := NewTokenAuthenticator(func(_ context.Context, secret, tenantSlug, stateName string) (Principal, error) {
		gotSecret, gotSlug, gotState = secret, tenantSlug, stateName
		return Principal{TenantID: "t1"}, nil
	})
	req := httptest.NewRequest("POST", "/state-unlock/ws_abc/env_def/project", nil)
	req.SetBasicAuth("ws_abc", "klp_secret")
	_, err := a.Authenticate(req)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if gotSecret != "klp_secret" || gotSlug != "ws_abc" || gotState != "ws_abc/env_def/project" {
		t.Fatalf("lookup args secret=%q slug=%q state=%q", gotSecret, gotSlug, gotState)
	}
}

func TestTokenAuthenticator_PassesStateNameToLookup_ForVersionedStatePath(t *testing.T) {
	var gotSecret, gotSlug, gotState string
	a := NewTokenAuthenticator(func(_ context.Context, secret, tenantSlug, stateName string) (Principal, error) {
		gotSecret, gotSlug, gotState = secret, tenantSlug, stateName
		return Principal{TenantID: "t1"}, nil
	})
	req := httptest.NewRequest("GET", "/v1/states/ws_abc/env_def/project", nil)
	req.SetBasicAuth("ws_abc", "klp_secret")
	_, err := a.Authenticate(req)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if gotSecret != "klp_secret" || gotSlug != "ws_abc" || gotState != "ws_abc/env_def/project" {
		t.Fatalf("lookup args secret=%q slug=%q state=%q", gotSecret, gotSlug, gotState)
	}
}

func TestTokenAuthenticator_PassesStateNameToLookup_ForVersionedStateUnlockAlias(t *testing.T) {
	var gotSecret, gotSlug, gotState string
	a := NewTokenAuthenticator(func(_ context.Context, secret, tenantSlug, stateName string) (Principal, error) {
		gotSecret, gotSlug, gotState = secret, tenantSlug, stateName
		return Principal{TenantID: "t1"}, nil
	})
	req := httptest.NewRequest("POST", "/v1/state-unlock/ws_abc/env_def/project", nil)
	req.SetBasicAuth("ws_abc", "klp_secret")
	_, err := a.Authenticate(req)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if gotSecret != "klp_secret" || gotSlug != "ws_abc" || gotState != "ws_abc/env_def/project" {
		t.Fatalf("lookup args secret=%q slug=%q state=%q", gotSecret, gotSlug, gotState)
	}
}
