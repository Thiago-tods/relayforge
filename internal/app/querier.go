package app

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// failingQuerier is returned when the pool is closed or missing. Every call
// reports the original error, so use cases fail with a normal error value.
type failingQuerier struct{ err error }

func (f failingQuerier) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, f.err
}

func (f failingQuerier) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, f.err
}

func (f failingQuerier) QueryRow(context.Context, string, ...any) pgx.Row { return failingRow{f.err} }

type failingRow struct{ err error }

func (r failingRow) Scan(...any) error { return r.err }
