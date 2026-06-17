package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

const (
	tokenPrefix     = "kl_"
	portalPATPrefix = "klp_"
)

func newPrefixedToken(prefix string) (plaintext string, hash []byte, displayPrefix string, err error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", nil, "", fmt.Errorf("rand: %w", err)
	}
	secret := base64.RawURLEncoding.EncodeToString(raw[:])
	plaintext = prefix + secret
	hash = HashAPIToken(plaintext)
	displayPrefix = plaintext
	if len(displayPrefix) > 12 {
		displayPrefix = displayPrefix[:12] + "…"
	}
	return plaintext, hash, displayPrefix, nil
}

// NewAPIToken generates a new random API token and its SHA-256 hash.
// The plaintext is returned once to the operator; only the hash is stored.
func NewAPIToken() (plaintext string, hash []byte, displayPrefix string, err error) {
	return newPrefixedToken(tokenPrefix)
}

// NewPortalPAT generates a personal access token for a portal account.
func NewPortalPAT() (plaintext string, hash []byte, displayPrefix string, err error) {
	return newPrefixedToken(portalPATPrefix)
}

// HashAPIToken returns the SHA-256 digest of a presented token.
func HashAPIToken(plaintext string) []byte {
	sum := sha256.Sum256([]byte(plaintext))
	return sum[:]
}
