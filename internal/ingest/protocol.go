// Package ingest implements webhook ingestion: ingestion endpoints, the HMAC
// signature protocol, replay protection, idempotent persistence and the
// transactional outbox.
//
// An ingestion endpoint is a RelayForge URL that receives events from an
// external producer. It is not a delivery destination.
package ingest

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// Protocol constants.
const (
	// HeaderTimestamp carries the Unix time in seconds, as ASCII digits.
	HeaderTimestamp = "X-RelayForge-Timestamp"
	// HeaderSignature carries the lowercase hexadecimal HMAC-SHA256 (64 characters).
	HeaderSignature = "X-RelayForge-Signature"
	// HeaderIdempotencyKey carries the producer's idempotency key.
	HeaderIdempotencyKey = "Idempotency-Key"

	// MaxBodyBytes is the largest accepted request body: 1 MiB.
	MaxBodyBytes = 1 << 20
	// DefaultTimestampSkew is the allowed difference between the producer's and the server's clock.
	DefaultTimestampSkew = 5 * time.Minute

	maxTimestampDigits = 12
	signatureHexLength = 64
	maxIdempotencyKey  = 255
	secretPrefix       = "rfs_"
	secretBytes        = 32
	publicIDPrefix     = "ep_"
	publicIDBytes      = 16
)

// Validation errors. Their messages are safe to return to clients.
var (
	ErrTimestampMissing   = errors.New("timestamp header is missing")
	ErrTimestampMalformed = errors.New("timestamp must be Unix seconds as ASCII digits")
	ErrTimestampExpired   = errors.New("timestamp is outside the allowed window")
	ErrSignatureMissing   = errors.New("signature header is missing")
	ErrSignatureMalformed = errors.New("signature must be 64 lowercase hexadecimal characters")
	ErrIdempotencyInvalid = errors.New("idempotency key must be 1 to 255 printable ASCII characters")
)

// Sign returns the signature a producer must send: the lowercase hexadecimal
// HMAC-SHA256 of timestamp + "." + body, keyed with the signing secret string
// (its UTF-8 bytes). body must be the exact bytes sent on the wire.
func Sign(secret, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifySignature reports whether signature is the correct signature of
// timestamp and body under secret. The comparison is constant time. A malformed
// signature is an error, not merely a mismatch, so callers can tell the cases apart.
func VerifySignature(secret, timestamp string, body []byte, signature string) (bool, error) {
	if err := ValidateSignatureFormat(signature); err != nil {
		return false, err
	}
	want, err := hex.DecodeString(Sign(secret, timestamp, body))
	if err != nil {
		return false, fmt.Errorf("decode expected signature: %w", err)
	}
	got, err := hex.DecodeString(signature)
	if err != nil {
		return false, ErrSignatureMalformed
	}
	return hmac.Equal(got, want), nil
}

// ValidateSignatureFormat accepts exactly 64 lowercase hexadecimal characters.
// Uppercase, prefixes such as "sha256=" and other encodings are rejected; there
// is no fallback format.
func ValidateSignatureFormat(signature string) error {
	if signature == "" {
		return ErrSignatureMissing
	}
	if len(signature) != signatureHexLength {
		return ErrSignatureMalformed
	}
	for i := 0; i < len(signature); i++ {
		c := signature[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return ErrSignatureMalformed
		}
	}
	return nil
}

// ParseTimestamp strictly parses a Unix-seconds timestamp (ASCII digits only,
// no sign, spaces, decimals or exponent) and checks it against now within
// +/- skew. The boundary is inclusive.
func ParseTimestamp(raw string, now time.Time, skew time.Duration) (time.Time, error) {
	if raw == "" {
		return time.Time{}, ErrTimestampMissing
	}
	if len(raw) > maxTimestampDigits {
		return time.Time{}, ErrTimestampMalformed
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] < '0' || raw[i] > '9' {
			return time.Time{}, ErrTimestampMalformed
		}
	}
	secs, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return time.Time{}, ErrTimestampMalformed
	}
	// Compare in whole seconds: time.Duration arithmetic would overflow for
	// timestamps thousands of years away and wrongly accept them.
	diff := now.Unix() - secs
	if diff < 0 {
		diff = -diff
	}
	if diff > int64(skew/time.Second) {
		return time.Time{}, ErrTimestampExpired
	}
	return time.Unix(secs, 0).UTC(), nil
}

// ValidateIdempotencyKey requires 1 to 255 printable ASCII characters (0x21-0x7e),
// so keys contain no whitespace or control characters.
func ValidateIdempotencyKey(key string) error {
	if key == "" || len(key) > maxIdempotencyKey {
		return ErrIdempotencyInvalid
	}
	for i := 0; i < len(key); i++ {
		if key[i] < 0x21 || key[i] > 0x7e {
			return ErrIdempotencyInvalid
		}
	}
	return nil
}

// PayloadHash returns the SHA-256 of the exact payload bytes.
func PayloadHash(payload []byte) []byte {
	sum := sha256.Sum256(payload)
	return sum[:]
}

// GenerateSecret returns a new signing secret: "rfs_" followed by 256 bits of
// crypto/rand output in unpadded base64url.
func GenerateSecret() (string, error) {
	b := make([]byte, secretBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate signing secret: %w", err)
	}
	return secretPrefix + base64.RawURLEncoding.EncodeToString(b), nil
}

// GeneratePublicID returns a new public endpoint identifier: "ep_" followed by
// 128 bits of crypto/rand output in unpadded base64url (22 characters).
func GeneratePublicID() (string, error) {
	b := make([]byte, publicIDBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate public id: %w", err)
	}
	return publicIDPrefix + base64.RawURLEncoding.EncodeToString(b), nil
}

// ValidPublicID reports whether s has the shape GeneratePublicID produces.
func ValidPublicID(s string) bool {
	if len(s) != len(publicIDPrefix)+22 || s[:len(publicIDPrefix)] != publicIDPrefix {
		return false
	}
	for i := len(publicIDPrefix); i < len(s); i++ {
		c := s[i]
		ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_'
		if !ok {
			return false
		}
	}
	return true
}

// NewUUID returns a random (version 4) UUID in canonical lowercase form.
func NewUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate uuid: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:], nil
}
