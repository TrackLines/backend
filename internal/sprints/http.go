package sprints

import (
	"errors"
	"net/http"
	"time"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/boards"
	"github.com/tracklines/backend/internal/httpx"
	"github.com/tracklines/backend/internal/organizations"
)

type System struct {
	store  Store
	admins organizations.Admins
}

// NewSystem exposes the sprint handlers; routes are declared in internal/service.go.
func NewSystem(db *pgxpool.Pool, admins organizations.Admins) System {
	return System{store: Store{DB: db}, admins: admins}
}

func writeErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, ErrAlreadyOpen), errors.Is(err, ErrKanban), errors.Is(err, ErrTooManyPlanned), errors.Is(err, ErrNotBacklog):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, ErrInvalidLength), errors.Is(err, ErrInvalidStart), errors.Is(err, boards.ErrBadEstimate):
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		logs.Errorf("sprints: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// Update changes the length of an open or scheduled sprint, keeping its start date fixed.
func (h System) Update(w http.ResponseWriter, r *http.Request) {
	if !h.admins.RequireResourceBoardManager(w, r, "sprint", r.PathValue("id")) {
		return
	}
	var in struct {
		LengthDays int `json:"length_days"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	sp, err := h.store.UpdateLength(r.Context(), auth.OrgID(r.Context()), r.PathValue("id"), in.LengthDays)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, sp)
}

// Start opens a sprint on board {id}: {"length_days": 7 | 14 | any 1–365}.
func (h System) Start(w http.ResponseWriter, r *http.Request) {
	if !h.admins.RequireResourceBoardManager(w, r, "board", r.PathValue("id")) {
		return
	}
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

// Get is sprint {id}, read-only: burn data and its tickets (a closed sprint's are what it finished).
func (h System) Get(w http.ResponseWriter, r *http.Request) {
	d, err := h.store.Detail(r.Context(), auth.OrgID(r.Context()), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, d)
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
	if !h.admins.RequireResourceBoardManager(w, r, "sprint", r.PathValue("id")) {
		return
	}
	org := auth.OrgID(r.Context())
	var in struct {
		NextLengthDays int        `json:"next_length_days"`
		NextStartsAt   *time.Time `json:"next_starts_at"`
	}
	if r.ContentLength != 0 && !httpx.Decode(w, r, &in) {
		return
	}
	next, err := h.store.CloseWithOptions(r.Context(), org, r.PathValue("id"), CloseOptions{
		NextLengthDays: in.NextLengthDays,
		NextStartsAt:   in.NextStartsAt,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, next)
}
