package identity

import (
	"bytes"
	"testing"
)

func TestNewTokenHasEnoughEntropyAndMatchingHash(t *testing.T) {
	raw, hash, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if !WellFormedToken(raw) {
		t.Fatalf("token %q is not well formed", raw)
	}
	if len(hash) != 32 || !bytes.Equal(hash, HashToken(raw)) {
		t.Fatal("hash must be the SHA-256 of the raw token")
	}
	if bytes.Contains(hash, []byte(raw)) {
		t.Fatal("hash must not contain the raw token")
	}
	if !TokenMatchesHash(raw, hash) {
		t.Fatal("raw token must match its own hash")
	}
}

func TestNewTokenIsUnique(t *testing.T) {
	seen := map[string]bool{}
	for range 1000 {
		raw, _, err := NewToken()
		if err != nil {
			t.Fatal(err)
		}
		if seen[raw] {
			t.Fatal("duplicate token generated")
		}
		seen[raw] = true
	}
}

func TestTokenMatchesHashRejectsOtherTokens(t *testing.T) {
	raw, hash, _ := NewToken()
	other, _, _ := NewToken()
	if TokenMatchesHash(other, hash) || TokenMatchesHash(raw, hash[:31]) || TokenMatchesHash("", hash) {
		t.Fatal("mismatched token accepted")
	}
}

func TestWellFormedToken(t *testing.T) {
	for _, bad := range []string{"", "short", "!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!", string(make([]byte, 43))} {
		if WellFormedToken(bad) {
			t.Errorf("%q must be rejected", bad)
		}
	}
	raw, _, _ := NewToken()
	if WellFormedToken(raw + "A") {
		t.Fatal("overlong token accepted")
	}
}
