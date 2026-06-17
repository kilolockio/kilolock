package auth

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// StaticTokenAuthenticator validates a shared secret on every HTTP
// request. It accepts either form Terraform's http backend can send:
//
//   - Authorization: Bearer <token>
//   - Authorization: Basic <base64> with password equal to <token>
//     (username is ignored; Terraform sets username/password in
//     backend "http" config)
//
// Use for self-hosted and Cloud Run deployments until OIDC lands.
type StaticTokenAuthenticator struct {
	token string
}

// NewStaticTokenAuthenticator returns an Authenticator that requires
// the given token. token must be non-empty.
func NewStaticTokenAuthenticator(token string) StaticTokenAuthenticator {
	return StaticTokenAuthenticator{token: token}
}

// Authenticate implements Authenticator.
func (a StaticTokenAuthenticator) Authenticate(r *http.Request) (Principal, error) {
	got, _, ok := extractCredentials(r)
	if !ok || !secureCompare(got, a.token) {
		return Principal{}, ErrUnauthenticated
	}
	return Principal{
		TenantID: SelfHostedTenantID,
		Source:   "http-token",
	}, nil
}

func secureCompare(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// AuthenticatorForToken returns SingleTenantAuthenticator when token
// is empty (open backend — trusted network only) and
// StaticTokenAuthenticator otherwise.
func AuthenticatorForToken(token string) Authenticator {
	if strings.TrimSpace(token) == "" {
		return SingleTenantAuthenticator{}
	}
	return NewStaticTokenAuthenticator(strings.TrimSpace(token))
}
