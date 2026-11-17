package httpapi

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/Thiago-tods/relayforge/internal/app"
	"github.com/Thiago-tods/relayforge/internal/ingest"
	"github.com/Thiago-tods/relayforge/internal/platform/apperror"
)

// ingestTimeout bounds one ingestion request after its body has been read.
const ingestTimeout = 15 * time.Second

func (a *API) registerIngestionRoutes(mux *http.ServeMux) {
	// Public ingestion: authenticated only by the endpoint's HMAC signature, never by cookie.
	mux.HandleFunc("POST /api/v1/ingest/{publicEndpointID}", a.handle(a.ingest))

	const eps = "/api/v1/organizations/{organizationID}/ingestion-endpoints"
	mux.HandleFunc("POST "+eps, a.handle(a.mutating(a.createEndpoint)))
	mux.HandleFunc("GET "+eps, a.handle(a.authed(a.listEndpoints)))
	mux.HandleFunc("GET "+eps+"/{endpointID}", a.handle(a.authed(a.getEndpoint)))
	mux.HandleFunc("PATCH "+eps+"/{endpointID}", a.handle(a.mutating(a.updateEndpoint)))
	mux.HandleFunc("POST "+eps+"/{endpointID}/pause", a.handle(a.mutating(a.pauseEndpoint)))
	mux.HandleFunc("POST "+eps+"/{endpointID}/activate", a.handle(a.mutating(a.activateEndpoint)))
	mux.HandleFunc("POST "+eps+"/{endpointID}/rotate-secret", a.handle(a.mutating(a.rotateSecret)))
	// DELETE revokes: the record and its events are kept, and revocation is permanent.
	mux.HandleFunc("DELETE "+eps+"/{endpointID}", a.handle(a.mutating(a.revokeEndpoint)))

	mux.HandleFunc("GET /api/v1/organizations/{organizationID}/events", a.handle(a.authed(a.listEvents)))
	mux.HandleFunc("GET /api/v1/organizations/{organizationID}/events/{eventID}", a.handle(a.authed(a.getEvent)))
}

// clientIP returns the address of the TCP peer. Forwarding headers are ignored
// because they are client-controlled unless a trusted proxy sets them; behind a
// reverse proxy every client would share the proxy's address (see SECURITY.md).
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// singleHeader returns the only value of a header. Repeated headers are
// ambiguous, so they yield an error value that fails validation downstream.
func singleHeader(r *http.Request, name string) string {
	v := r.Header.Values(name)
	switch len(v) {
	case 0:
		return ""
	case 1:
		return v[0]
	default:
		return "\x00duplicate"
	}
}

func (a *API) ingest(w http.ResponseWriter, r *http.Request) error {
	ctx, cancel := context.WithTimeout(r.Context(), ingestTimeout)
	defer cancel()

	ep, err := a.ing.Lookup(ctx, r.PathValue("publicEndpointID"), clientIP(r))
	if err != nil {
		return err
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, ingest.MaxBodyBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return apperror.New(http.StatusRequestEntityTooLarge, "request_too_large", "request body must not exceed 1 MiB")
		}
		return apperror.New(http.StatusBadRequest, "invalid_body", "request body could not be read")
	}

	res, err := a.ing.Accept(ctx, ep, ingest.Request{
		ContentType:    r.Header.Get("Content-Type"),
		Timestamp:      singleHeader(r, ingest.HeaderTimestamp),
		Signature:      singleHeader(r, ingest.HeaderSignature),
		IdempotencyKey: singleHeader(r, ingest.HeaderIdempotencyKey),
		Body:           body,
	})
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusAccepted, map[string]any{
		"event":     map[string]any{"id": res.EventID, "status": ingest.EventReceived},
		"duplicate": res.Duplicate,
	})
}

// endpointJSON adds the ingestion path, and the raw secret when it may be shown.
type endpointJSON struct {
	ingest.Endpoint
	IngestPath    string `json:"ingestPath"`
	SigningSecret string `json:"signingSecret,omitempty"`
}

func endpointView(e ingest.Endpoint, secret string) endpointJSON {
	return endpointJSON{Endpoint: e, IngestPath: "/api/v1/ingest/" + e.PublicID, SigningSecret: secret}
}

func (a *API) createEndpoint(w http.ResponseWriter, r *http.Request, p app.Principal) error {
	var req struct {
		Name string `json:"name"`
	}
	if err := decode(w, r, &req); err != nil {
		return err
	}
	res, err := a.svc.CreateEndpoint(r.Context(), p, r.PathValue("organizationID"), req.Name)
	if err != nil {
		return err
	}
	// The raw secret is shown exactly once and cannot be retrieved later.
	return writeJSON(w, http.StatusCreated, map[string]any{"endpoint": endpointView(res.Endpoint, res.SigningSecret)})
}

func (a *API) listEndpoints(w http.ResponseWriter, r *http.Request, p app.Principal) error {
	pg, err := pagination(r)
	if err != nil {
		return err
	}
	items, total, err := a.svc.ListEndpoints(r.Context(), p, r.PathValue("organizationID"), pg.Limit, pg.Offset)
	if err != nil {
		return err
	}
	views := make([]endpointJSON, len(items))
	for i, e := range items {
		views[i] = endpointView(e, "")
	}
	return writeList(w, views, total, pg)
}

func (a *API) getEndpoint(w http.ResponseWriter, r *http.Request, p app.Principal) error {
	e, err := a.svc.GetEndpoint(r.Context(), p, r.PathValue("organizationID"), r.PathValue("endpointID"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, map[string]any{"endpoint": endpointView(e, "")})
}

func (a *API) updateEndpoint(w http.ResponseWriter, r *http.Request, p app.Principal) error {
	var req struct {
		Name string `json:"name"`
	}
	if err := decode(w, r, &req); err != nil {
		return err
	}
	e, err := a.svc.UpdateEndpoint(r.Context(), p, r.PathValue("organizationID"), r.PathValue("endpointID"), req.Name)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, map[string]any{"endpoint": endpointView(e, "")})
}

func (a *API) pauseEndpoint(w http.ResponseWriter, r *http.Request, p app.Principal) error {
	e, err := a.svc.PauseEndpoint(r.Context(), p, r.PathValue("organizationID"), r.PathValue("endpointID"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, map[string]any{"endpoint": endpointView(e, "")})
}

func (a *API) activateEndpoint(w http.ResponseWriter, r *http.Request, p app.Principal) error {
	e, err := a.svc.ActivateEndpoint(r.Context(), p, r.PathValue("organizationID"), r.PathValue("endpointID"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, map[string]any{"endpoint": endpointView(e, "")})
}

func (a *API) rotateSecret(w http.ResponseWriter, r *http.Request, p app.Principal) error {
	res, err := a.svc.RotateSecret(r.Context(), p, r.PathValue("organizationID"), r.PathValue("endpointID"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, map[string]any{"endpoint": endpointView(res.Endpoint, res.SigningSecret)})
}

func (a *API) revokeEndpoint(w http.ResponseWriter, r *http.Request, p app.Principal) error {
	e, err := a.svc.RevokeEndpoint(r.Context(), p, r.PathValue("organizationID"), r.PathValue("endpointID"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, map[string]any{"endpoint": endpointView(e, "")})
}

// eventJSON presents event metadata, and the payload only on the detail endpoint.
type eventJSON struct {
	ingest.Event
	PayloadSHA256 string          `json:"payloadSha256"`
	Payload       json.RawMessage `json:"payload,omitempty"`
}

func eventView(e ingest.Event, withPayload bool) eventJSON {
	v := eventJSON{Event: e, PayloadSHA256: hex.EncodeToString(e.PayloadSHA256)}
	if withPayload {
		v.Payload = json.RawMessage(e.Payload)
	}
	return v
}

func (a *API) listEvents(w http.ResponseWriter, r *http.Request, p app.Principal) error {
	pg, err := pagination(r)
	if err != nil {
		return err
	}
	q := r.URL.Query()
	f := app.EventFilter{EndpointID: q.Get("endpointId"), Status: q.Get("status")}
	for name, dst := range map[string]*time.Time{"from": &f.From, "to": &f.To} {
		if raw := q.Get(name); raw != "" {
			t, err := time.Parse(time.RFC3339, raw)
			if err != nil {
				return app.Validation(name + " must be an RFC 3339 timestamp")
			}
			*dst = t
		}
	}
	items, total, err := a.svc.ListEvents(r.Context(), p, r.PathValue("organizationID"), f, pg.Limit, pg.Offset)
	if err != nil {
		return err
	}
	views := make([]eventJSON, len(items))
	for i, e := range items {
		views[i] = eventView(e, false)
	}
	return writeList(w, views, total, pg)
}

func (a *API) getEvent(w http.ResponseWriter, r *http.Request, p app.Principal) error {
	e, err := a.svc.GetEvent(r.Context(), p, r.PathValue("organizationID"), r.PathValue("eventID"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, map[string]any{"event": eventView(e, true)})
}
