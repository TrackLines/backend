package sprints

import (
	"errors"
	"net/http"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/httpx"
)

type System struct{ store Store }

// NewSystem exposes the sprint handlers; routes are declared in internal/service.go.
func NewSystem(db *pgxpool.Pool) System {
	return System{store: Store{DB: db}}
}

func writeErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, ErrAlreadyOpen), errors.Is(err, ErrKanban):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, ErrInvalidLength):
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		logs.Errorf("sprints: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// Start opens a sprint on board {id}: {"length_days": 7 | 14 | any 1–365}.
func (h System) Start(w http.ResponseWriter, r *http.Request) {
	var in struct {
		LengthDays int `json:"length_days"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	org := auth.OrgID(r.Context())
	sp, err := h.store.Start(r.Context(), org, r.PathValue("id"), in.LengthDays)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, sp)
}

// List returns board {id}'s sprints, newest first.
func (h System) List(w http.ResponseWriter, r *http.Request) {
	org := auth.OrgID(r.Context())
	out, err := h.store.List(r.Context(), org, r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

// Velocity is what board {id} finished per closed sprint, plus the open sprint's burn data.
func (h System) Velocity(w http.ResponseWriter, r *http.Request) {
	v, err := h.store.Velocity(r.Context(), auth.OrgID(r.Context()), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, v)
}

// Close closes open sprint {id} and returns the next sprint it opened.
func (h System) Close(w http.ResponseWriter, r *http.Request) {
	org := auth.OrgID(r.Context())
	next, err := h.store.Close(r.Context(), org, r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, next)
}
