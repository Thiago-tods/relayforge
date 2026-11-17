package ingest_test

import (
	"io"
	"log/slog"
	"strconv"
)

func formatInt(n int64) string { return strconv.FormatInt(n, 10) }

func nopLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
