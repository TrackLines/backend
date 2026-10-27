package workweek

import (
	"context"
	"net/http"

	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/httpx"
)

var (
	ErrAdmins   = httpx.Code("admins_working_week", "only organization admins, signed in, can change the working week")
	ErrManagers = httpx.Code("managers_working_week", "only someone who manages this project, signed in, can change its working week")
)

// System serves the working week. Everyone in the org reads it (it drives what they see); admins set the
// organization's, a project's managers its own. People only, like the other settings.
type System struct {
	Store            Store
	IsAdmin          func(ctx context.Context, org, user string) (bool, error)
	IsProjectManager func(ctx context.Context, org, user, project string) (bool, error)
}

// Get: GET /api/working-week: the organization's week and every project's own.
func (h System) Get(w http.ResponseWriter, r *http.Request) {
	all, err := h.Store.All(r.Context(), auth.OrgID(r.Context()))
	if err != nil {
		httpx.Error(w, r, http.StatusInternalServerError, err)
		return
	}
	httpx.JSON(w, http.StatusOK, all)
}

// person is the signed-in person (never an API key) in an org, or "".
func person(r *http.Request) (org, user string) {
	user, ok := auth.UserID(r.Context())
	org = auth.OrgID(r.Context())
	if !ok || org == "" || auth.ViaAPIKey(r.Context()) {
		return "", ""
	}
	return org, user
}

// SetOrg: PUT /api/working-week {days, week_start, start_minute, end_minute}: org admins.
func (h System) SetOrg(w http.ResponseWriter, r *http.Request) {
	org, user := person(r)
	if ok, _ := h.IsAdmin(r.Context(), org, user); user == "" || !ok {
		httpx.Error(w, r, http.StatusForbidden, ErrAdmins)
		return
	}
	h.save(w, r, org, "", user)
}

// SetProject: PUT /api/projects/{id}/working-week: the project's own week, by its managers.
func (h System) SetProject(w http.ResponseWriter, r *http.Request) {
	org, user, ok := h.manager(w, r)
	if ok {
		h.save(w, r, org, r.PathValue("id"), user)
	}
}

// ResetProject: DELETE /api/projects/{id}/working-week: the project follows the organization's week again.
func (h System) ResetProject(w http.ResponseWriter, r *http.Request) {
	org, user, ok := h.manager(w, r)
	if !ok {
		return
	}
	if err := h.Store.Reset(r.Context(), org, r.PathValue("id"), user); err != nil {
		httpx.Error(w, r, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h System) manager(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	org, user := person(r)
	if user == "" {
		httpx.Error(w, r, http.StatusForbidden, ErrManagers)
		return "", "", false
	}
	ok, err := h.IsProjectManager(r.Context(), org, user, r.PathValue("id"))
	if err != nil || !ok {
		httpx.Error(w, r, http.StatusForbidden, ErrManagers) // another org's project too: nothing to tell
		return "", "", false
	}
	return org, user, true
}

func (h System) save(w http.ResponseWriter, r *http.Request, org, project, user string) {
	var in Week
	if !httpx.Decode(w, r, &in) {
		return
	}
	if !in.Valid() {
		httpx.Error(w, r, http.StatusBadRequest, ErrInvalid)
		return
	}
	if err := h.Store.Set(r.Context(), org, project, user, in); err != nil {
		httpx.Error(w, r, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
