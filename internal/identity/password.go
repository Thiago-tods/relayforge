package identity

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// Password policy bounds, counted in Unicode characters.
const (
	MinPasswordLength = 12
	MaxPasswordLength = 128
)

// ErrPasswordPolicy is wrapped by every password policy violation.
var ErrPasswordPolicy = errors.New("password does not meet the policy")

// ValidatePassword enforces the password policy: 12 to 128 characters, not
// only whitespace, and no NUL bytes. Passwords are never trimmed or truncated.
func ValidatePassword(password string) error {
	if !utf8.ValidString(password) {
		return fmt.Errorf("%w: must be valid UTF-8", ErrPasswordPolicy)
	}
	if strings.TrimFunc(password, unicode.IsSpace) == "" {
		return fmt.Errorf("%w: must not consist only of whitespace", ErrPasswordPolicy)
	}
	if strings.ContainsRune(password, 0) {
		return fmt.Errorf("%w: must not contain NUL characters", ErrPasswordPolicy)
	}
	n := utf8.RuneCountInString(password)
	if n < MinPasswordLength {
		return fmt.Errorf("%w: must be at least %d characters", ErrPasswordPolicy, MinPasswordLength)
	}
	if n > MaxPasswordLength {
		return fmt.Errorf("%w: must be at most %d characters", ErrPasswordPolicy, MaxPasswordLength)
	}
	return nil
}

// Argon2Params are the Argon2id cost parameters.
type Argon2Params struct {
	MemoryKiB   uint32
	Iterations  uint32
	Parallelism uint8
	SaltLength  uint32
	KeyLength   uint32
}

// DefaultArgon2Params follow the RFC 9106 and OWASP guidance for Argon2id
// (memory-hard, at least 19 MiB with t=2): 64 MiB, 3 iterations, 2 lanes. One
// hash takes roughly 100 ms on a current laptop core.
var DefaultArgon2Params = Argon2Params{
	MemoryKiB: 64 * 1024, Iterations: 3, Parallelism: 2, SaltLength: 16, KeyLength: 32,
}

// maxConcurrentHashes bounds memory use: each hash allocates MemoryKiB.
const maxConcurrentHashes = 4

// Hasher hashes and verifies passwords with Argon2id.
type Hasher struct {
	params Argon2Params
	sem    chan struct{}
}

// NewHasher creates a Hasher that encodes hashes in the PHC string format.
func NewHasher(params Argon2Params) *Hasher {
	return &Hasher{params: params, sem: make(chan struct{}, maxConcurrentHashes)}
}

func (h *Hasher) acquire(ctx context.Context) error {
	select {
	case h.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *Hasher) release() { <-h.sem }

// Hash returns the PHC-encoded Argon2id hash of password with a fresh random salt.
// The caller must have validated the password with ValidatePassword.
func (h *Hasher) Hash(ctx context.Context, password string) (string, error) {
	salt := make([]byte, h.params.SaltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}
	if err := h.acquire(ctx); err != nil {
		return "", err
	}
	defer h.release()

	key := argon2.IDKey([]byte(password), salt, h.params.Iterations, h.params.MemoryKiB, h.params.Parallelism, h.params.KeyLength)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, h.params.MemoryKiB, h.params.Iterations, h.params.Parallelism,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

// ErrInvalidHash is returned when an encoded hash cannot be parsed.
var ErrInvalidHash = errors.New("invalid password hash encoding")

// Verify reports whether password matches encoded. The comparison is constant
// time. The parameters stored in encoded are used, so hashes created with older
// parameters keep verifying after the defaults change.
func (h *Hasher) Verify(ctx context.Context, password, encoded string) (bool, error) {
	params, salt, want, err := decodeHash(encoded)
	if err != nil {
		return false, err
	}
	if err := h.acquire(ctx); err != nil {
		return false, err
	}
	defer h.release()

	got := argon2.IDKey([]byte(password), salt, params.Iterations, params.MemoryKiB, params.Parallelism, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

func decodeHash(encoded string) (Argon2Params, []byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	// "", "argon2id", "v=19", "m=...,t=...,p=...", salt, key
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return Argon2Params{}, nil, nil, ErrInvalidHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return Argon2Params{}, nil, nil, ErrInvalidHash
	}
	var p Argon2Params
	var parallelism uint32
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.MemoryKiB, &p.Iterations, &parallelism); err != nil {
		return Argon2Params{}, nil, nil, ErrInvalidHash
	}
	if parallelism == 0 || parallelism > 255 || p.Iterations == 0 || p.MemoryKiB < 8 {
		return Argon2Params{}, nil, nil, ErrInvalidHash
	}
	// A stored hash is trusted data, but cap the cost so a corrupted row cannot
	// make verification allocate an absurd amount of memory.
	if p.MemoryKiB > 1<<20 || p.Iterations > 20 {
		return Argon2Params{}, nil, nil, ErrInvalidHash
	}
	p.Parallelism = uint8(parallelism)
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) < 8 {
		return Argon2Params{}, nil, nil, ErrInvalidHash
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(key) < 16 {
		return Argon2Params{}, nil, nil, ErrInvalidHash
	}
	return p, salt, key, nil
}
