package roadmaps

import (
	"net/http"
	"strings"

	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/httpx"
)

type roadmapInput struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	Visibility  string `json:"visibility"`
}

func (in *roadmapInput) valid(w http.ResponseWriter, r *http.Request) bool {
	in.Title = strings.TrimSpace(in.Title)
	if in.Title == "" {
		httpx.Error(w, r, http.StatusBadRequest, httpx.ErrTitleRequired)
		return false
	}
	return true
}

// Create adds a roadmap to project {id}.
func (h System) Create(w http.ResponseWriter, r *http.Request) {
	var in roadmapInput
	if !httpx.Decode(w, r, &in) || !in.valid(w, r) {
		return
	}
	if in.Visibility == "" { // left out (API, MCP): team only; publishing is a choice (the web form asks)
		in.Visibility = Team
	}
	if !h.manager(w, r, "", r.PathValue("id")) || (in.Visibility == Public && !h.publisher(w, r)) {
		return
	}
	org := auth.OrgID(r.Context())
	rm, err := h.store.Create(r.Context(), org, r.PathValue("id"), in.Title, in.Description, in.Visibility)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, rm)
}

func (h System) Update(w http.ResponseWriter, r *http.Request) {
	var in roadmapInput
	if !httpx.Decode(w, r, &in) || !in.valid(w, r) {
		return
	}
	if !h.manager(w, r, r.PathValue("id"), "") {
		return
	}
	cur, err := h.store.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, r, err)
		return
	}
	if in.Visibility == "" { // left out: unchanged
		in.Visibility = cur.Visibility
	}
	if (cur.Visibility == Public) != (in.Visibility == Public) && !h.publisher(w, r) {
		return
	}
	org := auth.OrgID(r.Context())
	if err := h.store.Update(r.Context(), r.PathValue("id"), org, in.Title, in.Description, in.Visibility); err != nil {
		writeErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h System) Delete(w http.ResponseWriter, r *http.Request) {
	if !h.manager(w, r, r.PathValue("id"), "") || !h.publisher(w, r) {
		return
	}
	org := auth.OrgID(r.Context())
	if err := h.store.Delete(r.Context(), r.PathValue("id"), org); err != nil {
		writeErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h System) ReplaceItems(w http.ResponseWriter, r *http.Request) {
	var items []Item
	if !httpx.Decode(w, r, &items) {
		return
	}
	for _, i := range items {
		if strings.TrimSpace(i.Title) == "" {
			httpx.Error(w, r, http.StatusBadRequest, httpx.Code("item_title_required", "item title is required"))
			return
		}
		if i.ManualStatus != "" && !ValidManualStatus(i.ManualStatus) {
			httpx.Error(w, r, http.StatusBadRequest, ErrInvalidItemStatus)
			return
		}
		if !ValidDates(i) {
			httpx.Error(w, r, http.StatusBadRequest, httpx.Code("bad_item_dates", ErrBadDates.Error()+": "+i.Title))
			return
		}
	}
	if !h.manager(w, r, r.PathValue("id"), "") {
		return
	}
	org := auth.OrgID(r.Context())
	if err := h.store.ReplaceItems(r.Context(), r.PathValue("id"), org, items); err != nil {
		writeErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// SetItemTickets links tickets to roadmap {id}'s item {item}: {"ticket_ids": [...]} ([] clears).
func (h System) SetItemTickets(w http.ResponseWriter, r *http.Request) {
	var in struct {
		TicketIDs []string `json:"ticket_ids"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	if !h.manager(w, r, r.PathValue("id"), "") {
		return
	}
	org := auth.OrgID(r.Context())
	if err := h.store.SetItemTickets(r.Context(), org, r.PathValue("id"), r.PathValue("item"), in.TicketIDs); err != nil {
		writeErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
