package boards

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/httpx"
	"github.com/tracklines/backend/internal/organizations"
)

// NewSystem exposes the board handlers; routes are declared in internal/service.go.
func NewSystem(db *pgxpool.Pool, admins organizations.Admins) System {
	return System{store: Store{DB: db}, admins: admins}
}

type System struct {
	store  Store
	admins organizations.Admins
}

func writeErr(w http.ResponseWriter, err error) {
	if httpx.Canceled(err) {
		return // the client left: nobody to answer, nothing to report
	}
	switch {
	case errors.Is(err, ErrNotFound):
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, ErrBadDevComplete):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, ErrBoardUsed), errors.Is(err, ErrOpenTickets), errors.Is(err, ErrBoardArchived):
		http.Error(w, err.Error(), http.StatusConflict)
	default:
		logs.Errorf("boards: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// Create adds a board to project {id} (one board per team).
func (h System) Create(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := h.admins.RequireAdmin(w, r); !ok {
		return
	}
	var in struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		TemplateID  string `json:"template_id"` // optional: a built-in or org template; default Simple
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	if in.Name = strings.TrimSpace(in.Name); in.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	org := auth.OrgID(r.Context())
	tpl := Builtins[0]
	if in.TemplateID != "" {
		var err error
		if tpl, err = h.store.Template(r.Context(), org, in.TemplateID); err != nil {
			http.Error(w, "template not found", http.StatusBadRequest)
			return
		}
	}
	b, err := h.store.CreateFrom(r.Context(), org, r.PathValue("id"), in.Name, in.Description, tpl)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, b)
}

func (h System) Get(w http.ResponseWriter, r *http.Request) {
	org := auth.OrgID(r.Context())
	b, err := h.store.Get(r.Context(), r.PathValue("id"), org)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, b)
}

// Flow reports completed tickets, cycle-time percentiles, and weekly throughput for a board.
func (h System) Flow(w http.ResponseWriter, r *http.Request) {
	weeks := 12
	if raw := r.URL.Query().Get("weeks"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 52 {
			http.Error(w, "weeks must be between 1 and 52", http.StatusBadRequest)
			return
		}
		weeks = n
	}
	f, err := h.store.Flow(r.Context(), auth.OrgID(r.Context()), r.PathValue("id"), weeks)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, f)
}

// Update changes the board name and estimate scale. Switching scale clears open estimates.
func (h System) Update(w http.ResponseWriter, r *http.Request) {
	if !h.admins.RequireResourceBoardManager(w, r, "board", r.PathValue("id")) {
		return
	}
	var in struct {
		Name          *string `json:"name"`
		EstimateScale *string `json:"estimate_scale"`
		Style         *string `json:"style"` // sprints | kanban
		AgentReview   *bool   `json:"agent_review"`
		DevComplete   *string `json:"dev_complete_column_id"` // "" turns finishing on release off
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	if in.Name == nil && in.EstimateScale == nil && in.Style == nil && in.AgentReview == nil && in.DevComplete == nil {
		http.Error(w, "name, estimate_scale, style, agent_review or dev_complete_column_id is required", http.StatusBadRequest)
		return
	}
	name := ""
	if in.Name != nil {
		name = strings.TrimSpace(*in.Name)
		if name == "" {
			http.Error(w, "name is required", http.StatusBadRequest)
			return
		}
	}
	if in.EstimateScale != nil {
		if _, ok := Scales[*in.EstimateScale]; !ok {
			http.Error(w, "estimate_scale must be none, fibonacci, tshirt, powers or linear", http.StatusBadRequest)
			return
		}
	}
	if in.Style != nil && *in.Style != "sprints" && *in.Style != "kanban" {
		http.Error(w, "style must be sprints or kanban", http.StatusBadRequest)
		return
	}
	if in.Style != nil {
		if err := h.store.SetStyle(r.Context(), r.PathValue("id"), auth.OrgID(r.Context()), *in.Style); err != nil {
			writeErr(w, err)
			return
		}
	}
	if in.AgentReview != nil {
		if err := h.store.SetAgentReview(r.Context(), r.PathValue("id"), auth.OrgID(r.Context()), *in.AgentReview); err != nil {
			writeErr(w, err)
			return
		}
	}
	if in.DevComplete != nil {
		if err := h.store.SetDevComplete(r.Context(), r.PathValue("id"), auth.OrgID(r.Context()), *in.DevComplete); err != nil {
			writeErr(w, err)
			return
		}
	}
	if in.Name != nil || in.EstimateScale != nil {
		if err := h.store.UpdateSettings(r.Context(), r.PathValue("id"), auth.OrgID(r.Context()), name, in.EstimateScale); err != nil {
			writeErr(w, err)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// Archive: POST /api/boards/{id}/archive (board managers). 409 while it has unfinished tickets.
func (h System) Archive(w http.ResponseWriter, r *http.Request) {
	if !h.admins.RequireResourceBoardManager(w, r, "board", r.PathValue("id")) {
		return
	}
	if err := h.store.Archive(r.Context(), r.PathValue("id"), auth.OrgID(r.Context())); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Restore: POST /api/boards/{id}/restore (board managers).
func (h System) Restore(w http.ResponseWriter, r *http.Request) {
	if !h.admins.RequireResourceBoardManager(w, r, "board", r.PathValue("id")) {
		return
	}
	if err := h.store.Restore(r.Context(), r.PathValue("id"), auth.OrgID(r.Context())); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Archived: GET /api/projects/{id}/archived-boards.
func (h System) Archived(w http.ResponseWriter, r *http.Request) {
	out, err := h.store.Archived(r.Context(), auth.OrgID(r.Context()), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

// Delete: DELETE /api/boards/{id} (org admins), only a board that was never used (409 otherwise: archive it).
func (h System) Delete(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := h.admins.RequireAdmin(w, r); !ok {
		return
	}
	org := auth.OrgID(r.Context())
	if err := h.store.Delete(r.Context(), r.PathValue("id"), org); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
