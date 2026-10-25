package tickets

import (
	"context"
	"errors"
	"github.com/tracklines/backend/internal/notifications"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/clerk/clerk-sdk-go/v2"
	"github.com/clerk/clerk-sdk-go/v2/organizationmembership"
	"github.com/clerk/clerk-sdk-go/v2/user"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/boards"
	"github.com/tracklines/backend/internal/httpx"
)

// NewSystem exposes the ticket handlers; routes are declared in internal/service.go.
func NewSystem(db *pgxpool.Pool) System {
	return System{store: Store{DB: db}, memberships: organizationmembership.NewClient(&clerk.ClientConfig{}), users: user.NewClient(&clerk.ClientConfig{})}
}

// CachingMembers lists assignees through l (e.g. clerkcache). Assignment checks still ask Clerk directly.
func (h System) CachingMembers(l membershipLister) System { h.assigneeMembers = l; return h }

// CachingUsers looks assignees' usernames up through l (e.g. clerkcache).
func (h System) CachingUsers(l userLister) System { h.users = l; return h }

type System struct {
	notify          notifications.Notifier // nil: nobody is told
	store           Store
	assigneeMembers membershipLister // nil: memberships
	memberships     membershipLister
	users           userLister // usernames for the @ picker; nil: none
	// moving an unassigned ticket assigns it to the mover (TL-266), except a board manager during a
	// standup (they're running it); nil: no auto-assign
	boardManager   func(ctx context.Context, org, user, board string) (bool, error)
	standupRunning func(ctx context.Context, board string) bool
}

// AssigningOnMove turns on assign-on-move (TL-266): manager says who runs a board, standup whether one is on.
func (h System) AssigningOnMove(manager func(ctx context.Context, org, user, board string) (bool, error), standup func(ctx context.Context, board string) bool) System {
	h.boardManager, h.standupRunning = manager, standup
	return h
}

// MaxTitle and MaxBody bound a ticket's title and its description (and a comment's body).
const (
	MaxTitle = 200
	MaxBody  = 100_000
)

type ticketInput struct {
	Type        string    `json:"type"` // bug | feature | task; defaults to task on create, unchanged on update
	Title       string    `json:"title"`
	Description string    `json:"description"`
	Priority    string    `json:"priority"` // low | medium | high | urgent; defaults to medium on create
	Labels      *[]string `json:"labels"`   // optional; omitted = unchanged, [] clears
	Estimate    *string   `json:"estimate"` // on the board's scale; omitted = unchanged, "" clears
	External    *External `json:"external"` // backlog create from an API key (e.g. Bugfixes): where it came from
	// a spike's timebox (TL-502): set together; both 0 clears; both omitted = unchanged
	TimeboxDays  *int `json:"timebox_days"`
	TimeboxHours *int `json:"timebox_hours"`
}

// intOr0 reads a *int sent on the input, treating "not sent" as 0.
func intOr0(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

func (in ticketInput) createType() string {
	if in.Type == "" {
		return "task"
	}
	return in.Type
}

func (in *ticketInput) valid(w http.ResponseWriter, r *http.Request) bool {
	if in.Title = strings.TrimSpace(in.Title); in.Title == "" {
		httpx.Error(w, r, http.StatusBadRequest, httpx.ErrTitleRequired)
		return false
	}
	// bounds (TL-251): a title shows on cards, pushes and MCP output; a near-1MB description broke search
	if utf8.RuneCountInString(in.Title) > MaxTitle {
		httpx.Error(w, r, http.StatusBadRequest, httpx.Code("title_too_long", "title must be "+strconv.Itoa(MaxTitle)+" characters or fewer"))
		return false
	}
	if utf8.RuneCountInString(in.Description) > MaxBody {
		httpx.Error(w, r, http.StatusBadRequest, httpx.Code("description_too_long", "description must be "+strconv.Itoa(MaxBody)+" characters or fewer"))
		return false
	}
	if !slices.Contains([]string{"", "low", "medium", "high", "urgent"}, in.Priority) {
		httpx.Error(w, r, http.StatusBadRequest, ErrBadPriority)
		return false
	}
	if in.Labels != nil {
		labels, err := NormalizeLabels(*in.Labels)
		if err != nil {
			httpx.Error(w, r, http.StatusBadRequest, err)
			return false
		}
		in.Labels = &labels
	}
	if in.TimeboxDays != nil || in.TimeboxHours != nil {
		if d, h := intOr0(in.TimeboxDays), intOr0(in.TimeboxHours); d < 0 || d > 60 || h < 0 || h > 23 {
			httpx.Error(w, r, http.StatusBadRequest, httpx.Code("bad_timebox", "timebox days must be 0–60 and hours 0–23"))
			return false
		}
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

// applyEstimate stores an estimate sent with a create/update (checked against the board's scale); t may be nil.
func (h System) applyEstimate(r *http.Request, user, id string, in ticketInput, t *Ticket) error {
	if in.Estimate == nil {
		return nil
	}
	if err := h.store.SetRequiredEstimate(r.Context(), user, id, *in.Estimate); err != nil {
		return err
	}
	if t != nil && *in.Estimate != "" {
		t.Estimate = in.Estimate
	}
	return nil
}

// applyTimebox stores a spike's timebox sent with an update (TL-502); t may be nil.
func (h System) applyTimebox(r *http.Request, user, id string, in ticketInput, t *Ticket) error {
	if in.TimeboxDays == nil && in.TimeboxHours == nil {
		return nil
	}
	days, hours := intOr0(in.TimeboxDays), intOr0(in.TimeboxHours)
	if err := h.store.SetTimebox(r.Context(), user, id, days, hours); err != nil {
		return err
	}
	if t != nil {
		if days == 0 && hours == 0 {
			t.TimeboxDays, t.TimeboxHours = nil, nil
		} else {
			t.TimeboxDays, t.TimeboxHours = &days, &hours
		}
	}
	return nil
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

func writeErr(w http.ResponseWriter, r *http.Request, err error) {
	if httpx.Canceled(err) {
		return // the client left: nobody to answer, nothing to report
	}
	var locked SprintLockedError
	var unestimated EstimateRequiredError
	switch {
	case errors.As(err, &locked):
		httpx.Error(w, r, http.StatusConflict, httpx.Code("sprint_locked", err.Error()))
		return
	case errors.As(err, &unestimated):
		httpx.Error(w, r, http.StatusBadRequest, httpx.Code("estimate_required", err.Error()))
		return
	case errors.Is(err, ErrNotFound):
		http.Error(w, "not found", http.StatusNotFound)
		return
	case errors.Is(err, ErrInvalidType), errors.Is(err, ErrBadPriority), errors.Is(err, ErrBadBacklogSort), errors.Is(err, ErrBadBacklogStatus), errors.Is(err, ErrBadBacklogPlanned), errors.Is(err, ErrBadExternal):
		httpx.Error(w, r, http.StatusBadRequest, err)
		return
	case errors.Is(err, ErrOnBoard), errors.Is(err, ErrIsDuplicate), errors.Is(err, boards.ErrBoardArchived), errors.Is(err, ErrAgentReviewBypass),
		errors.Is(err, ErrEpicOffBoard):
		httpx.Error(w, r, http.StatusConflict, err)
		return
	case errors.Is(err, ErrAlreadyAssigned):
		httpx.Error(w, r, http.StatusConflict, err)
		return
	case errors.Is(err, ErrNotAssignedToCaller), errors.Is(err, ErrBlocked), errors.Is(err, ErrDoneBlock), errors.Is(err, ErrDoneParent):
		httpx.Error(w, r, http.StatusConflict, err)
		return
	case errors.Is(err, ErrSelfBlock), errors.Is(err, ErrOtherProject), errors.Is(err, ErrCycle), errors.Is(err, ErrUnknownAssignee), errors.Is(err, ErrBadLabels),
		errors.Is(err, ErrSelfParent), errors.Is(err, ErrParentLoop), errors.Is(err, boards.ErrBadEstimate),
		errors.Is(err, ErrEpicInEpic), errors.Is(err, ErrEpicEstimate):
		httpx.Error(w, r, http.StatusBadRequest, err)
		return
	}
	logs.Errorf("tickets: %v", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

func (h System) Create(w http.ResponseWriter, r *http.Request) {
	var in ticketInput
	if !httpx.Decode(w, r, &in) || !in.valid(w, r) {
		return
	}
	org := auth.OrgID(r.Context())
	estimate := ""
	if in.Estimate != nil {
		estimate = *in.Estimate
	}
	t, err := h.store.CreateEstimated(r.Context(), org, auth.ActorID(r.Context()), r.PathValue("id"), in.createType(), in.Title, in.Description, estimate)
	if err == nil {
		err = h.applyLabels(r, org, t.ID, in, t)
	}
	if err == nil {
		err = h.applyPriority(r, org, in, t)
	}
	if err == nil {
		err = h.applyTimebox(r, org, t.ID, in, t)
	}
	if err != nil {
		writeErr(w, r, err)
		return
	}
	h.created(r, t.ID)
	if t.ColumnID == nil { // the board's sprint is on its last day: the ticket went to the backlog
		httpx.JSON(w, http.StatusCreated, struct {
			*Ticket
			RedirectedToBacklog bool `json:"redirected_to_backlog"`
		}{t, true})
		return
	}
	httpx.JSON(w, http.StatusCreated, t)
}

func (h System) Update(w http.ResponseWriter, r *http.Request) {
	var in ticketInput
	if !httpx.Decode(w, r, &in) || !in.valid(w, r) {
		return
	}
	org := auth.OrgID(r.Context())
	err := h.store.Update(r.Context(), org, r.PathValue("id"), in.Type, in.Title, in.Description, in.Priority)
	if err == nil {
		err = h.applyLabels(r, org, r.PathValue("id"), in, nil)
	}
	if err == nil {
		err = h.applyEstimate(r, org, r.PathValue("id"), in, nil)
	}
	if err == nil {
		err = h.applyTimebox(r, org, r.PathValue("id"), in, nil)
	}
	if err != nil {
		writeErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h System) Delete(w http.ResponseWriter, r *http.Request) {
	org := auth.OrgID(r.Context())
	if err := h.store.Delete(r.Context(), org, r.PathValue("id")); err != nil {
		writeErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h System) Move(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ColumnID string  `json:"column_id"`
		Position int     `json:"position"`
		Estimate *string `json:"estimate"` // size it on the way in; required entering a board with an estimate scale
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	if in.ColumnID == "" {
		httpx.Error(w, r, http.StatusBadRequest, httpx.Code("column_id_required", "column_id is required"))
		return
	}
	org := auth.OrgID(r.Context())
	before := h.detail(r.Context(), org, r.PathValue("id"))
	from, unassigned := h.store.moveFrom(r.Context(), org, r.PathValue("id"))
	review, alsoMoved, err := h.store.MoveGroup(r.Context(), org, r.PathValue("id"), in.ColumnID, in.Position, in.Estimate, true, auth.ViaAPIKey(r.Context()))
	if err != nil {
		writeErr(w, r, err)
		return
	}
	if unassigned && from != in.ColumnID {
		h.assignMover(r, org, r.PathValue("id"), in.ColumnID)
	}
	if after := h.detail(r.Context(), org, r.PathValue("id")); after != nil && before != nil {
		if review {
			h.reviewers(r, after)
		} else if after.Done && !before.Done {
			h.finished(r, after, "finished")
		} else if from != in.ColumnID && after.ColumnName != nil {
			h.watchers(r, after, nil, "moved "+ref(after)+" to "+*after.ColumnName)
		}
	}
	if review { // an agent finished it on a board with agent review: a person has to approve it
		httpx.JSON(w, http.StatusOK, map[string]any{"needs_review": true, "message": "Moved to the board's review column to wait for a person's review."})
		return
	}
	if len(alsoMoved) > 0 { // it left its board and took its open sub-tickets with it (TL-218)
		httpx.JSON(w, http.StatusOK, map[string]any{"also_moved": alsoMoved})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Backlog lists project {id}'s backlog (?type=bug|feature|task to filter).
func (h System) Backlog(w http.ResponseWriter, r *http.Request) {
	org := auth.OrgID(r.Context())
	out, err := h.store.Backlog(r.Context(), org, r.PathValue("id"), r.URL.Query().Get("type"))
	if err != nil {
		writeErr(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

// PageBacklog: GET /api/projects/{id}/backlog/page?type=&label=a&priority=high&sort=newest&planned=exclude&page=1&per_page=25.
func (h System) PageBacklog(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	page, ok := intParam(w, r, q.Get("page"), "page", 1, 100_000) // unbounded, (page-1)*per_page overflowed into a 500
	if !ok {
		return
	}
	perPage, ok := intParam(w, r, q.Get("per_page"), "per_page", 25, MaxPerPage)
	if !ok {
		return
	}
	org := auth.OrgID(r.Context())
	out, err := h.store.PageBacklogPlanned(r.Context(), org, r.PathValue("id"), q.Get("epic"), q.Get("type"), q.Get("status"), q.Get("planned"), q["label"], page, perPage, q.Get("sort"), q["priority"]...)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

// intParam parses an optional positive query int (def when empty, max 0 = unbounded); writes a 400 if bad.
func intParam(w http.ResponseWriter, r *http.Request, raw, name string, def, max int) (int, bool) {
	if raw == "" {
		return def, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || (max > 0 && n > max) {
		msg := name + " must be a positive integer"
		if max > 0 {
			msg += " up to " + strconv.Itoa(max)
		}
		httpx.Error(w, r, http.StatusBadRequest, httpx.Code("bad_number_param", msg))
		return 0, false
	}
	return n, true
}

// CreateBacklog adds a ticket to project {id}'s backlog.
func (h System) CreateBacklog(w http.ResponseWriter, r *http.Request) {
	var in ticketInput
	if !httpx.Decode(w, r, &in) || !in.valid(w, r) {
		return
	}
	if in.Estimate != nil && *in.Estimate != "" {
		httpx.Error(w, r, http.StatusBadRequest, httpx.Code("backlog_estimate", "backlog tickets can't be estimated; estimates follow the board's scale"))
		return
	}
	org := auth.OrgID(r.Context())
	if in.External != nil && !auth.ViaAPIKey(r.Context()) {
		httpx.Error(w, r, http.StatusForbidden, httpx.Code("external_api_only", "external is set by integrations (API keys)"))
		return
	}
	if in.External != nil && !in.External.valid() {
		writeErr(w, r, ErrBadExternal)
		return
	}
	t, err := h.store.CreateBacklogAs(r.Context(), org, auth.ActorID(r.Context()), r.PathValue("id"), in.createType(), in.Title, in.Description)
	if err == nil && in.External != nil {
		if err = h.store.SetExternal(r.Context(), org, t.ID, in.External); err == nil {
			t.ExternalSource, t.ExternalURL = &in.External.Source, &in.External.URL
		}
	}
	if err == nil {
		err = h.applyLabels(r, org, t.ID, in, t)
	}
	if err == nil {
		err = h.applyPriority(r, org, in, t)
	}
	if err == nil {
		err = h.applyTimebox(r, org, t.ID, in, t)
	}
	if err != nil {
		writeErr(w, r, err)
		return
	}
	h.created(r, t.ID)
	httpx.JSON(w, http.StatusCreated, t)
}

// Claim assigns a ticket to the authenticated caller (the API key's agent name, or
// the Clerk user ID). The database update makes competing claims mutually exclusive.
func (h System) Claim(w http.ResponseWriter, r *http.Request) {
	if auth.ViaAPIKey(r.Context()) && auth.APIKeyKind(r.Context()) == "service" {
		httpx.Error(w, r, http.StatusForbidden, httpx.Code("service_key_claim", "service keys can't claim tickets"))
		return
	}
	org := auth.OrgID(r.Context())
	t, err := h.store.Claim(r.Context(), org, auth.ActorID(r.Context()), r.PathValue("id"))
	if err != nil {
		writeErr(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, t)
}

// Release clears the current caller's assignment.
func (h System) Release(w http.ResponseWriter, r *http.Request) {
	if auth.ViaAPIKey(r.Context()) && auth.APIKeyKind(r.Context()) == "service" {
		httpx.Error(w, r, http.StatusForbidden, httpx.Code("service_key_release", "service keys can't release tickets"))
		return
	}
	org := auth.OrgID(r.Context())
	if err := h.store.Release(r.Context(), org, auth.ActorID(r.Context()), r.PathValue("id")); err != nil {
		writeErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ToBacklog sends ticket {id} back to its project's backlog.
func (h System) ToBacklog(w http.ResponseWriter, r *http.Request) {
	org := auth.OrgID(r.Context())
	alsoMoved, err := h.store.ToBacklogGroup(r.Context(), org, r.PathValue("id"), auth.ViaAPIKey(r.Context()))
	if err != nil {
		writeErr(w, r, err)
		return
	}
	if len(alsoMoved) > 0 { // its open sub-tickets on the board came too (TL-218)
		httpx.JSON(w, http.StatusOK, map[string]any{"also_moved": alsoMoved})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Get returns ticket {id} with its context (project, board, column, sprint) so it can be linked to.
// GetByKey: GET /api/ticket-keys/{key}, e.g. TL-123.
func (h System) GetByKey(w http.ResponseWriter, r *http.Request) {
	org := auth.OrgID(r.Context())
	id, err := h.store.IDByKey(r.Context(), org, r.PathValue("key"))
	if err != nil {
		writeErr(w, r, err)
		return
	}
	d, err := h.store.Get(r.Context(), org, id)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, d)
}

func (h System) Get(w http.ResponseWriter, r *http.Request) {
	org := auth.OrgID(r.Context())
	d, err := h.store.Get(r.Context(), org, r.PathValue("id"))
	if err != nil {
		writeErr(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, d)
}

// Epics: GET /api/projects/{id}/epics, the project's epics with their progress and size (TL-213).
func (h System) Epics(w http.ResponseWriter, r *http.Request) {
	out, err := h.store.Epics(r.Context(), auth.OrgID(r.Context()), r.PathValue("id"))
	if err != nil {
		writeErr(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

// History: GET /api/tickets/{id}/history, ticket {id}'s lifecycle oldest first (TL-76): each stage it
// entered or skipped, when, and who did it. Same access as reading the ticket; resolved tickets keep it.
func (h System) History(w http.ResponseWriter, r *http.Request) {
	out, err := h.store.History(r.Context(), auth.OrgID(r.Context()), r.PathValue("id"))
	if err != nil {
		writeErr(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
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
		writeErr(w, r, err)
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
		writeErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// SetExternal: PUT /api/tickets/{id}/external {source, project, id, url} (null body clears).
// For integrations (API keys) linking tickets they made earlier, e.g. Bugfixes backfilling reports.
func (h System) SetExternal(w http.ResponseWriter, r *http.Request) {
	if !auth.ViaAPIKey(r.Context()) {
		httpx.Error(w, r, http.StatusForbidden, httpx.Code("external_api_only", "external is set by integrations (API keys)"))
		return
	}
	var in *External
	if !httpx.Decode(w, r, &in) {
		return
	}
	if err := h.store.SetExternal(r.Context(), auth.OrgID(r.Context()), r.PathValue("id"), in); err != nil {
		writeErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
