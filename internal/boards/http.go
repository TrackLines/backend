package boards

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

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

func writeErr(w http.ResponseWriter, r *http.Request, err error) {
	if httpx.Canceled(err) {
		return // the client left: nobody to answer, nothing to report
	}
	switch {
	case errors.Is(err, ErrNotFound):
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, ErrBadDevComplete), errors.Is(err, ErrBadReviewColumn):
		httpx.Error(w, r, http.StatusBadRequest, err)
	case errors.Is(err, ErrBoardUsed), errors.Is(err, ErrOpenTickets), errors.Is(err, ErrBoardArchived):
		httpx.Error(w, r, http.StatusConflict, err)
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
		httpx.Error(w, r, http.StatusBadRequest, httpx.ErrNameRequired)
		return
	}
	org := auth.OrgID(r.Context())
	tpl := Builtins[0]
	if in.TemplateID != "" {
		var err error
		if tpl, err = h.store.Template(r.Context(), org, in.TemplateID); err != nil {
			httpx.Error(w, r, http.StatusBadRequest, httpx.Code("template_not_found", "template not found"))
			return
		}
	}
	b, err := h.store.CreateFrom(r.Context(), org, r.PathValue("id"), in.Name, in.Description, tpl)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, b)
}

func (h System) Get(w http.ResponseWriter, r *http.Request) {
	org := auth.OrgID(r.Context())
	b, err := h.store.Get(r.Context(), r.PathValue("id"), org)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, b)
}

// Flow reports completed tickets, cycle-time percentiles, weekly throughput, WIP and work mix for a
// board; ?format=md sends it as a Markdown download.
func (h System) Flow(w http.ResponseWriter, r *http.Request) {
	weeks := 12
	if raw := r.URL.Query().Get("weeks"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 52 {
			httpx.Error(w, r, http.StatusBadRequest, httpx.Code("bad_weeks", "weeks must be between 1 and 52"))
			return
		}
		weeks = n
	}
	f, err := h.store.Flow(r.Context(), auth.OrgID(r.Context()), r.PathValue("id"), weeks)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	if wantsMarkdown(r) { // TL-621
		loc, now := httpx.Zone(r.Context()), time.Now()
		sendMarkdown(w, f.BoardName, now.In(loc), FlowMarkdown(f, loc, now))
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
		ReviewColumn  *string `json:"review_column_id"`       // "": the column before Done (TL-592)
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	if in.Name == nil && in.EstimateScale == nil && in.Style == nil && in.AgentReview == nil && in.DevComplete == nil && in.ReviewColumn == nil {
		httpx.Error(w, r, http.StatusBadRequest, httpx.Code("board_update_empty", "name, estimate_scale, style, agent_review, dev_complete_column_id or review_column_id is required"))
		return
	}
	name := ""
	if in.Name != nil {
		name = strings.TrimSpace(*in.Name)
		if name == "" {
			httpx.Error(w, r, http.StatusBadRequest, httpx.ErrNameRequired)
			return
		}
	}
	if in.EstimateScale != nil {
		if _, ok := Scales[*in.EstimateScale]; !ok {
			httpx.Error(w, r, http.StatusBadRequest, httpx.Code("bad_estimate_scale", "estimate_scale must be none, fibonacci, tshirt, powers or linear"))
			return
		}
	}
	if in.Style != nil && *in.Style != "sprints" && *in.Style != "kanban" {
		httpx.Error(w, r, http.StatusBadRequest, httpx.Code("bad_board_style", "style must be sprints or kanban"))
		return
	}
	if in.Style != nil {
		if err := h.store.SetStyle(r.Context(), r.PathValue("id"), auth.OrgID(r.Context()), *in.Style); err != nil {
			writeErr(w, r, err)
			return
		}
	}
	if in.AgentReview != nil {
		if err := h.store.SetAgentReview(r.Context(), r.PathValue("id"), auth.OrgID(r.Context()), *in.AgentReview); err != nil {
			writeErr(w, r, err)
			return
		}
	}
	if in.DevComplete != nil {
		if err := h.store.SetDevComplete(r.Context(), r.PathValue("id"), auth.OrgID(r.Context()), *in.DevComplete); err != nil {
			writeErr(w, r, err)
			return
		}
	}
	if in.ReviewColumn != nil {
		if err := h.store.SetReviewColumn(r.Context(), r.PathValue("id"), auth.OrgID(r.Context()), *in.ReviewColumn); err != nil {
			writeErr(w, r, err)
			return
		}
	}
	if in.Name != nil || in.EstimateScale != nil {
		if err := h.store.UpdateSettings(r.Context(), r.PathValue("id"), auth.OrgID(r.Context()), name, in.EstimateScale); err != nil {
			writeErr(w, r, err)
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
		writeErr(w, r, err)
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
		writeErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Archived: GET /api/projects/{id}/archived-boards.
func (h System) Archived(w http.ResponseWriter, r *http.Request) {
	out, err := h.store.Archived(r.Context(), auth.OrgID(r.Context()), r.PathValue("id"))
	if err != nil {
		writeErr(w, r, err)
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
		writeErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
