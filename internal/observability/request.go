package observability

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// RequestIDHeader carries the correlation identifier of one HTTP request.
const RequestIDHeader = "X-Request-Id"

type requestIDKey struct{}

// validRequestID accepts a caller-supplied identifier only when it is short and
// made of safe characters, so a client can never inject log lines or large values.
var validRequestID = regexp.MustCompile(`^[A-Za-z0-9._-]{8,64}$`)

// NewRequestID returns a random 128-bit identifier in hexadecimal.
func NewRequestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// WithRequestID returns ctx carrying the request identifier.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// RequestIDFrom returns the request identifier of ctx, or "" when there is none.
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += int64(n)
	return n, err
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// quietRoutes are probes and scrapes: logged at debug level so they do not
// drown real traffic.
var quietRoutes = map[string]bool{"GET /healthz": true, "GET /readyz": true, "GET /metrics": true}

// RequestLog assigns every request an identifier, returns it in the response
// header, and writes one structured access line when the request ends.
//
// The line carries the route pattern (not the raw path, so identifiers and
// tokens in URLs never reach the log and cardinality stays bounded), the
// method, status, duration and size. The query string, headers and body are never logged.
func RequestLog(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(RequestIDHeader)
		if !validRequestID.MatchString(id) {
			id = NewRequestID()
		}
		w.Header().Set(RequestIDHeader, id)
		r = r.WithContext(WithRequestID(r.Context(), id))
		rec := &statusRecorder{ResponseWriter: w}
		start := time.Now()
		next.ServeHTTP(rec, r)

		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}
		route := r.Pattern
		if route == "" {
			route = "unmatched"
		}
		level := slog.LevelInfo
		switch {
		case quietRoutes[route]:
			level = slog.LevelDebug
		case status >= 500:
			level = slog.LevelError
		case status >= 400:
			level = slog.LevelWarn
		}
		log.Log(r.Context(), level, "http request", "request_id", id, "route", route, "method", r.Method,
			"status", status, "duration_ms", time.Since(start).Milliseconds(), "bytes", rec.bytes)
	})
}

// SafeError renders err for a log line without values that PostgreSQL puts in
// error details (a duplicate key error repeats the offending key, which can be
// an e-mail address or an idempotency key). For a database error it keeps the
// SQLSTATE, the constraint and the generic message; any other error keeps its text.
func SafeError(err error) string {
	if err == nil {
		return ""
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		var b strings.Builder
		b.WriteString("database error ")
		b.WriteString(pg.Code)
		if pg.ConstraintName != "" {
			b.WriteString(" constraint=")
			b.WriteString(pg.ConstraintName)
		}
		return b.String()
	}
	return err.Error()
}
