package attachments

import (
	"errors"
	"net/http"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/httpx"
)

type System struct {
	store Store
}

// NewSystem exposes the attachment handlers; routes are declared in internal/service.go.
func NewSystem(db *pgxpool.Pool) System {
	return System{store: Store{DB: db}}
}

func writeErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, ErrBadFile):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, ErrDone):
		http.Error(w, err.Error(), http.StatusConflict)
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
		writeErr(w, err)
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
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, a)
}

// Delete removes attachment {id}; its file on UploadThing goes with the next sweep (Sweeper), the
// same way files go when their ticket, project or organization is deleted.
func (h System) Delete(w http.ResponseWriter, r *http.Request) {
	org := auth.OrgID(r.Context())
	if _, err := h.store.Delete(r.Context(), org, r.PathValue("id")); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
