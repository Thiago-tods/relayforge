package app_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Thiago-tods/relayforge/internal/app"
	"github.com/Thiago-tods/relayforge/internal/app/apptest"
	"github.com/Thiago-tods/relayforge/internal/identity"
)

// shortDeadlineService is a second service over the same database whose operational reads give up
// after d. Sessions live in the database, so principals of env work with it.
func shortDeadlineService(t *testing.T, env *apptest.Env, d time.Duration) *app.Service {
	t.Helper()
	cfg := app.DefaultConfig()
	cfg.Box = env.Box
	cfg.OperationsDeadline = d
	svc, err := app.New(env.Pool, identity.NewHasher(apptest.FastArgon2Params), cfg, env.Clock.Now, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

// blockDeliveries holds an ACCESS EXCLUSIVE lock on the deliveries table for the duration of fn, so
// any read of that table waits, exactly like a statement that is too slow. Authorization reads other
// tables and is not blocked.
func blockDeliveries(t *testing.T, env *apptest.Env, fn func()) {
	t.Helper()
	err := env.Pool.InTx(context.Background(), func(tx pgx.Tx) error {
		if _, err := tx.Exec(context.Background(), "LOCK TABLE deliveries IN ACCESS EXCLUSIVE MODE"); err != nil {
			return err
		}
		fn()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestOperationalReadsGiveUpAtTheirDeadlineWithATimeoutError(t *testing.T) {
	env := apptest.New(t)
	u := env.Register(t, "owner@example.com")
	svc := shortDeadlineService(t, env, 300*time.Millisecond)

	blockDeliveries(t, env, func() {
		for name, call := range map[string]func() error{
			"queue": func() error { _, err := svc.QueueHealth(context.Background(), u.Principal, u.Org.ID); return err },
			"summary": func() error {
				_, err := svc.DeliverySummary(context.Background(), u.Principal, u.Org.ID, "", "")
				return err
			},
		} {
			start := time.Now()
			err := call()
			if !errors.Is(err, app.ErrOperationsTimeout) {
				t.Errorf("%s: err = %v, want ErrOperationsTimeout", name, err)
			}
			if took := time.Since(start); took < 250*time.Millisecond || took > 5*time.Second {
				t.Errorf("%s: gave up after %v, want about the 300ms deadline (the read must wait for the lock, then be cancelled)", name, took)
			}
		}
	})

	// The lock is gone and the connections were released: the same service answers normally.
	if _, err := svc.QueueHealth(context.Background(), u.Principal, u.Org.ID); err != nil {
		t.Fatalf("after the lock is released: %v", err)
	}
}

func TestClientCancellationIsNotATimeout(t *testing.T) {
	env := apptest.New(t)
	u := env.Register(t, "owner@example.com")
	svc := shortDeadlineService(t, env, time.Minute) // the deadline is far away: only the client gives up

	blockDeliveries(t, env, func() {
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(200*time.Millisecond, cancel)
		start := time.Now()
		_, err := svc.DeliverySummary(ctx, u.Principal, u.Org.ID, "", "")
		if err == nil || errors.Is(err, app.ErrOperationsTimeout) || !errors.Is(err, context.Canceled) {
			t.Fatalf("a client that goes away is not a timeout: %v", err)
		}
		if took := time.Since(start); took > 10*time.Second {
			t.Fatalf("the cancelled read did not stop: %v", took)
		}
	})
}

func TestAnInvalidWindowOrTimestampNeverBeatsAuthorization(t *testing.T) {
	env := apptest.New(t)
	owner := env.Register(t, "owner@example.com")
	stranger := env.Register(t, "stranger@example.com")
	for _, c := range []struct{ from, to string }{{"soon", ""}, {"0000-01-01T00:00:00Z", "0000-01-02T00:00:00Z"}, {"2020-01-01T00:00:00Z", "2026-01-01T00:00:00Z"}} {
		if _, err := env.Svc.DeliverySummary(context.Background(), stranger.Principal, owner.Org.ID, c.from, c.to); !errors.Is(err, app.ErrNotFound) {
			t.Errorf("a non-member with window %+v: %v, want not found, not a validation error", c, err)
		}
	}
	// A member gets the validation error for the same input.
	if _, err := env.Svc.DeliverySummary(context.Background(), owner.Principal, owner.Org.ID, "0000-01-01T00:00:00Z", "0000-01-02T00:00:00Z"); err == nil || errors.Is(err, app.ErrNotFound) {
		t.Errorf("a member with year 0000: %v, want a validation error", err)
	}
}

// With the deliveries table locked, any read of it would wait for the deadline. An unsupported
// window is refused at once, which proves it is rejected before the table is touched.
func TestAnUnsupportedWindowIsRejectedBeforeTheDeliveriesTableIsRead(t *testing.T) {
	env := apptest.New(t)
	u := env.Register(t, "owner@example.com")
	svc := shortDeadlineService(t, env, 30*time.Second) // a read that did reach the table would wait far longer than the test
	blockDeliveries(t, env, func() {
		for _, w := range []struct{ from, to string }{
			{"0000-01-01T00:00:00Z", "0000-01-02T00:00:00Z"},
			{"0001-01-01T00:00:00+14:00", "0001-01-02T00:00:00Z"},
			{"9999-12-31T00:00:00Z", "9999-12-31T23:59:59-05:00"},
			{"2026-01-01T00:00:00Z", "2026-03-01T00:00:00Z"},
			{"not a time", ""},
		} {
			start := time.Now()
			_, err := svc.DeliverySummary(context.Background(), u.Principal, u.Org.ID, w.from, w.to)
			if err == nil || errors.Is(err, app.ErrOperationsTimeout) || time.Since(start) > 5*time.Second {
				t.Errorf("window %+v: %v after %v, want an immediate validation error", w, err, time.Since(start))
			}
		}
	})
}
