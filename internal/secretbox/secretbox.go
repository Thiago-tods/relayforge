// Package secretbox encrypts small secrets at rest with AES-256-GCM.
//
// Every Seal call draws a fresh random 96-bit nonce and prepends it to the
// ciphertext; the 128-bit GCM tag authenticates both the ciphertext and the
// caller-supplied additional data (AAD), which binds a ciphertext to the record
// it belongs to. Tampering, a wrong key and a wrong AAD all fail to Open.
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
)

const keyLength = 32

// Errors returned by the package. They never contain key material or plaintext.
var (
	ErrInvalidKey  = errors.New("encryption key must be exactly 32 bytes encoded as base64")
	ErrOpenFailed  = errors.New("cannot decrypt: wrong key, wrong context or corrupted data")
	ErrKeyVersion  = errors.New("ciphertext was encrypted with an unknown key version")
	ErrShortCipher = errors.New("ciphertext is too short")
)

// Box encrypts and decrypts with one key. A Box is safe for concurrent use.
type Box struct {
	aead    cipher.AEAD
	version int
}

// ParseKey decodes a base64 (standard or URL alphabet, padded or not) key and
// requires exactly 32 bytes. Errors do not echo the input.
func ParseKey(encoded string) ([]byte, error) {
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if key, err := enc.DecodeString(encoded); err == nil {
			if len(key) != keyLength {
				return nil, ErrInvalidKey
			}
			return key, nil
		}
	}
	return nil, ErrInvalidKey
}

// New creates a Box for the given 32-byte key. version identifies the key and
// is stored next to every ciphertext so keys can be rotated later.
func New(key []byte, version int) (*Box, error) {
	if len(key) != keyLength || version < 1 {
		return nil, ErrInvalidKey
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create gcm: %w", err)
	}
	return &Box{aead: aead, version: version}, nil
}

// Version returns the key version written next to new ciphertexts.
func (b *Box) Version() int { return b.version }

// Seal encrypts plaintext. The result is nonce || ciphertext || tag.
func (b *Box) Seal(plaintext, aad []byte) ([]byte, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}
	return b.aead.Seal(nonce, nonce, plaintext, aad), nil
}

// Open decrypts the output of Seal. keyVersion is the version stored with the
// ciphertext; versions other than the Box's own are refused.
func (b *Box) Open(sealed, aad []byte, keyVersion int) ([]byte, error) {
	if keyVersion != b.version {
		return nil, ErrKeyVersion
	}
	n := b.aead.NonceSize()
	if len(sealed) < n+b.aead.Overhead() {
		return nil, ErrShortCipher
	}
	plaintext, err := b.aead.Open(nil, sealed[:n], sealed[n:], aad)
	if err != nil {
		return nil, ErrOpenFailed
	}
	return plaintext, nil
}
