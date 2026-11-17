package delivery

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
	"time"

	"github.com/Thiago-tods/relayforge/internal/ingest"
	"github.com/Thiago-tods/relayforge/internal/secretbox"
)

const (
	testDelivery = "0d5f8a0e-8a3c-4c43-9a55-0d2b3e1f6a11"
	testEvent    = "7c1e3f55-2b4d-4f9a-8d11-5a6b7c8d9e0f"
)

func TestSignOutboundMatchesTheDocumentedFormat(t *testing.T) {
	body := []byte("{ \"order\" : 42,\n \"note\":\"spacing is preserved\" }")
	mac := hmac.New(sha256.New, []byte("rfd_example-secret"))
	mac.Write([]byte("v1.1700000000." + testDelivery + "." + testEvent + "."))
	mac.Write(body)
	want := "v1=" + hex.EncodeToString(mac.Sum(nil))
	if got := SignOutbound("rfd_example-secret", "1700000000", testDelivery, testEvent, body); got != want {
		t.Fatalf("SignOutbound = %s, want %s", got, want)
	}
	// A fixed vector, computed with an independent HMAC implementation, pins the format.
	if got := SignOutbound("secret", "1", "d", "e", []byte("{}")); got != "v1=659f98bc234333114903f41b69ba54163ab99d6aca9a23dd5bcd6cb31a323d7a" {
		t.Fatalf("unexpected signature %q", got)
	}
}

func TestSignatureCoversEveryComponent(t *testing.T) {
	body := []byte(`{"a":1}`)
	base := SignOutbound("s", "100", testDelivery, testEvent, body)
	variants := map[string]string{
		"secret":    SignOutbound("t", "100", testDelivery, testEvent, body),
		"timestamp": SignOutbound("s", "101", testDelivery, testEvent, body),
		"delivery":  SignOutbound("s", "100", "other-delivery", testEvent, body),
		"event":     SignOutbound("s", "100", testDelivery, "other-event", body),
		"body":      SignOutbound("s", "100", testDelivery, testEvent, []byte(`{"a": 1}`)),
		"newline":   SignOutbound("s", "100", testDelivery, testEvent, []byte("{\"a\":1}\n")),
	}
	for name, sig := range variants {
		if sig == base {
			t.Errorf("changing the %s must change the signature", name)
		}
	}
	// Field boundaries cannot be shifted: moving a dot between delivery and event ids changes the message.
	if SignOutbound("s", "100", "ab", "c", body) == SignOutbound("s", "100", "a", "bc", body) {
		t.Error("ambiguous concatenation")
	}
}

func TestVerifyOutbound(t *testing.T) {
	body := []byte(`{"ok":true}`)
	good := SignOutbound("secret", "1700000000", testDelivery, testEvent, body)
	if !VerifyOutbound("secret", "1700000000", testDelivery, testEvent, body, good) {
		t.Fatal("a valid signature was rejected")
	}
	for name, ok := range map[string]bool{
		"wrong secret":   VerifyOutbound("other", "1700000000", testDelivery, testEvent, body, good),
		"wrong time":     VerifyOutbound("secret", "1700000001", testDelivery, testEvent, body, good),
		"wrong delivery": VerifyOutbound("secret", "1700000000", "x", testEvent, body, good),
		"wrong event":    VerifyOutbound("secret", "1700000000", testDelivery, "x", body, good),
		"wrong body":     VerifyOutbound("secret", "1700000000", testDelivery, testEvent, []byte(`{"ok":false}`), good),
		"empty":          VerifyOutbound("secret", "1700000000", testDelivery, testEvent, body, ""),
		"no version":     VerifyOutbound("secret", "1700000000", testDelivery, testEvent, body, strings.TrimPrefix(good, "v1=")),
		"uppercase":      VerifyOutbound("secret", "1700000000", testDelivery, testEvent, body, "v1="+strings.ToUpper(strings.TrimPrefix(good, "v1="))),
		"other version":  VerifyOutbound("secret", "1700000000", testDelivery, testEvent, body, "v2="+strings.TrimPrefix(good, "v1=")),
		"truncated":      VerifyOutbound("secret", "1700000000", testDelivery, testEvent, body, good[:len(good)-2]),
		"two signatures": VerifyOutbound("secret", "1700000000", testDelivery, testEvent, body, good+",v1="+strings.Repeat("0", 64)),
	} {
		if ok {
			t.Errorf("%s was accepted", name)
		}
	}
}

// TestVerificationUsesConstantTimeComparison keeps VerifyOutbound on hmac.Equal.
func TestVerificationUsesConstantTimeComparison(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "signing.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	usesEqual := false
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "VerifyOutbound" {
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
				isLen := func(e ast.Expr) bool {
					c, ok := e.(*ast.CallExpr)
					id, ok2 := c.Fun.(*ast.Ident)
					return ok && ok2 && id.Name == "len"
				}
				if (x.Op == token.EQL || x.Op == token.NEQ) && !isNil(x.X) && !isNil(x.Y) && !isLen(x.X) && !isLen(x.Y) {
					t.Errorf("VerifyOutbound must not compare values with %s", x.Op)
				}
			}
			return true
		})
		return false
	})
	if !usesEqual {
		t.Fatal("VerifyOutbound must compare with hmac.Equal")
	}
}

func TestDestinationSecretsAreDistinctFromIngestionSecrets(t *testing.T) {
	seen := map[string]bool{}
	for range 300 {
		s, err := GenerateDestinationSecret()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(s, "rfd_") || strings.HasPrefix(s, "rfs_") {
			t.Fatalf("destination secrets must use their own prefix: %.6s", s)
		}
		raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(s, "rfd_"))
		if err != nil || len(raw) < 32 {
			t.Fatalf("a destination secret needs at least 256 bits of entropy (%d bytes, %v)", len(raw), err)
		}
		if seen[s] {
			t.Fatal("duplicate secret")
		}
		seen[s] = true
	}
}

func TestSecretCiphertextsAreBoundToTheirKindAndRow(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	box, err := secretbox.New(key, 1)
	if err != nil {
		t.Fatal(err)
	}
	const id = testDelivery
	sealed, err := box.Seal([]byte("rfd_secret"), DestinationSecretAAD(id))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := box.Open(sealed, DestinationSecretAAD(id), 1); err != nil {
		t.Fatalf("a destination secret must open with its own context: %v", err)
	}
	if _, err := box.Open(sealed, DestinationSecretAAD("other-destination"), 1); err == nil {
		t.Error("a ciphertext copied to another destination must not open")
	}
	// An ingestion-endpoint ciphertext cannot be used as a destination secret, nor the reverse.
	if _, err := box.Open(sealed, ingest.SecretAAD(id), 1); err == nil {
		t.Error("a destination ciphertext must not open as an ingestion secret")
	}
	endpointSealed, _ := box.Seal([]byte("rfs_secret"), ingest.SecretAAD(id))
	if _, err := box.Open(endpointSealed, DestinationSecretAAD(id), 1); err == nil {
		t.Error("an ingestion ciphertext must not open as a destination secret")
	}
}

func TestOutboundHeaders(t *testing.T) {
	received := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	signedAt := received.Add(90 * time.Second)
	body := []byte(`{"x":1}`)
	h := OutboundHeaders(OutboundParams{
		Secret: "rfd_secret", SecretVersion: 3, DeliveryID: testDelivery, EventID: testEvent, Attempt: 2,
		ContentType: "application/json", EventReceivedAt: received, SignedAt: signedAt, Body: body,
	})
	want := map[string]string{
		HeaderEventID: testEvent, HeaderDeliveryID: testDelivery, HeaderAttempt: "2", HeaderKeyVersion: "3",
		HeaderEventTimestamp: "1767268800", HeaderTimestamp: "1767268890", "Content-Type": "application/json",
	}
	for k, v := range want {
		if h[k] != v {
			t.Errorf("%s = %q, want %q", k, h[k], v)
		}
	}
	if !VerifyOutbound("rfd_secret", h[HeaderTimestamp], h[HeaderDeliveryID], h[HeaderEventID], body, h[HeaderSignature]) {
		t.Fatal("the signature in the headers must verify")
	}
	for name := range h {
		if name != "Content-Type" && !strings.HasPrefix(strings.ToLower(name), "x-relayforge-") {
			t.Errorf("unexpected header %s", name)
		}
	}
	// The delivery id is stable across attempts; only the signature timestamp moves.
	h2 := OutboundHeaders(OutboundParams{
		Secret: "rfd_secret", SecretVersion: 3, DeliveryID: testDelivery, EventID: testEvent, Attempt: 3,
		ContentType: "application/json", EventReceivedAt: received, SignedAt: signedAt.Add(time.Hour), Body: body,
	})
	if h2[HeaderDeliveryID] != h[HeaderDeliveryID] || h2[HeaderEventID] != h[HeaderEventID] || h2[HeaderEventTimestamp] != h[HeaderEventTimestamp] || h2[HeaderTimestamp] == h[HeaderTimestamp] {
		t.Errorf("identifiers must be stable across attempts and the signature timestamp must be fresh: %v vs %v", h, h2)
	}
}
