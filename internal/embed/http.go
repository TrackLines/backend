package embed

import (
	"context"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/httpx"
)

// System is the signed-in management side: a project's managers choose which sites may show its
// roadmaps and request form, and the keys their widgets count usage under (TL-683). The public
// read/submit routes are the existing roadmap and intake handlers; this package only adds their dynamic
// CORS (see CORS). Every route is /api/projects/{id}/embed/…, for a project of the caller's org.
type System struct {
	store    Store
	managers Managers
}

// Managers says whether user manages project in org: an org admin, or a leader of a team with a board in
// it (organizations.Admins.IsProjectManager), as for the project's roadmaps.
type Managers interface {
	IsProjectManager(ctx context.Context, org, user, project string) (bool, error)
}

func NewSystem(db *pgxpool.Pool) System { return System{store: Store{DB: db}} }

// WithManagers lets a project's managers change its embedding (allowing an origin opens the API to it).
func (h System) WithManagers(m Managers) System { h.managers = m; return h }

// ErrNotManager: only a project's managers, signed in, change where its widgets may be shown.
var ErrNotManager = httpx.Code("embed_managers", "only someone who manages this project, signed in, can change where its roadmaps and request form can be embedded")

func writeErr(w http.ResponseWriter, r *http.Request, err error) {
	if httpx.Canceled(err) {
		return
	}
	switch {
	case errors.Is(err, ErrInvalid), errors.Is(err, ErrTooMany):
		httpx.Error(w, r, http.StatusBadRequest, err)
	case errors.Is(err, ErrNotManager):
		httpx.Error(w, r, http.StatusForbidden, err)
	case errors.Is(err, ErrNotFound):
		http.Error(w, "not found", http.StatusNotFound)
	default:
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// project is the request's project, if it's the caller's org's; otherwise it answers 404 and returns "".
func (h System) project(w http.ResponseWriter, r *http.Request) string {
	id := r.PathValue("id")
	if err := h.store.InOrg(r.Context(), auth.OrgID(r.Context()), id); err != nil {
		writeErr(w, r, err)
		return ""
	}
	return id
}

// canManage: a signed-in project manager, never an API key — allowing an origin exposes the API to it,
// so it's a person's decision (mirrors the request form, TL-432).
func (h System) canManage(r *http.Request, project string) (bool, error) {
	ctx := r.Context()
	if auth.ViaAPIKey(ctx) || h.managers == nil {
		return false, nil
	}
	user, _ := auth.UserID(ctx)
	return h.managers.IsProjectManager(ctx, auth.OrgID(ctx), user, project)
}

// manager is the request's project when the caller may change its embedding; otherwise it answers and
// returns "".
func (h System) manager(w http.ResponseWriter, r *http.Request) string {
	project := h.project(w, r)
	if project == "" {
		return ""
	}
	ok, err := h.canManage(r, project)
	if err == nil && !ok {
		err = ErrNotManager
	}
	if err != nil {
		writeErr(w, r, err)
		return ""
	}
	return project
}

type listOut struct {
	Origins   []Origin `json:"origins"`
	CanManage bool     `json:"can_manage"`
}

// List: GET /api/projects/{id}/embed/origins (org members) → the project's allowed origins and whether
// the caller may edit them.
func (h System) List(w http.ResponseWriter, r *http.Request) {
	project := h.project(w, r)
	if project == "" {
		return
	}
	out, err := h.store.List(r.Context(), project)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	can, err := h.canManage(r, project)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, listOut{Origins: out, CanManage: can})
}

// Add: POST /api/projects/{id}/embed/origins {origin} (project managers, signed in) → the added origin.
func (h System) Add(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Origin string `json:"origin"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	project := h.manager(w, r)
	if project == "" {
		return
	}
	o, err := h.store.Add(r.Context(), auth.OrgID(r.Context()), project, auth.ActorID(r.Context()), in.Origin)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, o)
}

// Remove: DELETE /api/projects/{id}/embed/origins/{origin} (project managers, signed in).
func (h System) Remove(w http.ResponseWriter, r *http.Request) {
	project := h.manager(w, r)
	if project == "" {
		return
	}
	if err := h.store.Remove(r.Context(), project, r.PathValue("origin")); err != nil {
		writeErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type keysOut struct {
	Keys      []Key `json:"keys"`
	CanManage bool  `json:"can_manage"`
}

// ListKeys: GET /api/projects/{id}/embed/keys (org members) → the project's embed keys with usage, and
// can_manage.
func (h System) ListKeys(w http.ResponseWriter, r *http.Request) {
	project := h.project(w, r)
	if project == "" {
		return
	}
	out, err := h.store.ListKeys(r.Context(), project)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	can, err := h.canManage(r, project)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, keysOut{Keys: out, CanManage: can})
}

// CreateKey: POST /api/projects/{id}/embed/keys {label?} (project managers) → the new key (not a secret,
// so it's also readable later).
func (h System) CreateKey(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Label string `json:"label"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	project := h.manager(w, r)
	if project == "" {
		return
	}
	k, err := h.store.CreateKey(r.Context(), auth.OrgID(r.Context()), project, auth.ActorID(r.Context()), in.Label)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, k)
}

// DeleteKey: DELETE /api/projects/{id}/embed/keys/{key} (project managers). Its usage goes with it.
func (h System) DeleteKey(w http.ResponseWriter, r *http.Request) {
	project := h.manager(w, r)
	if project == "" {
		return
	}
	if err := h.store.DeleteKey(r.Context(), project, r.PathValue("key")); err != nil {
		writeErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// KeyUsage: GET /api/projects/{id}/embed/keys/{key}/usage (org members) → hits per site and resource.
func (h System) KeyUsage(w http.ResponseWriter, r *http.Request) {
	project := h.project(w, r)
	if project == "" {
		return
	}
	out, err := h.store.KeyUsage(r.Context(), project, r.PathValue("key"))
	if err != nil {
		writeErr(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}
