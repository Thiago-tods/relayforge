package httpapi_test

import (
	"strings"
	"testing"
	"time"
)

// TestDeliveryListWithFailureReasonAuthorizesBeforeItValidatesAnything is the correction to ADR 32's
// authorization-first rule: for a request that carries failureReason the handler no longer reads pagination or
// timestamps before the service has authorized the caller. Every malformed parameter below is a 404 for a caller who is
// not a member, and a 400 for a member, so the status never tells an outsider whether the organization exists.
func TestDeliveryListWithFailureReasonAuthorizesBeforeItValidatesAnything(t *testing.T) {
	s := newServer(t, false)
	fs := newFailSeed(t, s, "Target")
	fs.failed(s.env.Clock.Now().Add(-time.Hour), "RETRIES_EXHAUSTED")
	org := "/api/v1/organizations/" + fs.is.org
	stranger := s.client(t)
	stranger.register("authz-stranger@example.com")
	viewer := join(t, fs.is.c, s, fs.is.org, "authz-viewer@example.com", "VIEWER")

	const reason = "failureReason=RETRIES_EXHAUSTED"
	malformed := map[string]string{
		"limit zero":           reason + "&limit=0",
		"limit too large":      reason + "&limit=101",
		"limit not a number":   reason + "&limit=ten",
		"offset negative":      reason + "&offset=-1",
		"offset not a number":  reason + "&offset=x",
		"from malformed":       reason + "&from=soon",
		"to malformed":         reason + "&to=later",
		"both malformed":       reason + "&from=soon&to=later",
		"from after to":        reason + "&from=2026-11-18T10:00:00Z&to=2026-11-18T09:00:00Z",
		"date only":            reason + "&from=2026-11-18",
		"destination not uuid": reason + "&destinationId=x",
		"event not uuid":       reason + "&eventId=x",
		"status unknown":       reason + "&status=BOGUS",
		"status conflict":      reason + "&status=PENDING",
		"reason unknown":       "failureReason=nope",
		"reason wrong case":    "failureReason=retries_exhausted",
		"reason and garbage":   "failureReason=nope&limit=0&from=soon&offset=-1&status=BOGUS",
	}
	valid := fs.is.c.do("GET", org+"/deliveries?"+reason, nil)
	if valid.Status != 200 || valid.json()["total"] != float64(1) {
		t.Fatalf("setup: %d %s", valid.Status, valid.Body)
	}
	// What an outsider sees for a perfectly valid request, as the reference for the bytes of every other 404.
	reference := stranger.do("GET", org+"/deliveries?"+reason, nil)
	if reference.Status != 404 || reference.errorCode() != "not_found" {
		t.Fatalf("a non-member with a valid request: %d %s", reference.Status, reference.Body)
	}

	for name, query := range malformed {
		// A non-member: the same 404 as for a valid request, byte for byte, whatever the parameters say.
		r := stranger.do("GET", org+"/deliveries?"+query, nil)
		if r.Status != 404 || r.errorCode() != "not_found" || r.Body != reference.Body {
			t.Errorf("%s, non-member: %d %s, want the 404 of a valid request (%s)", name, r.Status, r.Body, reference.Body)
		}
		// A member (any role that can read deliveries) gets the validation error for the same input.
		for who, c := range map[string]*client{"owner": fs.is.c, "viewer": viewer} {
			m := c.do("GET", org+"/deliveries?"+query, nil)
			if m.Status != 400 || m.errorCode() != "validation_failed" {
				t.Errorf("%s, %s: %d %s, want 400 validation_failed", name, who, m.Status, m.Body)
			}
			if strings.Contains(m.Body, "SQLSTATE") {
				t.Errorf("%s, %s: the error exposes internals: %s", name, who, m.Body)
			}
		}
	}

	// A malformed or unknown organization id is the same 404, with the same garbage.
	for _, id := range []string{"not-a-uuid", "00000000-0000-4000-8000-000000000000"} {
		for _, query := range []string{reason + "&limit=0&from=soon", "failureReason=nope&offset=-1"} {
			r := fs.is.c.do("GET", "/api/v1/organizations/"+id+"/deliveries?"+query, nil)
			if r.Status != 404 || r.errorCode() != "not_found" || r.Body != reference.Body {
				t.Errorf("organization %s ?%s: %d %s", id, query, r.Status, r.Body)
			}
		}
	}
	// Authentication still comes first of all: no session is a 401 whatever the parameters say.
	for _, query := range []string{reason, reason + "&limit=0", "failureReason=nope&from=soon"} {
		if r := s.client(t).do("GET", org+"/deliveries?"+query, nil); r.Status != 401 || r.errorCode() != "unauthenticated" {
			t.Errorf("anonymous ?%s: %d %s", query, r.Status, r.Body)
		}
	}
	// Once authorized, a valid filtered request is unchanged: the exact total, the offset pagination and the same page.
	page := fs.is.c.do("GET", org+"/deliveries?"+reason+"&limit=5&offset=0&from="+s.env.Clock.Now().Add(-3*time.Hour).UTC().Format(time.RFC3339)+"&to="+s.env.Clock.Now().Add(time.Hour).UTC().Format(time.RFC3339), nil)
	if page.Status != 200 || page.json()["total"] != float64(1) || page.json()["limit"] != float64(5) || page.json()["offset"] != float64(0) {
		t.Errorf("a valid filtered request: %d %s", page.Status, page.Body)
	}
	// The value is only ever consulted when it is non-empty: an empty failureReason is no filter.
	if r := stranger.do("GET", org+"/deliveries?failureReason=&limit=0", nil); r.Status != 400 {
		t.Errorf("an empty failureReason is an unfiltered request: %d %s", r.Status, r.Body)
	}
}

// TestDeliveryListWithoutFailureReasonKeepsItsExactBehavior pins what the correction must not change: the unfiltered
// handler validates pagination and timestamps in the order it always did, answers the same errors, and gives identical
// bytes with and without an empty failureReason.
func TestDeliveryListWithoutFailureReasonKeepsItsExactBehavior(t *testing.T) {
	s := newServer(t, false)
	fs := newFailSeed(t, s, "Target")
	fs.failed(s.env.Clock.Now().Add(-time.Hour), "RETRIES_EXHAUSTED")
	fs.failed(s.env.Clock.Now().Add(-2*time.Hour), "")
	org := "/api/v1/organizations/" + fs.is.org
	stranger := s.client(t)
	stranger.register("unfiltered-stranger@example.com")

	// Bodies: identical with and without the empty parameter, for several queries.
	for _, q := range []string{"", "?limit=1", "?limit=1&offset=1", "?status=FAILED", "?status=FAILED&limit=100&offset=0", "?destinationId=" + fs.dest} {
		plain := fs.is.c.do("GET", org+"/deliveries"+q, nil)
		sep := "?"
		if q != "" {
			sep = "&"
		}
		empty := fs.is.c.do("GET", org+"/deliveries"+q+sep+"failureReason=", nil)
		if plain.Status != 200 || plain.Body != empty.Body {
			t.Errorf("%q: the response changed:\n%s\n%s", q, plain.Body, empty.Body)
		}
	}
	// The items keep their exact shape: a legacy row omits failureReason, a reasoned row has it, and no field is null.
	body := fs.is.c.do("GET", org+"/deliveries?status=FAILED", nil).Body
	if !strings.Contains(body, `"failureReason":"RETRIES_EXHAUSTED"`) || strings.Contains(body, `null`) || strings.Count(body, `"failureReason"`) != 1 {
		t.Errorf("the list items changed: %s", body)
	}

	// Validation errors of the unfiltered handler: the same codes and messages, in the same order (pagination first).
	for q, want := range map[string]string{
		"?limit=0":           "limit must be an integer from 1 to 100",
		"?limit=101":         "limit must be an integer from 1 to 100",
		"?offset=-1":         "offset must be a non-negative integer",
		"?limit=0&from=soon": "limit must be an integer from 1 to 100",
		"?from=soon":         "from must be an RFC 3339 timestamp",
		"?to=later":          "to must be an RFC 3339 timestamp",
		"?status=BOGUS":      "status must be PENDING, IN_FLIGHT, SUCCEEDED, FAILED or CANCELLED",
		"?destinationId=x":   "destinationId and eventId must be UUIDs",
		"?from=2026-11-18T10:00:00Z&to=2026-11-18T09:00:00Z": "from must be before to",
	} {
		r := fs.is.c.do("GET", org+"/deliveries"+q, nil)
		if r.Status != 400 || r.errorCode() != "validation_failed" || !strings.Contains(r.Body, want) {
			t.Errorf("member %s: %d %s, want 400 %q", q, r.Status, r.Body, want)
		}
	}
	// Unchanged for a non-member of an unfiltered request too: valid input is a 404, as always.
	if r := stranger.do("GET", org+"/deliveries", nil); r.Status != 404 || r.errorCode() != "not_found" {
		t.Errorf("non-member, valid unfiltered request: %d %s", r.Status, r.Body)
	}
	// A filtered and an unfiltered request for the same rows return the same item bytes.
	all := rawItems(t, fs.is.c.do("GET", org+"/deliveries?status=FAILED", nil))
	filtered := rawItems(t, fs.is.c.do("GET", org+"/deliveries?failureReason=RETRIES_EXHAUSTED", nil))
	if len(filtered) != 1 {
		t.Fatalf("%d filtered items", len(filtered))
	}
	for id, raw := range filtered {
		if all[id] != raw {
			t.Errorf("item %s differs:\n%s\n%s", id, all[id], raw)
		}
	}
}

// TestDeliveryListWithRepeatedFailureReasonAuthorizesFirst covers the bypass found in review: the path was chosen from
// the first value of failureReason only, so "failureReason=&failureReason=X&limit=0" counted as unfiltered and its
// pagination was read before authorization. Every repeated form is now authorized first, and a member gets 400.
func TestDeliveryListWithRepeatedFailureReasonAuthorizesFirst(t *testing.T) {
	s := newServer(t, false)
	fs := newFailSeed(t, s, "Target")
	fs.failed(s.env.Clock.Now().Add(-time.Hour), "RETRIES_EXHAUSTED")
	org := "/api/v1/organizations/" + fs.is.org
	stranger := s.client(t)
	stranger.register("dup-stranger@example.com")
	viewer := join(t, fs.is.c, s, fs.is.org, "dup-viewer@example.com", "VIEWER")

	reference := stranger.do("GET", org+"/deliveries?failureReason=RETRIES_EXHAUSTED", nil)
	if reference.Status != 404 || reference.errorCode() != "not_found" {
		t.Fatalf("a non-member with a valid request: %d %s", reference.Status, reference.Body)
	}
	repeated := map[string]string{
		"empty then value":     "failureReason=&failureReason=RETRIES_EXHAUSTED",
		"value then empty":     "failureReason=RETRIES_EXHAUSTED&failureReason=",
		"two different values": "failureReason=RETRIES_EXHAUSTED&failureReason=HTTP_4XX",
		"two equal values":     "failureReason=RETRIES_EXHAUSTED&failureReason=RETRIES_EXHAUSTED",
		"two empty values":     "failureReason=&failureReason=",
		"three values":         "failureReason=&failureReason=&failureReason=HTTP_4XX",
	}
	for name, base := range repeated {
		for _, extra := range []string{"", "&limit=0", "&limit=ten&offset=-1&from=soon&status=BOGUS"} {
			query := base + extra
			r := stranger.do("GET", org+"/deliveries?"+query, nil)
			if r.Status != 404 || r.errorCode() != "not_found" || r.Body != reference.Body {
				t.Errorf("%s ?%s, non-member: %d %s, want the 404 of a valid request", name, query, r.Status, r.Body)
			}
			for who, c := range map[string]*client{"owner": fs.is.c, "viewer": viewer} {
				m := c.do("GET", org+"/deliveries?"+query, nil)
				if m.Status != 400 || m.errorCode() != "validation_failed" {
					t.Errorf("%s ?%s, %s: %d %s, want 400 validation_failed", name, query, who, m.Status, m.Body)
				}
			}
			if a := s.client(t).do("GET", org+"/deliveries?"+query, nil); a.Status != 401 || a.errorCode() != "unauthenticated" {
				t.Errorf("%s ?%s, anonymous: %d %s", name, query, a.Status, a.Body)
			}
			for _, id := range []string{"not-a-uuid", "00000000-0000-4000-8000-000000000000"} {
				if r := fs.is.c.do("GET", "/api/v1/organizations/"+id+"/deliveries?"+query, nil); r.Status != 404 || r.Body != reference.Body {
					t.Errorf("%s ?%s, organization %s: %d %s", name, query, id, r.Status, r.Body)
				}
			}
		}
	}

	// A single empty failureReason and no failureReason at all are the unfiltered request they always were.
	for _, q := range []string{"failureReason=&limit=0", "limit=0"} {
		if r := fs.is.c.do("GET", org+"/deliveries?"+q, nil); r.Status != 400 || !strings.Contains(r.Body, "limit must be an integer from 1 to 100") {
			t.Errorf("member ?%s: %d %s", q, r.Status, r.Body)
		}
		if r := stranger.do("GET", org+"/deliveries?"+q, nil); r.Status != 400 {
			t.Errorf("non-member ?%s keeps the unfiltered order (pagination first): %d %s", q, r.Status, r.Body)
		}
	}
	plain := fs.is.c.do("GET", org+"/deliveries", nil)
	single := fs.is.c.do("GET", org+"/deliveries?failureReason=", nil)
	if plain.Status != 200 || plain.Body != single.Body {
		t.Errorf("a single empty failureReason changed the response:\n%s\n%s", plain.Body, single.Body)
	}
	// A single valid reason still lists exactly as before.
	if r := fs.is.c.do("GET", org+"/deliveries?failureReason=RETRIES_EXHAUSTED", nil); r.Status != 200 || r.json()["total"] != float64(1) {
		t.Errorf("a single reason: %d %s", r.Status, r.Body)
	}
}
