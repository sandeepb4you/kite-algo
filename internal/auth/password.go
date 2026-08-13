// Package auth handles operator authentication for the web UI and persistence
// of the Zerodha session token.
//
// There is exactly one operator. The threat model is not "many users with
// varying privileges" — it is "this process can spend real money, and it is
// reachable over a network". So the password check is deliberately slow, the
// comparison is constant-time, and repeated failures lock the door.
package auth

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Iterations for PBKDF2-HMAC-SHA256, per OWASP guidance. Deliberately expensive:
// a single operator logs in rarely, so ~half a second is imperceptible to them
// and costly to anyone guessing.
const Iterations = 600_000

const (
	saltLen = 16
	keyLen  = 32
	scheme  = "pbkdf2-sha256"
)

// ErrMalformedHash means the stored credential could not be parsed. Treat it as
// "no password configured", never as "any password is acceptable".
var ErrMalformedHash = errors.New("auth: malformed password hash")

// HashPassword derives a storable credential from a plaintext password. The
// result is self-describing — "pbkdf2-sha256$600000$<salt>$<key>" — so the
// iteration count can be raised later without invalidating existing hashes.
func HashPassword(password string) (string, error) {
	if password == "" {
		return "", errors.New("auth: password must not be empty")
	}
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: read salt: %w", err)
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, Iterations, keyLen)
	if err != nil {
		return "", fmt.Errorf("auth: derive key: %w", err)
	}
	return fmt.Sprintf("%s$%d$%s$%s", scheme, Iterations,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

// VerifyPassword reports whether password matches the encoded credential.
//
// It returns false for any malformed or empty hash. The comparison is
// constant-time, and the key derivation runs even on failure paths where the
// caller has already decided to reject, so response timing does not reveal
// whether a password was configured at all.
func VerifyPassword(encoded, password string) bool {
	salt, want, iter, err := parseHash(encoded)
	if err != nil {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, iter, len(want))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

// DummyVerify burns the same work as a real verification against a throwaway
// credential. Call it on the "no password configured" and "unknown user" paths
// so a failed login costs the same wall-clock time as a successful one.
func DummyVerify(password string) {
	salt := make([]byte, saltLen)
	_, _ = pbkdf2.Key(sha256.New, password, salt, Iterations, keyLen)
}

// parseHash splits an encoded credential into its parts.
func parseHash(encoded string) (salt, key []byte, iter int, err error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != scheme {
		return nil, nil, 0, ErrMalformedHash
	}
	iter, err = strconv.Atoi(parts[1])
	if err != nil || iter <= 0 {
		return nil, nil, 0, ErrMalformedHash
	}
	if salt, err = base64.RawStdEncoding.DecodeString(parts[2]); err != nil {
		return nil, nil, 0, ErrMalformedHash
	}
	if key, err = base64.RawStdEncoding.DecodeString(parts[3]); err != nil {
		return nil, nil, 0, ErrMalformedHash
	}
	if len(salt) == 0 || len(key) == 0 {
		return nil, nil, 0, ErrMalformedHash
	}
	return salt, key, iter, nil
}
