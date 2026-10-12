package roadmaps

import (
	"net/http"
	"strings"

	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/httpx"
)

// ListUpdates returns roadmap update history to readers allowed by the roadmap visibility.
func (h System) ListUpdates(w http.ResponseWriter, r *http.Request) {
	rm, err := h.store.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	_, signedIn := auth.UserID(r.Context())
	if status := canRead(rm, auth.OrgID(r.Context()), signedIn); status != 0 {
		http.Error(w, http.StatusText(status), status)
		return
	}
	updates, err := h.store.ListUpdates(r.Context(), rm.ID)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, updates)
}

// PublishUpdate appends a progress update to a roadmap the caller's organization owns.
func (h System) PublishUpdate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Health string `json:"health"`
		Note   string `json:"note"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	in.Health = strings.TrimSpace(in.Health)
	in.Note = strings.TrimSpace(in.Note)
	if !ValidUpdate(in.Health, in.Note) {
		http.Error(w, ErrInvalidUpdate.Error(), http.StatusBadRequest)
		return
	}
	update, err := h.store.CreateUpdate(r.Context(), r.PathValue("id"), auth.OrgID(r.Context()), in.Health, in.Note)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, update)
}
