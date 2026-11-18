package ingest_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/Thiago-tods/relayforge/internal/ingest"
)

// eventsWithoutOutbox counts committed events that have no outbox row. Reconciliation
// works from outbox rows (ADR 29), so such an event would never be repaired.
func (f fixture) eventsWithoutOutbox(t *testing.T) int {
	t.Helper()
	return f.env.Count(t, "SELECT count(*) FROM webhook_events e WHERE NOT EXISTS (SELECT 1 FROM outbox o WHERE o.event_id = e.id)")
}

// TestNoCommittedEventLacksItsOutboxRow walks every way a request can end and checks the
// invariant after each: the event and its outbox row commit together or not at all.
func TestNoCommittedEventLacksItsOutboxRow(t *testing.T) {
	f := newFixture(t)
	secret := f.endpoint.SigningSecret
	check := func(step string, wantEvents int) {
		t.Helper()
		if got := f.eventsWithoutOutbox(t); got != 0 {
			t.Fatalf("%s: %d committed events lack their outbox row", step, got)
		}
		if f.events(t) != wantEvents || f.outbox(t) != wantEvents {
			t.Fatalf("%s: %d events and %d outbox rows, want %d of each", step, f.events(t), f.outbox(t), wantEvents)
		}
	}

	// Accepted ingestion.
	if res, err := f.send(f.env.Signed(secret, []byte(`{"n":1}`), "key-accepted")); err != nil || res.Duplicate {
		t.Fatalf("accepted: %+v %v", res, err)
	}
	check("accepted", 1)

	// Duplicate ingestion: same key, same body. No second event and no second outbox row.
	if res, err := f.send(f.env.Signed(secret, []byte(`{"n":1}`), "key-accepted")); err != nil || !res.Duplicate {
		t.Fatalf("duplicate: %+v %v", res, err)
	}
	check("duplicate", 1)

	// Idempotency conflict: same key, different body.
	if _, err := f.send(f.env.Signed(secret, []byte(`{"n":2}`), "key-accepted")); !errors.Is(err, ingest.ErrIdempotencyConflict) {
		t.Fatalf("conflict: %v", err)
	}
	check("idempotency conflict", 1)

	// Rejected signature.
	if _, err := f.send(f.env.Signed("rfs_not-the-secret", []byte(`{"n":3}`), "key-rejected")); !errors.Is(err, ingest.ErrSignatureMismatch) {
		t.Fatalf("signature: %v", err)
	}
	check("rejected signature", 1)

	// Injected outbox failure: the event must roll back with it.
	f.env.Exec(t, "ALTER TABLE outbox ADD CONSTRAINT outbox_block CHECK (false) NOT VALID")
	if _, err := f.send(f.env.Signed(secret, []byte(`{"n":4}`), "key-injected")); err == nil {
		t.Fatal("the injected outbox failure must fail the request")
	}
	check("injected outbox failure", 1)
	f.env.Exec(t, "ALTER TABLE outbox DROP CONSTRAINT outbox_block")

	// And the system carries on: the same key now succeeds.
	if res, err := f.send(f.env.Signed(secret, []byte(`{"n":4}`), "key-injected")); err != nil || res.Duplicate {
		t.Fatalf("after the fault: %+v %v", res, err)
	}
	check("retry after the fault", 2)
}

var (
	insertsEvent  = regexp.MustCompile(`(?is)insert\s+into\s+webhook_events\b`)
	insertsOutbox = regexp.MustCompile(`(?is)insert\s+into\s+outbox\b`)
)

// approvedWriter is the only production file that may insert into webhook_events or
// outbox. Ingestion writes both rows in one transaction (Service.persist), which is what
// keeps "every event has an outbox row" true without a database constraint (ADR 29).
const approvedWriter = "internal/ingest/store.go"

// TestEventAndOutboxInsertsStayInsideTheIngestionStore is an architecture guard. A second
// writer would bypass the transaction that pairs the two rows. If this test fails, either
// route the new code through ingest.Service, or add a database constraint (ADR 29) and
// then update this guard deliberately.
func TestEventAndOutboxInsertsStayInsideTheIngestionStore(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("cannot find the repository root from %s: %v", root, err)
	}
	var approvedHasBoth bool
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "web", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		src := string(raw)
		hasEvent, hasOutbox := insertsEvent.MatchString(src), insertsOutbox.MatchString(src)
		if rel == approvedWriter {
			approvedHasBoth = hasEvent && hasOutbox
			return nil
		}
		if hasEvent {
			t.Errorf("%s inserts into webhook_events; only %s may", rel, approvedWriter)
		}
		if hasOutbox {
			t.Errorf("%s inserts into outbox; only %s may", rel, approvedWriter)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !approvedHasBoth {
		t.Fatalf("%s no longer contains both inserts: the guard is stale, update it with the code", approvedWriter)
	}
}
