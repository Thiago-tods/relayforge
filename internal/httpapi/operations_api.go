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
