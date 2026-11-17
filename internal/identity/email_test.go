package identity

import (
	"strings"
	"testing"
)

func TestNormalizeEmailAccepts(t *testing.T) {
	cases := map[string]string{
		"user@example.com":         "user@example.com",
		"  User@Example.COM  ":     "user@example.com",
		"first.last+tag@sub.a.org": "first.last+tag@sub.a.org",
		"UPPER@EXAMPLE.IO":         "upper@example.io",
		"a@b.co":                   "a@b.co",
	}
	for raw, want := range cases {
		got, err := NormalizeEmail(raw)
		if err != nil || got != want {
			t.Errorf("NormalizeEmail(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
}

func TestNormalizeEmailRejects(t *testing.T) {
	long := strings.Repeat("a", 65) + "@example.com"
	cases := []string{
		"", "   ", "plain", "@example.com", "user@", "user@example", "user@@example.com",
		"User <user@example.com>", "<user@example.com>", "user@example.com, other@example.com",
		"us er@example.com", "user@exa mple.com", "user@example..com", "user@.example.com",
		"user@example.com.", "usér@example.com", "user@exämple.com", long,
		"user\x00@example.com", strings.Repeat("a", 250) + "@example.com",
		"(comment)user@example.com",
	}
	for _, raw := range cases {
		if got, err := NormalizeEmail(raw); err == nil {
			t.Errorf("NormalizeEmail(%q) = %q; want error", raw, got)
		}
	}
}

func TestNormalizeEmailIsIdempotent(t *testing.T) {
	once, err := NormalizeEmail("  MiXed@Example.Com ")
	if err != nil {
		t.Fatal(err)
	}
	twice, err := NormalizeEmail(once)
	if err != nil || twice != once {
		t.Fatalf("second normalization changed %q to %q (%v)", once, twice, err)
	}
}
