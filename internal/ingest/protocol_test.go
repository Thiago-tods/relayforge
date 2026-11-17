package ingest

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"strings"
	"testing"
	"time"
)

func TestSignMatchesDocumentedAlgorithm(t *testing.T) {
	const secret = "rfs_test-secret-not-real"
	body := []byte(`{"order": 42,  "note":"spacing is preserved"}`)
	// Independent implementation of: hex(HMAC-SHA256(secret, timestamp + "." + body)).
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("1700000000." + string(body)))
	want := hex.EncodeToString(mac.Sum(nil))

	if got := Sign(secret, "1700000000", body); got != want {
		t.Fatalf("Sign = %s, want %s", got, want)
	}
	// A fixed vector (computed with an independent HMAC implementation) so any
	// change of the algorithm is caught.
	if got := Sign("secret", "1", []byte("{}")); got != "1122767b193110cfec322b6f199b599edbf608ed087f2d27afb0b97d99523908" {
		t.Fatalf("unexpected signature %q", got)
	}
}

func TestSignCoversExactBytes(t *testing.T) {
	a := Sign("s", "100", []byte(`{"a":1}`))
	for name, body := range map[string][]byte{
		"extra space": []byte(`{"a": 1}`), "reordered": []byte(`{"a":1,}`), "trailing newline": []byte("{\"a\":1}\n"),
	} {
		if Sign("s", "100", body) == a {
			t.Errorf("%s: signature must change when the bytes change", name)
		}
	}
	if Sign("s", "101", []byte(`{"a":1}`)) == a {
		t.Error("signature must cover the timestamp")
	}
	if Sign("t", "100", []byte(`{"a":1}`)) == a {
		t.Error("signature must depend on the secret")
	}
}

func TestVerifySignature(t *testing.T) {
	body := []byte(`{"ok":true}`)
	good := Sign("secret", "1700000000", body)

	if ok, err := VerifySignature("secret", "1700000000", body, good); err != nil || !ok {
		t.Fatalf("valid signature rejected: %v %v", ok, err)
	}
	if ok, err := VerifySignature("other", "1700000000", body, good); err != nil || ok {
		t.Fatalf("wrong secret accepted: %v %v", ok, err)
	}
	if ok, err := VerifySignature("secret", "1700000001", body, good); err != nil || ok {
		t.Fatalf("wrong timestamp accepted: %v %v", ok, err)
	}
	if ok, err := VerifySignature("secret", "1700000000", []byte(`{"ok":false}`), good); err != nil || ok {
		t.Fatalf("tampered body accepted: %v %v", ok, err)
	}

	for name, sig := range map[string]string{
		"empty": "", "short": good[:63], "long": good + "0", "uppercase": strings.ToUpper(good),
		"prefixed": "sha256=" + good, "non hex": strings.Repeat("z", 64), "base64": base64.StdEncoding.EncodeToString([]byte(good)),
		"whitespace": " " + good[1:],
	} {
		ok, err := VerifySignature("secret", "1700000000", body, sig)
		if ok || err == nil {
			t.Errorf("%s: got ok=%v err=%v, want a format error", name, ok, err)
		}
	}
	if _, err := VerifySignature("secret", "1", body, ""); !errors.Is(err, ErrSignatureMissing) {
		t.Error("empty signature must be reported as missing")
	}
	if _, err := VerifySignature("secret", "1", body, "abc"); !errors.Is(err, ErrSignatureMalformed) {
		t.Error("short signature must be reported as malformed")
	}
}

// TestVerificationUsesConstantTimeComparison guards against replacing hmac.Equal
// with a byte-wise or string comparison, which would leak timing information.
func TestVerificationUsesConstantTimeComparison(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "protocol.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	usesEqual := false
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "VerifySignature" {
			return true
		}
		ast.Inspect(fn, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CallExpr:
				if sel, ok := x.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Equal" {
					if id, ok := sel.X.(*ast.Ident); ok && id.Name == "hmac" {
						usesEqual = true
					}
				}
			case *ast.BinaryExpr:
				isNil := func(e ast.Expr) bool { id, ok := e.(*ast.Ident); return ok && id.Name == "nil" }
				if (x.Op == token.EQL || x.Op == token.NEQ) && !isNil(x.X) && !isNil(x.Y) {
					t.Errorf("VerifySignature must not compare values with %s", x.Op)
				}
			}
			return true
		})
		return false
	})
	if !usesEqual {
		t.Fatal("VerifySignature must compare with hmac.Equal")
	}
}

func TestParseTimestamp(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	skew := 5 * time.Minute

	ok := map[string]int64{
		"1700000000": 1_700_000_000,
		"1699999700": 1_699_999_700, // exactly 5 minutes in the past: inclusive boundary
		"1700000300": 1_700_000_300, // exactly 5 minutes in the future
		"1700000001": 1_700_000_001,
	}
	for raw, want := range ok {
		ts, err := ParseTimestamp(raw, now, skew)
		if err != nil || ts.Unix() != want {
			t.Errorf("ParseTimestamp(%q) = %v, %v", raw, ts, err)
		}
	}

	expired := []string{"1699999699", "1700000301", "0", "1", "9999999999"}
	for _, raw := range expired {
		if _, err := ParseTimestamp(raw, now, skew); !errors.Is(err, ErrTimestampExpired) {
			t.Errorf("ParseTimestamp(%q): got %v, want ErrTimestampExpired", raw, err)
		}
	}

	if _, err := ParseTimestamp("", now, skew); !errors.Is(err, ErrTimestampMissing) {
		t.Errorf("empty: got %v", err)
	}
	malformed := []string{
		" 1700000000", "1700000000 ", "+1700000000", "-1700000000", "1700000000.5", "1.7e9", "0x6553f100",
		"abc", "17000000000000", "١٧٠٠٠٠٠٠٠٠", "1700000000\n", "1_700_000_000", strings.Repeat("9", 40),
		"2023-11-14T22:13:20Z",
	}
	for _, raw := range malformed {
		if _, err := ParseTimestamp(raw, now, skew); !errors.Is(err, ErrTimestampMalformed) {
			t.Errorf("ParseTimestamp(%q): got %v, want ErrTimestampMalformed", raw, err)
		}
	}
	// Extreme values must not overflow into acceptance.
	if _, err := ParseTimestamp("999999999999", now, skew); err == nil {
		t.Error("far-future timestamp accepted")
	}
	_ = math.MaxInt64
}

func TestValidateIdempotencyKey(t *testing.T) {
	for _, ok := range []string{"a", "order-123", "550e8400-e29b-41d4-a716-446655440000", strings.Repeat("k", 255), "a.b:c/d~e!"} {
		if err := ValidateIdempotencyKey(ok); err != nil {
			t.Errorf("%q should be valid: %v", ok, err)
		}
	}
	for _, bad := range []string{"", " ", "has space", "tab\tkey", "new\nline", "nul\x00", "ünïcode", strings.Repeat("k", 256), "del\x7f"} {
		if err := ValidateIdempotencyKey(bad); !errors.Is(err, ErrIdempotencyInvalid) {
			t.Errorf("%q should be invalid, got %v", bad, err)
		}
	}
}

func TestPayloadHashIsSHA256OfExactBytes(t *testing.T) {
	sum := sha256.Sum256([]byte(`{"a": 1}`))
	if got := PayloadHash([]byte(`{"a": 1}`)); hex.EncodeToString(got) != hex.EncodeToString(sum[:]) {
		t.Fatal("PayloadHash must be SHA-256")
	}
	if string(PayloadHash([]byte(`{"a":1}`))) == string(PayloadHash([]byte(`{"a": 1}`))) {
		t.Fatal("hash must not normalize JSON")
	}
}

func TestGenerateSecretHasAtLeast256BitsAndIsUnique(t *testing.T) {
	seen := map[string]bool{}
	for range 500 {
		s, err := GenerateSecret()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(s, "rfs_") {
			t.Fatalf("missing prefix: %s", s)
		}
		raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(s, "rfs_"))
		if err != nil || len(raw) < 32 {
			t.Fatalf("secret carries %d bytes of entropy (%v), want at least 32", len(raw), err)
		}
		if seen[s] {
			t.Fatal("duplicate secret")
		}
		seen[s] = true
	}
}

func TestPublicIDs(t *testing.T) {
	id, err := GeneratePublicID()
	if err != nil || !ValidPublicID(id) {
		t.Fatalf("generated id %q invalid: %v", id, err)
	}
	other, _ := GeneratePublicID()
	if id == other {
		t.Fatal("public ids must be random")
	}
	for _, bad := range []string{"", "ep_", "ep_short", id + "x", "xx_" + id[3:], "ep_" + strings.Repeat("!", 22), "ep_" + strings.Repeat("a", 21) + "/", "ep_../../../../etc/pa", id[:24] + " "} {
		if ValidPublicID(bad) {
			t.Errorf("%q must be rejected", bad)
		}
	}
}

func TestNewUUID(t *testing.T) {
	a, err := NewUUID()
	if err != nil || len(a) != 36 || a[14] != '4' {
		t.Fatalf("bad uuid %q: %v", a, err)
	}
	if b, _ := NewUUID(); a == b {
		t.Fatal("uuids must be random")
	}
}
