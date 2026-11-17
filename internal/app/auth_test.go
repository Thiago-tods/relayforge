package app_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Thiago-tods/relayforge/internal/app"
	"github.com/Thiago-tods/relayforge/internal/app/apptest"
	"github.com/Thiago-tods/relayforge/internal/identity"
	"github.com/Thiago-tods/relayforge/internal/platform/apperror"
	"github.com/Thiago-tods/relayforge/internal/ratelimit"
)

var bg = context.Background()

func register(t *testing.T, env *apptest.Env, in app.RegisterInput) (app.AuthResult, error) {
	t.Helper()
	res, _, err := env.Svc.Register(bg, in)
	return res, err
}

func validInput(email string) app.RegisterInput {
	return app.RegisterInput{Email: email, Password: apptest.Password, DisplayName: "Alice"}
}

func TestRegisterCreatesUserOrganizationOwnerSessionAndAudit(t *testing.T) {
	env := apptest.New(t)
	res, org, err := env.Svc.Register(bg, app.RegisterInput{
		Email: "  Alice@Example.COM ", Password: apptest.Password, DisplayName: "Alice",
		OrganizationName: "Acme Corp",
	})
	if err != nil {
		t.Fatal(err)
	}
	if org.Slug != "acme-corp" || org.Name != "Acme Corp" {
		t.Fatalf("unexpected organization: %+v", org)
	}
	if res.User.NormalizedEmail != "alice@example.com" || res.RawSessionToken == "" || res.CSRFToken == "" {
		t.Fatalf("unexpected result: %+v", res)
	}
	if n := env.Count(t, "SELECT count(*) FROM memberships WHERE user_id = $1 AND organization_id = $2 AND role = 'OWNER' AND status = 'ACTIVE'", res.User.ID, org.ID); n != 1 {
		t.Fatalf("expected one OWNER membership, got %d", n)
	}
	if n := env.Count(t, "SELECT count(*) FROM audit_events WHERE actor_user_id = $1 AND action IN ('user.registered', 'organization.created')", res.User.ID); n != 2 {
		t.Fatalf("expected user.registered and organization.created audit events, got %d", n)
	}
	// Password and tokens are stored only as hashes.
	if n := env.Count(t, "SELECT count(*) FROM users WHERE password_hash LIKE '$argon2id$%' AND password_hash NOT LIKE '%' || $1 || '%'", apptest.Password); n != 1 {
		t.Fatal("password must be stored as an Argon2id hash")
	}
	if n := env.Count(t, "SELECT count(*) FROM sessions WHERE token_hash = $1", identity.HashToken(res.RawSessionToken)); n != 1 {
		t.Fatal("session must be stored by token hash")
	}
}

func TestRegisterDefaultsOrganizationAndDerivesUniqueSlugs(t *testing.T) {
	env := apptest.New(t)
	a, orgA, err := env.Svc.Register(bg, app.RegisterInput{Email: "a@example.com", Password: apptest.Password, DisplayName: "Sam", OrganizationName: "Same Name"})
	if err != nil {
		t.Fatal(err)
	}
	_, orgB, err := env.Svc.Register(bg, app.RegisterInput{Email: "b@example.com", Password: apptest.Password, DisplayName: "Sam", OrganizationName: "Same Name"})
	if err != nil {
		t.Fatalf("derived slug collision must be resolved automatically: %v", err)
	}
	if orgA.Slug != "same-name" || orgB.Slug == orgA.Slug || !strings.HasPrefix(orgB.Slug, "same-name-") {
		t.Fatalf("slugs: %q and %q", orgA.Slug, orgB.Slug)
	}
	_ = a
}

func TestRegisterValidation(t *testing.T) {
	env := apptest.New(t)
	cases := map[string]app.RegisterInput{
		"bad email":      {Email: "not-an-email", Password: apptest.Password, DisplayName: "A"},
		"short password": {Email: "a@example.com", Password: "short", DisplayName: "A"},
		"blank password": {Email: "a@example.com", Password: strings.Repeat(" ", 20), DisplayName: "A"},
		"no name":        {Email: "a@example.com", Password: apptest.Password, DisplayName: "  "},
		"long name":      {Email: "a@example.com", Password: apptest.Password, DisplayName: strings.Repeat("n", 101)},
		"bad slug":       {Email: "a@example.com", Password: apptest.Password, DisplayName: "A", OrganizationSlug: "../etc"},
		"bad org name":   {Email: "a@example.com", Password: apptest.Password, DisplayName: "A", OrganizationName: "x\x00y"},
	}
	for name, in := range cases {
		_, err := register(t, env, in)
		var ae *apperror.Error
		if !errors.As(err, &ae) || ae.Status != 400 || ae.Code != "validation_failed" {
			t.Errorf("%s: got %v, want 400 validation_failed", name, err)
		}
	}
	if n := env.Count(t, "SELECT count(*) FROM users"); n != 0 {
		t.Fatalf("invalid registrations created %d users", n)
	}
}

func TestRegisterDuplicateEmailIsCaseInsensitiveAndLeavesNoPartialData(t *testing.T) {
	env := apptest.New(t)
	if _, err := register(t, env, validInput("dup@example.com")); err != nil {
		t.Fatal(err)
	}
	users := env.Count(t, "SELECT count(*) FROM users")
	orgs := env.Count(t, "SELECT count(*) FROM organizations")
	sessions := env.Count(t, "SELECT count(*) FROM sessions")

	_, err := register(t, env, validInput("DUP@Example.com"))
	if !errors.Is(err, app.ErrEmailTaken) {
		t.Fatalf("got %v, want ErrEmailTaken", err)
	}
	if env.Count(t, "SELECT count(*) FROM users") != users || env.Count(t, "SELECT count(*) FROM organizations") != orgs ||
		env.Count(t, "SELECT count(*) FROM sessions") != sessions {
		t.Fatal("failed registration left partial data")
	}
}

func TestRegisterRollsBackEverythingWhenALaterStepFails(t *testing.T) {
	env := apptest.New(t)
	first := validInput("first@example.com")
	first.OrganizationSlug = "taken-slug"
	if _, err := register(t, env, first); err != nil {
		t.Fatal(err)
	}
	users := env.Count(t, "SELECT count(*) FROM users")
	audits := env.Count(t, "SELECT count(*) FROM audit_events")

	// The user insert succeeds, then the explicit slug collides: the user must be rolled back.
	second := validInput("second@example.com")
	second.OrganizationSlug = "taken-slug"
	if _, err := register(t, env, second); !errors.Is(err, app.ErrSlugTaken) {
		t.Fatalf("got %v, want ErrSlugTaken", err)
	}
	if env.Count(t, "SELECT count(*) FROM users") != users {
		t.Fatal("user row survived a failed registration")
	}
	if env.Count(t, "SELECT count(*) FROM users WHERE normalized_email = 'second@example.com'") != 0 {
		t.Fatal("second user must not exist")
	}
	if env.Count(t, "SELECT count(*) FROM audit_events") != audits {
		t.Fatal("audit events survived a failed registration")
	}
}

func TestLoginSuccessCreatesNewSessionAndAudit(t *testing.T) {
	env := apptest.New(t)
	reg, _ := register(t, env, validInput("login@example.com"))

	res, err := env.Svc.Login(bg, " LOGIN@example.com ", apptest.Password)
	if err != nil {
		t.Fatal(err)
	}
	if res.RawSessionToken == reg.RawSessionToken || res.CSRFToken == "" || res.User.ID != reg.User.ID {
		t.Fatalf("login must create a distinct session: %+v", res)
	}
	if want := env.Clock.Now().Add(12 * time.Hour); res.ExpiresAt.Sub(want).Abs() > time.Second {
		t.Fatalf("session expiry %v, want about %v (12h absolute)", res.ExpiresAt, want)
	}
	if n := env.Count(t, "SELECT count(*) FROM audit_events WHERE action = 'user.login.succeeded' AND actor_user_id = $1", reg.User.ID); n != 1 {
		t.Fatalf("login audit events = %d", n)
	}
}

func TestLoginFailuresAreIndistinguishable(t *testing.T) {
	env := apptest.New(t)
	reg, _ := register(t, env, validInput("real@example.com"))
	disabled, _ := register(t, env, validInput("disabled@example.com"))
	env.Exec(t, "UPDATE users SET status = 'DISABLED' WHERE id = $1", disabled.User.ID)
	_ = reg

	attempts := []struct{ name, email, password string }{
		{"wrong password", "real@example.com", "wrong password value"},
		{"unknown email", "ghost@example.com", apptest.Password},
		{"malformed email", "not-an-email", apptest.Password},
		{"empty", "", ""},
		{"disabled account with right password", "disabled@example.com", apptest.Password},
		{"oversized password", "real@example.com", strings.Repeat("x", 5000)},
	}
	var first *apperror.Error
	for _, a := range attempts {
		_, err := env.Svc.Login(bg, a.email, a.password)
		var ae *apperror.Error
		if !errors.As(err, &ae) || ae.Status != 401 || ae.Code != "invalid_credentials" {
			t.Fatalf("%s: got %v, want 401 invalid_credentials", a.name, err)
		}
		if first == nil {
			first = ae
		} else if ae.Message != first.Message || ae.Code != first.Code || ae.Status != first.Status {
			t.Fatalf("%s: response differs from the other failures", a.name)
		}
	}
	// Failed logins must not create sessions.
	if n := env.Count(t, "SELECT count(*) FROM sessions"); n != 2 { // only the two registrations
		t.Fatalf("sessions = %d, want 2", n)
	}
}

func TestLoginRateLimitIsAtomicUnderConcurrentFailures(t *testing.T) {
	env := apptest.New(t)
	if _, err := register(t, env, validInput("concurrent-login@example.com")); err != nil {
		t.Fatal(err)
	}

	const attempts = 20
	var wg sync.WaitGroup
	var mu sync.Mutex
	unauthorized, limited, unexpected := 0, 0, 0
	for range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := env.Svc.LoginFromIP(bg, "203.0.113.10", "concurrent-login@example.com", "incorrect password")
			var ae *apperror.Error
			if !errors.As(err, &ae) {
				mu.Lock()
				unexpected++
				mu.Unlock()
				return
			}
			mu.Lock()
			defer mu.Unlock()
			switch ae.Status {
			case 401:
				if ae.Code == "invalid_credentials" {
					unauthorized++
				} else {
					unexpected++
				}
			case 429:
				if ae.Code == "rate_limited" {
					limited++
				} else {
					unexpected++
				}
			default:
				unexpected++
			}
		}()
	}
	wg.Wait()

	if unauthorized != 10 || limited != attempts-10 || unexpected != 0 {
		t.Fatalf("concurrent login outcomes: unauthorized=%d limited=%d unexpected=%d; want 10, 10, 0",
			unauthorized, limited, unexpected)
	}
	accountBucket := ratelimit.Bucket("login-account", "concurrent-login@example.com")
	ipBucket := ratelimit.Bucket("login-ip", "203.0.113.10")
	for _, bucket := range []string{accountBucket, ipBucket} {
		if got := env.Count(t, "SELECT count FROM rate_limit_counters WHERE bucket = $1", bucket); got != 10 {
			t.Errorf("bucket %q count = %d, want exactly 10", bucket, got)
		}
	}
}

func TestSuccessfulLoginReleasesItsRateLimitReservation(t *testing.T) {
	env := apptest.New(t)
	if _, err := register(t, env, validInput("reservation@example.com")); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 9; i++ {
		_, err := env.Svc.LoginFromIP(bg, "203.0.113.11", "reservation@example.com", "wrong password")
		var ae *apperror.Error
		if !errors.As(err, &ae) || ae.Status != 401 || ae.Code != "invalid_credentials" {
			t.Fatalf("failed login %d: got %v, want invalid_credentials", i+1, err)
		}
	}
	if _, err := env.Svc.LoginFromIP(bg, "203.0.113.11", "reservation@example.com", apptest.Password); err != nil {
		t.Fatalf("valid credentials should succeed at the limit boundary: %v", err)
	}

	// The successful attempt must release its reservation, leaving the nine
	// actual failures counted. One more failure is allowed; the next is blocked.
	_, err := env.Svc.LoginFromIP(bg, "203.0.113.11", "reservation@example.com", "wrong password")
	var ae *apperror.Error
	if !errors.As(err, &ae) || ae.Status != 401 {
		t.Fatalf("tenth failed attempt: got %v, want invalid_credentials", err)
	}
	_, err = env.Svc.LoginFromIP(bg, "203.0.113.11", "reservation@example.com", "wrong password")
	if !errors.As(err, &ae) || ae.Status != 429 || ae.Code != "rate_limited" {
		t.Fatalf("eleventh failed attempt: got %v, want rate_limited", err)
	}

	accountBucket := ratelimit.Bucket("login-account", "reservation@example.com")
	if got := env.Count(t, "SELECT count FROM rate_limit_counters WHERE bucket = $1", accountBucket); got != 10 {
		t.Fatalf("account failure count = %d, want 10", got)
	}
}

func TestConcurrentSuccessfulLoginsReleaseIndependentReservations(t *testing.T) {
	env := apptest.New(t)
	if _, err := register(t, env, validInput("parallel-success@example.com")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if _, err := env.Svc.LoginFromIP(bg, "203.0.113.12", "parallel-success@example.com", "wrong password"); err == nil {
			t.Fatal("wrong password unexpectedly succeeded")
		}
	}

	// Five failed attempts have consumed half of the account/IP budget.
	// Exactly five concurrent reservations remain available in this window.
	const attempts = 5
	var wg sync.WaitGroup
	var mu sync.Mutex
	successes, unexpected := 0
	for range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := env.Svc.LoginFromIP(bg, "203.0.113.12", "parallel-success@example.com", apptest.Password)
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				successes++
			} else {
				unexpected++
			}
		}()
	}
	wg.Wait()
	if successes != attempts || unexpected != 0 {
		t.Fatalf("successful logins=%d unexpected failures=%d; want %d, 0", successes, unexpected, attempts)
	}

	accountBucket := ratelimit.Bucket("login-account", "parallel-success@example.com")
	ipBucket := ratelimit.Bucket("login-ip", "203.0.113.12")
	for _, bucket := range []string{accountBucket, ipBucket} {
		if got := env.Count(t, "SELECT count FROM rate_limit_counters WHERE bucket = $1", bucket); got != 5 {
			t.Errorf("bucket %q count = %d after successful logins, want 5 actual failures", bucket, got)
		}
	}
}

func TestAuthenticateAcceptsOnlyLiveSessions(t *testing.T) {
	env := apptest.New(t)
	u := env.Register(t, "session@example.com")

	if _, err := env.Svc.Authenticate(bg, u.Auth.RawSessionToken); err != nil {
		t.Fatalf("fresh session rejected: %v", err)
	}
	for _, bad := range []string{"", "garbage", strings.Repeat("A", 43), u.Auth.RawSessionToken + "x"} {
		if _, err := env.Svc.Authenticate(bg, bad); !errors.Is(err, app.ErrUnauthenticated) {
			t.Errorf("token %q: got %v, want ErrUnauthenticated", bad, err)
		}
	}
	// The CSRF token is not a session token.
	if _, err := env.Svc.Authenticate(bg, u.Auth.CSRFToken); !errors.Is(err, app.ErrUnauthenticated) {
		t.Error("CSRF token must not authenticate")
	}
}

func TestSessionExpiresAtAbsoluteLifetime(t *testing.T) {
	env := apptest.New(t)
	u := env.Register(t, "expiry@example.com")

	env.Clock.Advance(11*time.Hour + 59*time.Minute)
	if _, err := env.Svc.Authenticate(bg, u.Auth.RawSessionToken); err != nil {
		t.Fatalf("session must still be valid before 12h: %v", err)
	}
	env.Clock.Advance(2 * time.Minute)
	if _, err := env.Svc.Authenticate(bg, u.Auth.RawSessionToken); !errors.Is(err, app.ErrUnauthenticated) {
		t.Fatalf("session must expire after 12h, got %v", err)
	}
}

func TestSessionDoesNotSlide(t *testing.T) {
	env := apptest.New(t)
	u := env.Register(t, "nosliding@example.com")
	for range 11 { // keep using the session every hour
		env.Clock.Advance(time.Hour)
		if _, err := env.Svc.Authenticate(bg, u.Auth.RawSessionToken); err != nil {
			t.Fatal(err)
		}
	}
	env.Clock.Advance(90 * time.Minute) // 12h30 after creation
	if _, err := env.Svc.Authenticate(bg, u.Auth.RawSessionToken); !errors.Is(err, app.ErrUnauthenticated) {
		t.Fatal("activity must not extend the absolute session lifetime")
	}
}

func TestLogoutRevokesSessionImmediatelyAndIsRepeatable(t *testing.T) {
	env := apptest.New(t)
	u := env.Register(t, "logout@example.com")
	other, err := env.Svc.Login(bg, u.Email, apptest.Password)
	if err != nil {
		t.Fatal(err)
	}

	if err := env.Svc.Logout(bg, u.Principal); err != nil {
		t.Fatal(err)
	}
	if _, err := env.Svc.Authenticate(bg, u.Auth.RawSessionToken); !errors.Is(err, app.ErrUnauthenticated) {
		t.Fatal("revoked session must be rejected immediately")
	}
	if err := env.Svc.Logout(bg, u.Principal); err != nil {
		t.Fatalf("repeating logout must be safe: %v", err)
	}
	// Only the current session is revoked.
	if _, err := env.Svc.Authenticate(bg, other.RawSessionToken); err != nil {
		t.Fatalf("other sessions must survive: %v", err)
	}
	if n := env.Count(t, "SELECT count(*) FROM audit_events WHERE action = 'user.logout' AND actor_user_id = $1", u.Principal.User.ID); n < 1 {
		t.Fatal("logout must be audited")
	}
}

func TestDisabledUserIsRejectedByAuthenticate(t *testing.T) {
	env := apptest.New(t)
	u := env.Register(t, "willbedisabled@example.com")
	env.Exec(t, "UPDATE users SET status = 'DISABLED' WHERE id = $1", u.Principal.User.ID)
	if _, err := env.Svc.Authenticate(bg, u.Auth.RawSessionToken); !errors.Is(err, app.ErrUnauthenticated) {
		t.Fatalf("disabled user must be rejected, got %v", err)
	}
}

func TestPrincipalCSRFVerification(t *testing.T) {
	env := apptest.New(t)
	u := env.Register(t, "csrf@example.com")
	if !u.Principal.VerifyCSRF(u.Auth.CSRFToken) {
		t.Fatal("the issued CSRF token must verify")
	}
	for _, bad := range []string{"", "x", u.Auth.RawSessionToken, strings.Repeat("A", 43)} {
		if u.Principal.VerifyCSRF(bad) {
			t.Errorf("CSRF token %q must not verify", bad)
		}
	}
}

func TestRotateCSRFInvalidatesPreviousToken(t *testing.T) {
	env := apptest.New(t)
	u := env.Register(t, "rotate@example.com")
	fresh, err := env.Svc.RotateCSRF(bg, u.Principal)
	if err != nil {
		t.Fatal(err)
	}
	p := env.Principal(t, u.Auth)
	if p.VerifyCSRF(u.Auth.CSRFToken) {
		t.Fatal("old CSRF token must stop working after rotation")
	}
	if !p.VerifyCSRF(fresh) {
		t.Fatal("new CSRF token must verify")
	}
}

func TestSecretsAreNeverStoredRaw(t *testing.T) {
	env := apptest.New(t)
	u := env.Register(t, "secrets@example.com")
	for name, secret := range map[string]string{"session": u.Auth.RawSessionToken, "csrf": u.Auth.CSRFToken, "password": apptest.Password} {
		for _, table := range []string{"users", "sessions", "audit_events"} {
			n := env.Count(t, "SELECT count(*) FROM "+table+" t WHERE t::text LIKE '%' || $1 || '%'", secret)
			if n != 0 {
				t.Errorf("raw %s found in table %s", name, table)
			}
		}
	}
}
