package database_test

import (
	"context"
	"strings"
	"testing"
)

const outboxStateSQL = `SELECT coalesce(string_agg(id::text || ':' || attempts || ':' || coalesce(published_at::text, '-') || ':' || available_at::text, ',' ORDER BY id), '') FROM outbox`

func TestOutboxFanoutMarkerSchema(t *testing.T) {
	f := newIngestFixture(t)
	count := func(sql string, args ...any) int { return f.env.Count(t, sql, args...) }

	if count(`SELECT count(*) FROM information_schema.columns WHERE table_name = 'outbox' AND column_name = 'fanned_out_at'
		AND data_type = 'timestamp with time zone' AND is_nullable = 'YES' AND column_default IS NULL`) != 1 {
		t.Fatal("outbox.fanned_out_at must be a nullable timestamptz without a default")
	}
	var def string
	var valid bool
	q, _ := f.env.Pool.Querier()
	if err := q.QueryRow(context.Background(), `SELECT pg_get_indexdef(i.indexrelid), i.indisvalid
		FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid WHERE c.relname = 'outbox_unfanned_idx'`).Scan(&def, &valid); err != nil {
		t.Fatalf("the unfanned index is missing: %v", err)
	}
	if !valid || !strings.Contains(def, "(created_at, id)") || !strings.Contains(def, "WHERE (fanned_out_at IS NULL)") {
		t.Fatalf("the index must be valid and partial on unmarked rows: %s (valid=%v)", def, valid)
	}

	var event string
	if err := q.QueryRow(context.Background(), f.insertEvent()+" RETURNING id", f.orgID, f.ep1, "k", []byte(`{}`)).Scan(&event); err != nil {
		t.Fatal(err)
	}
	f.accept(t, "outbox row", "INSERT INTO outbox (organization_id, event_id, message_type, payload) VALUES ($1, $2, 'webhook_event.accepted', '{}')", f.orgID, event)
	if count("SELECT count(*) FROM outbox WHERE fanned_out_at IS NULL") != 1 {
		t.Fatal("a new outbox row starts unmarked")
	}
	f.accept(t, "marking", "UPDATE outbox SET fanned_out_at = now() WHERE event_id = $1", event)
	f.accept(t, "unmarking (re-verification)", "UPDATE outbox SET fanned_out_at = NULL WHERE event_id = $1", event)
}

// TestMigration18KeepsExistingRowsUntouchedRollsBackAndReapplies covers the migration
// itself: it must not rewrite historical rows (the backfill is done by reconciliation in
// bounded batches), must be idempotent after an interrupted run, and must roll back.
func TestMigration18KeepsExistingRowsUntouchedRollsBackAndReapplies(t *testing.T) {
	f := newIngestFixture(t)
	ctx := context.Background()
	db, err := f.env.Pool.SQLDB()
	if err != nil {
		t.Fatal(err)
	}
	provider := newGooseProvider(t, db)
	count := func(sql string) int { return f.env.Count(t, sql) }
	indexes := "SELECT count(*) FROM pg_indexes WHERE indexname = 'outbox_unfanned_idx'"
	column := "SELECT count(*) FROM information_schema.columns WHERE table_name = 'outbox' AND column_name = 'fanned_out_at'"

	if _, err := provider.DownTo(ctx, 17); err != nil {
		t.Fatalf("rollback to 17: %v", err)
	}
	if count(indexes) != 0 || count(column) != 0 {
		t.Fatal("the rollback must remove the column and the index")
	}

	// Historical rows, written while the marker does not exist.
	q, _ := f.env.Pool.Querier()
	for i, key := range []string{"a", "b", "c"} {
		var ev string
		if err := q.QueryRow(ctx, f.insertEvent()+" RETURNING id", f.orgID, f.ep1, key, []byte(`{}`)).Scan(&ev); err != nil {
			t.Fatal(err)
		}
		pub := "NULL"
		if i == 0 {
			pub = "now() - interval '3 days'"
		}
		f.env.Exec(t, "INSERT INTO outbox (organization_id, event_id, message_type, payload, attempts, published_at, available_at) VALUES ($1, $2, 'webhook_event.accepted', '{}', $3, "+pub+", now() - interval '3 days')",
			f.orgID, ev, i)
	}
	var before string
	if err := q.QueryRow(ctx, outboxStateSQL).Scan(&before); err != nil {
		t.Fatal(err)
	}
	var updatesBefore int64
	if err := q.QueryRow(ctx, "SELECT n_tup_upd FROM pg_stat_user_tables WHERE relname = 'outbox'").Scan(&updatesBefore); err != nil {
		t.Fatal(err)
	}

	// An interrupted earlier run: the column exists and a wrongly shaped index holds the name.
	f.env.Exec(t, "ALTER TABLE outbox ADD COLUMN fanned_out_at timestamptz")
	f.env.Exec(t, "CREATE INDEX outbox_unfanned_idx ON outbox (id)")

	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("up must repair an interrupted run: %v", err)
	}
	var after string
	if err := q.QueryRow(ctx, outboxStateSQL).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("migrating must not change existing rows:\n%s\n%s", before, after)
	}
	if count("SELECT count(*) FROM outbox WHERE fanned_out_at IS NOT NULL") != 0 || count("SELECT count(*) FROM outbox WHERE fanned_out_at IS NULL") != 3 {
		t.Fatal("every historical row starts unmarked; reconciliation backfills them in bounded batches")
	}
	if count("SELECT count(*) FROM pg_indexes WHERE indexname = 'outbox_unfanned_idx' AND indexdef LIKE '%WHERE (fanned_out_at IS NULL)%'") != 1 ||
		count("SELECT count(*) FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid WHERE c.relname = 'outbox_unfanned_idx' AND i.indisvalid") != 1 {
		t.Fatal("the index must be rebuilt in its final, valid shape")
	}
	var updatesAfter int64
	if err := q.QueryRow(ctx, "SELECT n_tup_upd FROM pg_stat_user_tables WHERE relname = 'outbox'").Scan(&updatesAfter); err == nil && updatesAfter != updatesBefore {
		t.Logf("n_tup_upd moved from %d to %d (statistics are asynchronous; the row comparison above is authoritative)", updatesBefore, updatesAfter)
	}

	// Roll back and re-apply on top of existing data.
	if _, err := provider.DownTo(ctx, 17); err != nil {
		t.Fatalf("second rollback: %v", err)
	}
	if count(indexes) != 0 || count(column) != 0 || count("SELECT count(*) FROM outbox") != 3 {
		t.Fatal("the rollback removes only the marker and keeps the outbox rows")
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	if count(indexes) != 1 || count(column) != 1 {
		t.Fatal("re-applying must recreate the column and the index")
	}
	if res, err := provider.Up(ctx); err != nil || len(res) != 0 {
		t.Fatalf("a second up must be a no-op: %d %v", len(res), err)
	}
}
