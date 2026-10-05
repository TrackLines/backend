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

func (in *roadmapInput) valid(w http.ResponseWriter) bool {
	in.Title = strings.TrimSpace(in.Title)
	if in.Title == "" {
		http.Error(w, "title is required", http.StatusBadRequest)
		return false
	}
	if in.Visibility == "" {
		in.Visibility = Public
	}
	return true
}

func (h System) Create(w http.ResponseWriter, r *http.Request) {
	var in roadmapInput
	if !httpx.Decode(w, r, &in) || !in.valid(w) {
		return
	}
	user, _ := auth.UserID(r.Context())
	rm, err := h.store.Create(r.Context(), user, in.Title, in.Description, in.Visibility)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, rm)
}

func (h System) Update(w http.ResponseWriter, r *http.Request) {
	var in roadmapInput
	if !httpx.Decode(w, r, &in) || !in.valid(w) {
		return
	}
	user, _ := auth.UserID(r.Context())
	if err := h.store.Update(r.Context(), r.PathValue("id"), user, in.Title, in.Description, in.Visibility); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h System) Delete(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserID(r.Context())
	if err := h.store.Delete(r.Context(), r.PathValue("id"), user); err != nil {
		writeErr(w, err)
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
			http.Error(w, "item title is required", http.StatusBadRequest)
			return
		}
	}
	user, _ := auth.UserID(r.Context())
	if err := h.store.ReplaceItems(r.Context(), r.PathValue("id"), user, items); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
