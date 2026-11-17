package httpapi_test

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Thiago-tods/relayforge/internal/app/apptest"
	"github.com/Thiago-tods/relayforge/internal/httpapi"
	"github.com/Thiago-tods/relayforge/internal/ingest"
)

// owner registers a user and creates an endpoint through the management API.
type ingestSetup struct {
	s        *server
	c        *client
	org      string
	endpoint map[string]any
	secret   string
	pub      string
	id       string
}

func newIngestSetup(t *testing.T, s *server) ingestSetup {
	t.Helper()
	c := s.client(t)
	reg := c.register("owner@example.com")
	org := c.orgID(reg)
	res := c.mutate("POST", "/api/v1/organizations/"+org+"/ingestion-endpoints", map[string]string{"name": "Orders"})
	if res.Status != 201 {
		t.Fatalf("create endpoint: %d %s", res.Status, res.Body)
	}
	ep := res.json()["endpoint"].(map[string]any)
	return ingestSetup{s: s, c: c, org: org, endpoint: ep, secret: ep["signingSecret"].(string), pub: ep["publicId"].(string), id: ep["id"].(string)}
}

// post sends a raw ingestion request exactly as a producer would, without cookies.
func (is ingestSetup) post(publicID string, body []byte, headers map[string]string) response {
	return rawPost(is.s.url+"/api/v1/ingest/"+publicID, body, headers)
}

func rawPost(url string, body []byte, headers map[string]string) response {
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		panic(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		panic(err)
	}
	defer func() { _ = resp.Body.Close() }()
	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(resp.Body)
	return response{Status: resp.StatusCode, Header: resp.Header, Body: buf.String()}
}

func (is ingestSetup) signedHeaders(secret string, body []byte, key string) map[string]string {
	ts := strconv.FormatInt(is.s.env.Clock.Now().Unix(), 10)
	h := map[string]string{
		"Content-Type": "application/json", ingest.HeaderTimestamp: ts,
		ingest.HeaderSignature: ingest.Sign(secret, ts, body),
	}
	if key != "" {
		h[ingest.HeaderIdempotencyKey] = key
	}
	return h
}

func TestEndpointManagementOverHTTPNeverLeaksSecretsAfterCreation(t *testing.T) {
	s := newServer(t, false)
	is := newIngestSetup(t, s)
	base := "/api/v1/organizations/" + is.org + "/ingestion-endpoints"

	if is.endpoint["status"] != "ACTIVE" || is.endpoint["ingestPath"] != "/api/v1/ingest/"+is.pub || !strings.HasPrefix(is.secret, "rfs_") {
		t.Fatalf("unexpected endpoint: %v", is.endpoint)
	}
	if is.pub == is.id {
		t.Fatal("the public identifier must differ from the internal id")
	}

	list := is.c.do("GET", base, nil)
	get := is.c.do("GET", base+"/"+is.id, nil)
	patch := is.c.mutate("PATCH", base+"/"+is.id, map[string]string{"name": "Renamed"})
	pause := is.c.mutate("POST", base+"/"+is.id+"/pause", nil)
	activate := is.c.mutate("POST", base+"/"+is.id+"/activate", nil)
	for name, r := range map[string]response{"list": list, "get": get, "patch": patch, "pause": pause, "activate": activate} {
		if r.Status != 200 {
			t.Fatalf("%s: %d %s", name, r.Status, r.Body)
		}
		for _, banned := range []string{is.secret, "signingSecret", "ciphertext", "Ciphertext", "encryption", "secret_version", "secretVersion"} {
			if strings.Contains(r.Body, banned) {
				t.Errorf("%s response exposes %q: %s", name, banned, r.Body)
			}
		}
	}
	if patch.json()["endpoint"].(map[string]any)["name"] != "Renamed" || pause.json()["endpoint"].(map[string]any)["status"] != "PAUSED" {
		t.Fatal("PATCH and pause must return the updated endpoint")
	}

	rot := is.c.mutate("POST", base+"/"+is.id+"/rotate-secret", nil)
	if rot.Status != 200 {
		t.Fatalf("rotate: %d %s", rot.Status, rot.Body)
	}
	newSecret := rot.json()["endpoint"].(map[string]any)["signingSecret"].(string)
	if newSecret == is.secret || !strings.HasPrefix(newSecret, "rfs_") {
		t.Fatal("rotation must return a new secret once")
	}
	if again := is.c.do("GET", base+"/"+is.id, nil); strings.Contains(again.Body, newSecret) {
		t.Fatal("the rotated secret must not be retrievable later")
	}

	rev := is.c.mutate("DELETE", base+"/"+is.id, nil)
	if rev.Status != 200 || rev.json()["endpoint"].(map[string]any)["status"] != "REVOKED" {
		t.Fatalf("revoke: %d %s", rev.Status, rev.Body)
	}
	if r := is.c.mutate("POST", base+"/"+is.id+"/activate", nil); r.Status != 409 || r.errorCode() != "endpoint_revoked" {
		t.Fatalf("activating a revoked endpoint: %d %s", r.Status, r.Body)
	}
	if r := is.c.do("GET", base+"/"+is.id, nil); r.Status != 200 {
		t.Fatalf("revoked endpoints stay readable: %d", r.Status)
	}
}

func TestEndpointManagementRequiresSessionCSRFAndRole(t *testing.T) {
	s := newServer(t, false)
	is := newIngestSetup(t, s)
	base := "/api/v1/organizations/" + is.org + "/ingestion-endpoints"

	// No session.
	anon := s.client(t)
	for _, ep := range [][2]string{{"GET", base}, {"POST", base}, {"GET", base + "/" + is.id}, {"POST", base + "/" + is.id + "/rotate-secret"}, {"DELETE", base + "/" + is.id}, {"GET", "/api/v1/organizations/" + is.org + "/events"}} {
		if r := anon.do(ep[0], ep[1], nil); r.Status != 401 {
			t.Errorf("%s %s without session: %d", ep[0], ep[1], r.Status)
		}
	}
	// Session but no CSRF token.
	for _, ep := range [][2]string{{"POST", base}, {"PATCH", base + "/" + is.id}, {"POST", base + "/" + is.id + "/pause"}, {"POST", base + "/" + is.id + "/rotate-secret"}, {"DELETE", base + "/" + is.id}} {
		if r := is.c.do(ep[0], ep[1], map[string]string{"name": "x"}); r.Status != 403 || r.errorCode() != "csrf_invalid" {
			t.Errorf("%s %s without CSRF: %d %s", ep[0], ep[1], r.Status, r.Body)
		}
	}
	if n := s.env.Count(t, "SELECT count(*) FROM audit_events WHERE action LIKE 'ingestion_endpoint.%' AND action <> 'ingestion_endpoint.created'"); n != 0 {
		t.Fatal("requests rejected by CSRF protection changed state")
	}

	// RBAC over HTTP: viewer reads only; developer cannot rotate or revoke.
	viewer, dev := s.client(t), s.client(t)
	vReg, dReg := viewer.register("viewer@example.com"), dev.register("dev@example.com")
	_ = vReg
	_ = dReg
	for who, role := range map[*client]string{viewer: "VIEWER", dev: "DEVELOPER"} {
		email := "viewer@example.com"
		if role == "DEVELOPER" {
			email = "dev@example.com"
		}
		inv := is.c.mutate("POST", "/api/v1/organizations/"+is.org+"/invitations", map[string]string{"email": email, "role": role})
		if inv.Status != 201 {
			t.Fatalf("invite %s: %d %s", role, inv.Status, inv.Body)
		}
		if r := who.mutate("POST", "/api/v1/invitations/accept", map[string]string{"token": inv.json()["token"].(string)}); r.Status != 200 {
			t.Fatalf("accept %s: %d %s", role, r.Status, r.Body)
		}
	}
	if r := viewer.do("GET", base, nil); r.Status != 200 {
		t.Fatalf("viewer list: %d", r.Status)
	}
	if r := viewer.mutate("POST", base, map[string]string{"name": "x"}); r.Status != 403 {
		t.Fatalf("viewer create: %d", r.Status)
	}
	if r := dev.mutate("POST", base, map[string]string{"name": "Dev made"}); r.Status != 201 {
		t.Fatalf("developer create: %d %s", r.Status, r.Body)
	}
	if r := dev.mutate("POST", base+"/"+is.id+"/pause", nil); r.Status != 200 {
		t.Fatalf("developer pause: %d", r.Status)
	}
	for name, r := range map[string]response{
		"rotate": dev.mutate("POST", base+"/"+is.id+"/rotate-secret", nil),
		"revoke": dev.mutate("DELETE", base+"/"+is.id, nil),
	} {
		if r.Status != 403 || r.errorCode() != "forbidden" {
			t.Errorf("developer %s: %d %s", name, r.Status, r.Body)
		}
	}
}

func TestEndpointTenantIsolationOverHTTP(t *testing.T) {
	s := newServer(t, false)
	is := newIngestSetup(t, s)
	intruder := s.client(t)
	reg := intruder.register("intruder@example.com")
	own := intruder.orgID(reg)
	foreign := "/api/v1/organizations/" + is.org + "/ingestion-endpoints"
	through := "/api/v1/organizations/" + own + "/ingestion-endpoints"

	for name, r := range map[string]response{
		"list foreign org":      intruder.do("GET", foreign, nil),
		"get foreign endpoint":  intruder.do("GET", foreign+"/"+is.id, nil),
		"rotate foreign":        intruder.mutate("POST", foreign+"/"+is.id+"/rotate-secret", nil),
		"revoke foreign":        intruder.mutate("DELETE", foreign+"/"+is.id, nil),
		"get via own org":       intruder.do("GET", through+"/"+is.id, nil),
		"pause via own org":     intruder.mutate("POST", through+"/"+is.id+"/pause", nil),
		"rotate via own org":    intruder.mutate("POST", through+"/"+is.id+"/rotate-secret", nil),
		"events of foreign org": intruder.do("GET", "/api/v1/organizations/"+is.org+"/events", nil),
		"create in foreign org": intruder.mutate("POST", foreign, map[string]string{"name": "x"}),
		"malformed endpoint id": intruder.do("GET", through+"/not-a-uuid", nil),
		"unknown endpoint id":   intruder.do("GET", through+"/00000000-0000-4000-8000-000000000000", nil),
	} {
		if r.Status != 404 || r.errorCode() != "not_found" {
			t.Errorf("%s: %d %s", name, r.Status, r.Body)
		}
	}
	if is.c.do("GET", foreign+"/"+is.id, nil).json()["endpoint"].(map[string]any)["status"] != "ACTIVE" {
		t.Fatal("the endpoint was modified across tenants")
	}
}

func TestEndpointListPagination(t *testing.T) {
	s := newServer(t, false)
	is := newIngestSetup(t, s)
	base := "/api/v1/organizations/" + is.org + "/ingestion-endpoints"
	for i := 0; i < 4; i++ {
		is.c.mutate("POST", base, map[string]string{"name": "extra " + strconv.Itoa(i)})
	}
	page := is.c.do("GET", base+"?limit=2&offset=1", nil)
	if page.Status != 200 || len(page.json()["items"].([]any)) != 2 || page.json()["total"].(float64) != 5 {
		t.Fatalf("page: %d %s", page.Status, page.Body)
	}
	for _, q := range []string{"?limit=0", "?limit=101", "?offset=-1", "?limit=abc"} {
		if r := is.c.do("GET", base+q, nil); r.Status != 400 {
			t.Errorf("%s: %d", q, r.Status)
		}
	}
	if r := is.c.mutate("POST", base, map[string]string{"name": ""}); r.Status != 400 {
		t.Errorf("empty name: %d", r.Status)
	}
}

func TestIngestionHappyPathReplayAndConflict(t *testing.T) {
	s := newServer(t, false)
	is := newIngestSetup(t, s)
	body := []byte("{ \"order\": 1,\n \"total\": 9.50 }")

	first := is.post(is.pub, body, is.signedHeaders(is.secret, body, "order-1"))
	if first.Status != 202 {
		t.Fatalf("first: %d %s", first.Status, first.Body)
	}
	id := first.json()["event"].(map[string]any)["id"].(string)
	if first.json()["duplicate"] != false || first.json()["event"].(map[string]any)["status"] != "RECEIVED" {
		t.Fatalf("response: %s", first.Body)
	}
	for _, banned := range []string{is.secret, "ciphertext", "outbox", "postgres", "sql"} {
		if strings.Contains(strings.ToLower(first.Body), strings.ToLower(banned)) {
			t.Errorf("response leaks %q", banned)
		}
	}

	again := is.post(is.pub, body, is.signedHeaders(is.secret, body, "order-1"))
	if again.Status != 202 || again.json()["duplicate"] != true || again.json()["event"].(map[string]any)["id"] != id {
		t.Fatalf("replay must return the original event: %d %s", again.Status, again.Body)
	}
	other := []byte(`{"order":2}`)
	conflict := is.post(is.pub, other, is.signedHeaders(is.secret, other, "order-1"))
	if conflict.Status != 409 || conflict.errorCode() != "idempotency_conflict" {
		t.Fatalf("conflict: %d %s", conflict.Status, conflict.Body)
	}
	if s.env.Count(t, "SELECT count(*) FROM webhook_events") != 1 || s.env.Count(t, "SELECT count(*) FROM outbox") != 1 {
		t.Fatal("exactly one event and one outbox record expected")
	}

	// Event retrieval through the API: metadata in the list, payload only in the detail view.
	events := is.c.do("GET", "/api/v1/organizations/"+is.org+"/events", nil)
	if events.Status != 200 || strings.Contains(events.Body, "9.50") || strings.Contains(events.Body, `"payload"`) || strings.Contains(events.Body, "outbox") {
		t.Fatalf("event listing must be metadata only: %d %s", events.Status, events.Body)
	}
	item := events.json()["items"].([]any)[0].(map[string]any)
	if item["id"] != id || item["payloadSha256"] == "" || item["payloadSize"].(float64) != float64(len(body)) {
		t.Fatalf("event metadata: %v", item)
	}
	detail := is.c.do("GET", "/api/v1/organizations/"+is.org+"/events/"+id, nil)
	if detail.Status != 200 || !strings.Contains(detail.Body, `"total":9.50`) {
		t.Fatalf("detail must contain the payload (the JSON encoder compacts whitespace; the stored bytes stay exact): %d %s", detail.Status, detail.Body)
	}
}

func TestIngestionRejections(t *testing.T) {
	s := newServer(t, false)
	is := newIngestSetup(t, s)
	body := []byte(`{"ok":true}`)
	good := func() map[string]string { return is.signedHeaders(is.secret, body, "k-1") }
	without := func(h string) map[string]string { m := good(); delete(m, h); return m }
	with := func(h, v string) map[string]string { m := good(); m[h] = v; return m }
	now := is.s.env.Clock.Now().Unix()
	staleTS := strconv.FormatInt(now-600, 10)

	cases := []struct {
		name    string
		pub     string
		body    []byte
		headers map[string]string
		status  int
		code    string
	}{
		{"invalid hmac", is.pub, body, with(ingest.HeaderSignature, strings.Repeat("0", 64)), 401, "signature_mismatch"},
		{"signature of another body", is.pub, []byte(`{"ok":false}`), good(), 401, "signature_mismatch"},
		{"wrong secret", is.pub, body, is.signedHeaders("rfs_wrong", body, "k-1"), 401, "signature_mismatch"},
		{"missing signature", is.pub, body, without(ingest.HeaderSignature), 400, "invalid_signature"},
		{"malformed signature", is.pub, body, with(ingest.HeaderSignature, "sha256=abc"), 400, "invalid_signature"},
		{"missing timestamp", is.pub, body, without(ingest.HeaderTimestamp), 400, "invalid_timestamp"},
		{"malformed timestamp", is.pub, body, with(ingest.HeaderTimestamp, "1.5e9"), 400, "invalid_timestamp"},
		{"expired timestamp", is.pub, body, func() map[string]string {
			m := good()
			m[ingest.HeaderTimestamp] = staleTS
			m[ingest.HeaderSignature] = ingest.Sign(is.secret, staleTS, body)
			return m
		}(), 401, "timestamp_out_of_window"},
		{"missing idempotency key", is.pub, body, without(ingest.HeaderIdempotencyKey), 400, "invalid_idempotency_key"},
		{"bad idempotency key", is.pub, body, with(ingest.HeaderIdempotencyKey, "has space"), 400, "invalid_idempotency_key"},
		{"unsupported content type", is.pub, body, with("Content-Type", "text/plain"), 415, "unsupported_media_type"},
		{"missing content type", is.pub, body, without("Content-Type"), 415, "unsupported_media_type"},
		{"invalid json", is.pub, []byte(`{"ok":`), is.signedHeaders(is.secret, []byte(`{"ok":`), "k-1"), 400, "invalid_json"},
		{"empty body", is.pub, nil, is.signedHeaders(is.secret, nil, "k-1"), 400, "invalid_json"},
		{"unknown endpoint", "ep_AAAAAAAAAAAAAAAAAAAAAA", body, good(), 404, "not_found"},
		{"malformed endpoint id", "not-an-endpoint", body, good(), 404, "not_found"},
		{"path traversal id", "..%2F..%2Fetc", body, good(), 404, "not_found"},
		{"overlong endpoint id", strings.Repeat("a", 5000), body, good(), 404, "not_found"},
	}
	for _, c := range cases {
		r := is.post(c.pub, c.body, c.headers)
		if r.Status != c.status || r.errorCode() != c.code {
			t.Errorf("%s: got %d %s, want %d %s", c.name, r.Status, r.Body, c.status, c.code)
		}
		if strings.Contains(r.Body, is.secret) || strings.Contains(r.Body, string(body)) {
			t.Errorf("%s: error response echoes a secret or payload", c.name)
		}
	}
	if s.env.Count(t, "SELECT count(*) FROM webhook_events") != 0 {
		t.Fatal("a rejected request persisted an event")
	}

	// Repeated headers are ambiguous and rejected.
	req, _ := http.NewRequest("POST", s.url+"/api/v1/ingest/"+is.pub, bytes.NewReader(body))
	for k, v := range good() {
		req.Header.Set(k, v)
	}
	req.Header.Add(ingest.HeaderTimestamp, strconv.FormatInt(now, 10))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("duplicated timestamp header: %d", resp.StatusCode)
	}

	// Only POST is served.
	if r := is.c.do("GET", "/api/v1/ingest/"+is.pub, nil); r.Status != 404 && r.Status != 405 {
		t.Errorf("GET on the ingestion endpoint: %d", r.Status)
	}
}

func TestIngestionBodySizeLimit(t *testing.T) {
	s := newServer(t, false)
	is := newIngestSetup(t, s)

	exact := append([]byte(`"`), append(bytes.Repeat([]byte("a"), ingest.MaxBodyBytes-2), '"')...)
	if len(exact) != ingest.MaxBodyBytes {
		t.Fatal("test body must be exactly 1 MiB")
	}
	if r := is.post(is.pub, exact, is.signedHeaders(is.secret, exact, "big-ok")); r.Status != 202 {
		t.Fatalf("a body of exactly 1 MiB must be accepted: %d %.200s", r.Status, r.Body)
	}
	over := append(exact, ' ')
	r := is.post(is.pub, over, is.signedHeaders(is.secret, over, "big-bad"))
	if r.Status != 413 || r.errorCode() != "request_too_large" {
		t.Fatalf("1 MiB + 1 byte: %d %.200s", r.Status, r.Body)
	}
	if s.env.Count(t, "SELECT count(*) FROM webhook_events") != 1 {
		t.Fatal("an oversized request persisted an event")
	}
}

func TestIngestionIsIndependentOfCookies(t *testing.T) {
	s := newServer(t, false)
	is := newIngestSetup(t, s)
	body := []byte(`{"cookie":"test"}`)

	// A logged-in browser session grants nothing on the ingestion endpoint.
	headers := is.signedHeaders(is.secret, body, "cookie-1")
	delete(headers, ingest.HeaderSignature)
	headers["Cookie"] = httpapi.SessionCookieName + "=" + is.c.cookie.Value
	headers[httpapi.CSRFHeader] = is.c.csrf
	if r := is.post(is.pub, body, headers); r.Status != 400 || r.errorCode() != "invalid_signature" {
		t.Fatalf("cookie authentication must not apply to ingestion: %d %s", r.Status, r.Body)
	}
	// And a valid signature needs no cookie or CSRF token.
	if r := is.post(is.pub, body, is.signedHeaders(is.secret, body, "cookie-2")); r.Status != 202 {
		t.Fatalf("signed request without cookies: %d %s", r.Status, r.Body)
	}
}

func TestPausedAndRevokedEndpointsRejectEventsOverHTTP(t *testing.T) {
	s := newServer(t, false)
	is := newIngestSetup(t, s)
	base := "/api/v1/organizations/" + is.org + "/ingestion-endpoints/" + is.id
	body := []byte(`{}`)

	is.c.mutate("POST", base+"/pause", nil)
	r := is.post(is.pub, body, is.signedHeaders(is.secret, body, "p1"))
	if r.Status != 503 || r.errorCode() != "endpoint_paused" {
		t.Fatalf("paused: %d %s", r.Status, r.Body)
	}
	is.c.mutate("POST", base+"/activate", nil)
	if r := is.post(is.pub, body, is.signedHeaders(is.secret, body, "p2")); r.Status != 202 {
		t.Fatalf("activated: %d %s", r.Status, r.Body)
	}
	is.c.mutate("DELETE", base, nil)
	if r := is.post(is.pub, body, is.signedHeaders(is.secret, body, "p3")); r.Status != 404 {
		t.Fatalf("revoked: %d %s", r.Status, r.Body)
	}
}

func TestSecretRotationOverHTTP(t *testing.T) {
	s := newServer(t, false)
	is := newIngestSetup(t, s)
	body := []byte(`{"v":1}`)
	if r := is.post(is.pub, body, is.signedHeaders(is.secret, body, "r1")); r.Status != 202 {
		t.Fatalf("before rotation: %d", r.Status)
	}
	rot := is.c.mutate("POST", "/api/v1/organizations/"+is.org+"/ingestion-endpoints/"+is.id+"/rotate-secret", nil)
	newSecret := rot.json()["endpoint"].(map[string]any)["signingSecret"].(string)

	if r := is.post(is.pub, body, is.signedHeaders(is.secret, body, "r2")); r.Status != 401 {
		t.Fatalf("old secret after rotation: %d %s", r.Status, r.Body)
	}
	if r := is.post(is.pub, body, is.signedHeaders(newSecret, body, "r3")); r.Status != 202 {
		t.Fatalf("new secret: %d %s", r.Status, r.Body)
	}
}

func TestIngestionRateLimitOverHTTP(t *testing.T) {
	s := newServerWith(t, false, bytes.NewBuffer(nil), func(env *apptest.Env) *ingest.Service {
		cfg := env.IngestConfig
		cfg.PerEndpointPerMinute = 2
		cfg.PerIPPerMinute = 100
		return ingest.NewService(env.Pool, env.Box, cfg, env.Clock.Now, nil2(), nil)
	})
	is := newIngestSetup(t, s)
	for i := 0; i < 2; i++ {
		body := []byte(`{"i":` + strconv.Itoa(i) + `}`)
		if r := is.post(is.pub, body, is.signedHeaders(is.secret, body, "rl-"+strconv.Itoa(i))); r.Status != 202 {
			t.Fatalf("request %d: %d %s", i, r.Status, r.Body)
		}
	}
	body := []byte(`{"i":3}`)
	r := is.post(is.pub, body, is.signedHeaders(is.secret, body, "rl-3"))
	if r.Status != 429 || r.errorCode() != "rate_limited" {
		t.Fatalf("over limit: %d %s", r.Status, r.Body)
	}
	if secs, err := strconv.Atoi(r.Header.Get("Retry-After")); err != nil || secs < 1 || secs > 60 {
		t.Fatalf("Retry-After = %q", r.Header.Get("Retry-After"))
	}
	s.env.Clock.Advance(time.Minute + time.Second)
	if r := is.post(is.pub, body, is.signedHeaders(is.secret, body, "rl-4")); r.Status != 202 {
		t.Fatalf("after the window: %d %s", r.Status, r.Body)
	}
}

func TestLoginRateLimitOverHTTP(t *testing.T) {
	s := newServer(t, false)
	s.client(t).register("target@example.com")
	c := s.client(t)
	for i := 0; i < 10; i++ {
		if r := c.do("POST", "/api/v1/auth/login", map[string]string{"email": "target@example.com", "password": "wrong password " + strconv.Itoa(i)}); r.Status != 401 {
			t.Fatalf("attempt %d: %d", i, r.Status)
		}
	}
	r := c.do("POST", "/api/v1/auth/login", map[string]string{"email": "target@example.com", "password": apptest.Password})
	if r.Status != 429 || r.errorCode() != "rate_limited" || r.Header.Get("Retry-After") == "" {
		t.Fatalf("expected 429 with Retry-After: %d %s", r.Status, r.Body)
	}
	if c.cookie != nil {
		t.Fatal("a rate-limited login must not create a session")
	}
}

func TestNoSecretsInLogsOrErrors(t *testing.T) {
	var logs bytes.Buffer
	s := newServerWith(t, false, &logs, nil)
	is := newIngestSetup(t, s)
	body := []byte(`{"customer":"jane.doe@example.com","card":"4111-secret-payload"}`)
	headers := is.signedHeaders(is.secret, body, "log-1")
	signature := headers[ingest.HeaderSignature]

	is.post(is.pub, body, headers)                                                // success
	is.post(is.pub, body, is.signedHeaders("rfs_wrong-secret-value", body, "l2")) // signature failure
	is.post(is.pub, []byte(`{"customer":`), headers)                              // invalid json
	other := []byte(`{"card":"different-payload"}`)
	is.post(is.pub, other, is.signedHeaders(is.secret, other, "log-1")) // conflict
	is.c.mutate("POST", "/api/v1/organizations/"+is.org+"/ingestion-endpoints/"+is.id+"/rotate-secret", nil)
	is.c.do("POST", "/api/v1/auth/login", map[string]string{"email": "owner@example.com", "password": "wrong password value"})

	out := logs.String()
	for name, secret := range map[string]string{
		"signing secret": is.secret, "wrong secret": "rfs_wrong-secret-value", "signature": signature,
		"payload": "4111-secret-payload", "payload email": "jane.doe@example.com", "session token": is.c.cookie.Value,
		"csrf token": is.c.csrf, "encryption key": string(apptest.TestKey), "password": apptest.Password,
	} {
		if strings.Contains(out, secret) {
			t.Errorf("logs contain the %s", name)
		}
	}
	if strings.Contains(out, "rfs_") {
		t.Error("logs contain something that looks like a signing secret")
	}
}

func nil2() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
