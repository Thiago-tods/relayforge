package httpapi_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Thiago-tods/relayforge/internal/app/apptest"
	"github.com/Thiago-tods/relayforge/internal/delivery"
)

// lockedBuffer collects server logs; handlers write while the test reads.
type lockedBuffer struct {
	mu sync.Mutex
	sb strings.Builder
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.sb.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.sb.String()
}

var bg = context.Background()

const apiURL = "https://hooks.example.com/relayforge/in?token=QUERYSECRET"

func createDestination(t *testing.T, c *client, org, name string) (id, secret string) {
	t.Helper()
	res := c.mutate("POST", "/api/v1/organizations/"+org+"/destinations", map[string]string{"name": name, "url": apiURL})
	if res.Status != 201 {
		t.Fatalf("create destination: %d %s", res.Status, res.Body)
	}
	d := res.json()["destination"].(map[string]any)
	return d["id"].(string), d["signingSecret"].(string)
}

// join adds a user to the organization with a role through the real invitation flow.
func join(t *testing.T, owner *client, s *server, org, email, role string) *client {
	t.Helper()
	c := s.client(t)
	c.register(email)
	inv := owner.mutate("POST", "/api/v1/organizations/"+org+"/invitations", map[string]string{"email": email, "role": role})
	if inv.Status != 201 {
		t.Fatalf("invite %s: %d %s", role, inv.Status, inv.Body)
	}
	if r := c.mutate("POST", "/api/v1/invitations/accept", map[string]string{"token": inv.json()["token"].(string)}); r.Status != 200 {
		t.Fatalf("accept: %d %s", r.Status, r.Body)
	}
	return c
}

func TestDestinationAPIExposesTheSecretOnlyOnCreationAndRotation(t *testing.T) {
	logs := &lockedBuffer{}
	s := newServerWith(t, false, logs, nil)
	is := newIngestSetup(t, s)
	base := "/api/v1/organizations/" + is.org + "/destinations"

	id, secret := createDestination(t, is.c, is.org, "Billing")
	if !strings.HasPrefix(secret, "rfd_") {
		t.Fatalf("unexpected secret %q", secret[:4])
	}
	created := is.c.do("GET", base+"/"+id, nil)
	if created.json()["destination"].(map[string]any)["url"] != "https://hooks.example.com/relayforge/in" ||
		created.json()["destination"].(map[string]any)["urlHasQuery"] != true {
		t.Fatalf("the query string must be hidden: %s", created.Body)
	}

	list := is.c.do("GET", base, nil)
	get := is.c.do("GET", base+"/"+id, nil)
	patch := is.c.mutate("PATCH", base+"/"+id, map[string]string{"name": "Billing v2"})
	pause := is.c.mutate("POST", base+"/"+id+"/pause", nil)
	activate := is.c.mutate("POST", base+"/"+id+"/activate", nil)
	for name, r := range map[string]response{"list": list, "get": get, "patch": patch, "pause": pause, "activate": activate} {
		if r.Status != 200 {
			t.Fatalf("%s: %d %s", name, r.Status, r.Body)
		}
		for _, banned := range []string{secret, "signingSecret", "ciphertext", "Ciphertext", "encryption", "secretVersion", "secret_version", "QUERYSECRET", "token="} {
			if strings.Contains(r.Body, banned) {
				t.Errorf("%s response exposes %q: %s", name, banned, r.Body)
			}
		}
	}
	if patch.json()["destination"].(map[string]any)["name"] != "Billing v2" || pause.json()["destination"].(map[string]any)["status"] != "PAUSED" {
		t.Fatal("PATCH and pause must return the updated destination")
	}

	rot := is.c.mutate("POST", base+"/"+id+"/rotate-secret", nil)
	if rot.Status != 200 {
		t.Fatalf("rotate: %d %s", rot.Status, rot.Body)
	}
	newSecret := rot.json()["destination"].(map[string]any)["signingSecret"].(string)
	if newSecret == secret || !strings.HasPrefix(newSecret, "rfd_") {
		t.Fatal("rotation must return a new secret once")
	}
	if strings.Contains(is.c.do("GET", base+"/"+id, nil).Body, newSecret) {
		t.Fatal("the rotated secret must not be retrievable later")
	}

	rev := is.c.mutate("DELETE", base+"/"+id, nil)
	if rev.Status != 200 || rev.json()["destination"].(map[string]any)["status"] != "REVOKED" {
		t.Fatalf("revoke: %d %s", rev.Status, rev.Body)
	}
	for name, r := range map[string]response{
		"activate": is.c.mutate("POST", base+"/"+id+"/activate", nil),
		"pause":    is.c.mutate("POST", base+"/"+id+"/pause", nil),
		"rotate":   is.c.mutate("POST", base+"/"+id+"/rotate-secret", nil),
		"patch":    is.c.mutate("PATCH", base+"/"+id, map[string]string{"name": "again"}),
	} {
		if r.Status != 409 || r.errorCode() != "destination_revoked" {
			t.Errorf("%s after revocation: %d %s", name, r.Status, r.Body)
		}
	}
	if is.c.mutate("DELETE", base+"/"+id, nil).Status != 200 {
		t.Fatal("revoking twice must succeed as a no-op")
	}

	// Nothing the server logged may contain a secret, the URL query, or a ciphertext.
	out := logs.String()
	for _, banned := range []string{secret, newSecret, "QUERYSECRET", "signing_secret"} {
		if strings.Contains(out, banned) {
			t.Fatalf("server logs contain %q", banned)
		}
	}
}

func TestDestinationAPIValidationAndStatusCodes(t *testing.T) {
	s := newServer(t, false)
	is := newIngestSetup(t, s)
	base := "/api/v1/organizations/" + is.org + "/destinations"
	id, _ := createDestination(t, is.c, is.org, "Orders")

	post := func(body any) response { return is.c.mutate("POST", base, body) }
	cases := map[string]struct {
		r      response
		status int
		code   string
	}{
		"empty name":          {post(map[string]string{"name": "", "url": apiURL}), 400, "validation_failed"},
		"http url":            {post(map[string]string{"name": "a", "url": "http://hooks.example.com/x"}), 400, "validation_failed"},
		"loopback url":        {post(map[string]string{"name": "a", "url": "https://127.0.0.1/x"}), 400, "validation_failed"},
		"metadata address":    {post(map[string]string{"name": "a", "url": "https://169.254.169.254/x"}), 400, "validation_failed"},
		"private address":     {post(map[string]string{"name": "a", "url": "https://10.1.2.3/x"}), 400, "validation_failed"},
		"credentials in url":  {post(map[string]string{"name": "a", "url": "https://u:p@hooks.example.com/x"}), 400, "validation_failed"},
		"unknown field":       {post(map[string]any{"name": "a", "url": apiURL, "status": "ACTIVE"}), 400, "invalid_json"},
		"not json":            {post("{nope"), 400, "invalid_json"},
		"duplicate name":      {post(map[string]string{"name": "orders", "url": apiURL}), 409, "destination_name_taken"},
		"unknown destination": {is.c.do("GET", base+"/00000000-0000-4000-8000-000000000000", nil), 404, "not_found"},
		"malformed id":        {is.c.do("GET", base+"/not-a-uuid", nil), 404, "not_found"},
		"empty patch":         {is.c.mutate("PATCH", base+"/"+id, map[string]string{}), 400, "validation_failed"},
		"patch blocked url":   {is.c.mutate("PATCH", base+"/"+id, map[string]string{"url": "https://localhost/x"}), 400, "validation_failed"},
		"bad status filter":   {is.c.do("GET", base+"?status=NOPE", nil), 400, "validation_failed"},
		"bad limit":           {is.c.do("GET", base+"?limit=0", nil), 400, "validation_failed"},
		"limit too large":     {is.c.do("GET", base+"?limit=101", nil), 400, "validation_failed"},
		"negative offset":     {is.c.do("GET", base+"?offset=-1", nil), 400, "validation_failed"},
		"wrong content type":  {is.c.do("POST", base, "name=a", "Content-Type", "text/plain", "X-CSRF-Token", is.c.csrf), 415, "unsupported_media_type"},
		"oversized body":      {post(map[string]string{"name": "a", "url": "https://hooks.example.com/" + strings.Repeat("a", 200000)}), 413, "request_too_large"},
	}
	for name, c := range cases {
		if c.r.Status != c.status || (c.code != "" && c.r.errorCode() != c.code) {
			// The CSRF header name is defined by the API; fall back to the shared helper if the literal differs.
			t.Errorf("%s: %d %s, want %d %s", name, c.r.Status, c.r.Body, c.status, c.code)
		}
		if strings.Contains(c.r.Body, "QUERYSECRET") {
			t.Errorf("%s: the error response echoes the URL", name)
		}
	}
	if r := is.c.do("GET", base+"?status=ACTIVE&limit=1", nil); r.Status != 200 || r.json()["limit"] != float64(1) {
		t.Fatalf("pagination envelope: %d %s", r.Status, r.Body)
	}
}

// bodyFor returns a valid request body for the route, so a rejection can only come from authorization.
func bodyFor(path, endpointID, destinationID string) map[string]string {
	switch {
	case strings.HasSuffix(path, "/subscriptions"):
		return map[string]string{"endpointId": endpointID, "destinationId": destinationID}
	case strings.HasSuffix(path, "/destinations"):
		return map[string]string{"name": "x", "url": apiURL}
	case strings.Contains(path, "/destinations/") && !strings.HasSuffix(path, "/pause") && !strings.HasSuffix(path, "/rotate-secret") && !strings.Contains(path, "/subscriptions/"):
		return map[string]string{"name": "x"}
	}
	return nil
}

func TestDestinationSubscriptionAndDeliveryRoutesRequireSessionCSRFAndRole(t *testing.T) {
	s := newServer(t, false)
	is := newIngestSetup(t, s)
	org := "/api/v1/organizations/" + is.org
	id, _ := createDestination(t, is.c, is.org, "D")
	sub := is.c.mutate("POST", org+"/subscriptions", map[string]string{"endpointId": is.id, "destinationId": id})
	if sub.Status != 201 {
		t.Fatalf("create subscription: %d %s", sub.Status, sub.Body)
	}
	subID := sub.json()["subscription"].(map[string]any)["id"].(string)

	all := [][2]string{
		{"GET", org + "/destinations"}, {"POST", org + "/destinations"}, {"GET", org + "/destinations/" + id},
		{"PATCH", org + "/destinations/" + id}, {"POST", org + "/destinations/" + id + "/pause"},
		{"POST", org + "/destinations/" + id + "/activate"}, {"POST", org + "/destinations/" + id + "/rotate-secret"},
		{"DELETE", org + "/destinations/" + id},
		{"GET", org + "/subscriptions"}, {"POST", org + "/subscriptions"}, {"GET", org + "/subscriptions/" + subID}, {"DELETE", org + "/subscriptions/" + subID},
		{"GET", org + "/deliveries"}, {"GET", org + "/deliveries/00000000-0000-4000-8000-000000000000"},
	}
	anon := s.client(t)
	for _, ep := range all {
		if r := anon.do(ep[0], ep[1], nil); r.Status != 401 {
			t.Errorf("%s %s without a session: %d", ep[0], ep[1], r.Status)
		}
	}
	for _, ep := range all {
		if ep[0] == "GET" {
			continue
		}
		if r := is.c.do(ep[0], ep[1], map[string]string{"name": "x"}); r.Status != 403 || r.errorCode() != "csrf_invalid" {
			t.Errorf("%s %s without CSRF: %d %s", ep[0], ep[1], r.Status, r.Body)
		}
	}
	if is.c.do("GET", org+"/destinations/"+id, nil).json()["destination"].(map[string]any)["status"] != "ACTIVE" {
		t.Fatal("a request rejected by CSRF protection changed state")
	}

	viewer := join(t, is.c, s, is.org, "viewer@example.com", "VIEWER")
	dev := join(t, is.c, s, is.org, "dev@example.com", "DEVELOPER")
	for _, ep := range [][2]string{{"GET", org + "/destinations"}, {"GET", org + "/destinations/" + id}, {"GET", org + "/subscriptions"}, {"GET", org + "/deliveries"}} {
		if r := viewer.do(ep[0], ep[1], nil); r.Status != 200 {
			t.Errorf("viewer %s %s: %d", ep[0], ep[1], r.Status)
		}
	}
	for _, ep := range [][2]string{{"POST", org + "/destinations"}, {"PATCH", org + "/destinations/" + id}, {"POST", org + "/destinations/" + id + "/pause"},
		{"POST", org + "/destinations/" + id + "/rotate-secret"}, {"DELETE", org + "/destinations/" + id}, {"POST", org + "/subscriptions"}, {"DELETE", org + "/subscriptions/" + subID}} {
		if r := viewer.mutate(ep[0], ep[1], bodyFor(ep[1], is.id, id)); r.Status != 403 || r.errorCode() != "forbidden" {
			t.Errorf("viewer %s %s: %d %s", ep[0], ep[1], r.Status, r.Body)
		}
	}
	for _, ep := range [][2]string{{"POST", org + "/destinations/" + id + "/rotate-secret"}, {"DELETE", org + "/destinations/" + id}} {
		if r := dev.mutate(ep[0], ep[1], nil); r.Status != 403 {
			t.Errorf("developer %s %s: %d", ep[0], ep[1], r.Status)
		}
	}
	if r := dev.mutate("POST", org+"/destinations", map[string]string{"name": "Dev made", "url": apiURL}); r.Status != 201 {
		t.Fatalf("developer create: %d %s", r.Status, r.Body)
	}
}

func TestSubscriptionAPILifecycle(t *testing.T) {
	s := newServer(t, false)
	is := newIngestSetup(t, s)
	org := "/api/v1/organizations/" + is.org
	id, _ := createDestination(t, is.c, is.org, "D")

	body := map[string]string{"endpointId": is.id, "destinationId": id}
	first := is.c.mutate("POST", org+"/subscriptions", body)
	if first.Status != 201 {
		t.Fatalf("create: %d %s", first.Status, first.Body)
	}
	firstID := first.json()["subscription"].(map[string]any)["id"].(string)
	if r := is.c.mutate("POST", org+"/subscriptions", body); r.Status != 409 || r.errorCode() != "already_subscribed" {
		t.Fatalf("duplicate: %d %s", r.Status, r.Body)
	}
	if r := is.c.mutate("POST", org+"/subscriptions", map[string]string{"endpointId": "x", "destinationId": id}); r.Status != 400 {
		t.Fatalf("malformed endpoint id: %d", r.Status)
	}
	if r := is.c.mutate("POST", org+"/subscriptions", map[string]string{"endpointId": is.id, "destinationId": "00000000-0000-4000-8000-000000000000"}); r.Status != 404 {
		t.Fatalf("missing destination: %d", r.Status)
	}
	rev := is.c.mutate("DELETE", org+"/subscriptions/"+firstID, nil)
	if rev.Status != 200 || rev.json()["subscription"].(map[string]any)["status"] != "REVOKED" {
		t.Fatalf("revoke: %d %s", rev.Status, rev.Body)
	}
	if r := is.c.mutate("DELETE", org+"/subscriptions/"+firstID, nil); r.Status != 409 || r.errorCode() != "subscription_revoked" {
		t.Fatalf("revoking twice: %d %s", r.Status, r.Body)
	}
	if again := is.c.mutate("POST", org+"/subscriptions", body); again.Status != 201 || again.json()["subscription"].(map[string]any)["id"] == firstID {
		t.Fatalf("re-subscribing must create a new record: %d %s", again.Status, again.Body)
	}
	list := is.c.do("GET", org+"/subscriptions?destinationId="+id, nil)
	if list.Status != 200 || list.json()["total"] != float64(2) {
		t.Fatalf("list: %d %s", list.Status, list.Body)
	}
	if r := is.c.do("GET", org+"/subscriptions?status=ACTIVE", nil); r.json()["total"] != float64(1) {
		t.Fatalf("active filter: %s", r.Body)
	}
	if r := is.c.do("GET", org+"/subscriptions?status=BOGUS", nil); r.Status != 400 {
		t.Fatalf("bad status filter: %d", r.Status)
	}
}

// seedHTTPDelivery creates a delivery with attempts using the real store functions.
func seedHTTPDelivery(t *testing.T, env *apptest.Env, org, userID, endpointPublicID, endpointSecret, destID string, attempts int, createdAt time.Time) string {
	t.Helper()
	res, err := env.Send(endpointPublicID, env.Signed(endpointSecret, []byte(`{"secret":"PAYLOADSECRET"}`), fmt.Sprintf("k%d", time.Now().UnixNano())))
	if err != nil {
		t.Fatal(err)
	}
	q, _ := env.Pool.Querier()
	var id string
	if err := q.QueryRow(bg, `INSERT INTO deliveries (organization_id, event_id, destination_id, created_at) VALUES ($1, $2, $3, $4) RETURNING id::text`,
		org, res.EventID, destID, createdAt).Scan(&id); err != nil {
		t.Fatal(err)
	}
	for k := 1; k <= attempts; k++ {
		var token string
		if err := q.QueryRow(bg, `UPDATE deliveries SET status = 'IN_FLIGHT', attempt_count = $2, lease_expires_at = now() + interval '1 minute',
			claim_token = gen_random_uuid(), claimed_by = 'seed' WHERE id = $1 RETURNING claim_token::text`, id, k).Scan(&token); err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC()
		rec := delivery.AttemptRecord{Number: k, StartedAt: now.Add(-2 * time.Second), FinishedAt: now.Add(-time.Second), Outcome: delivery.OutcomeHTTPError, HTTPStatus: 503}
		dec := delivery.Decision{Status: delivery.StatusPending, NextAttemptAt: now.Add(time.Hour)}
		if k == attempts {
			rec = delivery.AttemptRecord{Number: k, StartedAt: now.Add(-2 * time.Second), FinishedAt: now.Add(-time.Second), Outcome: delivery.OutcomeNetworkError, ErrorClass: "connect"}
			dec = delivery.Decision{Status: delivery.StatusFailed, FailureReason: delivery.FailureRetriesExhausted}
		}
		if err := env.Pool.InTx(bg, func(tx pgx.Tx) error { return delivery.Finish(bg, tx, org, id, token, rec, dec, now) }); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func TestDeliveryHistoryAPIIsReadOnlyScopedAndSanitized(t *testing.T) {
	s := newServer(t, false)
	is := newIngestSetup(t, s)
	org := "/api/v1/organizations/" + is.org
	destID, destSecret := createDestination(t, is.c, is.org, "Target")
	var ownerID string
	if err := mustQuery(s, "SELECT id::text FROM users LIMIT 1", &ownerID); err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Second)
	d1 := seedHTTPDelivery(t, s.env, is.org, ownerID, is.pub, is.secret, destID, 3, base)
	d2 := seedHTTPDelivery(t, s.env, is.org, ownerID, is.pub, is.secret, destID, 1, base.Add(time.Hour))

	list := is.c.do("GET", org+"/deliveries?limit=1", nil)
	if list.Status != 200 || list.json()["total"] != float64(2) || len(list.json()["items"].([]any)) != 1 {
		t.Fatalf("list: %d %s", list.Status, list.Body)
	}
	if first := list.json()["items"].([]any)[0].(map[string]any); first["id"] != d2 || first["destinationName"] != "Target" || first["status"] != "FAILED" {
		t.Fatalf("newest first with the destination name joined: %v", first)
	}
	for name, q := range map[string]string{
		"status":      "?status=FAILED",
		"destination": "?destinationId=" + destID,
		"range":       "?from=" + base.Format(time.RFC3339) + "&to=" + base.Add(30*time.Minute).Format(time.RFC3339),
	} {
		r := is.c.do("GET", org+"/deliveries"+q, nil)
		want := float64(2)
		if name == "range" {
			want = 1
		}
		if r.Status != 200 || r.json()["total"] != want {
			t.Errorf("filter %s: %d %s", name, r.Status, r.Body)
		}
	}
	for name, q := range map[string]string{
		"bad date":   "?from=yesterday",
		"inverted":   "?from=" + base.Add(time.Hour).Format(time.RFC3339) + "&to=" + base.Format(time.RFC3339),
		"bad status": "?status=DONE",
		"bad uuid":   "?destinationId=nope",
		"bad event":  "?eventId=nope",
		"huge limit": "?limit=1000",
		"bad offset": "?offset=abc",
	} {
		if r := is.c.do("GET", org+"/deliveries"+q, nil); r.Status != 400 {
			t.Errorf("%s: %d %s, want 400", name, r.Status, r.Body)
		}
	}

	det := is.c.do("GET", org+"/deliveries/"+d1, nil)
	if det.Status != 200 {
		t.Fatalf("detail: %d %s", det.Status, det.Body)
	}
	attempts := det.json()["attempts"].([]any)
	if len(attempts) != 3 || attempts[0].(map[string]any)["attemptNumber"] != float64(1) || attempts[2].(map[string]any)["errorClass"] != "connect" {
		t.Fatalf("attempts: %s", det.Body)
	}
	// Only whitelisted, sanitized fields are present.
	allowedDelivery := map[string]bool{"id": true, "organizationId": true, "eventId": true, "destinationId": true, "destinationName": true, "status": true,
		"attemptCount": true, "maxAttempts": true, "nextAttemptAt": true, "lastHttpStatus": true, "lastErrorClass": true, "failureReason": true, "createdAt": true, "updatedAt": true, "completedAt": true}
	for k := range det.json()["delivery"].(map[string]any) {
		if !allowedDelivery[k] {
			t.Errorf("unexpected delivery field %q", k)
		}
	}
	allowedAttempt := map[string]bool{"attemptNumber": true, "startedAt": true, "finishedAt": true, "durationMs": true, "outcome": true, "httpStatus": true, "errorClass": true, "terminalReason": true}
	for _, a := range attempts {
		for k := range a.(map[string]any) {
			if !allowedAttempt[k] {
				t.Errorf("unexpected attempt field %q", k)
			}
		}
	}
	for _, r := range []response{list, det} {
		for _, banned := range []string{destSecret, "PAYLOADSECRET", "hooks.example.com", "QUERYSECRET", "claimToken", "claim_token", "claimedBy", "leaseExpiresAt", "payload"} {
			if strings.Contains(r.Body, banned) {
				t.Errorf("delivery history exposes %q", banned)
			}
		}
	}
	if r := is.c.do("GET", org+"/deliveries/00000000-0000-4000-8000-000000000000", nil); r.Status != 404 {
		t.Fatalf("missing delivery: %d", r.Status)
	}

	// Read-only: no verb other than GET is routed, even with a valid session and CSRF token.
	before := s.env.Count(t, "SELECT count(*) FROM delivery_attempts")
	for _, m := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		for _, path := range []string{org + "/deliveries", org + "/deliveries/" + d1, org + "/deliveries/" + d1 + "/attempts"} {
			if r := is.c.mutate(m, path, map[string]string{"status": "SUCCEEDED"}); r.Status != 404 && r.Status != 405 {
				t.Errorf("%s %s: %d, want 404 or 405", m, path, r.Status)
			}
		}
	}
	if s.env.Count(t, "SELECT count(*) FROM delivery_attempts") != before || s.env.Count(t, "SELECT count(*) FROM deliveries WHERE status = 'SUCCEEDED'") != 0 {
		t.Fatal("history changed through the API")
	}
}

func mustQuery(s *server, sql string, dst *string) error {
	q, err := s.env.Pool.Querier()
	if err != nil {
		return err
	}
	return q.QueryRow(bg, sql).Scan(dst)
}

func TestDestinationSubscriptionAndDeliveryTenantIsolationOverHTTP(t *testing.T) {
	s := newServer(t, false)
	is := newIngestSetup(t, s)
	org := "/api/v1/organizations/" + is.org
	destID, _ := createDestination(t, is.c, is.org, "Private")
	sub := is.c.mutate("POST", org+"/subscriptions", map[string]string{"endpointId": is.id, "destinationId": destID})
	subID := sub.json()["subscription"].(map[string]any)["id"].(string)
	var ownerID string
	if err := mustQuery(s, "SELECT id::text FROM users LIMIT 1", &ownerID); err != nil {
		t.Fatal(err)
	}
	delID := seedHTTPDelivery(t, s.env, is.org, ownerID, is.pub, is.secret, destID, 2, time.Now().UTC())

	intruder := s.client(t)
	own := intruder.orgID(intruder.register("intruder@example.com"))
	ownBase := "/api/v1/organizations/" + own
	ownDest, _ := createDestination(t, intruder, own, "Theirs")
	ownEndpoint := intruder.mutate("POST", ownBase+"/ingestion-endpoints", map[string]string{"name": "E"}).json()["endpoint"].(map[string]any)["id"].(string)

	for name, r := range map[string]response{
		"list destinations of foreign org":  intruder.do("GET", org+"/destinations", nil),
		"get foreign destination":           intruder.do("GET", org+"/destinations/"+destID, nil),
		"get via own org":                   intruder.do("GET", ownBase+"/destinations/"+destID, nil),
		"patch via own org":                 intruder.mutate("PATCH", ownBase+"/destinations/"+destID, map[string]string{"name": "x"}),
		"pause via own org":                 intruder.mutate("POST", ownBase+"/destinations/"+destID+"/pause", nil),
		"rotate via own org":                intruder.mutate("POST", ownBase+"/destinations/"+destID+"/rotate-secret", nil),
		"revoke via own org":                intruder.mutate("DELETE", ownBase+"/destinations/"+destID, nil),
		"create in foreign org":             intruder.mutate("POST", org+"/destinations", map[string]string{"name": "x", "url": apiURL}),
		"list subscriptions of foreign org": intruder.do("GET", org+"/subscriptions", nil),
		"get subscription via own org":      intruder.do("GET", ownBase+"/subscriptions/"+subID, nil),
		"revoke subscription via own org":   intruder.mutate("DELETE", ownBase+"/subscriptions/"+subID, nil),
		"subscribe own endpoint to foreign": intruder.mutate("POST", ownBase+"/subscriptions", map[string]string{"endpointId": ownEndpoint, "destinationId": destID}),
		"subscribe foreign endpoint to own": intruder.mutate("POST", ownBase+"/subscriptions", map[string]string{"endpointId": is.id, "destinationId": ownDest}),
		"deliveries of foreign org":         intruder.do("GET", org+"/deliveries", nil),
		"foreign delivery via own org":      intruder.do("GET", ownBase+"/deliveries/"+delID, nil),
	} {
		if r.Status != 404 || r.errorCode() != "not_found" {
			t.Errorf("%s: %d %s, want 404 not_found", name, r.Status, r.Body)
		}
		for _, leak := range []string{destID, subID, delID, "Private"} {
			if strings.Contains(r.Body, leak) {
				t.Errorf("%s leaks %q", name, leak)
			}
		}
	}
	// The same destination id filter on the intruder's own history finds nothing.
	if r := intruder.do("GET", ownBase+"/deliveries?destinationId="+destID, nil); r.Status != 200 || r.json()["total"] != float64(0) {
		t.Fatalf("filter by a foreign destination id: %d %s", r.Status, r.Body)
	}
	// Nothing changed for the owner.
	if r := is.c.do("GET", org+"/destinations/"+destID, nil); r.json()["destination"].(map[string]any)["status"] != "ACTIVE" || r.json()["destination"].(map[string]any)["name"] != "Private" {
		t.Fatal("a cross-tenant call changed the destination")
	}
	if r := is.c.do("GET", org+"/subscriptions/"+subID, nil); r.json()["subscription"].(map[string]any)["status"] != "ACTIVE" {
		t.Fatal("a cross-tenant call changed the subscription")
	}
	_ = http.StatusOK
}

// TestDeliveryAPIExposesTheFailureReasonOnlyWhereItExists covers the response shape of the
// terminal failure reasons (ADR 30): failureReason on a FAILED delivery, terminalReason on
// the one attempt that ended it, both omitted (not null) everywhere else, readable by every
// role that can read deliveries and invisible across tenants.
func TestDeliveryAPIExposesTheFailureReasonOnlyWhereItExists(t *testing.T) {
	s := newServer(t, false)
	is := newIngestSetup(t, s)
	org := "/api/v1/organizations/" + is.org
	destID, destSecret := createDestination(t, is.c, is.org, "Target")
	var ownerID string
	if err := mustQuery(s, "SELECT id::text FROM users LIMIT 1", &ownerID); err != nil {
		t.Fatal(err)
	}
	failed := seedHTTPDelivery(t, s.env, is.org, ownerID, is.pub, is.secret, destID, 3, time.Now().UTC().Add(-time.Hour))

	// A delivery that has not failed.
	ev, err := s.env.Send(is.pub, s.env.Signed(is.secret, []byte(`{"p":1}`), "pending-1"))
	if err != nil {
		t.Fatal(err)
	}
	var pending string
	if err := mustQuery(s, "INSERT INTO deliveries (organization_id, event_id, destination_id) VALUES ('"+is.org+"', '"+ev.EventID+"', '"+destID+"') RETURNING id::text", &pending); err != nil {
		t.Fatal(err)
	}

	check := func(who string, c *client) {
		t.Helper()
		det := c.do("GET", org+"/deliveries/"+failed, nil)
		if det.Status != 200 {
			t.Fatalf("%s detail: %d %s", who, det.Status, det.Body)
		}
		d := det.json()["delivery"].(map[string]any)
		if d["status"] != "FAILED" || d["failureReason"] != "RETRIES_EXHAUSTED" {
			t.Errorf("%s: delivery %v", who, d)
		}
		attempts := det.json()["attempts"].([]any)
		for i, a := range attempts {
			reason, has := a.(map[string]any)["terminalReason"]
			switch {
			case i < len(attempts)-1 && has:
				t.Errorf("%s: attempt %d is not terminal but has terminalReason %v", who, i+1, reason)
			case i == len(attempts)-1 && reason != "RETRIES_EXHAUSTED":
				t.Errorf("%s: the terminal attempt has terminalReason %v", who, reason)
			}
		}
		list := c.do("GET", org+"/deliveries?status=FAILED", nil)
		if list.Status != 200 || list.json()["total"] != float64(1) || list.json()["items"].([]any)[0].(map[string]any)["failureReason"] != "RETRIES_EXHAUSTED" {
			t.Errorf("%s list: %d %s", who, list.Status, list.Body)
		}
		for _, r := range []response{det, list} {
			for _, banned := range []string{destSecret, "PAYLOADSECRET", "hooks.example.com", "QUERYSECRET", "claim_token", "\"failureReason\":null", "\"terminalReason\":null"} {
				if strings.Contains(r.Body, banned) {
					t.Errorf("%s: response contains %q", who, banned)
				}
			}
		}
	}
	check("owner", is.c)
	for _, role := range []string{"ADMIN", "DEVELOPER", "VIEWER"} {
		check(role, join(t, is.c, s, is.org, strings.ToLower(role)+"@example.com", role))
	}

	// Fields that do not apply are omitted, not null.
	r := is.c.do("GET", org+"/deliveries/"+pending, nil)
	if r.Status != 200 || strings.Contains(r.Body, "failureReason") || strings.Contains(r.Body, "terminalReason") {
		t.Fatalf("a delivery that has not failed must omit both fields: %d %s", r.Status, r.Body)
	}

	// Another tenant sees nothing, and learns nothing from the id.
	intruder := s.client(t)
	own := intruder.orgID(intruder.register("reason-intruder@example.com"))
	if r := intruder.do("GET", "/api/v1/organizations/"+own+"/deliveries/"+failed, nil); r.Status != 404 || strings.Contains(r.Body, "RETRIES_EXHAUSTED") {
		t.Fatalf("cross-tenant read: %d %s", r.Status, r.Body)
	}
	if r := intruder.do("GET", org+"/deliveries/"+failed, nil); r.Status != 404 || strings.Contains(r.Body, "RETRIES_EXHAUSTED") {
		t.Fatalf("foreign organization path: %d %s", r.Status, r.Body)
	}
}
