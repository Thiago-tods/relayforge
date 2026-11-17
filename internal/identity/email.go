// Package identity implements user identities: email normalization, password
// policy and hashing, opaque tokens, and the user and session repositories.
package identity

import (
	"errors"
	"net/mail"
	"strings"
	"unicode"
)

// ErrInvalidEmail is returned for addresses that cannot be accepted.
var ErrInvalidEmail = errors.New("invalid email address")

const (
	maxEmailLength      = 254
	maxEmailLocalLength = 64
)

// NormalizeEmail validates raw and returns its canonical form, used for
// uniqueness and lookups.
//
// Rules: surrounding whitespace is trimmed; the address must be a single plain
// addr-spec (no display name, no comments, no angle brackets); only printable
// ASCII is accepted, which avoids Unicode lookalike and normalization
// ambiguities; the local part is at most 64 and the whole address at most 254
// characters; the result is lowercased. Plus-addressing is not collapsed.
func NormalizeEmail(raw string) (string, error) {
	email := strings.TrimSpace(raw)
	if email == "" || len(email) > maxEmailLength {
		return "", ErrInvalidEmail
	}
	for _, r := range email {
		if r > unicode.MaxASCII || unicode.IsSpace(r) || unicode.IsControl(r) {
			return "", ErrInvalidEmail
		}
	}
	addr, err := mail.ParseAddress(email)
	if err != nil || addr.Name != "" || addr.Address != email {
		return "", ErrInvalidEmail
	}
	at := strings.LastIndex(email, "@")
	if at < 1 || at == len(email)-1 || at > maxEmailLocalLength {
		return "", ErrInvalidEmail
	}
	domain := email[at+1:]
	if !strings.Contains(domain, ".") || strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") ||
		strings.Contains(domain, "..") {
		return "", ErrInvalidEmail
	}
	return strings.ToLower(email), nil
}
