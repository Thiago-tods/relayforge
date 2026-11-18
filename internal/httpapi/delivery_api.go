package httpapi

import (
	"net/http"
	"time"

	"github.com/Thiago-tods/relayforge/internal/app"
	"github.com/Thiago-tods/relayforge/internal/delivery"
)

func (a *API) registerDeliveryRoutes(mux *http.ServeMux) {
	const org = "/api/v1/organizations/{organizationID}"

	const dst = org + "/destinations"
	mux.HandleFunc("POST "+dst, a.handle(a.mutating(a.createDestination)))
	mux.HandleFunc("GET "+dst, a.handle(a.authed(a.listDestinations)))
	mux.HandleFunc("GET "+dst+"/{destinationID}", a.handle(a.authed(a.getDestination)))
	mux.HandleFunc("PATCH "+dst+"/{destinationID}", a.handle(a.mutating(a.updateDestination)))
	mux.HandleFunc("POST "+dst+"/{destinationID}/pause", a.handle(a.mutating(a.pauseDestination)))
	mux.HandleFunc("POST "+dst+"/{destinationID}/activate", a.handle(a.mutating(a.activateDestination)))
	mux.HandleFunc("POST "+dst+"/{destinationID}/rotate-secret", a.handle(a.mutating(a.rotateDestinationSecret)))
	// DELETE revokes: the record and its delivery history are kept, and revocation is permanent.
	mux.HandleFunc("DELETE "+dst+"/{destinationID}", a.handle(a.mutating(a.revokeDestination)))

	const sub = org + "/subscriptions"
	mux.HandleFunc("POST "+sub, a.handle(a.mutating(a.createSubscription)))
	mux.HandleFunc("GET "+sub, a.handle(a.authed(a.listSubscriptions)))
	mux.HandleFunc("GET "+sub+"/{subscriptionID}", a.handle(a.authed(a.getSubscription)))
	// DELETE revokes: the record stays as history (ADR 26).
	mux.HandleFunc("DELETE "+sub+"/{subscriptionID}", a.handle(a.mutating(a.revokeSubscription)))

	// Delivery history is read-only: there is deliberately no mutating route.
	mux.HandleFunc("GET "+org+"/deliveries", a.handle(a.authed(a.listDeliveries)))
	mux.HandleFunc("GET "+org+"/deliveries/{deliveryID}", a.handle(a.authed(a.getDelivery)))
}

// destinationJSON presents a destination. The URL is shown without its query
// string, which may hold a secret token; the raw secret appears only on creation
// and rotation. Ciphertext and key and secret versions are never serialized.
type destinationJSON struct {
	delivery.Destination
	URL           string `json:"url"`
	URLHasQuery   bool   `json:"urlHasQuery"`
	SigningSecret string `json:"signingSecret,omitempty"`
}

func destinationView(d delivery.Destination, secret string) destinationJSON {
	display, hasQuery := d.DisplayURL()
	return destinationJSON{Destination: d, URL: display, URLHasQuery: hasQuery, SigningSecret: secret}
}

func (a *API) createDestination(w http.ResponseWriter, r *http.Request, p app.Principal) error {
	var req struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	}
	if err := decode(w, r, &req); err != nil {
		return err
	}
	res, err := a.svc.CreateDestination(r.Context(), p, r.PathValue("organizationID"), req.Name, req.URL)
	if err != nil {
		return err
	}
	// The raw secret is shown exactly once and cannot be retrieved later.
	return writeJSON(w, http.StatusCreated, map[string]any{"destination": destinationView(res.Destination, res.SigningSecret)})
}

func (a *API) listDestinations(w http.ResponseWriter, r *http.Request, p app.Principal) error {
	pg, err := pagination(r)
	if err != nil {
		return err
	}
	items, total, err := a.svc.ListDestinations(r.Context(), p, r.PathValue("organizationID"), r.URL.Query().Get("status"), pg.Limit, pg.Offset)
	if err != nil {
		return err
	}
	views := make([]destinationJSON, len(items))
	for i, d := range items {
		views[i] = destinationView(d, "")
	}
	return writeList(w, views, total, pg)
}

func (a *API) getDestination(w http.ResponseWriter, r *http.Request, p app.Principal) error {
	d, err := a.svc.GetDestination(r.Context(), p, r.PathValue("organizationID"), r.PathValue("destinationID"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, map[string]any{"destination": destinationView(d, "")})
}

func (a *API) updateDestination(w http.ResponseWriter, r *http.Request, p app.Principal) error {
	var req struct {
		Name *string `json:"name"`
		URL  *string `json:"url"`
	}
	if err := decode(w, r, &req); err != nil {
		return err
	}
	d, err := a.svc.UpdateDestination(r.Context(), p, r.PathValue("organizationID"), r.PathValue("destinationID"),
		app.DestinationChanges{Name: req.Name, URL: req.URL})
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, map[string]any{"destination": destinationView(d, "")})
}

func (a *API) pauseDestination(w http.ResponseWriter, r *http.Request, p app.Principal) error {
	d, err := a.svc.PauseDestination(r.Context(), p, r.PathValue("organizationID"), r.PathValue("destinationID"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, map[string]any{"destination": destinationView(d, "")})
}

func (a *API) activateDestination(w http.ResponseWriter, r *http.Request, p app.Principal) error {
	d, err := a.svc.ActivateDestination(r.Context(), p, r.PathValue("organizationID"), r.PathValue("destinationID"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, map[string]any{"destination": destinationView(d, "")})
}

func (a *API) rotateDestinationSecret(w http.ResponseWriter, r *http.Request, p app.Principal) error {
	res, err := a.svc.RotateDestinationSecret(r.Context(), p, r.PathValue("organizationID"), r.PathValue("destinationID"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, map[string]any{"destination": destinationView(res.Destination, res.SigningSecret)})
}

func (a *API) revokeDestination(w http.ResponseWriter, r *http.Request, p app.Principal) error {
	d, err := a.svc.RevokeDestination(r.Context(), p, r.PathValue("organizationID"), r.PathValue("destinationID"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, map[string]any{"destination": destinationView(d, "")})
}

func (a *API) createSubscription(w http.ResponseWriter, r *http.Request, p app.Principal) error {
	var req struct {
		EndpointID    string `json:"endpointId"`
		DestinationID string `json:"destinationId"`
	}
	if err := decode(w, r, &req); err != nil {
		return err
	}
	s, err := a.svc.CreateSubscription(r.Context(), p, r.PathValue("organizationID"), req.EndpointID, req.DestinationID)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusCreated, map[string]any{"subscription": s})
}

func (a *API) listSubscriptions(w http.ResponseWriter, r *http.Request, p app.Principal) error {
	pg, err := pagination(r)
	if err != nil {
		return err
	}
	q := r.URL.Query()
	items, total, err := a.svc.ListSubscriptions(r.Context(), p, r.PathValue("organizationID"),
		app.SubscriptionFilter{EndpointID: q.Get("endpointId"), DestinationID: q.Get("destinationId"), Status: q.Get("status")}, pg.Limit, pg.Offset)
	if err != nil {
		return err
	}
	return writeList(w, items, total, pg)
}

func (a *API) getSubscription(w http.ResponseWriter, r *http.Request, p app.Principal) error {
	s, err := a.svc.GetSubscription(r.Context(), p, r.PathValue("organizationID"), r.PathValue("subscriptionID"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, map[string]any{"subscription": s})
}

func (a *API) revokeSubscription(w http.ResponseWriter, r *http.Request, p app.Principal) error {
	s, err := a.svc.RevokeSubscription(r.Context(), p, r.PathValue("organizationID"), r.PathValue("subscriptionID"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, map[string]any{"subscription": s})
}

func (a *API) listDeliveries(w http.ResponseWriter, r *http.Request, p app.Principal) error {
	pg, err := pagination(r)
	if err != nil {
		return err
	}
	q := r.URL.Query()
	f := app.DeliveryFilter{DestinationID: q.Get("destinationId"), EventID: q.Get("eventId"), Status: q.Get("status")}
	for name, dst := range map[string]*time.Time{"from": &f.From, "to": &f.To} {
		if raw := q.Get(name); raw != "" {
			t, err := time.Parse(time.RFC3339, raw)
			if err != nil {
				return app.Validation(name + " must be an RFC 3339 timestamp")
			}
			*dst = t
		}
	}
	items, total, err := a.svc.ListDeliveries(r.Context(), p, r.PathValue("organizationID"), f, pg.Limit, pg.Offset)
	if err != nil {
		return err
	}
	return writeList(w, items, total, pg)
}

func (a *API) getDelivery(w http.ResponseWriter, r *http.Request, p app.Principal) error {
	d, err := a.svc.GetDelivery(r.Context(), p, r.PathValue("organizationID"), r.PathValue("deliveryID"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, map[string]any{"delivery": d.Delivery, "attempts": d.Attempts})
}
