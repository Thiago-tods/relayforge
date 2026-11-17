package tenancy

import (
	"errors"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Validation errors for organization input.
var (
	ErrInvalidName = errors.New("invalid organization name")
	ErrInvalidSlug = errors.New("invalid organization slug")
)

const (
	maxNameLength = 100
	minSlugLength = 3
	maxSlugLength = 48
)

var slugPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// NormalizeName trims surrounding whitespace and validates an organization name:
// 1 to 100 characters, valid UTF-8, no control characters.
func NormalizeName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" || !utf8.ValidString(name) || utf8.RuneCountInString(name) > maxNameLength {
		return "", ErrInvalidName
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return "", ErrInvalidName
		}
	}
	return name, nil
}

// NormalizeSlug lowercases and trims raw, then validates it. A valid slug has 3
// to 48 characters of lowercase ASCII letters, digits and single hyphens, with no
// leading, trailing or repeated hyphen. That alphabet excludes dots, slashes and
// percent signs, so a slug can never express path traversal or an encoded path.
func NormalizeSlug(raw string) (string, error) {
	slug := strings.ToLower(strings.TrimSpace(raw))
	if len(slug) < minSlugLength || len(slug) > maxSlugLength || !slugPattern.MatchString(slug) {
		return "", ErrInvalidSlug
	}
	return slug, nil
}

// Slugify derives a candidate slug from a free-form name. Non-ASCII letters and
// punctuation become hyphens; the result is always a valid slug.
func Slugify(name string) string {
	var b strings.Builder
	lastHyphen := true // suppresses leading hyphens
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastHyphen = false
			continue
		}
		if !lastHyphen {
			b.WriteByte('-')
			lastHyphen = true
		}
	}
	slug := strings.TrimRight(b.String(), "-")
	if len(slug) > maxSlugLength-7 { // leave room for a "-xxxxxx" collision suffix
		slug = strings.TrimRight(slug[:maxSlugLength-7], "-")
	}
	if len(slug) < minSlugLength {
		slug = strings.TrimRight(slug+"-org", "-")
		if slug == "-org" || len(slug) < minSlugLength {
			slug = "org"
		}
	}
	return slug
}
