package httpapi_test

import (
	"encoding/base64"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// addAttempt records one finished attempt of a delivery at an exact start time. The claim-fence trigger of
// delivery_attempts is disabled for the insert inside one transaction, so the test can place attempts at chosen times
// without running a delivery through its lifecycle; every other constraint stays in force.
func (f *failSeed) addAttempt(delivery string, number int, started time.Time, outcome string, httpStatus, class, reason any) string {
	f.t.Helper()
	var id string
	err := f.s.env.Pool.InTx(bg, func(tx pgx.Tx) error {
		if _, err := tx.Exec(bg, "ALTER TABLE delivery_attempts DISABLE TRIGGER delivery_attempts_check_claimed"); err != nil {
			return err
		}
		if err := tx.QueryRow(bg, `INSERT INTO delivery_attempts (organization_id, delivery_id, attempt_number, started_at, finished_at, outcome,
			http_status, error_class, duration_ms, terminal_reason) VALUES ($1, $2, $3, $4, $4::timestamptz + interval '90 milliseconds', $5, $6, $7, 90, $8)
			RETURNING id::text`, f.is.org, delivery, number, started.UTC(), outcome, httpStatus, class, reason).Scan(&id); err != nil {
			return err
		}
		_, err := tx.Exec(bg, "ALTER TABLE delivery_attempts ENABLE TRIGGER delivery_attempts_check_claimed")
		return err
	})
	if err != nil {
		f.t.Fatalf("add attempt: %v", err)
	}
	return id
}

func attemptItemIDs(r response) []string {
	var out []string
	for _, it := range r.json()["items"].([]any) {
		out = append(out, it.(map[string]any)["attemptId"].(string))
	}
	return out
}

func TestAttemptsAPIRolesAuthenticationAndReadOnlyRoute(t *testing.T) {
	s := newServer(t, false)
	fs := newFailSeed(t, s, "Target")
	now := s.env.Clock.Now()
	d := fs.failed(now.Add(-time.Hour), "RETRIES_EXHAUSTED")
	fs.addAttempt(d, 1, now.Add(-time.Hour), "HTTP_ERROR", 503, "http_5xx", nil)
	path := "/api/v1/organizations/" + fs.is.org + "/operations/attempts"

	clients := map[string]*client{"OWNER": fs.is.c}
	for _, role := range []string{"ADMIN", "DEVELOPER", "VIEWER"} {
		clients[role] = join(t, fs.is.c, s, fs.is.org, strings.ToLower(role)+"@example.com", role)
	}
	for role, c := range clients {
		if r := c.do("GET", path, nil); r.Status != 200 || len(attemptItemIDs(r)) != 1 {
			t.Errorf("%s: %d %s", role, r.Status, r.Body)
		}
	}
	if r := s.client(t).do("GET", path, nil); r.Status != 401 || r.errorCode() != "unauthenticated" {
		t.Errorf("anonymous: %d %s", r.Status, r.Body)
	}
	// A non-member and a malformed or unknown organization id get the same 404, even when every parameter is garbage,
	// including repeated ones.
	garbage := "?limit=0&limit=5&cursor=!!!&outcome=nope&outcome=SUCCESS&deliveryId=x&from=soon&to=later"
	stranger := s.client(t)
	stranger.register("stranger@example.com")
	for _, q := range []string{"", garbage} {
		if r := stranger.do("GET", path+q, nil); r.Status != 404 || r.errorCode() != "not_found" {
			t.Errorf("non-member%s: %d %s", q, r.Status, r.Body)
		}
	}
	for _, id := range []string{"not-a-uuid", "00000000-0000-4000-8000-000000000000"} {
		if r := fs.is.c.do("GET", "/api/v1/organizations/"+id+"/operations/attempts"+garbage, nil); r.Status != 404 || r.errorCode() != "not_found" {
			t.Errorf("organization %s: %d %s", id, r.Status, r.Body)
		}
	}
	if r := fs.is.c.do("GET", path+"?limit=0", nil); r.Status != 400 || r.errorCode() != "validation_failed" {
		t.Errorf("member with limit=0: %d %s", r.Status, r.Body)
	}
	// Read-only: no other verb is routed, and nothing changed.
	before := s.env.Count(t, "SELECT count(*) FROM delivery_attempts")
	for _, m := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		if r := fs.is.c.mutate(m, path, map[string]string{"outcome": "SUCCESS"}); r.Status != 404 && r.Status != 405 {
			t.Errorf("%s: %d, want 404 or 405", m, r.Status)
		}
	}
	if s.env.Count(t, "SELECT count(*) FROM delivery_attempts") != before {
		t.Fatal("attempts changed through the operations API")
	}
}

func TestAttemptsAPIContractDefaultsAndNoSensitiveData(t *testing.T) {
	logs := &lockedBuffer{}
	s := newServerWith(t, false, logs, nil)
	fs := newFailSeed(t, s, "ConfidentialTargetName")
	now := s.env.Clock.Now().Truncate(time.Microsecond)
	d := fs.failed(now.Add(-time.Hour), "DESTINATION_REJECTED")
	terminal := fs.addAttempt(d, 1, now.Add(-time.Hour), "HTTP_ERROR", 400, nil, "DESTINATION_REJECTED")
	old := fs.addAttempt(fs.failed(now.Add(-30*time.Hour), "RETRIES_EXHAUSTED"), 1, now.Add(-25*time.Hour), "TIMEOUT", nil, "timeout", nil)
	org := "/api/v1/organizations/" + fs.is.org

	r := fs.is.c.do("GET", org+"/operations/attempts", nil)
	if r.Status != 200 || r.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("%d %v %s", r.Status, r.Header, r.Body)
	}
	body := r.json()
	if keysOf(body) != "from,generatedAt,items,nextCursor,to" || body["nextCursor"] != nil {
		t.Fatalf("fields: %s", r.Body)
	}
	// The default window is 24 hours: the attempt started 25 hours ago is not in it.
	if ids := attemptItemIDs(r); len(ids) != 1 || ids[0] != terminal {
		t.Fatalf("default window items: %v (the 25-hour-old attempt %s must be outside)", ids, old)
	}
	item := body["items"].([]any)[0].(map[string]any)
	if keysOf(item) != "attemptId,attemptNumber,deliveryId,destinationId,destinationName,durationMs,eventId,finishedAt,httpStatus,outcome,startedAt,terminalReason" {
		t.Fatalf("item fields: %s", keysOf(item))
	}
	if item["deliveryId"] != d || item["destinationId"] != fs.dest || item["destinationName"] != "ConfidentialTargetName" || item["outcome"] != "HTTP_ERROR" ||
		item["httpStatus"] != float64(400) || item["terminalReason"] != "DESTINATION_REJECTED" || item["attemptNumber"] != float64(1) || item["durationMs"] != float64(90) {
		t.Errorf("item: %v", item)
	}
	for _, key := range []string{"startedAt", "finishedAt"} {
		if v, _ := item[key].(string); !strings.HasSuffix(v, "Z") {
			t.Errorf("%s = %q, want a UTC timestamp", key, v)
		}
	}
	for _, key := range []string{"generatedAt", "from", "to"} {
		if v, _ := body[key].(string); !strings.HasSuffix(v, "Z") {
			t.Errorf("%s = %q, want a UTC timestamp", key, v)
		}
	}
	// Widening the window with `from` brings the older attempt in; a status-less outcome omits httpStatus and a missing reason omits terminalReason.
	wide := fs.is.c.do("GET", org+"/operations/attempts?from="+url.QueryEscape(now.Add(-48*time.Hour).Format(time.RFC3339)), nil)
	if ids := attemptItemIDs(wide); strings.Join(ids, ",") != terminal+","+old {
		t.Fatalf("wide window: %v", ids)
	}
	timeout := wide.json()["items"].([]any)[1].(map[string]any)
	if _, ok := timeout["httpStatus"]; ok || timeout["errorClass"] != "timeout" || timeout["terminalReason"] != nil {
		t.Errorf("a TIMEOUT attempt has no status and no terminal reason: %v", timeout)
	}
	// Never exposed, in the response or in the logs.
	all := r.Body + wide.Body + logs.String()
	for _, banned := range []string{fs.secret, "PAYLOADSECRET", "hooks.example.com", "https://", "claim", "worker", "lease", "signing", "\"url\""} {
		if strings.Contains(all, banned) {
			t.Errorf("the response or the logs contain %q", banned)
		}
	}
}

func TestAttemptsAPIFiltersOutcomeAndDeliveryAndIgnoresADestinationFilter(t *testing.T) {
	s := newServer(t, false)
	fs := newFailSeed(t, s, "Target")
	now := s.env.Clock.Now().Truncate(time.Microsecond)
	org := "/api/v1/organizations/" + fs.is.org
	a := fs.failed(now.Add(-time.Hour), "RETRIES_EXHAUSTED")
	b := fs.failed(now.Add(-time.Hour), "RETRIES_EXHAUSTED")
	a1 := fs.addAttempt(a, 1, now.Add(-5*time.Minute), "ABANDONED", nil, nil, nil)
	a2 := fs.addAttempt(a, 2, now.Add(-4*time.Minute), "HTTP_ERROR", 503, "http_5xx", nil)
	b1 := fs.addAttempt(b, 1, now.Add(-3*time.Minute), "ABANDONED", nil, nil, nil)
	b2 := fs.addAttempt(b, 2, now.Add(-2*time.Minute), "SUCCESS", 200, nil, nil)
	get := func(q string) []string { return attemptItemIDs(fs.is.c.do("GET", org+"/operations/attempts?"+q, nil)) }
	for q, want := range map[string]string{
		"outcome=ABANDONED":                    b1 + "," + a1,
		"outcome=SUCCESS":                      b2,
		"outcome=TIMEOUT":                      "",
		"deliveryId=" + a:                      a2 + "," + a1,
		"deliveryId=" + b + "&outcome=SUCCESS": b2,
		"deliveryId=" + a + "&outcome=SUCCESS": "",
		// destinationId is deferred: an unknown parameter is ignored, like everywhere else, and selects nothing.
		"destinationId=" + fs.dest:                           b2 + "," + b1 + "," + a2 + "," + a1,
		"destinationId=00000000-0000-4000-8000-000000000000": b2 + "," + b1 + "," + a2 + "," + a1,
	} {
		if got := strings.Join(get(q), ","); got != want {
			t.Errorf("%s: got %s, want %s", q, got, want)
		}
	}
	for q, why := range map[string]string{
		"outcome=success": "lower case", "outcome=FAILED": "not an outcome", "deliveryId=x": "not a uuid",
		"deliveryId=" + strings.ToUpper(a) + "x": "bad uuid", "limit=101": "limit", "limit=-1": "limit", "from=2026-13-01T00:00:00Z": "month",
		"from=" + url.QueryEscape("2026-01-01T00:00:00Z") + "&to=" + url.QueryEscape("2026-03-01T00:00:00Z"): "over 31 days",
	} {
		if r := fs.is.c.do("GET", org+"/operations/attempts?"+q, nil); r.Status != 400 || r.errorCode() != "validation_failed" {
			t.Errorf("%s (%s): %d %s", q, why, r.Status, r.Body)
		}
	}
}

func TestAttemptsAPIPagesWithAnOpaqueKeysetCursorAndRefusesTheFailureFeedsCursor(t *testing.T) {
	s := newServer(t, false)
	fs := newFailSeed(t, s, "Target")
	now := s.env.Clock.Now().Truncate(time.Microsecond)
	org := "/api/v1/organizations/" + fs.is.org
	d := fs.failed(now.Add(-time.Hour), "RETRIES_EXHAUSTED")
	// Seven attempts, three of them with the very same start time.
	for i, m := range []time.Duration{1, 2, 2, 2, 3, 4, 5} {
		fs.addAttempt(d, i+1, now.Add(-m*time.Minute), "HTTP_ERROR", 500, "http_5xx", nil)
	}
	whole := attemptItemIDs(fs.is.c.do("GET", org+"/operations/attempts?limit=100", nil))
	if len(whole) != 7 {
		t.Fatalf("%v", whole)
	}
	for _, size := range []string{"1", "2", "3", "7", "8"} {
		var got []string
		cursor := ""
		for page := 0; page < 20; page++ {
			path := org + "/operations/attempts?limit=" + size
			if cursor != "" {
				path += "&cursor=" + url.QueryEscape(cursor)
			}
			r := fs.is.c.do("GET", path, nil)
			if r.Status != 200 {
				t.Fatalf("limit %s page %d: %d %s", size, page, r.Status, r.Body)
			}
			got = append(got, attemptItemIDs(r)...)
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
	first := fs.is.c.do("GET", org+"/operations/attempts?limit=2", nil)
	cursor := first.json()["nextCursor"].(string)
	raw, _ := base64.RawURLEncoding.DecodeString(cursor)
	if !strings.HasPrefix(string(raw), "a1:") {
		t.Fatalf("the attempt cursor carries its own version tag: %q", raw)
	}
	for name, bad := range map[string]string{
		"garbage":       "!!!",
		"padded":        cursor + "=",
		"truncated":     cursor[:len(cursor)-3],
		"one more char": cursor + "A",
		"other version": base64.RawURLEncoding.EncodeToString([]byte("a9" + string(raw[2:]))),
		"failure feed":  base64.RawURLEncoding.EncodeToString([]byte("v1" + string(raw[2:]))),
		"sql":           base64.RawURLEncoding.EncodeToString([]byte("a1:1:' OR 1=1 --")),
	} {
		r := fs.is.c.do("GET", org+"/operations/attempts?cursor="+url.QueryEscape(bad), nil)
		if r.Status != 400 || r.errorCode() != "validation_failed" {
			t.Errorf("%s: %d %s, want 400", name, r.Status, r.Body)
		}
		if strings.Contains(r.Body, "SQLSTATE") || strings.Contains(r.Body, "OR 1=1") {
			t.Errorf("%s: the error echoes input or internals: %s", name, r.Body)
		}
	}
	// The failure feed refuses the attempt feed's cursor, the other way round.
	if r := fs.is.c.do("GET", org+"/operations/failures?cursor="+url.QueryEscape(cursor), nil); r.Status != 400 {
		t.Errorf("the failure feed accepted an attempt cursor: %d %s", r.Status, r.Body)
	}
}

func TestAttemptsAPIIsolatesTenantsAndTheirCursors(t *testing.T) {
	s := newServer(t, false)
	a := newFailSeed(t, s, "A-Target")
	now := s.env.Clock.Now().Truncate(time.Microsecond)
	da := a.failed(now.Add(-time.Hour), "RETRIES_EXHAUSTED")
	a1 := a.addAttempt(da, 1, now.Add(-time.Minute), "HTTP_ERROR", 500, "http_5xx", nil)
	a2 := a.addAttempt(da, 2, now.Add(-3*time.Minute), "HTTP_ERROR", 500, "http_5xx", nil)

	b := newFailSeedAs(t, s, "other@example.com", "B-Target")
	db := b.failed(now.Add(-time.Hour), "INTERNAL_ERROR")
	b1 := b.addAttempt(db, 1, now.Add(-2*time.Minute), "NETWORK_ERROR", nil, "network", nil)

	pageA := a.is.c.do("GET", "/api/v1/organizations/"+a.is.org+"/operations/attempts?limit=1", nil)
	cursorA := pageA.json()["nextCursor"].(string)
	if ids := attemptItemIDs(pageA); len(ids) != 1 || ids[0] != a1 {
		t.Fatalf("page A: %v", ids)
	}
	// B replays A's cursor: it only positions B's own rows, and never returns A's.
	r := b.is.c.do("GET", "/api/v1/organizations/"+b.is.org+"/operations/attempts?cursor="+url.QueryEscape(cursorA), nil)
	if r.Status != 200 {
		t.Fatalf("%d %s", r.Status, r.Body)
	}
	for _, id := range attemptItemIDs(r) {
		if id == a1 || id == a2 {
			t.Fatalf("a replayed cursor leaked tenant A's attempt %s", id)
		}
	}
	if strings.Contains(r.Body, "A-Target") || strings.Contains(r.Body, a.dest) {
		t.Fatalf("tenant B saw tenant A's destination: %s", r.Body)
	}
	if own := b.is.c.do("GET", "/api/v1/organizations/"+b.is.org+"/operations/attempts", nil); strings.Join(attemptItemIDs(own), ",") != b1 {
		t.Fatalf("B's feed: %s", own.Body)
	}
	// Another tenant's delivery id selects nothing.
	if r := b.is.c.do("GET", "/api/v1/organizations/"+b.is.org+"/operations/attempts?deliveryId="+da, nil); r.Status != 200 || len(attemptItemIDs(r)) != 0 {
		t.Errorf("a foreign delivery id selects nothing: %d %s", r.Status, r.Body)
	}
	// A's session on B's organization is a 404.
	if r := a.is.c.do("GET", "/api/v1/organizations/"+b.is.org+"/operations/attempts", nil); r.Status != 404 {
		t.Errorf("foreign organization: %d %s", r.Status, r.Body)
	}
}
