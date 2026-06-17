package auth

import (
	"encoding/base64"
	"net/http/httptest"
	"testing"
)

func TestStaticTokenAuthenticator_Bearer(t *testing.T) {
	a := NewStaticTokenAuthenticator("secret-token")
	req := httptest.NewRequest("GET", "/states/x", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	p, err := a.Authenticate(req)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if p.TenantID != SelfHostedTenantID || p.Source != "http-token" {
		t.Fatalf("principal: %+v", p)
	}
}

func TestStaticTokenAuthenticator_BasicPassword(t *testing.T) {
	a := NewStaticTokenAuthenticator("secret-token")
	req := httptest.NewRequest("GET", "/states/x", nil)
	req.SetBasicAuth("terraform", "secret-token")
	p, err := a.Authenticate(req)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if p.TenantID != SelfHostedTenantID {
		t.Fatalf("principal: %+v", p)
	}
}

func TestStaticTokenAuthenticator_RejectsWrongToken(t *testing.T) {
	a := NewStaticTokenAuthenticator("secret-token")
	req := httptest.NewRequest("GET", "/states/x", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	if _, err := a.Authenticate(req); err != ErrUnauthenticated {
		t.Fatalf("err = %v, want ErrUnauthenticated", err)
	}
}

func TestStaticTokenAuthenticator_RejectsMissingAuth(t *testing.T) {
	a := NewStaticTokenAuthenticator("secret-token")
	req := httptest.NewRequest("GET", "/states/x", nil)
	if _, err := a.Authenticate(req); err != ErrUnauthenticated {
		t.Fatalf("err = %v, want ErrUnauthenticated", err)
	}
}

func TestStaticTokenAuthenticator_RejectsBasicWrongPassword(t *testing.T) {
	a := NewStaticTokenAuthenticator("secret-token")
	req := httptest.NewRequest("GET", "/states/x", nil)
	req.SetBasicAuth("u", "nope")
	if _, err := a.Authenticate(req); err != ErrUnauthenticated {
		t.Fatalf("err = %v, want ErrUnauthenticated", err)
	}
}

func TestExtractCredentials_BasicViaHeader(t *testing.T) {
	raw := base64.StdEncoding.EncodeToString([]byte("acme:pass"))
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Basic "+raw)
	got, slug, ok := extractCredentials(req)
	if !ok || got != "pass" || slug != "acme" {
		t.Fatalf("got secret=%q slug=%q ok=%v", got, slug, ok)
	}
}

func TestAuthenticatorForToken_OpenWhenEmpty(t *testing.T) {
	a := AuthenticatorForToken("")
	if _, ok := a.(SingleTenantAuthenticator); !ok {
		t.Fatalf("want SingleTenantAuthenticator, got %T", a)
	}
}

func TestAuthenticatorForToken_StaticWhenSet(t *testing.T) {
	a := AuthenticatorForToken("x")
	if _, ok := a.(StaticTokenAuthenticator); !ok {
		t.Fatalf("want StaticTokenAuthenticator, got %T", a)
	}
}
