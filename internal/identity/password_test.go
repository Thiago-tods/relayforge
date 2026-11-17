package identity

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// fastParams keep unit tests quick; production uses DefaultArgon2Params.
var fastParams = Argon2Params{MemoryKiB: 64, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32}

func TestValidatePassword(t *testing.T) {
	valid := []string{
		strings.Repeat("a", 12),
		"correct horse battery staple",
		"pässwörd-ünïcode-ok",
		strings.Repeat("x", 128),
		"  leading and trailing spaces are kept  ",
	}
	for _, p := range valid {
		if err := ValidatePassword(p); err != nil {
			t.Errorf("%q should be valid: %v", p, err)
		}
	}

	invalid := map[string]string{
		"empty":            "",
		"short":            "elevenchars",
		"only whitespace":  strings.Repeat(" ", 20),
		"tabs and newline": "\t\n\t\n\t\n\t\n\t\n\t\n",
		"too long":         strings.Repeat("x", 129),
		"nul byte":         "valid-password\x00",
		"invalid utf8":     "valid-password-\xff\xfe",
	}
	for name, p := range invalid {
		err := ValidatePassword(p)
		if err == nil {
			t.Errorf("%s: should be rejected", name)
			continue
		}
		if !errors.Is(err, ErrPasswordPolicy) {
			t.Errorf("%s: error must wrap ErrPasswordPolicy: %v", name, err)
		}
		if strings.Contains(err.Error(), p) && p != "" {
			t.Errorf("%s: error must not echo the password", name)
		}
	}
}

func TestPasswordLengthCountsCharactersNotBytes(t *testing.T) {
	// 12 multi-byte characters are valid even though they exceed 12 bytes.
	if err := ValidatePassword(strings.Repeat("é", 12)); err != nil {
		t.Fatalf("12 characters must be accepted: %v", err)
	}
	if err := ValidatePassword(strings.Repeat("é", 11)); err == nil {
		t.Fatal("11 characters must be rejected")
	}
}

func TestHashAndVerify(t *testing.T) {
	h := NewHasher(fastParams)
	ctx := context.Background()
	const pw = "correct horse battery staple"

	enc, err := h.Hash(ctx, pw)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(enc, "$argon2id$v=19$m=64,t=1,p=1$") {
		t.Fatalf("unexpected encoding: %s", enc)
	}
	if strings.Contains(enc, pw) {
		t.Fatal("hash must not contain the password")
	}

	ok, err := h.Verify(ctx, pw, enc)
	if err != nil || !ok {
		t.Fatalf("correct password rejected: %v %v", ok, err)
	}
	ok, err = h.Verify(ctx, pw+"x", enc)
	if err != nil || ok {
		t.Fatalf("wrong password accepted: %v %v", ok, err)
	}
}

func TestHashUsesRandomSalt(t *testing.T) {
	h := NewHasher(fastParams)
	a, _ := h.Hash(context.Background(), "same password value")
	b, _ := h.Hash(context.Background(), "same password value")
	if a == b {
		t.Fatal("two hashes of the same password must differ")
	}
}

func TestVerifyUsesStoredParameters(t *testing.T) {
	old := NewHasher(fastParams)
	enc, err := old.Hash(context.Background(), "an older stored password")
	if err != nil {
		t.Fatal(err)
	}
	// A hasher configured with different defaults still verifies the old hash.
	current := NewHasher(Argon2Params{MemoryKiB: 128, Iterations: 2, Parallelism: 1, SaltLength: 16, KeyLength: 32})
	ok, err := current.Verify(context.Background(), "an older stored password", enc)
	if err != nil || !ok {
		t.Fatalf("hash with old parameters must verify: %v %v", ok, err)
	}
}

func TestVerifyRejectsMalformedHashes(t *testing.T) {
	h := NewHasher(fastParams)
	for _, enc := range []string{
		"", "plaintext", "$argon2i$v=19$m=64,t=1,p=1$c2FsdHNhbHQ$a2V5a2V5a2V5a2V5a2V5",
		"$argon2id$v=18$m=64,t=1,p=1$c2FsdHNhbHQ$a2V5a2V5a2V5a2V5a2V5",
		"$argon2id$v=19$m=64,t=1,p=0$c2FsdHNhbHQ$a2V5a2V5a2V5a2V5a2V5",
		"$argon2id$v=19$m=99999999,t=1,p=1$c2FsdHNhbHQ$a2V5a2V5a2V5a2V5a2V5",
		"$argon2id$v=19$m=64,t=1,p=1$!!!$a2V5a2V5a2V5a2V5a2V5",
		"$argon2id$v=19$m=64,t=1,p=1$c2FsdHNhbHQ",
	} {
		if ok, err := h.Verify(context.Background(), "whatever password", enc); ok || !errors.Is(err, ErrInvalidHash) {
			t.Errorf("Verify(%q) = %v, %v; want false, ErrInvalidHash", enc, ok, err)
		}
	}
}

func TestHashRespectsContextCancellationWhenSaturated(t *testing.T) {
	h := NewHasher(fastParams)
	for range maxConcurrentHashes {
		h.sem <- struct{}{} // occupy every slot
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := h.Hash(ctx, "some password value"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want deadline exceeded", err)
	}
}

func TestHasherIsSafeForConcurrentUse(t *testing.T) {
	h := NewHasher(fastParams)
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			enc, err := h.Hash(context.Background(), "concurrent password value")
			if err != nil {
				t.Error(err)
				return
			}
			if ok, err := h.Verify(context.Background(), "concurrent password value", enc); err != nil || !ok {
				t.Errorf("verify failed: %v %v", ok, err)
			}
		}()
	}
	wg.Wait()
}

func TestDefaultParamsAreStrongEnough(t *testing.T) {
	p := DefaultArgon2Params
	if p.MemoryKiB < 19*1024 || p.Iterations < 2 || p.Parallelism < 1 || p.SaltLength < 16 || p.KeyLength < 32 {
		t.Fatalf("default Argon2id parameters below the documented minimum: %+v", p)
	}
}
