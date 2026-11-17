package app_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Thiago-tods/relayforge/internal/app"
	"github.com/Thiago-tods/relayforge/internal/app/apptest"
	"github.com/Thiago-tods/relayforge/internal/audit"
	"github.com/Thiago-tods/relayforge/internal/ingest"
	"github.com/Thiago-tods/relayforge/internal/ratelimit"
	"github.com/Thiago-tods/relayforge/internal/tenancy"
)

func TestEndpointManagementLifecycleAndAudit(t *testing.T) {
	tm := newTeam(t)
	svc, org, owner := tm.env.Svc, tm.org.ID, tm.owner.Principal

	created, err := svc.CreateEndpoint(bg, owner, org, "  Billing events  ")
	if err != nil {
		t.Fatal(err)
	}
	ep := created.Endpoint
	if ep.Name != "Billing events" || ep.Status != ingest.StatusActive || !ingest.ValidPublicID(ep.PublicID) || ep.PublicID == ep.ID {
		t.Fatalf("unexpected endpoint: %+v", ep)
	}
	if created.SigningSecret == "" {
		t.Fatal("the creation response must carry the signing secret")
	}

	got, err := svc.GetEndpoint(bg, owner, org, ep.ID)
	if err != nil || got.ID != ep.ID {
		t.Fatalf("get: %v", err)
	}
	items, total, err := svc.ListEndpoints(bg, owner, org, 10, 0)
	if err != nil || total != 1 || len(items) != 1 {
		t.Fatalf("list: %d %v", total, err)
	}

	renamed, err := svc.UpdateEndpoint(bg, owner, org, ep.ID, "Payments")
	if err != nil || renamed.Name != "Payments" {
		t.Fatalf("rename: %v", err)
	}
	if paused, err := svc.PauseEndpoint(bg, owner, org, ep.ID); err != nil || paused.Status != ingest.StatusPaused {
		t.Fatalf("pause: %v", err)
	}
	if _, err := svc.PauseEndpoint(bg, owner, org, ep.ID); err != nil { // idempotent, no second audit event
		t.Fatalf("pause twice: %v", err)
	}
	if active, err := svc.ActivateEndpoint(bg, owner, org, ep.ID); err != nil || active.Status != ingest.StatusActive {
		t.Fatalf("activate: %v", err)
	}
	if _, err := svc.RotateSecret(bg, owner, org, ep.ID); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	revoked, err := svc.RevokeEndpoint(bg, owner, org, ep.ID)
	if err != nil || revoked.Status != ingest.StatusRevoked || revoked.RevokedAt == nil {
		t.Fatalf("revoke: %+v %v", revoked, err)
	}
	if _, err := svc.RevokeEndpoint(bg, owner, org, ep.ID); err != nil {
		t.Fatalf("revoking twice must be a no-op: %v", err)
	}
	// Revoked endpoints are immutable but remain visible for history.
	for name, op := range map[string]func() error{
		"rename":   func() error { _, err := svc.UpdateEndpoint(bg, owner, org, ep.ID, "x"); return err },
		"pause":    func() error { _, err := svc.PauseEndpoint(bg, owner, org, ep.ID); return err },
		"activate": func() error { _, err := svc.ActivateEndpoint(bg, owner, org, ep.ID); return err },
		"rotate":   func() error { _, err := svc.RotateSecret(bg, owner, org, ep.ID); return err },
	} {
		if err := op(); !errors.Is(err, app.ErrEndpointRevoked) {
			t.Errorf("%s on a revoked endpoint: %v", name, err)
		}
	}
	if _, total, _ := svc.ListEndpoints(bg, owner, org, 10, 0); total != 1 {
		t.Fatal("revoked endpoints must stay listed")
	}

	for action, want := range map[string]int{
		audit.EndpointCreated: 1, audit.EndpointUpdated: 1, audit.EndpointPaused: 1,
		audit.EndpointActivated: 1, audit.EndpointSecretRotated: 1, audit.EndpointRevoked: 1,
	} {
		if n := tm.env.Count(t, "SELECT count(*) FROM audit_events WHERE organization_id = $1 AND action = $2", org, action); n != want {
			t.Errorf("audit %s = %d, want %d", action, n, want)
		}
	}
	// No secrets, ciphertext, keys or signatures in the audit log.
	for _, secret := range []string{created.SigningSecret, string(apptest.TestKey), "rfs_"} {
		if n := tm.env.Count(t, "SELECT count(*) FROM audit_events t WHERE t::text LIKE '%' || $1 || '%'", secret); n != 0 {
			t.Errorf("audit log contains %q", secret[:4])
		}
	}
}

func TestEndpointNameValidation(t *testing.T) {
	tm := newTeam(t)
	for _, name := range []string{"", "   ", "bad\x00name", "line\nbreak", strings.Repeat("x", 101)} {
		if _, err := tm.env.Svc.CreateEndpoint(bg, tm.owner.Principal, tm.org.ID, name); err == nil {
			t.Errorf("name %q accepted", name)
		}
	}
}

func TestEndpointRBAC(t *testing.T) {
	type tc struct {
		role   string
		user   func(team) apptest.User
		create bool
		read   bool
		update bool
		pause  bool
		rotate bool
		revoke bool
	}
	cases := []tc{
		{"owner", func(t team) apptest.User { return t.owner }, true, true, true, true, true, true},
		{"admin", func(t team) apptest.User { return t.admin }, true, true, true, true, true, true},
		{"developer", func(t team) apptest.User { return t.developer }, true, true, true, true, false, false},
		{"viewer", func(t team) apptest.User { return t.viewer }, false, true, false, false, false, false},
	}
	for _, c := range cases {
		t.Run(c.role, func(t *testing.T) {
			tm := newTeam(t)
			actor := c.user(tm).Principal
			target, err := tm.env.Svc.CreateEndpoint(bg, tm.owner.Principal, tm.org.ID, "Seed")
			if err != nil {
				t.Fatal(err)
			}
			id := target.Endpoint.ID
			check := func(name string, allowed bool, err error) {
				t.Helper()
				if allowed && err != nil {
					t.Errorf("%s must be allowed: %v", name, err)
				}
				if !allowed && !errors.Is(err, app.ErrForbidden) {
					t.Errorf("%s must be forbidden, got %v", name, err)
				}
			}
			_, err = tm.env.Svc.CreateEndpoint(bg, actor, tm.org.ID, "Mine")
			check("create", c.create, err)
			_, _, err = tm.env.Svc.ListEndpoints(bg, actor, tm.org.ID, 10, 0)
			check("list", c.read, err)
			_, err = tm.env.Svc.GetEndpoint(bg, actor, tm.org.ID, id)
			check("get", c.read, err)
			_, err = tm.env.Svc.UpdateEndpoint(bg, actor, tm.org.ID, id, "Renamed")
			check("update", c.update, err)
			_, err = tm.env.Svc.PauseEndpoint(bg, actor, tm.org.ID, id)
			check("pause", c.pause, err)
			_, err = tm.env.Svc.ActivateEndpoint(bg, actor, tm.org.ID, id)
			check("activate", c.pause, err)
			_, err = tm.env.Svc.RotateSecret(bg, actor, tm.org.ID, id)
			check("rotate", c.rotate, err)
			_, err = tm.env.Svc.RevokeEndpoint(bg, actor, tm.org.ID, id)
			check("revoke", c.revoke, err)
		})
	}
}

func TestEndpointTenantIsolation(t *testing.T) {
	env := apptest.New(t)
	alice := env.Register(t, "alice@example.com")
	bob := env.Register(t, "bob@example.com")
	secret := env.CreateEndpoint(t, alice)
	aliceEP := secret.Endpoint

	missing := "00000000-0000-4000-8000-000000000000"
	ops := map[string]func(org, ep string) error{
		"create": func(org, _ string) error { _, err := env.Svc.CreateEndpoint(bg, bob.Principal, org, "x"); return err },
		"list": func(org, _ string) error {
			_, _, err := env.Svc.ListEndpoints(bg, bob.Principal, org, 10, 0)
			return err
		},
		"get": func(org, ep string) error { _, err := env.Svc.GetEndpoint(bg, bob.Principal, org, ep); return err },
		"update": func(org, ep string) error {
			_, err := env.Svc.UpdateEndpoint(bg, bob.Principal, org, ep, "x")
			return err
		},
		"pause":  func(org, ep string) error { _, err := env.Svc.PauseEndpoint(bg, bob.Principal, org, ep); return err },
		"rotate": func(org, ep string) error { _, err := env.Svc.RotateSecret(bg, bob.Principal, org, ep); return err },
		"revoke": func(org, ep string) error { _, err := env.Svc.RevokeEndpoint(bg, bob.Principal, org, ep); return err },
		"events": func(org, _ string) error {
			_, _, err := env.Svc.ListEvents(bg, bob.Principal, org, app.EventFilter{}, 10, 0)
			return err
		},
	}
	for name, op := range ops {
		// Bob targets Alice's organization directly.
		if err := op(alice.Org.ID, aliceEP.ID); !errors.Is(err, app.ErrNotFound) {
			t.Errorf("%s via Alice's organization: %v", name, err)
		}
		// Bob targets Alice's endpoint through his own organization.
		if name != "create" && name != "list" && name != "events" {
			if err := op(bob.Org.ID, aliceEP.ID); !errors.Is(err, app.ErrNotFound) {
				t.Errorf("%s of Alice's endpoint via Bob's organization: %v", name, err)
			}
		}
		if err := op(missing, missing); !errors.Is(err, app.ErrNotFound) {
			t.Errorf("%s on a missing organization: %v", name, err)
		}
	}
	// Nothing changed.
	got, err := env.Svc.GetEndpoint(bg, alice.Principal, alice.Org.ID, aliceEP.ID)
	if err != nil || got.Name != "Test endpoint" || got.Status != ingest.StatusActive {
		t.Fatalf("Alice's endpoint was modified: %+v %v", got, err)
	}
	if _, err := env.Send(aliceEP.PublicID, env.Signed(secret.SigningSecret, []byte(`{}`), "k")); err != nil {
		t.Fatalf("Alice's secret must still work: %v", err)
	}
}

func TestEventAccessPolicyAndFilters(t *testing.T) {
	tm := newTeam(t)
	env := tm.env
	epA := env.CreateEndpointIn(t, tm.owner, tm.org.ID)
	epB := env.CreateEndpointIn(t, tm.owner, tm.org.ID)

	var firstID string
	for i, ep := range []app.EndpointWithSecret{epA, epA, epB} {
		res, err := env.Send(ep.Endpoint.PublicID, env.Signed(ep.SigningSecret, []byte(`{"n":`+string(rune('0'+i))+`}`), "e"+string(rune('0'+i))))
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			firstID = res.EventID
		}
		env.Clock.Advance(time.Minute)
	}

	list := func(u apptest.User, f app.EventFilter, limit, offset int) ([]ingest.Event, int, error) {
		return env.Svc.ListEvents(bg, u.Principal, tm.org.ID, f, limit, offset)
	}
	for name, u := range map[string]apptest.User{"owner": tm.owner, "admin": tm.admin, "developer": tm.developer, "viewer": tm.viewer} {
		events, total, err := list(u, app.EventFilter{}, 50, 0)
		if err != nil || total != 3 || len(events) != 3 {
			t.Errorf("%s must list event metadata: %d %v", name, total, err)
		}
	}
	for _, e := range func() []ingest.Event { ev, _, _ := list(tm.viewer, app.EventFilter{}, 50, 0); return ev }() {
		if e.Payload != nil {
			t.Fatal("event listings must not carry payloads")
		}
	}

	// Payloads: OWNER, ADMIN and DEVELOPER only.
	for name, u := range map[string]apptest.User{"owner": tm.owner, "admin": tm.admin, "developer": tm.developer} {
		ev, err := env.Svc.GetEvent(bg, u.Principal, tm.org.ID, firstID)
		if err != nil || string(ev.Payload) != `{"n":0}` {
			t.Errorf("%s must read the payload: %v", name, err)
		}
	}
	if _, err := env.Svc.GetEvent(bg, tm.viewer.Principal, tm.org.ID, firstID); !errors.Is(err, app.ErrForbidden) {
		t.Fatalf("a viewer must not read payloads: %v", err)
	}

	// Filters and pagination.
	if _, total, _ := list(tm.owner, app.EventFilter{EndpointID: epA.Endpoint.ID}, 50, 0); total != 2 {
		t.Errorf("endpoint filter: %d", total)
	}
	if _, total, _ := list(tm.owner, app.EventFilter{Status: ingest.EventReceived}, 50, 0); total != 3 {
		t.Errorf("status filter: %d", total)
	}
	now := env.Clock.Now()
	if _, total, _ := list(tm.owner, app.EventFilter{From: now.Add(-time.Hour), To: now.Add(time.Hour)}, 50, 0); total != 3 {
		t.Errorf("date range: %d", total)
	}
	if _, total, _ := list(tm.owner, app.EventFilter{From: now.Add(time.Hour)}, 50, 0); total != 0 {
		t.Errorf("future range: %d", total)
	}
	page, total, err := list(tm.owner, app.EventFilter{}, 2, 1)
	if err != nil || total != 3 || len(page) != 2 {
		t.Errorf("pagination: %d items, total %d, %v", len(page), total, err)
	}
	for _, bad := range []app.EventFilter{{EndpointID: "x"}, {Status: "DONE"}, {From: now, To: now.Add(-time.Hour)}} {
		if _, _, err := list(tm.owner, bad, 10, 0); err == nil {
			t.Errorf("filter %+v accepted", bad)
		}
	}

	// Another tenant sees nothing and cannot fetch by id.
	other := env.Register(t, "other@example.com")
	if _, _, err := env.Svc.ListEvents(bg, other.Principal, tm.org.ID, app.EventFilter{}, 10, 0); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("cross-tenant event listing: %v", err)
	}
	if _, err := env.Svc.GetEvent(bg, other.Principal, other.Org.ID, firstID); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("event of another tenant via own organization: %v", err)
	}
	if events, total, _ := env.Svc.ListEvents(bg, other.Principal, other.Org.ID, app.EventFilter{}, 10, 0); total != 0 || len(events) != 0 {
		t.Fatal("another tenant's events leaked into a listing")
	}
}

func TestLoginRateLimitingPerAccount(t *testing.T) {
	env := apptest.New(t)
	victim := env.Register(t, "victim@example.com")
	bystander := env.Register(t, "bystander@example.com")
	limit := app.DefaultConfig().LoginFailuresPerAccount

	for i := 0; i < limit; i++ {
		if _, err := env.Svc.Login(bg, victim.Email, "wrong password "+string(rune('a'+i))); !errors.Is(err, app.ErrInvalidCredentials) {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	// Locked: even the correct password is refused without being checked.
	_, err := env.Svc.Login(bg, victim.Email, apptest.Password)
	var ae interface{ Is(error) bool }
	_ = ae
	if !errors.Is(err, ratelimit.Limited(0)) {
		t.Fatalf("expected a rate limit error after %d failures, got %v", limit, err)
	}
	// Other accounts are unaffected, and so is a login that does not reach the limit.
	if _, err := env.Svc.Login(bg, bystander.Email, apptest.Password); err != nil {
		t.Fatalf("an unrelated account was affected: %v", err)
	}
	// The limit applies to unknown accounts in exactly the same way, so it reveals nothing.
	for i := 0; i < limit; i++ {
		env.Svc.Login(bg, "ghost@example.com", "wrong password value")
	}
	if _, err := env.Svc.Login(bg, "ghost@example.com", "wrong password value"); !errors.Is(err, ratelimit.Limited(0)) {
		t.Fatalf("unknown accounts must be limited like known ones: %v", err)
	}
	// Normalization: case and spacing do not give a fresh budget.
	if _, err := env.Svc.Login(bg, "  VICTIM@Example.com ", apptest.Password); !errors.Is(err, ratelimit.Limited(0)) {
		t.Fatalf("email variants must share one counter: %v", err)
	}
	// The window ends and the account recovers.
	env.Clock.Advance(16 * time.Minute)
	if _, err := env.Svc.Login(bg, victim.Email, apptest.Password); err != nil {
		t.Fatalf("login after the window: %v", err)
	}
}

func TestSuccessfulLoginsDoNotConsumeTheFailureBudget(t *testing.T) {
	env := apptest.New(t)
	u := env.Register(t, "steady@example.com")
	for i := 0; i < 3*app.DefaultConfig().LoginFailuresPerAccount; i++ {
		if _, err := env.Svc.Login(bg, u.Email, apptest.Password); err != nil {
			t.Fatalf("successful login %d refused: %v", i, err)
		}
	}
}

func TestLoginRateLimitingPerClientAddress(t *testing.T) {
	env := apptest.New(t)
	limit := app.DefaultConfig().LoginFailuresPerIP
	good := env.Register(t, "good@example.com")

	// One address sprays many different accounts.
	for i := 0; i < limit; i++ {
		email := "spray" + string(rune('a'+i%26)) + string(rune('a'+i/26)) + "@example.com"
		env.Svc.LoginFromIP(bg, "198.51.100.77", email, "wrong password value")
	}
	if _, err := env.Svc.LoginFromIP(bg, "198.51.100.77", good.Email, apptest.Password); !errors.Is(err, ratelimit.Limited(0)) {
		t.Fatalf("the address must be blocked after %d failures: %v", limit, err)
	}
	// A different address is fine.
	if _, err := env.Svc.LoginFromIP(bg, "203.0.113.20", good.Email, apptest.Password); err != nil {
		t.Fatalf("another address was affected: %v", err)
	}
	if n := env.Count(t, "SELECT count(*) FROM rate_limit_counters WHERE bucket LIKE '%198.51.100.77%' OR bucket LIKE '%good@example.com%'"); n != 0 {
		t.Fatal("counters must hold only hashed identifiers")
	}
}

var _ = tenancy.RoleOwner
