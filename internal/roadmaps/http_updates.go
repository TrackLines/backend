package roadmaps

import (
	"context"
	"net/http"
	"strings"

	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/httpx"
)

// ListUpdates returns roadmap update history to readers allowed by the roadmap visibility.
func (h System) ListUpdates(w http.ResponseWriter, r *http.Request) {
	rm, err := h.store.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, r, err)
		return
	}
	_, signedIn := auth.UserID(r.Context())
	if status := canRead(rm, auth.OrgID(r.Context()), signedIn); status != 0 {
		http.Error(w, http.StatusText(status), status)
		return
	}
	updates, err := h.store.ListUpdates(r.Context(), rm.ID)
	if err != nil {
		writeErr(w, r, err)
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
		httpx.Error(w, r, http.StatusBadRequest, ErrInvalidUpdate)
		return
	}
	if !h.manager(w, r, r.PathValue("id"), "") {
		return
	}
	org := auth.OrgID(r.Context())
	author := auth.ActorID(r.Context())
	owner, _ := auth.UserID(r.Context())
	update, err := h.store.CreateUpdate(r.Context(), r.PathValue("id"), org, in.Health, in.Note, author, h.authorName(r.Context(), org, author, owner))
	if err != nil {
		writeErr(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, update)
}

// authorName: the poster's name from the org's people (members and agents), or "" if not found. ids:
// the actor first (an agent key is named for itself, e.g. "Claude"), then the person behind it.
func (h System) authorName(ctx context.Context, org string, ids ...string) string {
	if h.people == nil {
		return ""
	}
	people, err := h.people(ctx, org)
	if err != nil {
		return ""
	}
	for _, id := range ids {
		for _, p := range people {
			if id != "" && p.ID == id && len(p.Names) > 0 {
				return p.Names[0]
			}
		}
	}
	return ""
}
