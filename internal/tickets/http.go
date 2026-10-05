package tickets

import (
	"errors"
	"net/http"
	"strings"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/httpx"
)

// NewSystem exposes the ticket handlers; routes are declared in internal/service.go.
func NewSystem(db *pgxpool.Pool) System {
	return System{Store{DB: db}}
}

type System struct{ store Store }

type ticketInput struct {
	Title       string `json:"title"`
	Description string `json:"description"`
}

func (in *ticketInput) valid(w http.ResponseWriter) bool {
	if in.Title = strings.TrimSpace(in.Title); in.Title == "" {
		http.Error(w, "title is required", http.StatusBadRequest)
		return false
	}
	return true
}

func writeErr(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrNotFound) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	logs.Errorf("tickets: %v", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

func (h System) Create(w http.ResponseWriter, r *http.Request) {
	var in ticketInput
	if !httpx.Decode(w, r, &in) || !in.valid(w) {
		return
	}
	user, _ := auth.UserID(r.Context())
	t, err := h.store.Create(r.Context(), user, r.PathValue("id"), in.Title, in.Description)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, t)
}

func (h System) Update(w http.ResponseWriter, r *http.Request) {
	var in ticketInput
	if !httpx.Decode(w, r, &in) || !in.valid(w) {
		return
	}
	user, _ := auth.UserID(r.Context())
	if err := h.store.Update(r.Context(), user, r.PathValue("id"), in.Title, in.Description); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h System) Delete(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserID(r.Context())
	if err := h.store.Delete(r.Context(), user, r.PathValue("id")); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h System) Move(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ColumnID string `json:"column_id"`
		Position int    `json:"position"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	if in.ColumnID == "" {
		http.Error(w, "column_id is required", http.StatusBadRequest)
		return
	}
	user, _ := auth.UserID(r.Context())
	if err := h.store.Move(r.Context(), user, r.PathValue("id"), in.ColumnID, in.Position); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
