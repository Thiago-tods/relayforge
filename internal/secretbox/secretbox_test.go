package secretbox

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func newKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return k
}

func newBox(t *testing.T) *Box {
	t.Helper()
	b, err := New(newKey(t), 1)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSealOpenRoundTrip(t *testing.T) {
	b := newBox(t)
	aad := []byte("endpoint-1")
	sealed, err := b.Seal([]byte("rfs_super-secret-value"), aad)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("super-secret")) {
		t.Fatal("ciphertext contains the plaintext")
	}
	got, err := b.Open(sealed, aad, 1)
	if err != nil || string(got) != "rfs_super-secret-value" {
		t.Fatalf("Open = %q, %v", got, err)
	}
}

func TestNonceIsUniquePerSeal(t *testing.T) {
	b := newBox(t)
	seen := map[string]bool{}
	for range 2000 {
		sealed, err := b.Seal([]byte("same plaintext"), nil)
		if err != nil {
			t.Fatal(err)
		}
		nonce := string(sealed[:12])
		if seen[nonce] {
			t.Fatal("nonce reused")
		}
		seen[nonce] = true
	}
}

func TestSameInputsGiveDifferentCiphertexts(t *testing.T) {
	b := newBox(t)
	a, _ := b.Seal([]byte("x"), nil)
	c, _ := b.Seal([]byte("x"), nil)
	if bytes.Equal(a, c) {
		t.Fatal("encryption must be randomized")
	}
}

func TestOpenRejectsWrongKey(t *testing.T) {
	sealed, _ := newBox(t).Seal([]byte("secret"), []byte("aad"))
	if _, err := newBox(t).Open(sealed, []byte("aad"), 1); !errors.Is(err, ErrOpenFailed) {
		t.Fatalf("got %v, want ErrOpenFailed", err)
	}
}

func TestOpenRejectsWrongAAD(t *testing.T) {
	b := newBox(t)
	sealed, _ := b.Seal([]byte("secret"), []byte("endpoint-1"))
	if _, err := b.Open(sealed, []byte("endpoint-2"), 1); !errors.Is(err, ErrOpenFailed) {
		t.Fatalf("a ciphertext must not open under another record's AAD: %v", err)
	}
}

func TestOpenRejectsTampering(t *testing.T) {
	b := newBox(t)
	sealed, _ := b.Seal([]byte("secret value"), []byte("aad"))
	for i := range sealed {
		tampered := bytes.Clone(sealed)
		tampered[i] ^= 0x01
		if _, err := b.Open(tampered, []byte("aad"), 1); err == nil {
			t.Fatalf("flipping byte %d went undetected", i)
		}
	}
	if _, err := b.Open(sealed[:len(sealed)-1], []byte("aad"), 1); err == nil {
		t.Fatal("truncated ciphertext accepted")
	}
}

func TestOpenRejectsShortAndUnknownVersion(t *testing.T) {
	b := newBox(t)
	if _, err := b.Open([]byte("short"), nil, 1); !errors.Is(err, ErrShortCipher) {
		t.Fatalf("got %v", err)
	}
	sealed, _ := b.Seal([]byte("x"), nil)
	if _, err := b.Open(sealed, nil, 2); !errors.Is(err, ErrKeyVersion) {
		t.Fatalf("got %v, want ErrKeyVersion", err)
	}
}

func TestNewRejectsBadKeys(t *testing.T) {
	for _, n := range []int{0, 16, 24, 31, 33, 64} {
		if _, err := New(make([]byte, n), 1); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("%d-byte key accepted", n)
		}
	}
	if _, err := New(make([]byte, 32), 0); err == nil {
		t.Error("version 0 accepted")
	}
}

func TestParseKey(t *testing.T) {
	key := newKey(t)
	for name, enc := range map[string]string{
		"std":    base64.StdEncoding.EncodeToString(key),
		"rawstd": base64.RawStdEncoding.EncodeToString(key),
		"url":    base64.URLEncoding.EncodeToString(key),
		"rawurl": base64.RawURLEncoding.EncodeToString(key),
	} {
		got, err := ParseKey(enc)
		if err != nil || !bytes.Equal(got, key) {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, enc := range map[string]string{
		"empty":          "",
		"not b64":        "!!!not base64!!!",
		"16 bytes":       base64.StdEncoding.EncodeToString(make([]byte, 16)),
		"33 bytes":       base64.StdEncoding.EncodeToString(make([]byte, 33)),
		"plain 32 chars": strings.Repeat("a", 32),
	} {
		_, err := ParseKey(enc)
		if err == nil && name != "plain 32 chars" {
			t.Errorf("%s accepted", name)
		}
		if err != nil && strings.Contains(err.Error(), enc) && enc != "" {
			t.Errorf("%s: error echoes the input", name)
		}
	}
}
