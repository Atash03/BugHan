package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// NewToken returns a cryptographically random URL-safe token.
func NewToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// HashToken is the storage form of every one-time token and session/API token:
// SHA-256 hex. Lookup is by hash; the raw value is never stored.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// NewAPIToken mints a user-facing API token. Returns the full token (shown
// once) and its display prefix.
func NewAPIToken() (full, prefix string, err error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	full = "bghan_" + hex.EncodeToString(b)
	prefix = full[:len("bghan_")+6]
	return full, prefix, nil
}

// ValidAPITokenShape checks the token looks like ours before hitting the DB.
func ValidAPITokenShape(token string) bool {
	if !strings.HasPrefix(token, "bghan_") {
		return false
	}
	rest := strings.TrimPrefix(token, "bghan_")
	return len(rest) == 48 && isHex(rest)
}

func isHex(s string) bool {
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// Slugify turns a name into a URL slug.
func Slugify(name string) string {
	var b strings.Builder
	lastHyphen := true // avoid leading hyphen
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
			lastHyphen = false
		case r == ' ' || r == '-' || r == '_' || r == '.':
			if !lastHyphen {
				b.WriteByte('-')
				lastHyphen = true
			}
		}
		// Non-ASCII letters are dropped; slug stays conservative.
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "item"
	}
	return out
}
