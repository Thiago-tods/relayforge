package egress

import (
	"errors"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

// Reason codes of URLError. They describe the problem without echoing the input,
// because destination URLs may carry tokens in their path or query.
const (
	CodeEmpty                = "empty"
	CodeTooLong              = "too_long"
	CodeInvalidCharacters    = "invalid_characters"
	CodeEncodedControl       = "encoded_control_character"
	CodeMalformed            = "malformed"
	CodeSchemeNotAllowed     = "scheme_not_allowed"
	CodeCredentials          = "credentials_not_allowed"
	CodeFragment             = "fragment_not_allowed"
	CodeMissingHost          = "missing_host"
	CodeHostNotAllowed       = "host_not_allowed"
	CodeInvalidHost          = "invalid_host"
	CodeAmbiguousNumericHost = "ambiguous_numeric_host"
	CodeBlockedIP            = "blocked_ip_address"
	CodeIPLiteralNotAllowed  = "ip_literal_not_allowed"
	CodeInvalidPort          = "invalid_port"
	CodePortNotAllowed       = "port_not_allowed"
)

// ErrInvalidURL is matched by errors.Is for every URL rejection.
var ErrInvalidURL = errors.New("invalid destination URL")

// URLError is the error returned for a rejected URL. Its message names only the
// reason code, never the URL.
type URLError struct{ Code string }

func (e *URLError) Error() string { return "invalid destination URL: " + e.Code }

// Is makes errors.Is(err, ErrInvalidURL) true.
func (e *URLError) Is(target error) bool { return target == ErrInvalidURL }

func reject(code string) (ValidatedURL, error) { return ValidatedURL{}, &URLError{Code: code} }

// URLPolicy controls which URLs are acceptable. It never relaxes the address
// rules: blocked IP ranges, reserved names and ambiguous numeric hosts are always
// rejected.
type URLPolicy struct {
	// AllowHTTP permits http:// in addition to https://. Off by default.
	AllowHTTP bool
	// AllowedPorts lists the permitted ports; the scheme's default port is
	// subject to the same list. Default: 443 only.
	AllowedPorts []int
	// MaxLength bounds the raw URL in bytes. Default: 2048.
	MaxLength int
}

// DefaultURLPolicy is the production policy: https, port 443, at most 2048 bytes.
func DefaultURLPolicy() URLPolicy {
	return URLPolicy{AllowedPorts: []int{443}, MaxLength: 2048}
}

// ValidatedURL is a destination URL that passed validation.
type ValidatedURL struct {
	// Normalized is the canonical form to store: lowercase scheme and host, no
	// trailing dot, default port omitted, no fragment.
	Normalized string
	// Display is Normalized without the query string. Show or log this form only;
	// the query may hold a secret token.
	Display string
	Scheme  string
	Host    string
	Port    int
}

// reservedSuffixes are names that identify internal networks.
var reservedSuffixes = []string{
	".localhost", ".local", ".internal", ".localdomain", ".home.arpa", ".lan", ".intranet", ".corp", ".home",
}

// ValidateDestinationURL checks a destination URL against policy without any
// network access. It is a first filter only; the connection itself must still
// be restricted with BlockedReason at connect time, because DNS can change
// after validation.
//
// Internationalized names must be supplied in punycode (xn--...): non-ASCII
// input is rejected rather than converted, which avoids homograph and
// normalization surprises.
func ValidateDestinationURL(raw string, policy URLPolicy) (ValidatedURL, error) {
	if raw == "" {
		return reject(CodeEmpty)
	}
	maxLen := policy.MaxLength
	if maxLen <= 0 {
		maxLen = 2048
	}
	if len(raw) > maxLen {
		return reject(CodeTooLong)
	}
	for i := 0; i < len(raw); i++ {
		if c := raw[i]; c <= 0x20 || c >= 0x7f || c == '\\' {
			return reject(CodeInvalidCharacters)
		}
	}
	if hasEncodedControl(raw) {
		return reject(CodeEncodedControl)
	}

	u, err := url.Parse(raw)
	if err != nil {
		switch {
		case strings.Contains(err.Error(), "invalid port"):
			return reject(CodeInvalidPort)
		case strings.Contains(raw, "["): // a malformed bracketed IP literal
			return reject(CodeInvalidHost)
		}
		return reject(CodeMalformed)
	}

	scheme := strings.ToLower(u.Scheme)
	if scheme != "https" && !(scheme == "http" && policy.AllowHTTP) {
		return reject(CodeSchemeNotAllowed)
	}
	if u.Opaque != "" {
		return reject(CodeMalformed)
	}
	if u.User != nil {
		return reject(CodeCredentials)
	}
	if strings.Contains(raw, "#") {
		return reject(CodeFragment)
	}
	if u.Host == "" {
		return reject(CodeMissingHost)
	}

	port, code := parsePort(u, scheme, policy)
	if code != "" {
		return reject(code)
	}

	host, code := validateHost(u)
	if code != "" {
		return reject(code)
	}

	n := scheme + "://" + host
	if port != defaultPort(scheme) {
		n += ":" + strconv.Itoa(port)
	}
	n += u.EscapedPath()
	display := n
	if u.RawQuery != "" || u.ForceQuery {
		n += "?" + u.RawQuery
	}
	return ValidatedURL{Normalized: n, Display: display, Scheme: scheme, Host: host, Port: port}, nil
}

func defaultPort(scheme string) int {
	if scheme == "http" {
		return 80
	}
	return 443
}

func parsePort(u *url.URL, scheme string, policy URLPolicy) (int, string) {
	port := defaultPort(scheme)
	if strings.HasSuffix(u.Host, ":") {
		return 0, CodeInvalidPort // "host:" with an empty port
	}
	if s := u.Port(); s != "" {
		if len(s) > 5 || s[0] == '0' {
			return 0, CodeInvalidPort
		}
		for i := 0; i < len(s); i++ {
			if s[i] < '0' || s[i] > '9' {
				return 0, CodeInvalidPort
			}
		}
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > 65535 {
			return 0, CodeInvalidPort
		}
		port = n
	}
	allowed := policy.AllowedPorts
	if len(allowed) == 0 {
		allowed = []int{443}
	}
	for _, p := range allowed {
		if p == port {
			return port, ""
		}
	}
	return 0, CodePortNotAllowed
}

// validateHost returns the normalized host or a reason code.
func validateHost(u *url.URL) (string, string) {
	hostname := u.Hostname()
	if hostname == "" {
		return "", CodeMissingHost
	}

	// IP literals: report blocked ranges precisely, then refuse every literal.
	// Destinations must be DNS names so that connect-time checks apply to the answer.
	if strings.HasPrefix(u.Host, "[") {
		addr, err := netip.ParseAddr(hostname)
		if err != nil {
			return "", CodeInvalidHost
		}
		return "", ipLiteralCode(addr)
	}
	if addr, err := netip.ParseAddr(hostname); err == nil {
		return "", ipLiteralCode(addr)
	}

	host := strings.ToLower(hostname)
	host = strings.TrimSuffix(host, ".") // a single trailing dot marks a fully qualified name
	if host == "" || len(host) > 253 {
		return "", CodeInvalidHost
	}
	labels := strings.Split(host, ".")
	for _, l := range labels {
		if !validLabel(l) {
			return "", CodeInvalidHost
		}
	}
	// WHATWG "ends in a number": 127.1, 0x7f000001, 2130706433 and 0177.0.0.1 are
	// all spellings of IPv4 addresses that some resolvers accept.
	if last := labels[len(labels)-1]; isNumericLabel(last) {
		return "", CodeAmbiguousNumericHost
	}
	if len(labels) < 2 || host == "localhost" {
		return "", CodeHostNotAllowed
	}
	for _, suffix := range reservedSuffixes {
		if strings.HasSuffix("."+host, suffix) {
			return "", CodeHostNotAllowed
		}
	}
	return host, ""
}

func ipLiteralCode(addr netip.Addr) string {
	if BlockedReason(addr) != "" {
		return CodeBlockedIP
	}
	return CodeIPLiteralNotAllowed
}

func validLabel(l string) bool {
	if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
		return false
	}
	for i := 0; i < len(l); i++ {
		c := l[i]
		if !(c >= 'a' && c <= 'z') && !(c >= '0' && c <= '9') && c != '-' {
			return false
		}
	}
	return true
}

// isNumericLabel reports whether a label is all decimal digits or a 0x hex number.
func isNumericLabel(l string) bool {
	if strings.HasPrefix(l, "0x") {
		for i := 2; i < len(l); i++ {
			c := l[i]
			if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
				return false
			}
		}
		return true
	}
	for i := 0; i < len(l); i++ {
		if l[i] < '0' || l[i] > '9' {
			return false
		}
	}
	return len(l) > 0
}

// hasEncodedControl reports a percent-encoded control character (%00-%1f, %7f),
// which could smuggle line breaks or NUL bytes into a request.
func hasEncodedControl(s string) bool {
	for i := 0; i+2 < len(s); i++ {
		if s[i] != '%' {
			continue
		}
		hi, lo := unhex(s[i+1]), unhex(s[i+2])
		if hi < 0 || lo < 0 {
			continue
		}
		if v := hi<<4 | lo; v < 0x20 || v == 0x7f {
			return true
		}
	}
	return false
}

func unhex(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}
