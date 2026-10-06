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
	case errors.Is(err, ErrAlreadyOpen):
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
	user, _ := auth.UserID(r.Context())
	sp, err := h.store.Start(r.Context(), user, r.PathValue("id"), in.LengthDays)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, sp)
}

// List returns board {id}'s sprints, newest first.
func (h System) List(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserID(r.Context())
	out, err := h.store.List(r.Context(), user, r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

// Close closes open sprint {id} and returns the next sprint it opened.
func (h System) Close(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserID(r.Context())
	next, err := h.store.Close(r.Context(), user, r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, next)
}
