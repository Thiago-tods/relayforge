package httpapi

import (
	"net/http"

	"github.com/Thiago-tods/relayforge/internal/app"
)

// registerOperationsRoutes adds the operational read models (ADR 31). Both are GET-only,
// organization-scoped and need deliveries:read. There is deliberately no mutating route.
func (a *API) registerOperationsRoutes(mux *http.ServeMux) {
	const ops = "/api/v1/organizations/{organizationID}/operations"
	mux.HandleFunc("GET "+ops+"/queue", a.handle(a.authed(a.queueHealth)))
	mux.HandleFunc("GET "+ops+"/deliveries", a.handle(a.authed(a.deliverySummary)))
	// ADR 32.
	mux.HandleFunc("GET "+ops+"/failures", a.handle(a.authed(a.failureFeed)))
	mux.HandleFunc("GET "+ops+"/queue/age", a.handle(a.authed(a.queueAge)))
}

func (a *API) queueAge(w http.ResponseWriter, r *http.Request, p app.Principal) error {
	out, err := a.svc.QueueAge(r.Context(), p, r.PathValue("organizationID"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, out)
}

// failureFeed passes the raw query text to the service, which validates it only after authorization (a
// non-member gets 404, never a validation error). Unknown parameters are ignored, like everywhere else.
func (a *API) failureFeed(w http.ResponseWriter, r *http.Request, p app.Principal) error {
	q := r.URL.Query()
	out, err := a.svc.FailureFeed(r.Context(), p, r.PathValue("organizationID"), app.FailureFeedQuery{
		Limit: q.Get("limit"), Cursor: q.Get("cursor"), FailureReason: q.Get("failureReason"),
		DestinationID: q.Get("destinationId"), From: q.Get("from"), To: q.Get("to"),
	})
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, out)
}

func (a *API) queueHealth(w http.ResponseWriter, r *http.Request, p app.Principal) error {
	q, err := a.svc.QueueHealth(r.Context(), p, r.PathValue("organizationID"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, q)
}

// deliverySummary reads `from` and `to`. Like every other route, it ignores query parameters it
// does not know.
func (a *API) deliverySummary(w http.ResponseWriter, r *http.Request, p app.Principal) error {
	q := r.URL.Query()
	s, err := a.svc.DeliverySummary(r.Context(), p, r.PathValue("organizationID"), q.Get("from"), q.Get("to"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, s)
}
