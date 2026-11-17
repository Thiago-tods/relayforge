package egress

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateDestinationURLAccepts(t *testing.T) {
	cases := []struct {
		raw, normalized, display, host string
		port                           int
	}{
		{"https://example.com", "https://example.com", "https://example.com", "example.com", 443},
		{"https://example.com/hooks/abc", "https://example.com/hooks/abc", "https://example.com/hooks/abc", "example.com", 443},
		{"https://Example.COM:443/x?token=s3cret&b=2", "https://example.com/x?token=s3cret&b=2", "https://example.com/x", "example.com", 443},
		{"https://example.com./x", "https://example.com/x", "https://example.com/x", "example.com", 443},
		{"https://sub.a.example.co.uk/a%20b", "https://sub.a.example.co.uk/a%20b", "https://sub.a.example.co.uk/a%20b", "sub.a.example.co.uk", 443},
		{"https://xn--bcher-kva.example/x", "https://xn--bcher-kva.example/x", "https://xn--bcher-kva.example/x", "xn--bcher-kva.example", 443},
		{"https://example.com:8443/x", "https://example.com:8443/x", "https://example.com:8443/x", "example.com", 8443},
		{"https://example.com?x=1", "https://example.com?x=1", "https://example.com", "example.com", 443},
	}
	policy := DefaultURLPolicy()
	policy.AllowedPorts = []int{443, 8443}
	for _, c := range cases {
		got, err := ValidateDestinationURL(c.raw, policy)
		if err != nil {
			t.Errorf("%q rejected: %v", c.raw, err)
			continue
		}
		if got.Normalized != c.normalized || got.Display != c.display || got.Host != c.host || got.Port != c.port || got.Scheme != "https" {
			t.Errorf("%q => %+v, want normalized=%q display=%q host=%q port=%d", c.raw, got, c.normalized, c.display, c.host, c.port)
		}
		if strings.Contains(got.Display, "?") || strings.Contains(got.Display, "s3cret") {
			t.Errorf("%q: the display form must not contain the query", c.raw)
		}
	}
}

func TestValidateDestinationURLRejects(t *testing.T) {
	long := "https://example.com/" + strings.Repeat("a", 2100)
	label64 := strings.Repeat("a", 64)
	host254 := strings.Repeat("a.", 127) + "com"

	cases := []struct{ raw, code string }{
		// Empty, size and characters.
		{"", CodeEmpty}, {long, CodeTooLong},
		{"https://example.com/a b", CodeInvalidCharacters}, {"https://example.com/\n", CodeInvalidCharacters},
		{"https://exa\tmple.com", CodeInvalidCharacters}, {"https://example.com/\x00", CodeInvalidCharacters},
		{" https://example.com", CodeInvalidCharacters}, {"https://example.com ", CodeInvalidCharacters},
		{"https://exämple.com", CodeInvalidCharacters}, {"https://例え.jp/", CodeInvalidCharacters},
		{"https://example.com\\@evil.com", CodeInvalidCharacters}, {"https://example.com/\\..\\x", CodeInvalidCharacters},
		{"https://example.com/a%0d%0aHost:evil", CodeEncodedControl}, {"https://example.com/%00", CodeEncodedControl},
		{"https://example.com/?q=%0A", CodeEncodedControl}, {"https://example.com/%7F", CodeEncodedControl},
		// Schemes and structure.
		{"http://example.com", CodeSchemeNotAllowed}, {"ftp://example.com", CodeSchemeNotAllowed},
		{"file:///etc/passwd", CodeSchemeNotAllowed}, {"gopher://example.com", CodeSchemeNotAllowed},
		{"javascript:alert(1)", CodeSchemeNotAllowed}, {"//example.com/x", CodeSchemeNotAllowed},
		{"example.com/x", CodeSchemeNotAllowed}, {"data:text/plain,hi", CodeSchemeNotAllowed},
		{"https:example.com", CodeMalformed}, {"https:///path", CodeMissingHost}, {"https://", CodeMissingHost},
		{"https://example.com/%zz", CodeMalformed},
		// Credentials and fragments.
		{"https://user:pass@example.com", CodeCredentials}, {"https://user@example.com", CodeCredentials},
		{"https://@example.com", CodeCredentials}, {"https://example.com@evil.com/", CodeCredentials},
		{"https://example.com/#frag", CodeFragment}, {"https://example.com#", CodeFragment},
		// Reserved and internal names.
		{"https://localhost/", CodeHostNotAllowed}, {"https://LOCALHOST./", CodeHostNotAllowed},
		{"https://foo.localhost/", CodeHostNotAllowed}, {"https://printer.local/", CodeHostNotAllowed},
		{"https://metadata.google.internal/", CodeHostNotAllowed}, {"https://db.internal", CodeHostNotAllowed},
		{"https://router.home.arpa", CodeHostNotAllowed}, {"https://nas.lan", CodeHostNotAllowed},
		{"https://intranet/", CodeHostNotAllowed}, {"https://wiki.corp", CodeHostNotAllowed},
		// Ambiguous numeric spellings of IPv4 addresses.
		{"https://127.1/", CodeAmbiguousNumericHost}, {"https://0x7f000001/", CodeAmbiguousNumericHost},
		{"https://2130706433/", CodeAmbiguousNumericHost}, {"https://0177.0.0.1/", CodeAmbiguousNumericHost},
		{"https://0x7f.0.0.1/", CodeAmbiguousNumericHost}, {"https://1.2.3/", CodeAmbiguousNumericHost},
		{"https://010.010.010.010/", CodeAmbiguousNumericHost},
		// IP literals: blocked ranges, then all literals.
		{"https://127.0.0.1/", CodeBlockedIP}, {"https://10.0.0.5", CodeBlockedIP},
		{"https://169.254.169.254/latest/meta-data", CodeBlockedIP}, {"https://192.168.1.1", CodeBlockedIP},
		{"https://0.0.0.0", CodeBlockedIP}, {"https://[::1]/", CodeBlockedIP}, {"https://[::ffff:127.0.0.1]/", CodeBlockedIP},
		{"https://[fe80::1%25eth0]/", CodeBlockedIP}, {"https://[fd00:ec2::254]/", CodeBlockedIP},
		{"https://[64:ff9b::7f00:1]/", CodeBlockedIP}, {"https://[2002:7f00:1::1]/", CodeBlockedIP},
		{"https://8.8.8.8/", CodeIPLiteralNotAllowed}, {"https://[2606:4700:4700::1111]/", CodeIPLiteralNotAllowed},
		{"https://[not-an-ip]/", CodeInvalidHost},
		// Hostname syntax.
		{"https://-bad.example.com", CodeInvalidHost}, {"https://bad-.example.com", CodeInvalidHost},
		{"https://a..b.com", CodeInvalidHost}, {"https://.example.com", CodeInvalidHost},
		{"https://" + label64 + ".com", CodeInvalidHost}, {"https://" + host254, CodeInvalidHost},
		{"https://exa_mple.com", CodeInvalidHost}, {"https://example.com..", CodeInvalidHost},
		// Ports.
		{"https://example.com:80", CodePortNotAllowed}, {"https://example.com:22", CodePortNotAllowed},
		{"https://example.com:8443", CodePortNotAllowed}, {"https://example.com:0", CodeInvalidPort},
		{"https://example.com:65536", CodeInvalidPort}, {"https://example.com:99999999", CodeInvalidPort},
		{"https://example.com:", CodeInvalidPort}, {"https://example.com:abc", CodeInvalidPort},
		{"https://example.com:0443", CodeInvalidPort}, {"https://example.com:-1", CodeInvalidPort},
	}
	for _, c := range cases {
		_, err := ValidateDestinationURL(c.raw, DefaultURLPolicy())
		var ue *URLError
		if !errors.As(err, &ue) {
			t.Errorf("%q: got %v, want a URLError with code %s", c.raw, err, c.code)
			continue
		}
		if ue.Code != c.code {
			t.Errorf("%q: code %s, want %s", c.raw, ue.Code, c.code)
		}
		if !errors.Is(err, ErrInvalidURL) {
			t.Errorf("%q: errors.Is(err, ErrInvalidURL) must hold", c.raw)
		}
	}
}

func TestURLErrorsNeverEchoTheInput(t *testing.T) {
	for _, raw := range []string{
		"https://user:hunter2@example.com/hook?token=abc123", "https://example.com/hook?token=abc123#frag",
		"http://example.com/hook?token=abc123", "https://127.0.0.1/hook?token=abc123", "https://example.com/%0d?token=abc123",
	} {
		_, err := ValidateDestinationURL(raw, DefaultURLPolicy())
		if err == nil {
			t.Fatalf("%q must be rejected", raw)
		}
		for _, secret := range []string{"hunter2", "abc123", "example.com", "127.0.0.1"} {
			if strings.Contains(err.Error(), secret) {
				t.Errorf("error for %q leaks %q: %v", raw, secret, err)
			}
		}
	}
}

func TestPolicyCanAllowHTTPAndPortsButNeverBlockedAddresses(t *testing.T) {
	policy := URLPolicy{AllowHTTP: true, AllowedPorts: []int{80, 443, 8080}, MaxLength: 2048}
	for _, ok := range []string{"http://example.com/x", "https://example.com/x", "http://example.com:8080/x"} {
		if _, err := ValidateDestinationURL(ok, policy); err != nil {
			t.Errorf("%q should be accepted by the relaxed policy: %v", ok, err)
		}
	}
	got, _ := ValidateDestinationURL("http://example.com/x", policy)
	if got.Port != 80 || got.Normalized != "http://example.com/x" {
		t.Errorf("default port of http: %+v", got)
	}
	// Relaxing scheme and ports never relaxes the address rules.
	for _, bad := range []string{"http://127.0.0.1/", "http://localhost/", "http://169.254.169.254/", "http://[::1]:8080/", "http://2130706433/"} {
		if _, err := ValidateDestinationURL(bad, policy); err == nil {
			t.Errorf("%q must stay rejected under the relaxed policy", bad)
		}
	}
	if _, err := ValidateDestinationURL("https://example.com:9000", policy); err == nil {
		t.Error("a port outside the policy must be rejected")
	}
}

func TestDefaultPolicyIsStrict(t *testing.T) {
	p := DefaultURLPolicy()
	if p.AllowHTTP || len(p.AllowedPorts) != 1 || p.AllowedPorts[0] != 443 || p.MaxLength != 2048 {
		t.Fatalf("default policy must be https, port 443, 2048 bytes: %+v", p)
	}
}
