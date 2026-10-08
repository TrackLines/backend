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
	ut    UploadThing
}

// NewSystem exposes the attachment handlers; routes are declared in internal/service.go.
func NewSystem(db *pgxpool.Pool, ut UploadThing) System {
	return System{store: Store{DB: db}, ut: ut}
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

// Delete removes attachment {id} and its file on UploadThing. The record goes first; a failed
// storage delete is logged (orphaned file) rather than leaving a dangling attachment.
func (h System) Delete(w http.ResponseWriter, r *http.Request) {
	org := auth.OrgID(r.Context())
	key, err := h.store.Delete(r.Context(), org, r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := h.ut.DeleteFiles(r.Context(), key); err != nil {
		logs.Errorf("attachments: file %s left on UploadThing: %v", key, err)
	}
	w.WriteHeader(http.StatusNoContent)
}
