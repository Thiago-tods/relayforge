package tenancy

import (
	"strings"
	"testing"
)

func TestNormalizeSlug(t *testing.T) {
	valid := map[string]string{
		"acme": "acme", "  Acme-Corp  ": "acme-corp", "a1b": "a1b", "team-42-eu": "team-42-eu",
		strings.Repeat("a", 48): strings.Repeat("a", 48),
	}
	for raw, want := range valid {
		if got, err := NormalizeSlug(raw); err != nil || got != want {
			t.Errorf("NormalizeSlug(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	invalid := []string{
		"", "ab", "-acme", "acme-", "ac--me", "ac me", "acme_corp", "acme.corp", "../etc/passwd",
		"acme/evil", "acme%2fevil", "..", "acme\x00", "ácme", strings.Repeat("a", 49),
		"<script>", "acme;drop",
	}
	for _, raw := range invalid {
		if got, err := NormalizeSlug(raw); err == nil {
			t.Errorf("NormalizeSlug(%q) = %q; want error", raw, got)
		}
	}
}

func TestSlugifyAlwaysProducesValidSlugs(t *testing.T) {
	for _, name := range []string{
		"Acme Corp", "  spaced   out  ", "Ünïcödé Name", "日本語", "!!!", "a", "ab", "x y",
		strings.Repeat("long name ", 20), "../../etc", "A&B Co.", "1234",
	} {
		slug := Slugify(name)
		if _, err := NormalizeSlug(slug); err != nil {
			t.Errorf("Slugify(%q) = %q is not a valid slug", name, slug)
		}
		if len(slug) > maxSlugLength-7 {
			t.Errorf("Slugify(%q) = %q leaves no room for a collision suffix", name, slug)
		}
	}
	if got := Slugify("Acme Corp"); got != "acme-corp" {
		t.Errorf("Slugify(Acme Corp) = %q", got)
	}
}

func TestNormalizeName(t *testing.T) {
	if got, err := NormalizeName("  Acme Corp  "); err != nil || got != "Acme Corp" {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, err := NormalizeName("日本語の組織"); err != nil {
		t.Fatalf("unicode names must be accepted: %v", err)
	}
	for _, bad := range []string{"", "   ", "bad\x00name", "line\nbreak", strings.Repeat("x", 101), "bad\xffutf8"} {
		if _, err := NormalizeName(bad); err == nil {
			t.Errorf("NormalizeName(%q) should fail", bad)
		}
	}
}
