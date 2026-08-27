// Package auth issues and verifies tenant API keys.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"regexp"
	"strings"
)

// KeyPrefix is the fixed marker every gateway key starts with. Scanners look
// for patterns like this, which is exactly what we want if a key leaks.
const KeyPrefix = "sk"

// secretBytes is the length of the random part of a key. 24 bytes is 192 bits
// of entropy, well past anything brute forceable.
const secretBytes = 24

var prefixPattern = regexp.MustCompile(`^[a-z0-9]{3,16}$`)

// Key is a freshly generated API key. The plaintext exists only here and in
// the response that hands it to the caller; the database stores the hash.
type Key struct {
	Plaintext string
	Hash      []byte
	LastFour  string
}

// Generate creates a key for a tenant prefix, in the form sk_<prefix>_<random>.
func Generate(tenantPrefix string) (Key, error) {
	if !prefixPattern.MatchString(tenantPrefix) {
		return Key{}, fmt.Errorf("tenant prefix must be 3-16 lowercase letters or digits, got %q", tenantPrefix)
	}

	buf := make([]byte, secretBytes)
	if _, err := rand.Read(buf); err != nil {
		return Key{}, fmt.Errorf("read random bytes: %w", err)
	}

	// URL-safe base64 without padding keeps the key copy-pasteable.
	secret := base64.RawURLEncoding.EncodeToString(buf)
	plaintext := strings.Join([]string{KeyPrefix, tenantPrefix, secret}, "_")

	return Key{
		Plaintext: plaintext,
		Hash:      HashKey(plaintext),
		LastFour:  plaintext[len(plaintext)-4:],
	}, nil
}

// HashKey returns the sha256 of a key. A plain hash is enough here and a
// deliberate choice: unlike a password, the key already has full entropy, so
// there is nothing for a slow KDF like bcrypt to protect against, and the
// lookup happens on every request.
func HashKey(plaintext string) []byte {
	sum := sha256.Sum256([]byte(plaintext))
	return sum[:]
}

// EqualHash compares two hashes in constant time.
func EqualHash(a, b []byte) bool {
	return subtle.ConstantTimeCompare(a, b) == 1
}

// ParsePrefix returns the tenant prefix encoded in a key.
func ParsePrefix(plaintext string) (string, bool) {
	parts := strings.Split(plaintext, "_")
	if len(parts) != 3 || parts[0] != KeyPrefix {
		return "", false
	}
	if !prefixPattern.MatchString(parts[1]) {
		return "", false
	}
	return parts[1], true
}

// FromHeader extracts a bearer token from an Authorization header value.
func FromHeader(header string) (string, bool) {
	const bearer = "Bearer "
	if len(header) <= len(bearer) || !strings.EqualFold(header[:len(bearer)], bearer) {
		return "", false
	}
	token := strings.TrimSpace(header[len(bearer):])
	if token == "" {
		return "", false
	}
	return token, true
}
