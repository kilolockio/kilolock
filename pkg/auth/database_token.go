package auth

import (
	"context"
	"net/http"
	"strings"
)

// TokenAuthenticator looks up API tokens in persistent storage.
type TokenAuthenticator struct {
	lookup func(ctx context.Context, secret, tenantSlug, stateName string) (Principal, error)
}

// NewTokenAuthenticator returns an Authenticator backed by lookup.
// lookup receives tenantSlug from HTTP Basic username; empty for Bearer.
func NewTokenAuthenticator(lookup func(ctx context.Context, secret, tenantSlug, stateName string) (Principal, error)) TokenAuthenticator {
	return TokenAuthenticator{lookup: lookup}
}

func stateNameFromRequestPath(r *http.Request) string {
	if r != nil {
		if v := strings.TrimSpace(r.Header.Get("X-Kilolock-State-Name")); v != "" {
			return v
		}
		if v := strings.TrimSpace(r.URL.Query().Get("state_name")); v != "" {
			return v
		}
		if v := strings.TrimSpace(r.URL.Query().Get("name")); v != "" {
			return v
		}
	}
	path := strings.TrimSpace(r.URL.Path)
	if path == "" {
		return ""
	}
	path = strings.TrimPrefix(path, "/")
	path = strings.TrimPrefix(path, "states/")
	path = strings.TrimPrefix(path, "state-unlock/")
	path = strings.TrimPrefix(path, "portal/")
	path = strings.TrimPrefix(path, "/")
	return strings.TrimSpace(path)
}

// Authenticate implements Authenticator.
func (a TokenAuthenticator) Authenticate(r *http.Request) (Principal, error) {
	secret, tenantSlug, ok := extractCredentials(r)
	if !ok {
		return Principal{}, ErrUnauthenticated
	}
	return a.lookup(r.Context(), secret, tenantSlug, stateNameFromRequestPath(r))
}
