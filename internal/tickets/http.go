package tickets

import (
	"errors"
	"net/http"
	"slices"
	"strconv"
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
	Type        string    `json:"type"` // bug | feature | task; defaults to task on create, unchanged on update
	Title       string    `json:"title"`
	Description string    `json:"description"`
	Priority    string    `json:"priority"` // low | medium | high | urgent; defaults to medium on create
	Labels      *[]string `json:"labels"`   // optional; omitted = unchanged, [] clears
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
	if !slices.Contains([]string{"", "low", "medium", "high", "urgent"}, in.Priority) {
		http.Error(w, "priority must be low, medium, high or urgent", http.StatusBadRequest)
		return false
	}
	if in.Labels != nil {
		labels, err := NormalizeLabels(*in.Labels)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return false
		}
		in.Labels = &labels
	}
	return true
}

// applyPriority sets the priority sent with a create (the insert always starts at medium), like BugFixes create.
func (h System) applyPriority(r *http.Request, user string, in ticketInput, t *Ticket) error {
	if in.Priority == "" {
		return nil
	}
	t.Priority = in.Priority
	return h.store.Update(r.Context(), user, t.ID, "", t.Title, t.Description, in.Priority)
}

// applyLabels stores labels sent with a create/update (validated by valid); t may be nil.
func (h System) applyLabels(r *http.Request, user, id string, in ticketInput, t *Ticket) error {
	if in.Labels == nil {
		return nil
	}
	if t != nil {
		t.Labels = *in.Labels
	}
	return h.store.SetLabels(r.Context(), user, id, *in.Labels)
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
	case errors.Is(err, ErrSelfBlock), errors.Is(err, ErrOtherProject), errors.Is(err, ErrCycle), errors.Is(err, ErrUnknownAssignee), errors.Is(err, ErrBadLabels),
		errors.Is(err, ErrSelfParent), errors.Is(err, ErrParentLoop):
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
	org := auth.OrgID(r.Context())
	t, err := h.store.CreateAs(r.Context(), org, auth.ActorID(r.Context()), r.PathValue("id"), in.createType(), in.Title, in.Description)
	if err == nil {
		err = h.applyLabels(r, org, t.ID, in, t)
	}
	if err == nil {
		err = h.applyPriority(r, org, in, t)
	}
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
	org := auth.OrgID(r.Context())
	err := h.store.Update(r.Context(), org, r.PathValue("id"), in.Type, in.Title, in.Description, in.Priority)
	if err == nil {
		err = h.applyLabels(r, org, r.PathValue("id"), in, nil)
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h System) Delete(w http.ResponseWriter, r *http.Request) {
	org := auth.OrgID(r.Context())
	if err := h.store.Delete(r.Context(), org, r.PathValue("id")); err != nil {
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
	org := auth.OrgID(r.Context())
	if err := h.store.Move(r.Context(), org, r.PathValue("id"), in.ColumnID, in.Position); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Backlog lists project {id}'s backlog (?type=bug|feature|task to filter).
func (h System) Backlog(w http.ResponseWriter, r *http.Request) {
	org := auth.OrgID(r.Context())
	out, err := h.store.Backlog(r.Context(), org, r.PathValue("id"), r.URL.Query().Get("type"))
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

// PageBacklog: GET /api/projects/{id}/backlog/page?type=&label=a&label=b&page=1&per_page=25.
func (h System) PageBacklog(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	page, ok := intParam(w, q.Get("page"), "page", 1, 0)
	if !ok {
		return
	}
	perPage, ok := intParam(w, q.Get("per_page"), "per_page", 25, MaxPerPage)
	if !ok {
		return
	}
	org := auth.OrgID(r.Context())
	out, err := h.store.PageBacklog(r.Context(), org, r.PathValue("id"), q.Get("type"), q["label"], page, perPage)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

// intParam parses an optional positive query int (def when empty, max 0 = unbounded); writes a 400 if bad.
func intParam(w http.ResponseWriter, raw, name string, def, max int) (int, bool) {
	if raw == "" {
		return def, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || (max > 0 && n > max) {
		msg := name + " must be a positive integer"
		if max > 0 {
			msg += " up to " + strconv.Itoa(max)
		}
		http.Error(w, msg, http.StatusBadRequest)
		return 0, false
	}
	return n, true
}

// CreateBacklog adds a ticket to project {id}'s backlog.
func (h System) CreateBacklog(w http.ResponseWriter, r *http.Request) {
	var in ticketInput
	if !httpx.Decode(w, r, &in) || !in.valid(w) {
		return
	}
	org := auth.OrgID(r.Context())
	t, err := h.store.CreateBacklogAs(r.Context(), org, auth.ActorID(r.Context()), r.PathValue("id"), in.createType(), in.Title, in.Description)
	if err == nil {
		err = h.applyLabels(r, org, t.ID, in, t)
	}
	if err == nil {
		err = h.applyPriority(r, org, in, t)
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, t)
}

// Claim assigns a ticket to the authenticated caller (the API key's agent name, or
// the Clerk user ID). The database update makes competing claims mutually exclusive.
func (h System) Claim(w http.ResponseWriter, r *http.Request) {
	if auth.ViaAPIKey(r.Context()) && auth.APIKeyKind(r.Context()) != "ai" {
		http.Error(w, "only AI keys can claim tickets", http.StatusForbidden)
		return
	}
	org := auth.OrgID(r.Context())
	t, err := h.store.Claim(r.Context(), org, auth.ActorID(r.Context()), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, t)
}

// Release clears the current caller's assignment.
func (h System) Release(w http.ResponseWriter, r *http.Request) {
	if auth.ViaAPIKey(r.Context()) && auth.APIKeyKind(r.Context()) != "ai" {
		http.Error(w, "only AI keys can release tickets", http.StatusForbidden)
		return
	}
	org := auth.OrgID(r.Context())
	if err := h.store.Release(r.Context(), org, auth.ActorID(r.Context()), r.PathValue("id")); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ToBacklog sends ticket {id} back to its project's backlog.
func (h System) ToBacklog(w http.ResponseWriter, r *http.Request) {
	org := auth.OrgID(r.Context())
	if err := h.store.ToBacklog(r.Context(), org, r.PathValue("id")); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Get returns ticket {id} with its context (project, board, column, sprint) so it can be linked to.
func (h System) Get(w http.ResponseWriter, r *http.Request) {
	org := auth.OrgID(r.Context())
	d, err := h.store.Get(r.Context(), org, r.PathValue("id"))
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
	org := auth.OrgID(r.Context())
	if err := h.store.SetBlockedBy(r.Context(), org, r.PathValue("id"), in.TicketIDs); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// SetParent: PUT /api/tickets/{id}/parent {"parent_id": "<ticket>" | null} makes {id} a sub-ticket.
func (h System) SetParent(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ParentID *string `json:"parent_id"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	org := auth.OrgID(r.Context())
	if err := h.store.SetParent(r.Context(), org, r.PathValue("id"), in.ParentID); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
