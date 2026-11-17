package httpapi

import (
	"net/http"
	"time"

	"github.com/Thiago-tods/relayforge/internal/app"
	"github.com/Thiago-tods/relayforge/internal/identity"
	"github.com/Thiago-tods/relayforge/internal/platform/apperror"
	"github.com/Thiago-tods/relayforge/internal/tenancy"
)

type sessionBody struct {
	ExpiresAt time.Time `json:"expiresAt"`
	CSRFToken string    `json:"csrfToken,omitempty"`
}

type authResponse struct {
	User          identity.User                  `json:"user"`
	Session       sessionBody                    `json:"session"`
	Organization  *tenancy.Organization          `json:"organization,omitempty"`
	Organizations []tenancy.OrganizationWithRole `json:"organizations,omitempty"`
}

func (a *API) register(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		Email            string `json:"email"`
		Password         string `json:"password"`
		DisplayName      string `json:"displayName"`
		OrganizationName string `json:"organizationName"`
		OrganizationSlug string `json:"organizationSlug"`
	}
	if err := decode(w, r, &req); err != nil {
		return err
	}
	res, org, err := a.svc.Register(r.Context(), app.RegisterInput{
		Email: req.Email, Password: req.Password, DisplayName: req.DisplayName,
		OrganizationName: req.OrganizationName, OrganizationSlug: req.OrganizationSlug,
	})
	if err != nil {
		return err
	}
	a.setSessionCookie(w, res.RawSessionToken, res.ExpiresAt)
	return writeJSON(w, http.StatusCreated, authResponse{
		User: res.User, Session: sessionBody{ExpiresAt: res.ExpiresAt, CSRFToken: res.CSRFToken}, Organization: &org,
	})
}

func (a *API) login(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := decode(w, r, &req); err != nil {
		return err
	}
	res, err := a.svc.LoginFromIP(r.Context(), clientIP(r), req.Email, req.Password)
	if err != nil {
		return err
	}
	orgs, err := a.svc.Memberships(r.Context(), app.Principal{User: res.User})
	if err != nil {
		return err
	}
	a.setSessionCookie(w, res.RawSessionToken, res.ExpiresAt)
	return writeJSON(w, http.StatusOK, authResponse{
		User: res.User, Session: sessionBody{ExpiresAt: res.ExpiresAt, CSRFToken: res.CSRFToken}, Organizations: orgs,
	})
}

// logout is idempotent: with no valid session it still answers 204 and clears
// the cookie. With a valid session it requires the CSRF token.
func (a *API) logout(w http.ResponseWriter, r *http.Request) error {
	p, err := a.principal(r)
	if err != nil {
		a.clearSessionCookie(w)
		w.WriteHeader(http.StatusNoContent)
		return nil
	}
	if !p.VerifyCSRF(r.Header.Get(CSRFHeader)) {
		return apperror.New(http.StatusForbidden, "csrf_invalid", "missing or invalid CSRF token")
	}
	if err := a.svc.Logout(r.Context(), p); err != nil {
		return err
	}
	a.clearSessionCookie(w)
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (a *API) me(w http.ResponseWriter, r *http.Request, p app.Principal) error {
	orgs, err := a.svc.Memberships(r.Context(), p)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, authResponse{
		User: p.User, Session: sessionBody{ExpiresAt: p.Session.ExpiresAt}, Organizations: orgs,
	})
}

// session returns session metadata. The CSRF token is not included because only
// its hash is stored; POST /auth/csrf issues a fresh one.
func (a *API) session(w http.ResponseWriter, _ *http.Request, p app.Principal) error {
	return writeJSON(w, http.StatusOK, map[string]any{"session": map[string]any{
		"expiresAt": p.Session.ExpiresAt, "createdAt": p.Session.CreatedAt, "lastSeenAt": p.Session.LastSeenAt,
	}})
}

func (a *API) rotateCSRF(w http.ResponseWriter, r *http.Request, p app.Principal) error {
	if err := requireSameOriginClient(r); err != nil {
		return err
	}
	token, err := a.svc.RotateCSRF(r.Context(), p)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, map[string]any{"session": sessionBody{ExpiresAt: p.Session.ExpiresAt, CSRFToken: token}})
}

func (a *API) createOrganization(w http.ResponseWriter, r *http.Request, p app.Principal) error {
	var req struct {
		Name string `json:"name"`
		Slug string `json:"slug"`
	}
	if err := decode(w, r, &req); err != nil {
		return err
	}
	org, err := a.svc.CreateOrganization(r.Context(), p, req.Name, req.Slug)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusCreated, map[string]any{"organization": tenancy.OrganizationWithRole{Organization: org, Role: tenancy.RoleOwner}})
}

func (a *API) listOrganizations(w http.ResponseWriter, r *http.Request, p app.Principal) error {
	orgs, err := a.svc.Memberships(r.Context(), p)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, map[string]any{"items": orgs})
}

func (a *API) getOrganization(w http.ResponseWriter, r *http.Request, p app.Principal) error {
	org, err := a.svc.GetOrganization(r.Context(), p, r.PathValue("organizationID"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, map[string]any{"organization": org})
}

func (a *API) updateOrganization(w http.ResponseWriter, r *http.Request, p app.Principal) error {
	var req struct {
		Name *string `json:"name"`
		Slug *string `json:"slug"`
	}
	if err := decode(w, r, &req); err != nil {
		return err
	}
	org, err := a.svc.UpdateOrganization(r.Context(), p, r.PathValue("organizationID"), req.Name, req.Slug)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, map[string]any{"organization": org})
}

func (a *API) listMembers(w http.ResponseWriter, r *http.Request, p app.Principal) error {
	pg, err := pagination(r)
	if err != nil {
		return err
	}
	members, total, err := a.svc.ListMembers(r.Context(), p, r.PathValue("organizationID"), pg.Limit, pg.Offset)
	if err != nil {
		return err
	}
	return writeList(w, members, total, pg)
}

func (a *API) changeRole(w http.ResponseWriter, r *http.Request, p app.Principal) error {
	var req struct {
		Role string `json:"role"`
	}
	if err := decode(w, r, &req); err != nil {
		return err
	}
	role, err := tenancy.ParseRole(req.Role)
	if err != nil {
		return app.Validation("role must be one of OWNER, ADMIN, DEVELOPER, VIEWER")
	}
	if err := a.svc.ChangeRole(r.Context(), p, r.PathValue("organizationID"), r.PathValue("userID"), role); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (a *API) removeMember(w http.ResponseWriter, r *http.Request, p app.Principal) error {
	if err := a.svc.RemoveMember(r.Context(), p, r.PathValue("organizationID"), r.PathValue("userID")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (a *API) transferOwnership(w http.ResponseWriter, r *http.Request, p app.Principal) error {
	var req struct {
		UserID string `json:"userId"`
	}
	if err := decode(w, r, &req); err != nil {
		return err
	}
	if err := a.svc.TransferOwnership(r.Context(), p, r.PathValue("organizationID"), req.UserID); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (a *API) createInvitation(w http.ResponseWriter, r *http.Request, p app.Principal) error {
	var req struct {
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	if err := decode(w, r, &req); err != nil {
		return err
	}
	role, err := tenancy.ParseRole(req.Role)
	if err != nil {
		return app.Validation("role must be one of OWNER, ADMIN, DEVELOPER, VIEWER")
	}
	inv, token, err := a.svc.CreateInvitation(r.Context(), p, r.PathValue("organizationID"), req.Email, role)
	if err != nil {
		return err
	}
	// The raw token is returned exactly once. The caller must deliver it securely.
	return writeJSON(w, http.StatusCreated, map[string]any{"invitation": inv, "token": token})
}

func (a *API) listInvitations(w http.ResponseWriter, r *http.Request, p app.Principal) error {
	pg, err := pagination(r)
	if err != nil {
		return err
	}
	items, total, err := a.svc.ListInvitations(r.Context(), p, r.PathValue("organizationID"), pg.Limit, pg.Offset)
	if err != nil {
		return err
	}
	return writeList(w, items, total, pg)
}

func (a *API) revokeInvitation(w http.ResponseWriter, r *http.Request, p app.Principal) error {
	if err := a.svc.RevokeInvitation(r.Context(), p, r.PathValue("organizationID"), r.PathValue("invitationID")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (a *API) acceptInvitation(w http.ResponseWriter, r *http.Request, p app.Principal) error {
	var req struct {
		Token string `json:"token"`
	}
	if err := decode(w, r, &req); err != nil {
		return err
	}
	org, err := a.svc.AcceptInvitation(r.Context(), p, req.Token)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, map[string]any{"organization": org})
}

func (a *API) listAuditEvents(w http.ResponseWriter, r *http.Request, p app.Principal) error {
	pg, err := pagination(r)
	if err != nil {
		return err
	}
	events, total, err := a.svc.ListAuditEvents(r.Context(), p, r.PathValue("organizationID"), pg.Limit, pg.Offset)
	if err != nil {
		return err
	}
	return writeList(w, events, total, pg)
}
