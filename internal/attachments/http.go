package attachments

import (
	"context"
	"errors"
	"net/http"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/httpx"
)

type System struct {
	store Store
	// IsAdmin: is user an org admin (organizations.Admins.IsAdmin); admins can keep any file (TL-259)
	IsAdmin func(ctx context.Context, org, user string) (bool, error)
}

// NewSystem exposes the attachment handlers; routes are declared in internal/service.go.
func NewSystem(db *pgxpool.Pool) System {
	return System{store: Store{DB: db}}
}

func writeErr(w http.ResponseWriter, r *http.Request, err error) {
	if httpx.Canceled(err) {
		return // the client left: nobody to answer, nothing to report
	}
	switch {
	case errors.Is(err, ErrNotFound):
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, ErrBadFile), errors.Is(err, ErrTooBig):
		httpx.Error(w, r, http.StatusBadRequest, err)
	case errors.Is(err, ErrCantKeep):
		httpx.Error(w, r, http.StatusForbidden, err)
	case errors.Is(err, ErrTooMany):
		httpx.Error(w, r, http.StatusConflict, err)
	case errors.Is(err, ErrDone):
		httpx.Error(w, r, http.StatusConflict, err)
	default:
		logs.Errorf("attachments: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// List returns ticket {id}'s attachments.
func (h System) List(w http.ResponseWriter, r *http.Request) {
	org := auth.OrgID(r.Context())
	out, err := h.store.List(r.Context(), org, r.PathValue("id"))
	if err != nil {
		writeErr(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

// Create records a file the browser already uploaded to UploadThing: {"key","url","name","size","content_type"}.
func (h System) Create(w http.ResponseWriter, r *http.Request) {
	var in Attachment
	if !httpx.Decode(w, r, &in) {
		return
	}
	org := auth.OrgID(r.Context())
	a, err := h.store.Create(r.Context(), org, auth.ActorID(r.Context()), r.PathValue("id"), in)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, a)
}

// Delete removes attachment {id}; its file on UploadThing goes with the next sweep (Sweeper), the
// same way files go when their ticket, project or organization is deleted.
func (h System) Delete(w http.ResponseWriter, r *http.Request) {
	org := auth.OrgID(r.Context())
	if _, err := h.store.Delete(r.Context(), org, r.PathValue("id")); err != nil {
		writeErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Keep: POST /api/attachments/{id}/keep holds on to a file due for clean-up for another year (TL-259).
// People only: an org admin, or whoever uploaded it.
func (h System) Keep(w http.ResponseWriter, r *http.Request) {
	user, signedIn := auth.UserID(r.Context())
	if auth.ViaAPIKey(r.Context()) || !signedIn {
		httpx.Error(w, r, http.StatusForbidden, httpx.Code("sign_in_keep_file", "sign in to keep a file"))
		return
	}
	org := auth.OrgID(r.Context())
	err := h.store.Keep(r.Context(), org, r.PathValue("id"), func(uploader string) bool {
		if uploader == user {
			return true
		}
		admin := false
		if h.IsAdmin != nil {
			admin, _ = h.IsAdmin(r.Context(), org, user)
		}
		return admin
	})
	if err != nil {
		writeErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Retention: GET /api/attachments/retention is the org's file clean-up (TL-260): files due to be
// deleted and files already deleted. Org admins only, and people only.
func (h System) Retention(w http.ResponseWriter, r *http.Request) {
	user, signedIn := auth.UserID(r.Context())
	org := auth.OrgID(r.Context())
	admin := false
	if signedIn && !auth.ViaAPIKey(r.Context()) && h.IsAdmin != nil {
		admin, _ = h.IsAdmin(r.Context(), org, user)
	}
	if !admin {
		httpx.Error(w, r, http.StatusForbidden, httpx.Code("admins_file_cleanup", "only org admins can see the file clean-up"))
		return
	}
	out, err := h.store.Retention(r.Context(), org)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}
