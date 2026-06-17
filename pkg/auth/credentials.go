package auth

import (
	"net/http"
	"strings"
)

// extractCredentials returns the secret, optional tenant slug (Basic
// username), and whether Authorization was present and recognized.
func extractCredentials(r *http.Request) (secret, tenantSlug string, ok bool) {
	h := strings.TrimSpace(r.Header.Get("Authorization"))
	if h == "" {
		return "", "", false
	}
	lower := strings.ToLower(h)
	if strings.HasPrefix(lower, "bearer ") {
		t := strings.TrimSpace(h[7:])
		return t, "", t != ""
	}
	if strings.HasPrefix(lower, "basic ") {
		user, pass, ok := r.BasicAuth()
		if !ok || pass == "" {
			return "", "", false
		}
		return pass, user, true
	}
	return "", "", false
}
