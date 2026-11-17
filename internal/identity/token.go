package identity

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
)

// tokenBytes is the entropy of every opaque token: 256 bits.
const tokenBytes = 32

// encodedTokenLength is the length of a base64url (unpadded) 32-byte token.
const encodedTokenLength = 43

// NewToken returns a random opaque token and its SHA-256 hash. Only the hash is
// ever stored; the raw token is given to the client once.
func NewToken() (raw string, hash []byte, err error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", nil, fmt.Errorf("generate token: %w", err)
	}
	raw = base64.RawURLEncoding.EncodeToString(b)
	return raw, HashToken(raw), nil
}

// HashToken returns the SHA-256 hash of a raw token.
func HashToken(raw string) []byte {
	sum := sha256.Sum256([]byte(raw))
	return sum[:]
}

// WellFormedToken reports whether raw has the shape NewToken produces. It lets
// callers reject garbage before touching the database.
func WellFormedToken(raw string) bool {
	if len(raw) != encodedTokenLength {
		return false
	}
	b, err := base64.RawURLEncoding.DecodeString(raw)
	return err == nil && len(b) == tokenBytes
}

// TokenMatchesHash compares raw against a stored hash in constant time.
func TokenMatchesHash(raw string, storedHash []byte) bool {
	return subtle.ConstantTimeCompare(HashToken(raw), storedHash) == 1
}
