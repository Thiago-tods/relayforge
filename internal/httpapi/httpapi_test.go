package httpapi_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Thiago-tods/relayforge/internal/app/apptest"
	"github.com/Thiago-tods/relayforge/internal/httpapi"
)

type server struct {
	env *apptest.Env
	url string
}

func newServer(t *testing.T, cookieSecure bool) *server {
	t.Helper()
	env := apptest.New(t)
	api := httpapi.New(env.Svc, slog.New(slog.NewTextHandler(io.Discard, nil)), cookieSecure)
	ts := httptest.NewServer(api.Handler())
	t.Cleanup(ts.Close)
	return &server{env: env, url: ts.URL}
}

// client is a minimal browser: it keeps the session cookie and the CSRF token.
type client struct {
	t      *testing.T
	s      *server
	cookie *http.Cookie
	csrf   string
}

func (s *server) client(t *testing.T) *client { return &client{t: t, s: s} }

type response struct {
	Status int
	Header http.Header
	Body   string
}

func (r response) json() map[string]any {
	var m map[string]any
	if err := json.Unmarshal([]byte(r.Body), &m); err != nil {
		panic("response is not a JSON object: " + r.Body)
	}
	return m
}

func (r response) errorCode() string {
	e, _ := r.json()["error"].(map[string]any)
	code, _ := e["code"].(string)
	return code
}

// do sends a request. header values are applied last, so tests can override defaults.
func (c *client) do(method, path string, body any, headers ...string) response {
	c.t.Helper()
	var rdr io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rdr = strings.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			c.t.Fatal(err)
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, c.s.url+path, rdr)
	if err != nil {
		c.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.cookie != nil {
		req.AddCookie(c.cookie)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		if headers[i+1] == "" {
			req.Header.Del(headers[i])
		} else {
			req.Header.Set(headers[i], headers[i+1])
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	for _, ck := range resp.Cookies() {
		if ck.Name == httpapi.SessionCookieName {
			if ck.MaxAge < 0 || ck.Value == "" {
				c.cookie = nil
			} else {
				c.cookie = ck
			}
		}
	}
	return response{Status: resp.StatusCode, Header: resp.Header, Body: string(raw)}
}

// mutate sends a state-changing request with the CSRF token.
func (c *client) mutate(method, path string, body any) response {
	c.t.Helper()
	return c.do(method, path, body, httpapi.CSRFHeader, c.csrf)
}

func (c *client) register(email string) response {
	c.t.Helper()
	res := c.do("POST", "/api/v1/auth/register", map[string]any{
		"email": email, "password": apptest.Password, "displayName": "Test User",
	})
	if res.Status != http.StatusCreated {
		c.t.Fatalf("register %s: %d %s", email, res.Status, res.Body)
	}
	c.csrf = res.json()["session"].(map[string]any)["csrfToken"].(string)
	return res
}

func (c *client) orgID(res response) string {
	return res.json()["organization"].(map[string]any)["id"].(string)
}

func (c *client) userID(res response) string {
	return res.json()["user"].(map[string]any)["id"].(string)
}

func TestRegisterSetsHardenedSessionCookieAndHidesSecrets(t *testing.T) {
	for _, secure := range []bool{false, true} {
		s := newServer(t, secure)
		c := s.client(t)
		res := c.register("cookie@example.com")

		var ck *http.Cookie
		for _, x := range (&http.Response{Header: res.Header}).Cookies() {
			if x.Name == httpapi.SessionCookieName {
				ck = x
			}
		}
		if ck == nil {
			t.Fatal("no session cookie set")
		}
		if !ck.HttpOnly || ck.SameSite != http.SameSiteLaxMode || ck.Path != "/api" || ck.Secure != secure {
			t.Fatalf("cookie attributes (secure=%v): %+v", secure, ck)
		}
		if ck.MaxAge < int((12*time.Hour-time.Minute).Seconds()) || ck.MaxAge > int((12*time.Hour).Seconds()) {
			t.Fatalf("cookie Max-Age = %d, want about 12h", ck.MaxAge)
		}
		if ck.Expires.IsZero() {
			t.Fatal("cookie must carry an explicit expiry")
		}
		for _, banned := range []string{"password", "hash", "argon2", ck.Value} {
			if strings.Contains(strings.ToLower(res.Body), strings.ToLower(banned)) {
				t.Fatalf("response body leaks %q: %s", banned, res.Body)
			}
		}
		if res.Header.Get("Cache-Control") != "no-store" {
			t.Fatal("API responses must not be cached")
		}
	}
}

func TestRegisterAndLoginAndMe(t *testing.T) {
	s := newServer(t, false)
	c := s.client(t)
	reg := c.register("flow@example.com")
	if reg.json()["organization"] == nil {
		t.Fatal("registration must return the initial organization")
	}

	me := c.do("GET", "/api/v1/auth/me", nil)
	if me.Status != 200 || me.json()["user"].(map[string]any)["email"] != "flow@example.com" {
		t.Fatalf("me: %d %s", me.Status, me.Body)
	}
	if orgs := me.json()["organizations"].([]any); len(orgs) != 1 || orgs[0].(map[string]any)["role"] != "OWNER" {
		t.Fatalf("me organizations: %s", me.Body)
	}
	if sess := c.do("GET", "/api/v1/auth/session", nil); sess.Status != 200 || strings.Contains(sess.Body, "csrfToken") {
		t.Fatalf("session endpoint: %d %s", sess.Status, sess.Body)
	}

	c2 := s.client(t)
	login := c2.do("POST", "/api/v1/auth/login", map[string]string{"email": "FLOW@example.com", "password": apptest.Password})
	if login.Status != 200 || c2.cookie == nil || login.json()["session"].(map[string]any)["csrfToken"] == "" {
		t.Fatalf("login: %d %s", login.Status, login.Body)
	}
}

func TestLoginFailuresAreGenericAndIdentical(t *testing.T) {
	s := newServer(t, false)
	s.client(t).register("exists@example.com")
	c := s.client(t)

	wrongPassword := c.do("POST", "/api/v1/auth/login", map[string]string{"email": "exists@example.com", "password": "definitely wrong password"})
	unknownEmail := c.do("POST", "/api/v1/auth/login", map[string]string{"email": "ghost@example.com", "password": apptest.Password})
	if wrongPassword.Status != 401 || unknownEmail.Status != 401 || wrongPassword.Body != unknownEmail.Body {
		t.Fatalf("responses must be identical: %d %q vs %d %q", wrongPassword.Status, wrongPassword.Body, unknownEmail.Status, unknownEmail.Body)
	}
	if wrongPassword.errorCode() != "invalid_credentials" || c.cookie != nil {
		t.Fatal("failed login must not set a cookie")
	}
}

func TestRequestValidationAndErrorFormat(t *testing.T) {
	s := newServer(t, false)
	c := s.client(t)
	valid := `{"email":"v@example.com","password":"correct horse battery staple","displayName":"V"}`

	cases := []struct {
		name    string
		path    string
		body    any
		headers []string
		status  int
		code    string
	}{
		{"malformed json", "/api/v1/auth/register", `{"email":`, nil, 400, "invalid_json"},
		{"unknown field", "/api/v1/auth/register", `{"email":"a@b.co","password":"x","displayName":"V","role":"OWNER"}`, nil, 400, "invalid_json"},
		{"trailing data", "/api/v1/auth/register", valid + `{"x":1}`, nil, 400, "invalid_json"},
		{"wrong content type", "/api/v1/auth/register", valid, []string{"Content-Type", "text/plain"}, 415, "unsupported_media_type"},
		{"missing content type", "/api/v1/auth/register", valid, []string{"Content-Type", ""}, 415, "unsupported_media_type"},
		{"oversized body", "/api/v1/auth/register", `{"email":"` + strings.Repeat("a", 70<<10) + `"}`, nil, 413, "request_too_large"},
		{"weak password", "/api/v1/auth/register", `{"email":"a@b.co","password":"short","displayName":"V"}`, nil, 400, "validation_failed"},
		{"bad email", "/api/v1/auth/register", `{"email":"nope","password":"correct horse battery staple","displayName":"V"}`, nil, 400, "validation_failed"},
	}
	for _, tc := range cases {
		res := c.do("POST", tc.path, tc.body, tc.headers...)
		if res.Status != tc.status || res.errorCode() != tc.code {
			t.Errorf("%s: got %d %s, want %d %s", tc.name, res.Status, res.Body, tc.status, tc.code)
		}
		if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("%s: error content type %q", tc.name, ct)
		}
		if msg := res.json()["error"].(map[string]any)["message"].(string); msg == "" || strings.Contains(strings.ToLower(msg), "sql") {
			t.Errorf("%s: unsafe or empty message %q", tc.name, msg)
		}
	}
	if n := s.env.Count(t, "SELECT count(*) FROM users"); n != 0 {
		t.Fatalf("rejected requests created %d users", n)
	}
	if res := c.do("GET", "/api/v1/nope", nil); res.Status != 404 || res.errorCode() != "not_found" {
		t.Errorf("unknown route: %d %s", res.Status, res.Body)
	}
}

func TestDuplicateRegistrationIsAConflict(t *testing.T) {
	s := newServer(t, false)
	s.client(t).register("dup@example.com")
	res := s.client(t).do("POST", "/api/v1/auth/register", map[string]string{
		"email": "DUP@example.com", "password": apptest.Password, "displayName": "Again",
	})
	if res.Status != 409 || res.errorCode() != "email_taken" {
		t.Fatalf("got %d %s", res.Status, res.Body)
	}
}

func TestAuthenticationMiddlewareRejectsInvalidSessions(t *testing.T) {
	s := newServer(t, false)
	c := s.client(t)
	c.register("mw@example.com")
	valid := c.cookie

	check := func(name string, cookie *http.Cookie) {
		t.Helper()
		other := s.client(t)
		other.cookie = cookie
		res := other.do("GET", "/api/v1/auth/me", nil)
		if res.Status != 401 || res.errorCode() != "unauthenticated" {
			t.Errorf("%s: got %d %s", name, res.Status, res.Body)
		}
	}
	check("missing cookie", nil)
	check("empty cookie", &http.Cookie{Name: httpapi.SessionCookieName, Value: ""})
	check("garbage cookie", &http.Cookie{Name: httpapi.SessionCookieName, Value: "garbage"})
	check("unknown well-formed token", &http.Cookie{Name: httpapi.SessionCookieName, Value: strings.Repeat("A", 43)})
	check("tampered token", &http.Cookie{Name: httpapi.SessionCookieName, Value: valid.Value[:42] + "A"})

	// Protected endpoints all refuse anonymous callers.
	for _, ep := range [][2]string{
		{"GET", "/api/v1/organizations"}, {"POST", "/api/v1/organizations"}, {"GET", "/api/v1/auth/session"},
		{"POST", "/api/v1/auth/csrf"}, {"POST", "/api/v1/invitations/accept"},
		{"GET", "/api/v1/organizations/00000000-0000-4000-8000-000000000000/members"},
	} {
		if res := s.client(t).do(ep[0], ep[1], nil); res.Status != 401 {
			t.Errorf("%s %s without session: %d", ep[0], ep[1], res.Status)
		}
	}

	// Expired.
	s.env.Clock.Advance(13 * time.Hour)
	check("expired session", valid)
}

func TestDisabledUserIsRejected(t *testing.T) {
	s := newServer(t, false)
	c := s.client(t)
	c.register("disabled@example.com")
	s.env.Exec(t, "UPDATE users SET status = 'DISABLED'")
	if res := c.do("GET", "/api/v1/auth/me", nil); res.Status != 401 {
		t.Fatalf("got %d", res.Status)
	}
}

func TestCSRFProtection(t *testing.T) {
	s := newServer(t, false)
	c := s.client(t)
	c.register("csrf@example.com")
	other := s.client(t)
	other.register("csrf-other@example.com")
	body := map[string]string{"name": "New Org"}

	// Reads are not subject to CSRF.
	if res := c.do("GET", "/api/v1/organizations", nil); res.Status != 200 {
		t.Fatalf("GET without token: %d", res.Status)
	}
	for name, token := range map[string]string{
		"missing":                "",
		"garbage":                "garbage",
		"well-formed but wrong":  strings.Repeat("A", 43),
		"token of other session": other.csrf,
		"session token":          c.cookie.Value,
	} {
		res := c.do("POST", "/api/v1/organizations", body, httpapi.CSRFHeader, token)
		if res.Status != 403 || res.errorCode() != "csrf_invalid" {
			t.Errorf("%s: got %d %s", name, res.Status, res.Body)
		}
	}
	if n := s.env.Count(t, "SELECT count(*) FROM organizations WHERE name = 'New Org'"); n != 0 {
		t.Fatal("a request rejected by CSRF protection still created data")
	}
	if res := c.mutate("POST", "/api/v1/organizations", body); res.Status != 201 {
		t.Fatalf("valid token rejected: %d %s", res.Status, res.Body)
	}
	// CSRF is checked after authentication: anonymous callers get 401, not 403.
	if res := s.client(t).do("POST", "/api/v1/organizations", body, httpapi.CSRFHeader, c.csrf); res.Status != 401 {
		t.Fatalf("anonymous mutation: %d", res.Status)
	}
}

func TestCSRFTokenRotationRequiresSameOriginClientHeader(t *testing.T) {
	s := newServer(t, false)
	c := s.client(t)
	c.register("rotate@example.com")
	old := c.csrf

	if res := c.do("POST", "/api/v1/auth/csrf", nil); res.Status != 403 {
		t.Fatalf("rotation without client header: %d", res.Status)
	}
	if res := c.do("POST", "/api/v1/auth/csrf", nil, "X-Requested-With", "XMLHttpRequest", "Origin", "https://evil.example"); res.Status != 403 {
		t.Fatalf("rotation with foreign Origin: %d", res.Status)
	}
	res := c.do("POST", "/api/v1/auth/csrf", nil, "X-Requested-With", "XMLHttpRequest")
	if res.Status != 200 {
		t.Fatalf("rotation: %d %s", res.Status, res.Body)
	}
	c.csrf = res.json()["session"].(map[string]any)["csrfToken"].(string)
	if c.csrf == old {
		t.Fatal("rotation must issue a new token")
	}
	if r := c.do("POST", "/api/v1/organizations", map[string]string{"name": "X1"}, httpapi.CSRFHeader, old); r.Status != 403 {
		t.Fatalf("old token still accepted: %d", r.Status)
	}
	if r := c.mutate("POST", "/api/v1/organizations", map[string]string{"name": "X2"}); r.Status != 201 {
		t.Fatalf("new token rejected: %d %s", r.Status, r.Body)
	}
}

func TestLogout(t *testing.T) {
	s := newServer(t, false)
	c := s.client(t)
	c.register("bye@example.com")
	session := c.cookie

	if res := c.do("POST", "/api/v1/auth/logout", nil); res.Status != 403 || res.errorCode() != "csrf_invalid" {
		t.Fatalf("logout without CSRF: %d %s", res.Status, res.Body)
	}
	if res := c.do("GET", "/api/v1/auth/me", nil); res.Status != 200 {
		t.Fatal("a rejected logout must not revoke the session")
	}

	res := c.mutate("POST", "/api/v1/auth/logout", nil)
	if res.Status != 204 {
		t.Fatalf("logout: %d %s", res.Status, res.Body)
	}
	if c.cookie != nil {
		t.Fatal("logout must clear the cookie")
	}
	// The old cookie is dead server-side even if a client keeps it.
	stale := s.client(t)
	stale.cookie = session
	if r := stale.do("GET", "/api/v1/auth/me", nil); r.Status != 401 {
		t.Fatalf("revoked session accepted: %d", r.Status)
	}
	// Safe to retry, with or without the stale cookie.
	if r := stale.do("POST", "/api/v1/auth/logout", nil); r.Status != 204 {
		t.Fatalf("repeated logout: %d", r.Status)
	}
	if r := s.client(t).do("POST", "/api/v1/auth/logout", nil); r.Status != 204 {
		t.Fatalf("anonymous logout: %d", r.Status)
	}
}

func TestOrganizationLifecycleAndTenantIsolationOverHTTP(t *testing.T) {
	s := newServer(t, false)
	alice, bob := s.client(t), s.client(t)
	aliceReg := alice.register("alice@example.com")
	bob.register("bob@example.com")
	aliceOrg := alice.orgID(aliceReg)

	create := alice.mutate("POST", "/api/v1/organizations", map[string]string{"name": "Second", "slug": "alice-second"})
	if create.Status != 201 {
		t.Fatalf("create: %d %s", create.Status, create.Body)
	}
	if dup := bob.mutate("POST", "/api/v1/organizations", map[string]string{"name": "X", "slug": "alice-second"}); dup.Status != 409 || dup.errorCode() != "slug_taken" {
		t.Fatalf("duplicate slug: %d %s", dup.Status, dup.Body)
	}
	if bad := alice.mutate("POST", "/api/v1/organizations", map[string]string{"name": "X", "slug": "../etc"}); bad.Status != 400 {
		t.Fatalf("path-like slug: %d", bad.Status)
	}
	list := alice.do("GET", "/api/v1/organizations", nil)
	if items := list.json()["items"].([]any); len(items) != 2 {
		t.Fatalf("alice sees %d organizations, want 2", len(items))
	}
	if items := bob.do("GET", "/api/v1/organizations", nil).json()["items"].([]any); len(items) != 1 {
		t.Fatalf("bob sees %d organizations, want 1", len(items))
	}

	missing := "00000000-0000-4000-8000-000000000000"
	foreign := bob.do("GET", "/api/v1/organizations/"+aliceOrg, nil)
	absent := bob.do("GET", "/api/v1/organizations/"+missing, nil)
	malformed := bob.do("GET", "/api/v1/organizations/not-a-uuid", nil)
	for name, r := range map[string]response{"foreign": foreign, "absent": absent, "malformed": malformed} {
		if r.Status != 404 || r.errorCode() != "not_found" {
			t.Errorf("%s: %d %s", name, r.Status, r.Body)
		}
	}
	if foreign.Body != absent.Body {
		t.Fatal("foreign and absent organizations must be indistinguishable")
	}
	for _, path := range []string{"/members", "/invitations", "/audit-events"} {
		if r := bob.do("GET", "/api/v1/organizations/"+aliceOrg+path, nil); r.Status != 404 {
			t.Errorf("bob GET %s: %d", path, r.Status)
		}
	}
	if r := bob.mutate("PATCH", "/api/v1/organizations/"+aliceOrg, map[string]string{"name": "pwned"}); r.Status != 404 {
		t.Fatalf("cross-tenant PATCH: %d", r.Status)
	}

	patch := alice.mutate("PATCH", "/api/v1/organizations/"+aliceOrg, map[string]string{"name": "Renamed"})
	if patch.Status != 200 || patch.json()["organization"].(map[string]any)["name"] != "Renamed" {
		t.Fatalf("patch: %d %s", patch.Status, patch.Body)
	}
	if r := alice.mutate("PATCH", "/api/v1/organizations/"+aliceOrg, map[string]string{}); r.Status != 400 {
		t.Fatalf("empty patch: %d", r.Status)
	}
	if r := alice.mutate("DELETE", "/api/v1/organizations/"+aliceOrg, nil); r.Status != 404 && r.Status != 405 {
		t.Fatalf("organization deletion must not exist: %d", r.Status)
	}
	if got := alice.do("GET", "/api/v1/organizations/"+aliceOrg, nil); got.Status != 200 {
		t.Fatalf("organization must survive a DELETE attempt: %d", got.Status)
	}
}

func TestInvitationFlowRBACAndOwnershipOverHTTP(t *testing.T) {
	s := newServer(t, false)
	owner, dev, viewer := s.client(t), s.client(t), s.client(t)
	ownerReg := owner.register("owner@example.com")
	devReg := dev.register("dev@example.com")
	viewerReg := viewer.register("viewer@example.com")
	org := owner.orgID(ownerReg)
	base := "/api/v1/organizations/" + org

	inv := owner.mutate("POST", base+"/invitations", map[string]string{"email": "dev@example.com", "role": "DEVELOPER"})
	if inv.Status != 201 {
		t.Fatalf("invite: %d %s", inv.Status, inv.Body)
	}
	token := inv.json()["token"].(string)
	if len(token) != 43 {
		t.Fatalf("token = %q", token)
	}
	if strings.Contains(owner.do("GET", base+"/invitations", nil).Body, token) {
		t.Fatal("the raw token must never be listed again")
	}
	if r := owner.mutate("POST", base+"/invitations", map[string]string{"email": "x@example.com", "role": "OWNER"}); r.Status != 403 {
		t.Fatalf("inviting an OWNER: %d", r.Status)
	}
	if r := owner.mutate("POST", base+"/invitations", map[string]string{"email": "x@example.com", "role": "WIZARD"}); r.Status != 400 {
		t.Fatalf("invalid role: %d", r.Status)
	}

	// The wrong user cannot use the token; the right one can, once.
	if r := viewer.mutate("POST", "/api/v1/invitations/accept", map[string]string{"token": token}); r.Status != 400 || r.errorCode() != "invalid_invitation" {
		t.Fatalf("wrong recipient: %d %s", r.Status, r.Body)
	}
	if r := dev.mutate("POST", "/api/v1/invitations/accept", map[string]string{"token": token}); r.Status != 200 {
		t.Fatalf("accept: %d %s", r.Status, r.Body)
	}
	if r := dev.mutate("POST", "/api/v1/invitations/accept", map[string]string{"token": token}); r.Status != 400 {
		t.Fatalf("reuse: %d", r.Status)
	}
	owner.mutate("POST", base+"/invitations", map[string]string{"email": "viewer@example.com", "role": "VIEWER"})
	tok2 := owner.do("GET", base+"/invitations", nil).json()["items"].([]any)
	if len(tok2) != 1 {
		t.Fatalf("pending invitations = %d, want 1", len(tok2))
	}
	revoke := owner.mutate("DELETE", base+"/invitations/"+tok2[0].(map[string]any)["id"].(string), nil)
	if revoke.Status != 204 {
		t.Fatalf("revoke: %d %s", revoke.Status, revoke.Body)
	}

	// RBAC: a DEVELOPER can read but not administer.
	if r := dev.do("GET", base+"/members", nil); r.Status != 200 {
		t.Fatalf("developer members: %d", r.Status)
	}
	for name, r := range map[string]response{
		"update":   dev.mutate("PATCH", base, map[string]string{"name": "x"}),
		"invite":   dev.mutate("POST", base+"/invitations", map[string]string{"email": "q@example.com", "role": "VIEWER"}),
		"list inv": dev.do("GET", base+"/invitations", nil),
		"audit":    dev.do("GET", base+"/audit-events", nil),
		"transfer": dev.mutate("POST", base+"/transfer-ownership", map[string]string{"userId": c(devReg, dev)}),
	} {
		if r.Status != 403 || r.errorCode() != "forbidden" {
			t.Errorf("developer %s: %d %s", name, r.Status, r.Body)
		}
	}

	// Pagination.
	if r := owner.do("GET", base+"/members?limit=1&offset=0", nil); r.Status != 200 || len(r.json()["items"].([]any)) != 1 || r.json()["total"].(float64) != 2 {
		t.Fatalf("paged members: %d %s", r.Status, r.Body)
	}
	for _, q := range []string{"?limit=0", "?limit=101", "?limit=x", "?offset=-1", "?offset=x"} {
		if r := owner.do("GET", base+"/members"+q, nil); r.Status != 400 {
			t.Errorf("members%s: %d", q, r.Status)
		}
	}

	// Role change, then ownership transfer, then removal.
	devID := owner.userID(devReg)
	if r := owner.mutate("PATCH", base+"/members/"+devID, map[string]string{"role": "VIEWER"}); r.Status != 204 {
		t.Fatalf("change role: %d %s", r.Status, r.Body)
	}
	if r := owner.mutate("PATCH", base+"/members/"+devID, map[string]string{"role": "OWNER"}); r.Status != 409 || r.errorCode() != "use_ownership_transfer" {
		t.Fatalf("assigning OWNER: %d %s", r.Status, r.Body)
	}
	if r := owner.mutate("PATCH", base+"/members/"+devID, map[string]string{"role": "bogus"}); r.Status != 400 {
		t.Fatalf("bogus role: %d", r.Status)
	}
	if r := owner.mutate("POST", base+"/transfer-ownership", map[string]string{"userId": devID}); r.Status != 204 {
		t.Fatalf("transfer: %d %s", r.Status, r.Body)
	}
	if r := dev.do("GET", base+"/audit-events", nil); r.Status != 200 || len(r.json()["items"].([]any)) == 0 {
		t.Fatalf("new owner must read audit events: %d", r.Status)
	}
	if r := owner.mutate("POST", base+"/transfer-ownership", map[string]string{"userId": devID}); r.Status != 403 {
		t.Fatalf("former owner transfer: %d", r.Status)
	}
	if r := dev.mutate("DELETE", base+"/members/"+owner.userID(ownerReg), nil); r.Status != 204 {
		t.Fatalf("new owner removing the former owner (now ADMIN): %d %s", r.Status, r.Body)
	}
	if r := owner.do("GET", base, nil); r.Status != 404 {
		t.Fatalf("removed member keeps access: %d", r.Status)
	}
	_ = viewerReg
}

// c returns the user ID for transfer targets in table-driven checks.
func c(reg response, _ *client) string { return reg.json()["user"].(map[string]any)["id"].(string) }
