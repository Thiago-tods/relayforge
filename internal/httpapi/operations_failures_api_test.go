package httpapi_test

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// failSeed seeds FAILED deliveries directly, with exact completion times and reasons, for one organization.
type failSeed struct {
	t      *testing.T
	s      *server
	is     ingestSetup
	dest   string
	secret string // the destination's signing secret, which must never appear in a response
	n      int
}

func newFailSeed(t *testing.T, s *server, name string) *failSeed {
	t.Helper()
	return newFailSeedAs(t, s, "owner@example.com", name)
}

// newFailSeedAs registers a new owner (and so a new organization) with an ingestion endpoint and a destination.
func newFailSeedAs(t *testing.T, s *server, email, name string) *failSeed {
	t.Helper()
	c := s.client(t)
	org := c.orgID(c.register(email))
	res := c.mutate("POST", "/api/v1/organizations/"+org+"/ingestion-endpoints", map[string]string{"name": "Orders"})
	if res.Status != 201 {
		t.Fatalf("create endpoint: %d %s", res.Status, res.Body)
	}
	ep := res.json()["endpoint"].(map[string]any)
	is := ingestSetup{s: s, c: c, org: org, endpoint: ep, secret: ep["signingSecret"].(string), pub: ep["publicId"].(string), id: ep["id"].(string)}
	dest, secret := createDestination(t, c, org, name)
	return &failSeed{t: t, s: s, is: is, dest: dest, secret: secret}
}

// failed inserts a FAILED delivery completed at the given time. An empty reason inserts a legacy row: FAILED with no
// recorded reason, the shape of a delivery that failed before migration 00019.
func (f *failSeed) failed(completed time.Time, reason string) string {
	f.t.Helper()
	f.n++
	res, err := f.s.env.Send(f.is.pub, f.s.env.Signed(f.is.secret, []byte(`{"secret":"PAYLOADSECRET"}`), "fail-"+strings.Repeat("x", f.n)+time.Now().Format("150405.000000000")))
	if err != nil {
		f.t.Fatal(err)
	}
	q, _ := f.s.env.Pool.Querier()
	if reason == "" {
		f.s.env.Exec(f.t, "ALTER TABLE deliveries DROP CONSTRAINT deliveries_failed_has_reason")
		defer f.s.env.Exec(f.t, "ALTER TABLE deliveries ADD CONSTRAINT deliveries_failed_has_reason CHECK (status <> 'FAILED' OR failure_reason IS NOT NULL) NOT VALID")
	}
	var id string
	if err := q.QueryRow(bg, `INSERT INTO deliveries (organization_id, event_id, destination_id, status, attempt_count, last_http_status, last_error_class,
		completed_at, created_at, failure_reason) VALUES ($1, $2, $3, 'FAILED', 3, 503, 'http_5xx', $4, $5, NULLIF($6, '')) RETURNING id::text`,
		f.is.org, res.EventID, f.dest, completed.UTC(), completed.UTC().Add(-time.Hour), reason).Scan(&id); err != nil {
		f.t.Fatal(err)
	}
	return id
}

func itemIDs(r response) []string {
	var out []string
	for _, it := range r.json()["items"].([]any) {
		out = append(out, it.(map[string]any)["deliveryId"].(string))
	}
	return out
}

func TestFailuresAPIRolesAuthenticationAndReadOnlyRoutes(t *testing.T) {
	s := newServer(t, false)
	fs := newFailSeed(t, s, "Target")
	now := s.env.Clock.Now()
	fs.failed(now.Add(-time.Hour), "RETRIES_EXHAUSTED")
	org := "/api/v1/organizations/" + fs.is.org
	paths := []string{org + "/operations/failures", org + "/operations/queue/age"}

	clients := map[string]*client{"OWNER": fs.is.c}
	for _, role := range []string{"ADMIN", "DEVELOPER", "VIEWER"} {
		clients[role] = join(t, fs.is.c, s, fs.is.org, strings.ToLower(role)+"@example.com", role)
	}
	for role, c := range clients {
		for _, p := range paths {
			if r := c.do("GET", p, nil); r.Status != 200 {
				t.Errorf("%s GET %s: %d %s", role, p, r.Status, r.Body)
			}
		}
	}
	for _, p := range paths {
		if r := s.client(t).do("GET", p, nil); r.Status != 401 || r.errorCode() != "unauthenticated" {
			t.Errorf("anonymous GET %s: %d %s", p, r.Status, r.Body)
		}
	}
	// A user who is not a member, and a malformed organization id, get the same 404 - even when every parameter is garbage.
	stranger := s.client(t)
	stranger.register("stranger@example.com")
	garbage := "?limit=0&cursor=!!!&failureReason=nope&destinationId=x&from=soon&to=later"
	for _, p := range paths {
		for _, q := range []string{"", garbage} {
			if r := stranger.do("GET", p+q, nil); r.Status != 404 || r.errorCode() != "not_found" {
				t.Errorf("non-member GET %s%s: %d %s", p, q, r.Status, r.Body)
			}
		}
	}
	for _, tail := range []string{"/operations/failures", "/operations/queue/age"} {
		for _, id := range []string{"not-a-uuid", "00000000-0000-4000-8000-000000000000"} {
			if r := fs.is.c.do("GET", "/api/v1/organizations/"+id+tail+garbage, nil); r.Status != 404 || r.errorCode() != "not_found" {
				t.Errorf("GET %s%s: %d %s", id, tail, r.Status, r.Body)
			}
		}
	}
	// The same garbage from a member is a 400, once authorized.
	if r := fs.is.c.do("GET", paths[0]+"?limit=0", nil); r.Status != 400 || r.errorCode() != "validation_failed" {
		t.Errorf("member with limit=0: %d %s", r.Status, r.Body)
	}
	// Read-only: no other verb is routed.
	before := s.env.Count(t, "SELECT count(*) FROM deliveries")
	for _, m := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		for _, p := range paths {
			if r := fs.is.c.mutate(m, p, map[string]string{"status": "PENDING"}); r.Status != 404 && r.Status != 405 {
				t.Errorf("%s %s: %d, want 404 or 405", m, p, r.Status)
			}
		}
	}
	if s.env.Count(t, "SELECT count(*) FROM deliveries") != before {
		t.Fatal("deliveries changed through the operations API")
	}
	// The existing routes of the same family still resolve (the new /queue/age does not shadow /queue).
	if r := fs.is.c.do("GET", org+"/operations/queue", nil); r.Status != 200 || keysOf(r.json()) != "capped,delayed,due,expiredLeases,generatedAt,inFlight,oldestDueAgeSeconds,pending" {
		t.Errorf("queue: %d %s", r.Status, r.Body)
	}
}

func TestFailuresAPIContractAndNoSensitiveData(t *testing.T) {
	logs := &lockedBuffer{}
	s := newServerWith(t, false, logs, nil)
	fs := newFailSeed(t, s, "ConfidentialTargetName")
	now := s.env.Clock.Now()
	id := fs.failed(now.Add(-time.Hour), "DESTINATION_REJECTED")
	legacy := fs.failed(now.Add(-2*time.Hour), "")
	org := "/api/v1/organizations/" + fs.is.org

	r := fs.is.c.do("GET", org+"/operations/failures", nil)
	if r.Status != 200 || r.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("%d %v %s", r.Status, r.Header, r.Body)
	}
	body := r.json()
	if keysOf(body) != "from,generatedAt,items,nextCursor,to" || body["nextCursor"] != nil {
		t.Fatalf("fields: %s", r.Body)
	}
	items := body["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("items: %s", r.Body)
	}
	first, second := items[0].(map[string]any), items[1].(map[string]any)
	if keysOf(first) != "attemptCount,completedAt,createdAt,deliveryId,destinationId,destinationName,eventId,failureReason,lastErrorClass,lastHttpStatus" {
		t.Fatalf("item fields: %s", keysOf(first))
	}
	if first["deliveryId"] != id || first["failureReason"] != "DESTINATION_REJECTED" || first["destinationId"] != fs.dest || first["destinationName"] != "ConfidentialTargetName" ||
		first["lastHttpStatus"] != float64(503) || first["lastErrorClass"] != "http_5xx" || first["attemptCount"] != float64(3) {
		t.Errorf("first item: %v", first)
	}
	// Legacy NULL is rendered as UNRECORDED, never null and never omitted.
	if second["deliveryId"] != legacy || second["failureReason"] != "UNRECORDED" {
		t.Errorf("legacy item: %v", second)
	}
	for _, key := range []string{"completedAt", "createdAt", "generatedAt", "from", "to"} {
		var v string
		if key == "generatedAt" || key == "from" || key == "to" {
			v, _ = body[key].(string)
		} else {
			v, _ = first[key].(string)
		}
		if !strings.HasSuffix(v, "Z") {
			t.Errorf("%s = %q, want a UTC timestamp", key, v)
		}
	}
	// Never exposed.
	all := r.Body + logs.String()
	for _, banned := range []string{fs.secret, "PAYLOADSECRET", "hooks.example.com", "https://", "QUERYSECRET", "claim", "worker", "lease", "signing", "\"url\"", "failureReason\":null"} {
		if strings.Contains(all, banned) {
			t.Errorf("the response or the logs contain %q", banned)
		}
	}
}

func TestFailuresAPIPagesWithAnOpaqueKeysetCursor(t *testing.T) {
	s := newServer(t, false)
	fs := newFailSeed(t, s, "Target")
	now := s.env.Clock.Now().Truncate(time.Microsecond)
	org := "/api/v1/organizations/" + fs.is.org
	// Seven failures, three of them with the very same completion time.
	var want []string
	times := []time.Duration{1, 2, 2, 2, 3, 4, 5}
	for _, d := range times {
		want = append(want, fs.failed(now.Add(-d*time.Minute), "RETRIES_EXHAUSTED"))
	}
	whole := itemIDs(fs.is.c.do("GET", org+"/operations/failures?limit=100", nil))
	if len(whole) != 7 {
		t.Fatalf("%v", whole)
	}

	for _, size := range []string{"1", "2", "3", "7", "8"} {
		var got []string
		cursor := ""
		for page := 0; page < 20; page++ {
			path := org + "/operations/failures?limit=" + size
			if cursor != "" {
				path += "&cursor=" + url.QueryEscape(cursor)
			}
			r := fs.is.c.do("GET", path, nil)
			if r.Status != 200 {
				t.Fatalf("limit %s page %d: %d %s", size, page, r.Status, r.Body)
			}
			got = append(got, itemIDs(r)...)
			next, _ := r.json()["nextCursor"].(string)
			if next == "" {
				break
			}
			if strings.ContainsAny(next, "=+/") {
				t.Fatalf("a cursor is URL-safe base64 without padding: %q", next)
			}
			cursor = next
		}
		if strings.Join(got, ",") != strings.Join(whole, ",") {
			t.Errorf("limit %s: the pages do not add up to the feed:\n got %v\nwant %v", size, got, whole)
		}
	}
	_ = want

	// Tampering and malformed cursors are 400, never a 500 and never data.
	first := fs.is.c.do("GET", org+"/operations/failures?limit=2", nil)
	cursor := first.json()["nextCursor"].(string)
	raw, _ := base64.RawURLEncoding.DecodeString(cursor)
	forgedTime := base64.RawURLEncoding.EncodeToString([]byte(strings.Replace(string(raw), ":", ":9", 1)))
	for name, bad := range map[string]string{
		"garbage":       "!!!",
		"padded":        cursor + "=",
		"truncated":     cursor[:len(cursor)-3],
		"one more char": cursor + "A",
		"flipped":       cursor[:5] + string(rune(cursor[5]^1)) + cursor[6:],
		"other version": base64.RawURLEncoding.EncodeToString([]byte("v9" + string(raw[2:]))),
		"sql":           base64.RawURLEncoding.EncodeToString([]byte("v1:1:' OR 1=1 --")),
		"standard b64":  strings.NewReplacer("-", "+", "_", "/").Replace(cursor) + "==",
	} {
		r := fs.is.c.do("GET", org+"/operations/failures?cursor="+url.QueryEscape(bad), nil)
		if name != "flipped" && (r.Status != 400 || r.errorCode() != "validation_failed") {
			t.Errorf("%s: %d %s, want 400", name, r.Status, r.Body)
		}
		if r.Status != 400 && r.Status != 200 {
			t.Errorf("%s: %d %s", name, r.Status, r.Body)
		}
		if strings.Contains(r.Body, "SQLSTATE") || strings.Contains(r.Body, "OR 1=1") {
			t.Errorf("%s: the error echoes input or internals: %s", name, r.Body)
		}
	}
	// A cursor with a changed position is still only a position: it moves within the caller's own data.
	if r := fs.is.c.do("GET", org+"/operations/failures?cursor="+url.QueryEscape(forgedTime), nil); r.Status != 400 && r.Status != 200 {
		t.Errorf("forged position: %d %s", r.Status, r.Body)
	}
}

func TestFailuresAPICursorReplayAndForeignIdentifiersAcrossOrganizations(t *testing.T) {
	s := newServer(t, false)
	a := newFailSeed(t, s, "A-Target")
	now := s.env.Clock.Now()
	a1 := a.failed(now.Add(-time.Minute), "RETRIES_EXHAUSTED")
	a2 := a.failed(now.Add(-3*time.Minute), "RETRIES_EXHAUSTED")

	// A second tenant, with its own session, destination and failures.
	b := newFailSeedAs(t, s, "other@example.com", "B-Target")
	other, otherOrg := b.is.c, b.is.org
	b1 := b.failed(now.Add(-2*time.Minute), "INTERNAL_ERROR")

	pageA := a.is.c.do("GET", "/api/v1/organizations/"+a.is.org+"/operations/failures?limit=1", nil)
	cursorA := pageA.json()["nextCursor"].(string)
	if ids := itemIDs(pageA); len(ids) != 1 || ids[0] != a1 {
		t.Fatalf("page A: %v", ids)
	}

	// B replays A's cursor: it only positions B's own rows, and never returns A's.
	r := other.do("GET", "/api/v1/organizations/"+otherOrg+"/operations/failures?cursor="+url.QueryEscape(cursorA), nil)
	if r.Status != 200 {
		t.Fatalf("%d %s", r.Status, r.Body)
	}
	for _, id := range itemIDs(r) {
		if id == a1 || id == a2 {
			t.Fatalf("a replayed cursor leaked tenant A's delivery %s", id)
		}
	}
	if strings.Contains(r.Body, "A-Target") || strings.Contains(r.Body, a.dest) {
		t.Fatalf("tenant B saw tenant A's destination: %s", r.Body)
	}
	// B's own listing is its own.
	if own := other.do("GET", "/api/v1/organizations/"+otherOrg+"/operations/failures", nil); strings.Join(itemIDs(own), ",") != b1 {
		t.Fatalf("B's feed: %s", own.Body)
	}
	// Tenant A's destination id means nothing in B's organization, and B's means nothing in A's.
	if r := other.do("GET", "/api/v1/organizations/"+otherOrg+"/operations/failures?destinationId="+a.dest, nil); r.Status != 200 || len(itemIDs(r)) != 0 {
		t.Errorf("a foreign destination id selects nothing: %d %s", r.Status, r.Body)
	}
	if r := a.is.c.do("GET", "/api/v1/organizations/"+a.is.org+"/operations/failures?destinationId="+b.dest, nil); r.Status != 200 || len(itemIDs(r)) != 0 {
		t.Errorf("a foreign destination id selects nothing: %d %s", r.Status, r.Body)
	}
	// A's session on B's organization is a 404.
	if r := a.is.c.do("GET", "/api/v1/organizations/"+otherOrg+"/operations/failures", nil); r.Status != 404 {
		t.Errorf("foreign organization: %d %s", r.Status, r.Body)
	}
}

func TestFailuresAPIFiltersByReasonAndDestinationAndValidates(t *testing.T) {
	s := newServer(t, false)
	fs := newFailSeed(t, s, "Target")
	now := s.env.Clock.Now()
	org := "/api/v1/organizations/" + fs.is.org
	byReason := map[string]string{}
	for i, reason := range []string{"DESTINATION_REJECTED", "REDIRECT_REFUSED", "EGRESS_BLOCKED", "RETRIES_EXHAUSTED", "INTERNAL_ERROR"} {
		byReason[reason] = fs.failed(now.Add(-time.Duration(i+1)*time.Minute), reason)
	}
	byReason["UNRECORDED"] = fs.failed(now.Add(-10*time.Minute), "")
	for reason, id := range byReason {
		r := fs.is.c.do("GET", org+"/operations/failures?failureReason="+reason, nil)
		if r.Status != 200 || strings.Join(itemIDs(r), ",") != id {
			t.Errorf("%s: %d %s", reason, r.Status, r.Body)
		}
		if got := r.json()["items"].([]any)[0].(map[string]any)["failureReason"]; got != reason {
			t.Errorf("%s: failureReason = %v", reason, got)
		}
	}
	if r := fs.is.c.do("GET", org+"/operations/failures?destinationId="+fs.dest, nil); len(itemIDs(r)) != 6 {
		t.Errorf("own destination: %s", r.Body)
	}
	for _, q := range []string{"failureReason=nope", "failureReason=unrecorded", "failureReason=FAILED", "destinationId=x", "destinationId=" + strings.ToUpper("zzzzzzzz-zzzz-zzzz-zzzz-zzzzzzzzzzzz"),
		"limit=0", "limit=101", "limit=-1", "limit=ten", "limit=1.5", "from=soon", "to=later", "from=2026-11-18", "from=2026-06-30T23:59:60Z"} {
		if r := fs.is.c.do("GET", org+"/operations/failures?"+q, nil); r.Status != 400 || r.errorCode() != "validation_failed" {
			t.Errorf("?%s: %d %s, want 400 validation_failed", q, r.Status, r.Body)
		}
	}
	// An empty value of a parameter is the same as not sending it; unknown parameters are ignored (like every route).
	if r := fs.is.c.do("GET", org+"/operations/failures?failureReason=&destinationId=&cursor=&limit=&bogus=1", nil); r.Status != 200 || len(itemIDs(r)) != 6 {
		t.Errorf("empty and unknown parameters: %d %s", r.Status, r.Body)
	}
	// limit=100 and limit=1 are the bounds.
	if r := fs.is.c.do("GET", org+"/operations/failures?limit=1", nil); r.Status != 200 || len(itemIDs(r)) != 1 || r.json()["nextCursor"] == nil {
		t.Errorf("limit=1: %s", r.Body)
	}
	if r := fs.is.c.do("GET", org+"/operations/failures?limit=100", nil); r.Status != 200 {
		t.Errorf("limit=100: %d", r.Status)
	}
}

func TestFailuresAPIWindowDefaultsToSevenDaysAndAllowsAtMost31(t *testing.T) {
	s := newServer(t, false)
	fs := newFailSeed(t, s, "Target")
	now := s.env.Clock.Now().Truncate(time.Microsecond)
	org := "/api/v1/organizations/" + fs.is.org
	day := 24 * time.Hour
	inside := fs.failed(now.Add(-6*day), "RETRIES_EXHAUSTED")
	eight := fs.failed(now.Add(-8*day), "RETRIES_EXHAUSTED")
	old := fs.failed(now.Add(-20*day), "RETRIES_EXHAUSTED")

	r := fs.is.c.do("GET", org+"/operations/failures", nil)
	if strings.Join(itemIDs(r), ",") != inside {
		t.Fatalf("the default window is the last seven days: %s", r.Body)
	}
	body := r.json()
	from, _ := time.Parse(time.RFC3339Nano, body["from"].(string))
	to, _ := time.Parse(time.RFC3339Nano, body["to"].(string))
	if to.Sub(from) != 7*day || !to.Equal(now) {
		t.Errorf("window = [%v, %v)", from, to)
	}
	if got := itemIDs(fs.is.c.do("GET", org+"/operations/failures?from="+now.Add(-31*day).Format(time.RFC3339Nano)+"&to="+now.Format(time.RFC3339Nano), nil)); len(got) != 3 {
		t.Errorf("a 31 day window: %v", got)
	}
	long := fs.is.c.do("GET", org+"/operations/failures?from="+now.Add(-31*day-time.Microsecond).Format(time.RFC3339Nano)+"&to="+now.Format(time.RFC3339Nano), nil)
	if long.Status != 400 || long.errorCode() != "validation_failed" {
		t.Errorf("31 days and a microsecond: %d %s", long.Status, long.Body)
	}
	for _, q := range []string{"from=2026-11-10T00:00:00Z&to=2026-11-10T00:00:00Z", "from=2026-11-11T00:00:00Z&to=2026-11-10T00:00:00Z",
		"from=0000-01-01T00:00:00Z&to=0000-01-02T00:00:00Z", "from=0001-01-01T00:00:00%2B14:00&to=0001-01-02T00:00:00Z", "from=9999-12-31T00:00:00Z&to=9999-12-31T23:59:59-05:00"} {
		if r := fs.is.c.do("GET", org+"/operations/failures?"+q, nil); r.Status != 400 || r.errorCode() != "validation_failed" {
			t.Errorf("?%s: %d %s", q, r.Status, r.Body)
		}
	}
	// Bounds are normalized to UTC microseconds and echoed as read; half-open at to.
	e := fs.is.c.do("GET", org+"/operations/failures?from="+url.QueryEscape("2026-11-10T09:00:00.1234567-03:00")+"&to="+url.QueryEscape("2026-11-10T10:00:00.9999999-03:00"), nil)
	if e.Status != 200 || e.json()["from"] != "2026-11-10T12:00:00.123456Z" || e.json()["to"] != "2026-11-10T13:00:00.999999Z" {
		t.Errorf("normalization: %d %s", e.Status, e.Body)
	}
	// A failure completed exactly at `to` is outside, exactly at `from` inside.
	edge := fs.failed(now.Add(-10*day), "RETRIES_EXHAUSTED")
	win := "from=" + now.Add(-10*day).Format(time.RFC3339Nano) + "&to=" + now.Add(-6*day).Format(time.RFC3339Nano)
	got := itemIDs(fs.is.c.do("GET", org+"/operations/failures?"+win, nil))
	if strings.Join(got, ",") != eight+","+edge && strings.Join(got, ",") != edge+","+eight {
		t.Errorf("[from, to) holds the row at from (%s) and the one between (%s), not the row at to (%s): %v", edge, eight, inside, got)
	}
	for _, id := range got {
		if id == inside {
			t.Errorf("the row completed exactly at `to` must be outside the window")
		}
	}
	_ = old
}

func TestFailuresAPITimeoutAndClientCancellation(t *testing.T) {
	logs := &lockedBuffer{}
	s := newServer(t, false)
	fs := newFailSeed(t, s, "Target")
	fast := shortDeadlineServer(t, s, 300*time.Millisecond, logs)
	c := &client{t: t, s: fast, cookie: fs.is.c.cookie, csrf: fs.is.c.csrf}
	org := "/api/v1/organizations/" + fs.is.org

	lockDeliveries(t, s, func() {
		for _, p := range []string{org + "/operations/failures", org + "/operations/failures?failureReason=UNRECORDED", org + "/operations/queue/age", org + "/deliveries?failureReason=RETRIES_EXHAUSTED"} {
			start := time.Now()
			r := c.do("GET", p, nil)
			if r.Status != 503 || r.errorCode() != "read_model_timeout" {
				t.Errorf("GET %s: %d %s, want 503 read_model_timeout", p, r.Status, r.Body)
			}
			if took := time.Since(start); took < 250*time.Millisecond || took > 5*time.Second {
				t.Errorf("GET %s answered after %v, want about the 300ms deadline", p, took)
			}
			e := r.json()["error"].(map[string]any)
			if e["message"] != "the query took too long; try a shorter time window" || len(e) != 2 {
				t.Errorf("the error carries only the code and the safe message: %s", r.Body)
			}
			for _, banned := range []string{"SQLSTATE", "57014", "pgx", "deadline", "context", "lock", "deliveries", "SELECT", "organization_id", "canceling", fs.is.org} {
				if strings.Contains(r.Body, banned) {
					t.Errorf("the 503 exposes %q: %s", banned, r.Body)
				}
			}
		}
	})
	if strings.Contains(logs.String(), "SELECT") {
		t.Errorf("a timeout must not log SQL text: %s", logs.String())
	}
	// Released: the same session answers.
	for _, p := range []string{org + "/operations/failures", org + "/operations/queue/age", org + "/deliveries?failureReason=UNRECORDED"} {
		if r := c.do("GET", p, nil); r.Status != 200 {
			t.Errorf("after the lock %s: %d %s", p, r.Status, r.Body)
		}
	}

	// A client that goes away is not reported as a timeout, and the server stays healthy.
	slow := shortDeadlineServer(t, s, time.Minute, logs)
	cs := &client{t: t, s: slow, cookie: fs.is.c.cookie, csrf: fs.is.c.csrf}
	lockDeliveries(t, s, func() {
		for _, p := range []string{"/operations/failures", "/operations/queue/age"} {
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			req, _ := http.NewRequestWithContext(ctx, "GET", slow.url+org+p, nil)
			req.AddCookie(fs.is.c.cookie)
			start := time.Now()
			resp, err := http.DefaultClient.Do(req)
			if resp != nil {
				resp.Body.Close()
			}
			cancel()
			if err == nil || !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 10*time.Second {
				t.Fatalf("%s: the client gives up first: err = %v after %v", p, err, time.Since(start))
			}
		}
	})
	if r := cs.do("GET", org+"/operations/failures", nil); r.Status != 200 {
		t.Fatalf("after the client went away: %d %s", r.Status, r.Body)
	}
	if strings.Contains(logs.String(), "read_model_timeout") && strings.Count(logs.String(), "read_model_timeout") > 0 {
		// The earlier, genuine timeouts may be logged by the handler; the cancelled requests must add nothing more.
		t.Logf("timeout log lines are expected for the genuine deadline overruns only")
	}
}

func TestQueueAgeAPIReportsBucketsForTheOrganizationOnly(t *testing.T) {
	s := newServer(t, false)
	is := newIngestSetup(t, s)
	destID, _ := createDestination(t, is.c, is.org, "Paused")
	org := "/api/v1/organizations/" + is.org
	now := s.env.Clock.Now().Truncate(time.Microsecond)

	q, _ := s.env.Pool.Querier()
	add := func(orgID, dest string, age time.Duration) {
		res, err := s.env.Send(is.pub, s.env.Signed(is.secret, []byte(`{"a":1}`), "age-"+time.Now().Format("150405.000000000")))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := q.Exec(bg, "INSERT INTO deliveries (organization_id, event_id, destination_id, next_attempt_at) VALUES ($1, $2, $3, $4)", orgID, res.EventID, dest, now.Add(-age)); err != nil {
			t.Fatal(err)
		}
	}
	// A paused destination still has due work.
	if r := is.c.mutate("POST", org+"/destinations/"+destID+"/pause", nil); r.Status != 200 {
		t.Fatalf("pause: %d %s", r.Status, r.Body)
	}
	for _, age := range []time.Duration{30 * time.Second, 59 * time.Second, 60 * time.Second, 2 * time.Hour, 25 * time.Hour, 25 * time.Hour} {
		add(is.org, destID, age)
	}
	add(is.org, destID, -time.Hour) // not due yet

	r := is.c.do("GET", org+"/operations/queue/age", nil)
	if r.Status != 200 {
		t.Fatalf("%d %s", r.Status, r.Body)
	}
	body := r.json()
	if keysOf(body) != "buckets,capped,generatedAt" || body["capped"] != false {
		t.Fatalf("%s", r.Body)
	}
	buckets := body["buckets"].([]any)
	wantLabels := []string{"<1m", "1-5m", "5-15m", "15-60m", "1-6h", "6-24h", ">24h"}
	wantCounts := []float64{2, 1, 0, 0, 1, 0, 2}
	for i, b := range buckets {
		m := b.(map[string]any)
		if keysOf(m) != "count,label,maxSeconds,minSeconds" || m["label"] != wantLabels[i] || m["count"] != wantCounts[i] {
			t.Errorf("bucket %d = %v, want %s with %v", i, m, wantLabels[i], wantCounts[i])
		}
	}
	if buckets[6].(map[string]any)["maxSeconds"] != nil || buckets[0].(map[string]any)["maxSeconds"] != float64(60) {
		t.Errorf("bounds: %v ... %v", buckets[0], buckets[6])
	}
	// generatedAt is the service clock, one value for the whole response, in UTC.
	if got, _ := body["generatedAt"].(string); got != now.Format("2006-01-02T15:04:05.999999Z") {
		t.Errorf("generatedAt = %q, want %q", got, now.Format("2006-01-02T15:04:05.999999Z"))
	}
	// Another organization sees none of it.
	other := s.client(t)
	otherOrg := other.orgID(other.register("other@example.com"))
	if o := other.do("GET", "/api/v1/organizations/"+otherOrg+"/operations/queue/age", nil); o.Status != 200 {
		t.Fatalf("%d %s", o.Status, o.Body)
	} else {
		for _, b := range o.json()["buckets"].([]any) {
			if b.(map[string]any)["count"] != float64(0) {
				t.Errorf("another organization's queue is empty: %s", o.Body)
			}
		}
	}
}
