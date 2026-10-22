package embed

import (
	"context"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/httpx"
)

// System is the signed-in management side: an org's admins add and remove the origins their widgets
// may be embedded on. The public read/submit routes are the existing roadmap and intake handlers;
// this package only adds their dynamic CORS (see CORS).
type System struct {
	store  Store
	admins Admins
}

// Admins says whether user administers org.
type Admins interface {
	IsAdmin(ctx context.Context, org, user string) (bool, error)
}

func NewSystem(db *pgxpool.Pool) System { return System{store: Store{DB: db}} }

// WithAdmins lets org admins change the origin list (allowing an origin opens the API to it).
func (h System) WithAdmins(a Admins) System { h.admins = a; return h }

func writeErr(w http.ResponseWriter, err error) {
	if httpx.Canceled(err) {
		return
	}
	switch {
	case errors.Is(err, ErrInvalid), errors.Is(err, ErrTooMany):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, ErrNotFound):
		http.Error(w, "not found", http.StatusNotFound)
	default:
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// canManage: a signed-in org admin, never an API key — allowing an origin exposes the API to it, so
// it's a person's decision (mirrors the request form, TL-432).
func (h System) canManage(r *http.Request) (bool, error) {
	ctx := r.Context()
	if auth.ViaAPIKey(ctx) || h.admins == nil {
		return false, nil
	}
	user, _ := auth.UserID(ctx)
	return h.admins.IsAdmin(ctx, auth.OrgID(ctx), user)
}

type listOut struct {
	Origins   []Origin `json:"origins"`
	CanManage bool     `json:"can_manage"`
}

// List: GET /api/embed/origins (org members) → the org's allowed origins and whether the caller may edit them.
func (h System) List(w http.ResponseWriter, r *http.Request) {
	out, err := h.store.List(r.Context(), auth.OrgID(r.Context()))
	if err != nil {
		writeErr(w, err)
		return
	}
	can, err := h.canManage(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, listOut{Origins: out, CanManage: can})
}

// Add: POST /api/embed/origins {origin} (org admins, signed in) → the added origin.
func (h System) Add(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Origin string `json:"origin"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	if !h.requireManager(w, r) {
		return
	}
	o, err := h.store.Add(r.Context(), auth.OrgID(r.Context()), auth.ActorID(r.Context()), in.Origin)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, o)
}

// Remove: DELETE /api/embed/origins/{id} (org admins, signed in).
func (h System) Remove(w http.ResponseWriter, r *http.Request) {
	if !h.requireManager(w, r) {
		return
	}
	if err := h.store.Remove(r.Context(), auth.OrgID(r.Context()), r.PathValue("id")); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type keysOut struct {
	Keys      []Key `json:"keys"`
	CanManage bool  `json:"can_manage"`
}

// ListKeys: GET /api/embed/keys (org members) → the org's embed keys with usage, and can_manage.
func (h System) ListKeys(w http.ResponseWriter, r *http.Request) {
	out, err := h.store.ListKeys(r.Context(), auth.OrgID(r.Context()))
	if err != nil {
		writeErr(w, err)
		return
	}
	can, err := h.canManage(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, keysOut{Keys: out, CanManage: can})
}

// CreateKey: POST /api/embed/keys {label?} (org admins) → the new key (its token is shown once here, but
// it isn't a secret, so it's also readable later).
func (h System) CreateKey(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Label string `json:"label"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	if !h.requireManager(w, r) {
		return
	}
	k, err := h.store.CreateKey(r.Context(), auth.OrgID(r.Context()), auth.ActorID(r.Context()), in.Label)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, k)
}

// DeleteKey: DELETE /api/embed/keys/{id} (org admins). Its usage goes with it.
func (h System) DeleteKey(w http.ResponseWriter, r *http.Request) {
	if !h.requireManager(w, r) {
		return
	}
	if err := h.store.DeleteKey(r.Context(), auth.OrgID(r.Context()), r.PathValue("id")); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// KeyUsage: GET /api/embed/keys/{id}/usage (org members) → hits per site and resource.
func (h System) KeyUsage(w http.ResponseWriter, r *http.Request) {
	out, err := h.store.KeyUsage(r.Context(), auth.OrgID(r.Context()), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

// requireManager answers the request and returns false unless the caller may change the list.
func (h System) requireManager(w http.ResponseWriter, r *http.Request) bool {
	admin, err := h.canManage(r)
	if err != nil {
		writeErr(w, err)
		return false
	}
	if !admin {
		http.Error(w, "only an organization admin, signed in, can change embed origins", http.StatusForbidden)
		return false
	}
	return true
}
