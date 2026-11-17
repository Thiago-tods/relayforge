// Package httpapi exposes the /api/v1 HTTP interface. It validates input,
// enforces cookie authentication and CSRF protection, and renders results and
// errors; authorization and business rules live in internal/app.
package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Thiago-tods/relayforge/internal/app"
	"github.com/Thiago-tods/relayforge/internal/ingest"
	"github.com/Thiago-tods/relayforge/internal/platform/apperror"
)

const (
	// SessionCookieName is the name of the session cookie.
	SessionCookieName = "relayforge_session"
	// CSRFHeader carries the synchronizer token on state-changing requests.
	CSRFHeader = "X-CSRF-Token"

	maxBodyBytes   = 64 << 10
	defaultLimit   = 50
	maxLimit       = 100
	maxOffset      = 1_000_000
	cookiePath     = "/api"
	clientHeader   = "X-Requested-With"
	clientHeaderOK = "XMLHttpRequest"
)

// API holds the dependencies of the HTTP handlers.
type API struct {
	svc          *app.Service
	ing          *ingest.Service
	log          *slog.Logger
	cookieSecure bool
}

// New creates the API. cookieSecure sets the Secure attribute on the session cookie.
func New(svc *app.Service, ing *ingest.Service, log *slog.Logger, cookieSecure bool) *API {
	return &API{svc: svc, ing: ing, log: log, cookieSecure: cookieSecure}
}

// Handler returns the router for all /api/ paths.
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()

	// Authentication. Register and login need no session and therefore no CSRF token.
	mux.HandleFunc("POST /api/v1/auth/register", a.handle(a.register))
	mux.HandleFunc("POST /api/v1/auth/login", a.handle(a.login))
	mux.HandleFunc("POST /api/v1/auth/logout", a.handle(a.logout))
	mux.HandleFunc("GET /api/v1/auth/me", a.handle(a.authed(a.me)))
	mux.HandleFunc("GET /api/v1/auth/session", a.handle(a.authed(a.session)))
	mux.HandleFunc("POST /api/v1/auth/csrf", a.handle(a.authed(a.rotateCSRF)))

	// Organizations.
	mux.HandleFunc("POST /api/v1/organizations", a.handle(a.mutating(a.createOrganization)))
	mux.HandleFunc("GET /api/v1/organizations", a.handle(a.authed(a.listOrganizations)))
	mux.HandleFunc("GET /api/v1/organizations/{organizationID}", a.handle(a.authed(a.getOrganization)))
	mux.HandleFunc("PATCH /api/v1/organizations/{organizationID}", a.handle(a.mutating(a.updateOrganization)))

	// Members and ownership.
	mux.HandleFunc("GET /api/v1/organizations/{organizationID}/members", a.handle(a.authed(a.listMembers)))
	mux.HandleFunc("PATCH /api/v1/organizations/{organizationID}/members/{userID}", a.handle(a.mutating(a.changeRole)))
	mux.HandleFunc("DELETE /api/v1/organizations/{organizationID}/members/{userID}", a.handle(a.mutating(a.removeMember)))
	mux.HandleFunc("POST /api/v1/organizations/{organizationID}/transfer-ownership", a.handle(a.mutating(a.transferOwnership)))

	// Invitations.
	mux.HandleFunc("POST /api/v1/organizations/{organizationID}/invitations", a.handle(a.mutating(a.createInvitation)))
	mux.HandleFunc("GET /api/v1/organizations/{organizationID}/invitations", a.handle(a.authed(a.listInvitations)))
	mux.HandleFunc("DELETE /api/v1/organizations/{organizationID}/invitations/{invitationID}", a.handle(a.mutating(a.revokeInvitation)))
	mux.HandleFunc("POST /api/v1/invitations/accept", a.handle(a.mutating(a.acceptInvitation)))

	// Audit.
	mux.HandleFunc("GET /api/v1/organizations/{organizationID}/audit-events", a.handle(a.authed(a.listAuditEvents)))

	a.registerIngestionRoutes(mux)

	mux.HandleFunc("/api/", a.handle(func(http.ResponseWriter, *http.Request) error { return app.ErrNotFound }))
	return mux
}

// handlerFunc is a handler that returns an error for central rendering.
type handlerFunc func(w http.ResponseWriter, r *http.Request) error

// authedFunc is a handler that receives the authenticated principal.
type authedFunc func(w http.ResponseWriter, r *http.Request, p app.Principal) error

// handle adds the headers every API response carries and renders returned errors.
func (a *API) handle(h handlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if err := h(w, r); err != nil {
			a.fail(w, r, err)
		}
	}
}

// fail renders err. Known application errors keep their status and code; every
// other error is logged and reported as a generic 500, so SQL errors, hashes
// and URLs never reach clients.
func (a *API) fail(w http.ResponseWriter, r *http.Request, err error) {
	var ae *apperror.Error
	if errors.As(err, &ae) {
		if ae.Status == http.StatusUnauthorized && ae.Code == "unauthenticated" {
			a.clearSessionCookie(w)
		}
		apperror.Write(w, ae)
		return
	}
	a.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "error", err)
	apperror.Write(w, err)
}

// authed requires a valid session cookie.
func (a *API) authed(h authedFunc) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		p, err := a.principal(r)
		if err != nil {
			return err
		}
		return h(w, r, p)
	}
}

// mutating requires a valid session and a matching CSRF token.
func (a *API) mutating(h authedFunc) handlerFunc {
	return a.authed(func(w http.ResponseWriter, r *http.Request, p app.Principal) error {
		if !p.VerifyCSRF(r.Header.Get(CSRFHeader)) {
			return apperror.New(http.StatusForbidden, "csrf_invalid", "missing or invalid CSRF token")
		}
		return h(w, r, p)
	})
}

func (a *API) principal(r *http.Request) (app.Principal, error) {
	c, err := r.Cookie(SessionCookieName)
	if err != nil || c.Value == "" {
		return app.Principal{}, app.ErrUnauthenticated
	}
	return a.svc.Authenticate(r.Context(), c.Value)
}

func (a *API) setSessionCookie(w http.ResponseWriter, token string, expires time.Time) {
	maxAge := int(time.Until(expires).Seconds())
	if maxAge < 1 {
		maxAge = 1
	}
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookieName, Value: token, Path: cookiePath, Expires: expires, MaxAge: maxAge,
		HttpOnly: true, Secure: a.cookieSecure, SameSite: http.SameSiteLaxMode,
	})
}

func (a *API) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookieName, Value: "", Path: cookiePath, MaxAge: -1,
		Expires: time.Unix(0, 0), HttpOnly: true, Secure: a.cookieSecure, SameSite: http.SameSiteLaxMode,
	})
}

// decode reads a single strict JSON object from the request body.
func decode(w http.ResponseWriter, r *http.Request, dst any) error {
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		return apperror.New(http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json")
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return apperror.New(http.StatusRequestEntityTooLarge, "request_too_large", "request body is too large")
		}
		return apperror.New(http.StatusBadRequest, "invalid_json", "request body is not valid JSON for this endpoint")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return apperror.New(http.StatusBadRequest, "invalid_json", "request body must contain a single JSON object")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) error {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	return json.NewEncoder(w).Encode(v)
}

type page struct {
	Limit  int
	Offset int
}

// pagination parses ?limit= and ?offset=.
func pagination(r *http.Request) (page, error) {
	p := page{Limit: defaultLimit}
	q := r.URL.Query()
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxLimit {
			return page{}, app.Validation("limit must be an integer from 1 to 100")
		}
		p.Limit = n
	}
	if v := q.Get("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > maxOffset {
			return page{}, app.Validation("offset must be a non-negative integer")
		}
		p.Offset = n
	}
	return p, nil
}

type listResponse[T any] struct {
	Items  []T `json:"items"`
	Total  int `json:"total"`
	Limit  int `json:"limit"`
	Offset int `json:"offset"`
}

func writeList[T any](w http.ResponseWriter, items []T, total int, p page) error {
	return writeJSON(w, http.StatusOK, listResponse[T]{Items: items, Total: total, Limit: p.Limit, Offset: p.Offset})
}

// requireSameOriginClient guards the one authenticated POST that is exempt from
// the CSRF header (CSRF token rotation). The custom header cannot be set by a
// cross-site form or simple request, and any Origin must match the host.
func requireSameOriginClient(r *http.Request) error {
	if r.Header.Get(clientHeader) != clientHeaderOK {
		return apperror.New(http.StatusForbidden, "csrf_invalid", "missing required client header")
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || !strings.EqualFold(u.Host, r.Host) {
			return apperror.New(http.StatusForbidden, "csrf_invalid", "cross-origin request rejected")
		}
	}
	return nil
}
