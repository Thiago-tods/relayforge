package app_test

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Thiago-tods/relayforge/internal/app"
	"github.com/Thiago-tods/relayforge/internal/app/apptest"
	"github.com/Thiago-tods/relayforge/internal/audit"
	"github.com/Thiago-tods/relayforge/internal/delivery"
	"github.com/Thiago-tods/relayforge/internal/platform/apperror"
)

const goodURL = "https://hooks.example.com/relayforge/in?token=SECRETQUERYTOKEN"

func (tm team) dest(t *testing.T, name string) app.DestinationWithSecret {
	t.Helper()
	d, err := tm.env.Svc.CreateDestination(bg, tm.owner.Principal, tm.org.ID, name, goodURL)
	if err != nil {
		t.Fatalf("create destination %q: %v", name, err)
	}
	return d
}

func (tm team) audits(t *testing.T, action string) int {
	t.Helper()
	return tm.env.Count(t, "SELECT count(*) FROM audit_events WHERE organization_id = $1 AND action = $2", tm.org.ID, action)
}

func isValidation(err error) bool {
	var ae *apperror.Error
	return errors.As(err, &ae) && ae.Status == 400
}

func TestDestinationLifecycleSecretsAndAudit(t *testing.T) {
	tm := newTeam(t)
	svc, org, owner := tm.env.Svc, tm.org.ID, tm.owner.Principal

	created, err := svc.CreateDestination(bg, owner, org, "  Billing hook  ", goodURL)
	if err != nil {
		t.Fatal(err)
	}
	d := created.Destination
	if d.Name != "Billing hook" || d.Status != delivery.DestinationActive || d.OrganizationID != org || d.SecretVersion != 1 {
		t.Fatalf("unexpected destination: %+v", d)
	}
	if !strings.HasPrefix(created.SigningSecret, "rfd_") {
		t.Fatalf("destination secrets use the rfd_ prefix, got %q", created.SigningSecret[:4])
	}
	display, hasQuery := d.DisplayURL()
	if display != "https://hooks.example.com/relayforge/in" || !hasQuery {
		t.Fatalf("display url = %q (query: %v): the query may hold a token and must be hidden", display, hasQuery)
	}

	// The secret is stored sealed with the destination-specific associated data.
	q, _ := tm.env.Pool.Querier()
	var stored []byte
	if err := q.QueryRow(bg, "SELECT signing_secret_ciphertext FROM destinations WHERE id = $1", d.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stored), created.SigningSecret) {
		t.Fatal("the secret is stored in plaintext")
	}
	plain, err := tm.env.Box.Open(stored, delivery.DestinationSecretAAD(d.ID), tm.env.Box.Version())
	if err != nil || string(plain) != created.SigningSecret {
		t.Fatalf("the stored ciphertext must open with the destination's associated data: %v", err)
	}
	for name, aad := range map[string][]byte{
		"another destination": delivery.DestinationSecretAAD("00000000-0000-4000-8000-000000000000"),
		"an ingestion secret": []byte("relayforge:ingestion-endpoint:v1:" + d.ID),
	} {
		if _, err := tm.env.Box.Open(stored, aad, tm.env.Box.Version()); err == nil {
			t.Errorf("the ciphertext opened with the associated data of %s", name)
		}
	}

	got, err := svc.GetDestination(bg, owner, org, d.ID)
	if err != nil || got.ID != d.ID {
		t.Fatalf("get: %v", err)
	}
	items, total, err := svc.ListDestinations(bg, owner, org, "", 10, 0)
	if err != nil || total != 1 || len(items) != 1 {
		t.Fatalf("list: %d %v", total, err)
	}
	if _, _, err := svc.ListDestinations(bg, owner, org, "BOGUS", 10, 0); !isValidation(err) {
		t.Fatalf("an unknown status filter must be a validation error: %v", err)
	}

	// Rename and re-target.
	newName, newURL := "Billing hook v2", "https://hooks.example.com/v2?key=ANOTHERSECRET"
	upd, err := svc.UpdateDestination(bg, owner, org, d.ID, app.DestinationChanges{Name: &newName, URL: &newURL})
	if err != nil || upd.Name != newName || upd.URL != "https://hooks.example.com/v2?key=ANOTHERSECRET" {
		t.Fatalf("update: %+v %v", upd, err)
	}
	if _, err := svc.UpdateDestination(bg, owner, org, d.ID, app.DestinationChanges{}); !isValidation(err) {
		t.Fatalf("an empty update must be rejected: %v", err)
	}
	blocked := "https://127.0.0.1/x"
	if _, err := svc.UpdateDestination(bg, owner, org, d.ID, app.DestinationChanges{URL: &blocked}); !isValidation(err) {
		t.Fatalf("a blocked URL must be rejected on update too: %v", err)
	}
	same := newName
	before := tm.audits(t, audit.DestinationUpdated)
	if _, err := svc.UpdateDestination(bg, owner, org, d.ID, app.DestinationChanges{Name: &same}); err != nil || tm.audits(t, audit.DestinationUpdated) != before {
		t.Fatal("an update that changes nothing must not be audited")
	}

	// Pause, activate, rotate.
	if p, err := svc.PauseDestination(bg, owner, org, d.ID); err != nil || p.Status != delivery.DestinationPaused {
		t.Fatalf("pause: %v", err)
	}
	if a, err := svc.ActivateDestination(bg, owner, org, d.ID); err != nil || a.Status != delivery.DestinationActive {
		t.Fatalf("activate: %v", err)
	}
	rot, err := svc.RotateDestinationSecret(bg, owner, org, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rot.SigningSecret == created.SigningSecret || !strings.HasPrefix(rot.SigningSecret, "rfd_") || rot.Destination.SecretVersion != 2 {
		t.Fatalf("rotation must produce a new secret and version 2, got version %d", rot.Destination.SecretVersion)
	}
	if err := q.QueryRow(bg, "SELECT signing_secret_ciphertext FROM destinations WHERE id = $1", d.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if plain, err := tm.env.Box.Open(stored, delivery.DestinationSecretAAD(d.ID), tm.env.Box.Version()); err != nil || string(plain) != rot.SigningSecret {
		t.Fatalf("the rotated secret must open with the destination's associated data: %v", err)
	}
	if rot2, err := svc.RotateDestinationSecret(bg, owner, org, d.ID); err != nil || rot2.Destination.SecretVersion != 3 {
		t.Fatalf("the secret version must keep increasing: %v", err)
	}

	// A pending delivery is cancelled by revocation, and the record is kept.
	ev := tm.event(t)
	tm.env.Exec(t, "INSERT INTO deliveries (organization_id, event_id, destination_id) VALUES ($1, $2, $3)", org, ev, d.ID)
	rev, err := svc.RevokeDestination(bg, owner, org, d.ID)
	if err != nil || rev.Status != delivery.DestinationRevoked || rev.RevokedAt == nil {
		t.Fatalf("revoke: %+v %v", rev, err)
	}
	if tm.env.Count(t, "SELECT count(*) FROM deliveries WHERE destination_id = $1 AND status = 'CANCELLED' AND completed_at IS NOT NULL", d.ID) != 1 {
		t.Fatal("revoking must cancel pending deliveries and keep them as history")
	}
	// Revocation is permanent and total.
	for name, err := range map[string]error{
		"activate": second(svc.ActivateDestination(bg, owner, org, d.ID)),
		"pause":    second(svc.PauseDestination(bg, owner, org, d.ID)),
		"rotate":   second(svc.RotateDestinationSecret(bg, owner, org, d.ID)),
		"rename":   second(svc.UpdateDestination(bg, owner, org, d.ID, app.DestinationChanges{Name: &same})),
	} {
		if !errors.Is(err, app.ErrDestinationRevoked) {
			t.Errorf("%s after revocation: %v, want ErrDestinationRevoked", name, err)
		}
	}
	if again, err := svc.RevokeDestination(bg, owner, org, d.ID); err != nil || again.Status != delivery.DestinationRevoked {
		t.Fatalf("revoking twice must be a no-op: %v", err)
	}
	if got, err := svc.GetDestination(bg, owner, org, d.ID); err != nil || got.Status != delivery.DestinationRevoked {
		t.Fatal("a revoked destination stays readable for history")
	}
	// The database refuses reactivation even for a writer that skips the application.
	if _, err := q.Exec(bg, "UPDATE destinations SET status = 'ACTIVE', revoked_at = NULL WHERE id = $1", d.ID); err == nil {
		t.Fatal("the database accepted reactivating a revoked destination")
	}

	for action, want := range map[string]int{
		audit.DestinationCreated: 1, audit.DestinationUpdated: 1, audit.DestinationPaused: 1, audit.DestinationActivated: 1,
		audit.DestinationSecretRotated: 2, audit.DestinationRevoked: 1,
	} {
		if got := tm.audits(t, action); got != want {
			t.Errorf("audit %s: %d events, want %d", action, got, want)
		}
	}
	// Audit metadata never carries secrets or URL queries.
	rows, err := q.Query(bg, "SELECT metadata::text FROM audit_events WHERE organization_id = $1 AND resource_type = 'destination'", org)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var meta string
		if err := rows.Scan(&meta); err != nil {
			t.Fatal(err)
		}
		for _, banned := range []string{"SECRETQUERYTOKEN", "ANOTHERSECRET", created.SigningSecret, rot.SigningSecret, "rfd_", "token=", "key="} {
			if strings.Contains(meta, banned) {
				t.Errorf("audit metadata leaks %q: %s", banned, meta)
			}
		}
	}
}

func second[T any](_ T, err error) error { return err }

func (tm team) event(t *testing.T) string {
	t.Helper()
	ep := tm.env.CreateEndpointIn(t, tm.owner, tm.org.ID)
	res, err := tm.env.Send(ep.Endpoint.PublicID, tm.env.Signed(ep.SigningSecret, []byte(`{"n":1}`), fmt.Sprintf("k-%d", time.Now().UnixNano())))
	if err != nil {
		t.Fatal(err)
	}
	return res.EventID
}

func TestDestinationInputValidationAndSSRFPolicy(t *testing.T) {
	tm := newTeam(t)
	svc, org, owner := tm.env.Svc, tm.org.ID, tm.owner.Principal

	for _, name := range []string{"", "   ", strings.Repeat("x", 101), "bad\x00name", "tab\tname", "\xff\xfe"} {
		if _, err := svc.CreateDestination(bg, owner, org, name, goodURL); !isValidation(err) {
			t.Errorf("name %q: %v, want a validation error", name, err)
		}
	}
	secretURLs := []string{
		"http://hooks.example.com/x?token=LEAKME",
		"https://127.0.0.1/x?token=LEAKME",
		"https://localhost/x?token=LEAKME",
		"https://10.0.0.5/x?token=LEAKME",
		"https://192.168.1.1/x?token=LEAKME",
		"https://169.254.169.254/latest?token=LEAKME",
		"https://[::1]/x?token=LEAKME",
		"https://[fe80::1]/x?token=LEAKME",
		"https://0.0.0.0/x?token=LEAKME",
		"https://224.0.0.1/x?token=LEAKME",
		"https://printer.internal/x?token=LEAKME",
		"https://user:pass@hooks.example.com/x?token=LEAKME",
		"https://hooks.example.com:8443/x?token=LEAKME",
		"ftp://hooks.example.com/x?token=LEAKME",
		"https://2130706433/x?token=LEAKME",
		"https://hooks.example.com/x#frag?token=LEAKME",
		"https://hooks.example.com/" + strings.Repeat("a", 2100) + "?token=LEAKME",
		"", "not a url",
	}
	for _, raw := range secretURLs {
		_, err := svc.CreateDestination(bg, owner, org, "x", raw)
		if !isValidation(err) {
			t.Errorf("url %.60q must be rejected with a validation error, got %v", raw, err)
			continue
		}
		if strings.Contains(err.Error(), "LEAKME") {
			t.Errorf("the validation error echoes the submitted URL: %v", err)
		}
	}
	if n := tm.env.Count(t, "SELECT count(*) FROM destinations"); n != 0 {
		t.Fatalf("%d destinations were created from invalid input", n)
	}

	a := tm.dest(t, "Orders")
	if _, err := svc.CreateDestination(bg, owner, org, "orders", goodURL); !errors.Is(err, app.ErrDestinationNameTaken) {
		t.Fatalf("names are unique per organization, case-insensitively: %v", err)
	}
	b := tm.dest(t, "Other")
	taken := "ORDERS"
	if _, err := svc.UpdateDestination(bg, owner, org, b.Destination.ID, app.DestinationChanges{Name: &taken}); !errors.Is(err, app.ErrDestinationNameTaken) {
		t.Fatalf("renaming onto an existing name: %v", err)
	}
	// Another organization may use the same name; a revoked destination frees its name.
	other := tm.env.Register(t, "other@example.com")
	if _, err := svc.CreateDestination(bg, other.Principal, other.Org.ID, "Orders", goodURL); err != nil {
		t.Fatalf("another organization must be able to reuse the name: %v", err)
	}
	if _, err := svc.RevokeDestination(bg, owner, org, a.Destination.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateDestination(bg, owner, org, "Orders", goodURL); err != nil {
		t.Fatalf("a revoked destination frees its name: %v", err)
	}
}

func TestDestinationSubscriptionAndDeliveryRBAC(t *testing.T) {
	type tc struct {
		role                                        string
		user                                        func(team) apptest.User
		read, create, update, pause, rotate, revoke bool
		subRead, subWrite, deliveriesRead           bool
	}
	cases := []tc{
		{"owner", func(t team) apptest.User { return t.owner }, true, true, true, true, true, true, true, true, true},
		{"admin", func(t team) apptest.User { return t.admin }, true, true, true, true, true, true, true, true, true},
		{"developer", func(t team) apptest.User { return t.developer }, true, true, true, true, false, false, true, true, true},
		{"viewer", func(t team) apptest.User { return t.viewer }, true, false, false, false, false, false, true, false, true},
	}
	for _, c := range cases {
		t.Run(c.role, func(t *testing.T) {
			tm := newTeam(t)
			actor := c.user(tm).Principal
			svc, org := tm.env.Svc, tm.org.ID
			seed := tm.dest(t, "Seed")
			id := seed.Destination.ID
			ep := tm.env.CreateEndpointIn(t, tm.owner, org)
			sub, err := svc.CreateSubscription(bg, tm.owner.Principal, org, ep.Endpoint.ID, id)
			if err != nil {
				t.Fatal(err)
			}
			check := func(name string, allowed bool, err error) {
				t.Helper()
				if allowed && err != nil {
					t.Errorf("%s must be allowed: %v", name, err)
				}
				if !allowed && !errors.Is(err, app.ErrForbidden) {
					t.Errorf("%s must be forbidden, got %v", name, err)
				}
			}
			_, _, err = svc.ListDestinations(bg, actor, org, "", 10, 0)
			check("list destinations", c.read, err)
			_, err = svc.GetDestination(bg, actor, org, id)
			check("get destination", c.read, err)
			_, err = svc.CreateDestination(bg, actor, org, "Mine", goodURL)
			check("create destination", c.create, err)
			n := "Renamed"
			_, err = svc.UpdateDestination(bg, actor, org, id, app.DestinationChanges{Name: &n})
			check("update destination", c.update, err)
			_, err = svc.PauseDestination(bg, actor, org, id)
			check("pause destination", c.pause, err)
			_, err = svc.ActivateDestination(bg, actor, org, id)
			check("activate destination", c.pause, err)
			_, err = svc.RotateDestinationSecret(bg, actor, org, id)
			check("rotate secret", c.rotate, err)
			_, _, err = svc.ListSubscriptions(bg, actor, org, app.SubscriptionFilter{}, 10, 0)
			check("list subscriptions", c.subRead, err)
			_, err = svc.GetSubscription(bg, actor, org, sub.ID)
			check("get subscription", c.subRead, err)
			ep2 := tm.env.CreateEndpointIn(t, tm.owner, org)
			_, err = svc.CreateSubscription(bg, actor, org, ep2.Endpoint.ID, id)
			check("create subscription", c.subWrite, err)
			_, err = svc.RevokeSubscription(bg, actor, org, sub.ID)
			check("revoke subscription", c.subWrite, err)
			_, _, err = svc.ListDeliveries(bg, actor, org, app.DeliveryFilter{}, 10, 0)
			check("list deliveries", c.deliveriesRead, err)
			_, err = svc.GetDelivery(bg, actor, org, "00000000-0000-4000-8000-000000000000")
			if c.deliveriesRead && !errors.Is(err, app.ErrNotFound) {
				t.Errorf("get delivery: %v, want not found for a missing id", err)
			}
			if !c.deliveriesRead {
				check("get delivery", false, err)
			}
			_, err = svc.RevokeDestination(bg, actor, org, id)
			check("revoke destination", c.revoke, err)
		})
	}
}

func TestDestinationAccessWithoutActiveMembershipIsNotFound(t *testing.T) {
	tm := newTeam(t)
	d := tm.dest(t, "D")
	outsider := tm.env.Register(t, "outsider@example.com")
	svc := tm.env.Svc
	if err := svc.RemoveMember(bg, tm.owner.Principal, tm.org.ID, tm.developer.Principal.User.ID); err != nil {
		t.Fatal(err)
	}
	for name, p := range map[string]app.Principal{"outsider": outsider.Principal, "removed member": tm.developer.Principal} {
		if _, err := svc.GetDestination(bg, p, tm.org.ID, d.Destination.ID); !errors.Is(err, app.ErrNotFound) {
			t.Errorf("%s: %v, want not found (organizations are not enumerable)", name, err)
		}
		if _, err := svc.PauseDestination(bg, p, tm.org.ID, d.Destination.ID); !errors.Is(err, app.ErrNotFound) {
			t.Errorf("%s pause: %v, want not found", name, err)
		}
	}
}

func TestTenantIsolationOfDestinationsSubscriptionsAndDeliveries(t *testing.T) {
	tm := newTeam(t)
	svc, owner := tm.env.Svc, tm.owner.Principal
	other := tm.env.Register(t, "other@example.com")
	mine := tm.dest(t, "Mine").Destination
	theirs, err := svc.CreateDestination(bg, other.Principal, other.Org.ID, "Theirs", goodURL)
	if err != nil {
		t.Fatal(err)
	}
	myEp := tm.env.CreateEndpoint(t, tm.owner).Endpoint
	theirEp := tm.env.CreateEndpoint(t, other).Endpoint
	theirSub, err := svc.CreateSubscription(bg, other.Principal, other.Org.ID, theirEp.ID, theirs.Destination.ID)
	if err != nil {
		t.Fatal(err)
	}
	// A delivery with an attempt in the other tenant.
	theirDelivery := seedDelivery(t, tm.env, other, theirs.Destination.ID, 2, "FAILED")

	n := "Hijacked"
	cross := map[string]error{
		"get destination":                 second(svc.GetDestination(bg, owner, tm.org.ID, theirs.Destination.ID)),
		"update destination":              second(svc.UpdateDestination(bg, owner, tm.org.ID, theirs.Destination.ID, app.DestinationChanges{Name: &n})),
		"pause destination":               second(svc.PauseDestination(bg, owner, tm.org.ID, theirs.Destination.ID)),
		"rotate destination":              second(svc.RotateDestinationSecret(bg, owner, tm.org.ID, theirs.Destination.ID)),
		"revoke destination":              second(svc.RevokeDestination(bg, owner, tm.org.ID, theirs.Destination.ID)),
		"get subscription":                second(svc.GetSubscription(bg, owner, tm.org.ID, theirSub.ID)),
		"revoke subscription":             second(svc.RevokeSubscription(bg, owner, tm.org.ID, theirSub.ID)),
		"get delivery":                    second(svc.GetDelivery(bg, owner, tm.org.ID, theirDelivery)),
		"my endpoint + their destination": second(svc.CreateSubscription(bg, owner, tm.org.ID, myEp.ID, theirs.Destination.ID)),
		"their endpoint + my destination": second(svc.CreateSubscription(bg, owner, tm.org.ID, theirEp.ID, mine.ID)),
		"their org id with my session":    second(svc.GetDestination(bg, owner, other.Org.ID, theirs.Destination.ID)),
	}
	missing := map[string]error{
		"get destination":                 second(svc.GetDestination(bg, owner, tm.org.ID, "00000000-0000-4000-8000-000000000000")),
		"get subscription":                second(svc.GetSubscription(bg, owner, tm.org.ID, "00000000-0000-4000-8000-000000000000")),
		"get delivery":                    second(svc.GetDelivery(bg, owner, tm.org.ID, "00000000-0000-4000-8000-000000000000")),
		"my endpoint + their destination": second(svc.CreateSubscription(bg, owner, tm.org.ID, myEp.ID, "00000000-0000-4000-8000-000000000000")),
	}
	for name, err := range cross {
		if !errors.Is(err, app.ErrNotFound) {
			t.Errorf("%s: %v, want ErrNotFound (never Forbidden or Conflict)", name, err)
		}
		if want, ok := missing[name]; ok && err.Error() != want.Error() {
			t.Errorf("%s: the answer for another tenant's resource (%v) differs from a missing one (%v)", name, err, want)
		}
	}
	// Nothing leaked into list results, and nothing of theirs changed.
	items, total, _ := svc.ListDestinations(bg, owner, tm.org.ID, "", 50, 0)
	if total != 1 || len(items) != 1 || items[0].ID != mine.ID {
		t.Fatalf("destination list shows %d items", len(items))
	}
	if _, total, _ := svc.ListSubscriptions(bg, owner, tm.org.ID, app.SubscriptionFilter{}, 50, 0); total != 0 {
		t.Fatal("another tenant's subscription is listed")
	}
	if _, total, _ := svc.ListDeliveries(bg, owner, tm.org.ID, app.DeliveryFilter{}, 50, 0); total != 0 {
		t.Fatal("another tenant's delivery is listed")
	}
	if _, total, _ := svc.ListDeliveries(bg, owner, tm.org.ID, app.DeliveryFilter{DestinationID: theirs.Destination.ID}, 50, 0); total != 0 {
		t.Fatal("filtering by another tenant's destination id must find nothing")
	}
	if got, _ := svc.GetDestination(bg, other.Principal, other.Org.ID, theirs.Destination.ID); got.Name != "Theirs" || got.Status != delivery.DestinationActive {
		t.Fatal("a cross-tenant call changed the other tenant's destination")
	}
}

func TestConcurrentRotationsAreSerializedAndEachVersionIsUnique(t *testing.T) {
	tm := newTeam(t)
	d := tm.dest(t, "D").Destination
	var wg sync.WaitGroup
	secrets := make(chan string, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := tm.env.Svc.RotateDestinationSecret(bg, tm.owner.Principal, tm.org.ID, d.ID)
			if err != nil {
				t.Error(err)
				return
			}
			secrets <- res.SigningSecret
		}()
	}
	wg.Wait()
	close(secrets)
	var last string
	seen := map[string]bool{}
	for s := range secrets {
		if seen[s] {
			t.Fatal("two rotations returned the same secret")
		}
		seen[s] = true
		last = s
	}
	got, _ := tm.env.Svc.GetDestination(bg, tm.owner.Principal, tm.org.ID, d.ID)
	if got.SecretVersion != 9 || len(seen) != 8 || tm.audits(t, audit.DestinationSecretRotated) != 8 {
		t.Fatalf("version %d, %d distinct secrets, %d audit events; want 9, 8, 8", got.SecretVersion, len(seen), tm.audits(t, audit.DestinationSecretRotated))
	}
	q, _ := tm.env.Pool.Querier()
	var stored []byte
	_ = q.QueryRow(bg, "SELECT signing_secret_ciphertext FROM destinations WHERE id = $1", d.ID).Scan(&stored)
	plain, err := tm.env.Box.Open(stored, delivery.DestinationSecretAAD(d.ID), tm.env.Box.Version())
	if err != nil || !seen[string(plain)] {
		t.Fatalf("the stored secret must be one of the returned ones (last seen %q): %v", last[:4], err)
	}
}

func TestRotationAndRevocationRaceLeavesAConsistentState(t *testing.T) {
	for round := 0; round < 12; round++ {
		tm := newTeam(t)
		d := tm.dest(t, "D").Destination
		var rotErr, revErr error
		var wg sync.WaitGroup
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, rotErr = tm.env.Svc.RotateDestinationSecret(bg, tm.owner.Principal, tm.org.ID, d.ID)
		}()
		go func() {
			defer wg.Done()
			<-start
			_, revErr = tm.env.Svc.RevokeDestination(bg, tm.owner.Principal, tm.org.ID, d.ID)
		}()
		close(start)
		wg.Wait()
		if revErr != nil {
			t.Fatalf("revoke: %v", revErr)
		}
		got, _ := tm.env.Svc.GetDestination(bg, tm.owner.Principal, tm.org.ID, d.ID)
		rotated := tm.audits(t, audit.DestinationSecretRotated)
		switch {
		case rotErr == nil && got.SecretVersion == 2 && rotated == 1: // rotated, then revoked
		case errors.Is(rotErr, app.ErrDestinationRevoked) && got.SecretVersion == 1 && rotated == 0: // revoked first
		default:
			t.Fatalf("round %d: rotate=%v version=%d rotatedAudits=%d", round, rotErr, got.SecretVersion, rotated)
		}
		if got.Status != delivery.DestinationRevoked || tm.audits(t, audit.DestinationRevoked) != 1 {
			t.Fatalf("round %d: the destination must end revoked with one audit event", round)
		}
	}
}

func TestPausedDestinationIsNeverClaimedAfterPauseReturns(t *testing.T) {
	tm := newTeam(t)
	org := tm.org.ID
	d := tm.dest(t, "D").Destination
	ep := tm.env.CreateEndpointIn(t, tm.owner, org)
	for i := 0; i < 60; i++ {
		res, err := tm.env.Send(ep.Endpoint.PublicID, tm.env.Signed(ep.SigningSecret, []byte(`{"i":1}`), fmt.Sprintf("p-%d", i)))
		if err != nil {
			t.Fatal(err)
		}
		tm.env.Exec(t, "INSERT INTO deliveries (organization_id, event_id, destination_id) VALUES ($1, $2, $3)", org, res.EventID, d.ID)
	}
	claim := func() int {
		var got []delivery.Claimed
		if err := tm.env.Pool.InTx(bg, func(tx pgx.Tx) error {
			var err error
			got, err = delivery.ClaimDue(bg, tx, time.Now().UTC().Add(time.Minute), 1, time.Minute, "w")
			return err
		}); err != nil {
			t.Error(err)
		}
		return len(got)
	}
	var paused atomic.Bool
	var claimedAfterPause atomic.Int64
	var claimedTotal atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			wasPaused := paused.Load() // read before the claim: a claim that starts after Pause returned must find nothing
			n := claim()
			claimedTotal.Add(int64(n))
			if wasPaused {
				claimedAfterPause.Add(int64(n))
			}
			time.Sleep(3 * time.Millisecond)
		}
	}()
	for deadline := time.Now().Add(10 * time.Second); claimedTotal.Load() < 3 && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond) // pause while the claimer is in the middle of the backlog
	}
	if _, err := tm.env.Svc.PauseDestination(bg, tm.owner.Principal, org, d.ID); err != nil {
		t.Fatal(err)
	}
	paused.Store(true)
	time.Sleep(300 * time.Millisecond)
	close(stop)
	wg.Wait()
	if claimedAfterPause.Load() != 0 {
		t.Fatalf("%d deliveries were claimed after the pause returned", claimedAfterPause.Load())
	}
	if total := claimedTotal.Load(); total < 3 || total >= 60 {
		t.Fatalf("the race was not exercised: %d of 60 deliveries were claimed", total)
	}
	if left := tm.env.Count(t, "SELECT count(*) FROM deliveries WHERE destination_id = $1 AND status = 'PENDING'", d.ID); left == 0 {
		t.Fatal("paused deliveries must stay PENDING")
	}
}
