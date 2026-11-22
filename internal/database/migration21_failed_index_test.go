package database_test

import (
	"context"
	"strings"
	"testing"
	"time"
)

const failedIdx = "deliveries_org_failed_idx"

func TestMigration21CreatesTheFailedIndexWithTheDocumentedDefinition(t *testing.T) {
	f := newDeliveryFixture(t)
	s := f.indexState(t, failedIdx)
	if !s.exists || !s.valid {
		t.Fatalf("%s after a clean migration: %+v", failedIdx, s)
	}
	for _, fragment := range []string{"CREATE INDEX " + failedIdx, "USING btree (organization_id, completed_at DESC, id DESC)", "WHERE (status = 'FAILED'"} {
		if !strings.Contains(s.def, fragment) {
			t.Errorf("definition %q lacks %q", s.def, fragment)
		}
	}
	// The earlier operational and claim indexes are untouched.
	for _, name := range []string{"deliveries_org_pending_idx", "deliveries_org_completed_idx", "deliveries_due_idx", "deliveries_lease_idx"} {
		if st := f.indexState(t, name); !st.exists || !st.valid {
			t.Errorf("%s: %+v", name, st)
		}
	}
	// 00021 is the only migration of that phase: exactly one migration has version 21. (00022 is the redrive trigger
	// of a later phase, tested in schema_delivery_redrive_test.go.)
	if n := f.env.Count(t, "SELECT count(*) FROM goose_db_version WHERE version_id = 21 AND is_applied"); n != 1 {
		t.Errorf("migrations with version 21: %d, want exactly one", n)
	}
}

// TestMigration21RollsBackReappliesAndRepairsInterruptedBuilds drives the real goose provider, like the tests of 00020: up/down/up,
// an INVALID index left by an interrupted CREATE INDEX CONCURRENTLY (made for real with a unique build over duplicate
// rows), a wrong leftover definition, and a rerun of a finished migration. The catalog is read after each step.
func TestMigration21RollsBackReappliesAndRepairsInterruptedBuilds(t *testing.T) {
	f := newDeliveryFixture(t)
	ctx := context.Background()
	db, err := f.env.Pool.SQLDB()
	if err != nil {
		t.Fatal(err)
	}
	provider := newGooseProvider(t, db)
	q, _ := f.env.Pool.Querier()

	gold := f.indexState(t, failedIdx)
	if !gold.exists || !gold.valid {
		t.Fatalf("setup: %+v", gold)
	}
	// Two FAILED rows of one organization: duplicates for a unique concurrent build, and data the migration must keep.
	a := f.failed(t, feedEpoch, "RETRIES_EXHAUSTED")
	f.failed(t, feedEpoch.Add(time.Second), "RETRIES_EXHAUSTED")
	rows := f.env.Count(t, "SELECT count(*) FROM deliveries")

	rollBack := func() {
		t.Helper()
		if _, err := provider.DownTo(ctx, 20); err != nil {
			t.Fatalf("rollback to 20: %v", err)
		}
		if s := f.indexState(t, failedIdx); s.exists {
			t.Fatalf("the rollback must drop %s: %+v", failedIdx, s)
		}
		for _, name := range []string{"deliveries_org_pending_idx", "deliveries_org_completed_idx", "deliveries_due_idx"} {
			if s := f.indexState(t, name); !s.exists || !s.valid {
				t.Fatalf("the rollback of 21 must leave %s alone: %+v", name, s)
			}
		}
		if f.env.Count(t, "SELECT count(*) FROM deliveries") != rows {
			t.Fatal("the rollback must keep every delivery")
		}
	}
	assertRepaired := func(label string) {
		t.Helper()
		s := f.indexState(t, failedIdx)
		t.Logf("catalog %s: %s exists=%v indisvalid=%v def=%s", label, failedIdx, s.exists, s.valid, s.def)
		if !s.exists || !s.valid || s.def != gold.def {
			t.Errorf("%s: %+v, want a valid index defined as %q", label, s, gold.def)
		}
	}

	t.Run("an invalid index left by an interrupted build is replaced", func(t *testing.T) {
		rollBack()
		if _, err := q.Exec(ctx, "CREATE UNIQUE INDEX CONCURRENTLY "+failedIdx+" ON deliveries (organization_id) WHERE status = 'FAILED'"); err == nil {
			t.Fatal("setup: the unique build must fail on the duplicate rows")
		}
		if s := f.indexState(t, failedIdx); !s.exists || s.valid {
			t.Fatalf("setup: the index must exist and be INVALID, got %+v", s)
		} else {
			t.Logf("catalog before the repair: %s exists=%v indisvalid=%v", failedIdx, s.exists, s.valid)
		}
		if _, err := provider.Up(ctx); err != nil {
			t.Fatalf("up must repair an invalid index: %v", err)
		}
		assertRepaired("after repairing an invalid index")
	})

	t.Run("a valid index with another definition is replaced", func(t *testing.T) {
		rollBack()
		f.env.Exec(t, "CREATE INDEX "+failedIdx+" ON deliveries (created_at)")
		if _, err := provider.Up(ctx); err != nil {
			t.Fatalf("up must replace a leftover index: %v", err)
		}
		assertRepaired("after replacing a leftover definition")
	})

	t.Run("up, down, up, and a rerun on a finished migration", func(t *testing.T) {
		assertRepaired("starting point")
		rollBack()
		if _, err := provider.Up(ctx); err != nil {
			t.Fatalf("up after down: %v", err)
		}
		assertRepaired("after down and up")
		if f.env.Count(t, "SELECT count(*) FROM deliveries WHERE id = $1", a) != 1 {
			t.Fatal("the data must be intact")
		}
		// A second up has nothing to run and rebuilds nothing.
		oid := f.indexState(t, failedIdx).oid
		if _, err := provider.Up(ctx); err != nil {
			t.Fatalf("up with nothing to do: %v", err)
		}
		if f.indexState(t, failedIdx).oid != oid {
			t.Error("a finished migration must not rebuild the index")
		}
	})
}
