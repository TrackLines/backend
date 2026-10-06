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
	Type        string `json:"type"` // bug | feature | task; defaults to task on create, unchanged on update
	Title       string `json:"title"`
	Description string `json:"description"`
	Priority    string `json:"priority"` // low | medium | high | urgent; defaults to medium on create
}

func (in ticketInput) createType() string {
	if in.Type == "" {
		return "task"
	}
	return in.Type
}

func (in *ticketInput) valid(w http.ResponseWriter) bool {
	if in.Title = strings.TrimSpace(in.Title); in.Title == "" {
		http.Error(w, "title is required", http.StatusBadRequest)
		return false
	}
	return true
}

func writeErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		http.Error(w, "not found", http.StatusNotFound)
		return
	case errors.Is(err, ErrInvalidType):
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	case errors.Is(err, ErrAlreadyAssigned):
		http.Error(w, err.Error(), http.StatusConflict)
		return
	case errors.Is(err, ErrNotAssignedToCaller), errors.Is(err, ErrBlocked):
		http.Error(w, err.Error(), http.StatusConflict)
		return
	case errors.Is(err, ErrSelfBlock), errors.Is(err, ErrOtherProject), errors.Is(err, ErrCycle), errors.Is(err, ErrUnknownAssignee):
		http.Error(w, err.Error(), http.StatusBadRequest)
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
	t, err := h.store.CreateAs(r.Context(), user, auth.ActorID(r.Context()), r.PathValue("id"), in.createType(), in.Title, in.Description)
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
	if err := h.store.Update(r.Context(), user, r.PathValue("id"), in.Type, in.Title, in.Description, in.Priority); err != nil {
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

// Backlog lists project {id}'s backlog (?type=bug|feature|task to filter).
func (h System) Backlog(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserID(r.Context())
	out, err := h.store.Backlog(r.Context(), user, r.PathValue("id"), r.URL.Query().Get("type"))
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

// CreateBacklog adds a ticket to project {id}'s backlog.
func (h System) CreateBacklog(w http.ResponseWriter, r *http.Request) {
	var in ticketInput
	if !httpx.Decode(w, r, &in) || !in.valid(w) {
		return
	}
	user, _ := auth.UserID(r.Context())
	t, err := h.store.CreateBacklogAs(r.Context(), user, auth.ActorID(r.Context()), r.PathValue("id"), in.createType(), in.Title, in.Description)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, t)
}

// Claim assigns a ticket to the authenticated caller (the API key's agent name, or
// the Clerk user ID). The database update makes competing claims mutually exclusive.
func (h System) Claim(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserID(r.Context())
	t, err := h.store.Claim(r.Context(), user, auth.ActorID(r.Context()), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, t)
}

// Release clears the current caller's assignment.
func (h System) Release(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserID(r.Context())
	if err := h.store.Release(r.Context(), user, auth.ActorID(r.Context()), r.PathValue("id")); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ToBacklog sends ticket {id} back to its project's backlog.
func (h System) ToBacklog(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserID(r.Context())
	if err := h.store.ToBacklog(r.Context(), user, r.PathValue("id")); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Get returns ticket {id} with its context (project, board, column, sprint) so it can be linked to.
func (h System) Get(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserID(r.Context())
	d, err := h.store.Get(r.Context(), user, r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, d)
}

// SetBlockedBy replaces the tickets that ticket {id} waits on: {"ticket_ids": [...]} ([] clears).
func (h System) SetBlockedBy(w http.ResponseWriter, r *http.Request) {
	var in struct {
		TicketIDs []string `json:"ticket_ids"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	user, _ := auth.UserID(r.Context())
	if err := h.store.SetBlockedBy(r.Context(), user, r.PathValue("id"), in.TicketIDs); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
