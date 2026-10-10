package sprints

import (
	"context"
	"errors"
	"math"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/boards"
	"github.com/tracklines/backend/internal/httpx"
)

// Refinement: a board plans up to MaxPlanned sprints after its current one. Backlog tickets are
// planned into them (staying in the backlog) and sized on the board's scale. A planned sprint holds
// up to its capacity, velocity + 10%; going over needs an org admin or the board team's leader to
// approve the total. Closing the current sprint promotes planned sprint 1 (see promotePlanned).

const MaxPlanned = 2

// velocityWindow is how many recent closed sprints velocity averages over.
const velocityWindow = 3

var (
	ErrTooManyPlanned = errors.New("a board plans at most 2 sprints ahead")
	ErrNotBacklog     = errors.New("only backlog tickets can be planned into a sprint")
)

type PlannedSprint struct {
	ID         string          `json:"id"`
	BoardID    string          `json:"board_id"`
	Position   int             `json:"position"` // 1 = the next sprint
	Number     int             `json:"number"`   // the sprint number it will get
	LengthDays int             `json:"length_days"`
	Tickets    []boards.Ticket `json:"tickets"`
	Capacity   Capacity        `json:"capacity"`
}

// Capacity is how full a planned sprint is, in the board's velocity unit.
type Capacity struct {
	Unit          string   `json:"unit"`     // points (board has a scale) | tickets
	Velocity      *float64 `json:"velocity"` // average completed over the last 3 closed sprints; null = no history yet
	Cap           *float64 `json:"cap"`      // velocity + 10%; null = no limit yet
	Used          float64  `json:"used"`
	Unestimated   int      `json:"unestimated"` // points boards: planned tickets without an estimate (count 0)
	Over          bool     `json:"over"`
	ApprovedTotal *float64 `json:"approved_total"`
	ApprovedBy    *string  `json:"approved_by"`
	ApprovedAt    *string  `json:"approved_at"`
	NeedsApproval bool     `json:"needs_approval"` // over and the approval doesn't cover today's total
}

type querier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

// velocity averages what the board's last velocityWindow closed sprints finished; nil without history.
func velocity(ctx context.Context, q querier, boardID, unit string) (*float64, error) {
	var closed int
	if err := q.QueryRow(ctx, `SELECT count(*) FROM (SELECT 1 FROM sprints WHERE board_id = $1 AND closed_at IS NOT NULL LIMIT $2) recent`,
		boardID, velocityWindow).Scan(&closed); err != nil || closed == 0 {
		return nil, err
	}
	rows, err := q.Query(ctx, `SELECT t.estimate FROM tickets t WHERE `+boards.TicketDoneSQL+` AND t.sprint_id IN (
		SELECT id FROM sprints WHERE board_id = $1 AND closed_at IS NOT NULL ORDER BY number DESC LIMIT $2)`, boardID, velocityWindow)
	if err != nil {
		return nil, err
	}
	value, total := valueIn(unit), 0.0
	var estimate *string
	if _, err := pgx.ForEachRow(rows, []any{&estimate}, func() error { total += value(estimate); return nil }); err != nil {
		return nil, err
	}
	avg := total / float64(closed)
	return &avg, nil
}

// capacity fills in how full planned sprint p is, from its tickets and the board's velocity.
func capacity(ctx context.Context, q querier, scale string, p *PlannedSprint, approvedTotal *float64, approvedBy, approvedAt *string) error {
	c := Capacity{Unit: unitOf(scale), ApprovedTotal: approvedTotal, ApprovedBy: approvedBy, ApprovedAt: approvedAt}
	value := valueIn(c.Unit)
	for _, t := range p.Tickets {
		c.Used += value(t.Estimate)
		if c.Unit == "points" && t.Estimate == nil {
			c.Unestimated++
		}
	}
	v, err := velocity(ctx, q, p.BoardID, c.Unit)
	if err != nil {
		return err
	}
	if v != nil {
		limit := math.Round(*v*1.1*100) / 100
		c.Velocity, c.Cap = v, &limit
		c.Over = c.Used > limit
	}
	c.NeedsApproval = c.Over && (approvedTotal == nil || c.Used > *approvedTotal)
	p.Capacity = c
	return nil
}

// planned reads the board's planned sprints with their tickets and capacity, next first.
func planned(ctx context.Context, q querier, boardID string) ([]PlannedSprint, error) {
	var scale string
	var base int // the current (or last) sprint's number; planned sprints follow it
	if err := q.QueryRow(ctx, `SELECT b.estimate_scale, COALESCE((SELECT number FROM sprints WHERE board_id = b.id AND closed_at IS NULL),
			(SELECT max(number) FROM sprints WHERE board_id = b.id), 0) FROM boards b WHERE b.id = $1`, boardID).Scan(&scale, &base); err != nil {
		return nil, notFound(err)
	}
	rows, err := q.Query(ctx, `SELECT id, board_id, position, length_days, approved_total::float8, approved_by,
			to_char(approved_at AT TIME ZONE 'UTC', `+ts+`)
		FROM planned_sprints WHERE board_id = $1 ORDER BY position`, boardID)
	if err != nil {
		return nil, err
	}
	type approval struct {
		total    *float64
		by, when *string
	}
	var out []PlannedSprint
	var approvals []approval
	var p PlannedSprint
	var a approval
	if _, err := pgx.ForEachRow(rows, []any{&p.ID, &p.BoardID, &p.Position, &p.LengthDays, &a.total, &a.by, &a.when}, func() error {
		p.Number = base + p.Position
		out, approvals = append(out, p), append(approvals, a)
		return nil
	}); err != nil {
		return nil, err
	}
	for i := range out {
		trows, err := q.Query(ctx, `SELECT `+boards.TicketCols+` FROM tickets t WHERE t.planned_sprint_id = $1 AND NOT `+boards.TicketDoneSQL+`
			ORDER BY t.position, t.created_at`, out[i].ID)
		if err != nil {
			return nil, err
		}
		if out[i].Tickets, err = pgx.CollectRows(trows, func(row pgx.CollectableRow) (boards.Ticket, error) { return boards.ScanTicket(row) }); err != nil {
			return nil, err
		}
		if out[i].Tickets == nil {
			out[i].Tickets = []boards.Ticket{}
		}
		if err := capacity(ctx, q, scale, &out[i], approvals[i].total, approvals[i].by, approvals[i].when); err != nil {
			return nil, err
		}
	}
	if out == nil {
		out = []PlannedSprint{}
	}
	return out, nil
}

func (s Store) owns(ctx context.Context, owner, boardID string) error {
	var ok bool
	return notFound(s.DB.QueryRow(ctx, `SELECT true FROM boards b JOIN projects p ON p.id = b.project_id
		WHERE b.id = $1 AND p.owner_clerk_id = $2`, boardID, owner).Scan(&ok))
}

// Planned lists the owner's board's planned sprints.
func (s Store) Planned(ctx context.Context, owner, boardID string) ([]PlannedSprint, error) {
	if err := s.owns(ctx, owner, boardID); err != nil {
		return nil, err
	}
	return planned(ctx, s.DB, boardID)
}

// Plan adds a planned sprint after the board's last one (at most MaxPlanned); returns the board's plans.
func (s Store) Plan(ctx context.Context, owner, boardID string, lengthDays int) ([]PlannedSprint, error) {
	if lengthDays < 1 || lengthDays > 365 {
		return nil, ErrInvalidLength
	}
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		var style string
		var count int
		if err := tx.QueryRow(ctx, `SELECT b.style FROM boards b JOIN projects p ON p.id = b.project_id
			WHERE b.id = $1 AND p.owner_clerk_id = $2 FOR UPDATE OF b`, boardID, owner).Scan(&style); err != nil {
			return notFound(err)
		}
		if style == "kanban" {
			return ErrKanban
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM planned_sprints WHERE board_id = $1`, boardID).Scan(&count); err != nil {
			return err
		}
		if count >= MaxPlanned {
			return ErrTooManyPlanned
		}
		_, err := tx.Exec(ctx, `INSERT INTO planned_sprints (board_id, position, length_days) VALUES ($1, $2, $3)`, boardID, count+1, lengthDays)
		return err
	})
	if err != nil {
		return nil, err
	}
	return planned(ctx, s.DB, boardID)
}

// plannedBoard is the board of the owner's planned sprint id.
func (s Store) plannedBoard(ctx context.Context, q querier, owner, id string) (string, error) {
	var boardID string
	err := q.QueryRow(ctx, `SELECT ps.board_id FROM planned_sprints ps JOIN boards b ON b.id = ps.board_id
		JOIN projects p ON p.id = b.project_id WHERE ps.id = $1 AND p.owner_clerk_id = $2`, id, owner).Scan(&boardID)
	return boardID, notFound(err)
}

// Unplan removes a planned sprint: its tickets stay in the backlog, unplanned and unestimated (the
// backlog has no scale), and a later plan moves up.
func (s Store) Unplan(ctx context.Context, owner, id string) error {
	return pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		boardID, err := s.plannedBoard(ctx, tx, owner, id)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE tickets SET planned_sprint_id = NULL, estimate = NULL, updated_at = now() WHERE planned_sprint_id = $1`, id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM planned_sprints WHERE id = $1`, id); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE planned_sprints SET position = position - 1 WHERE board_id = $1 AND position > 1
			AND NOT EXISTS (SELECT 1 FROM planned_sprints o WHERE o.board_id = $1 AND o.position = 1)`, boardID)
		return err
	})
}

// PlanTicket puts backlog ticket ticketID into planned sprint plannedID (nil takes it out), sized
// with estimate on that board's scale. Without an estimate it keeps one already on the same scale.
func (s Store) PlanTicket(ctx context.Context, owner, ticketID string, plannedID, estimate *string) error {
	return pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		var project string
		var board, oldScale, current *string
		if err := tx.QueryRow(ctx, `SELECT t.project_id, t.board_id, t.estimate,
				(SELECT pb.estimate_scale FROM planned_sprints ps JOIN boards pb ON pb.id = ps.board_id WHERE ps.id = t.planned_sprint_id)
			FROM tickets t JOIN projects p ON p.id = t.project_id AND p.owner_clerk_id = $2
			WHERE t.id = $1 FOR UPDATE OF t`, ticketID, owner).Scan(&project, &board, &current, &oldScale); err != nil {
			return notFound(err)
		}
		if board != nil {
			return ErrNotBacklog
		}
		if plannedID == nil {
			_, err := tx.Exec(ctx, `UPDATE tickets SET planned_sprint_id = NULL, estimate = NULL, updated_at = now() WHERE id = $1`, ticketID)
			return err
		}
		var scale string
		if err := tx.QueryRow(ctx, `SELECT b.estimate_scale FROM planned_sprints ps JOIN boards b ON b.id = ps.board_id
			WHERE ps.id = $1 AND b.project_id = $2`, *plannedID, project).Scan(&scale); err != nil {
			return notFound(err)
		}
		next := current
		if oldScale == nil || *oldScale != scale {
			next = nil // a different scale: sized again
		}
		if estimate != nil {
			if !boards.ValidEstimate(scale, *estimate) {
				return boards.ErrBadEstimate
			}
			next = estimate
			if *estimate == "" {
				next = nil
			}
		}
		_, err := tx.Exec(ctx, `UPDATE tickets SET planned_sprint_id = $2, estimate = $3, updated_at = now() WHERE id = $1`, ticketID, *plannedID, next)
		return err
	})
}

// Approve accepts planned sprint id's current total over capacity, as actor.
func (s Store) Approve(ctx context.Context, owner, actor, id string) (*PlannedSprint, error) {
	boardID, err := s.plannedBoard(ctx, s.DB, owner, id)
	if err != nil {
		return nil, err
	}
	plans, err := planned(ctx, s.DB, boardID)
	if err != nil {
		return nil, err
	}
	for _, p := range plans {
		if p.ID != id {
			continue
		}
		if _, err := s.DB.Exec(ctx, `UPDATE planned_sprints SET approved_total = $2, approved_by = $3, approved_at = now() WHERE id = $1`,
			id, p.Capacity.Used, actor); err != nil {
			return nil, err
		}
		plans, err = planned(ctx, s.DB, boardID)
		if err != nil {
			return nil, err
		}
		for _, p := range plans {
			if p.ID == id {
				return &p, nil
			}
		}
	}
	return nil, ErrNotFound
}

// nextPlan is the board's planned sprint 1 if it can become the next sprint: a plan over capacity
// without approval stays planned until it's approved or trimmed. nil = nothing to promote.
func nextPlan(ctx context.Context, tx pgx.Tx, boardID string) (*PlannedSprint, error) {
	plans, err := planned(ctx, tx, boardID)
	if err != nil || len(plans) == 0 || plans[0].Capacity.NeedsApproval {
		return nil, err
	}
	return &plans[0], nil
}

// movePlanned moves plan's tickets into sprint next (the board's first column, estimates kept)
// and drops the plan; the other plan becomes planned sprint 1.
func movePlanned(ctx context.Context, tx pgx.Tx, plan *PlannedSprint, next Sprint) error {
	id, boardID := plan.ID, plan.BoardID
	if _, err := tx.Exec(ctx, `WITH first AS (SELECT id FROM columns WHERE board_id = $2 ORDER BY position LIMIT 1),
			planned AS (SELECT t.id, row_number() OVER (ORDER BY t.position, t.created_at) AS n FROM tickets t WHERE t.planned_sprint_id = $1)
		UPDATE tickets t SET board_id = $2, column_id = (SELECT id FROM first), sprint_id = $3, planned_sprint_id = NULL, updated_at = now(),
			position = COALESCE((SELECT max(x.position) FROM tickets x WHERE x.column_id = (SELECT id FROM first) AND x.sprint_id = $3), -1) + planned.n
		FROM planned WHERE t.id = planned.id`, id, boardID, next.ID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM planned_sprints WHERE id = $1`, id); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE planned_sprints SET position = position - 1 WHERE board_id = $1`, boardID)
	return err
}

// HTTP

// ListPlanned: GET /api/boards/{id}/planned-sprints
func (h System) ListPlanned(w http.ResponseWriter, r *http.Request) {
	out, err := h.store.Planned(r.Context(), auth.OrgID(r.Context()), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

// Plan: POST /api/boards/{id}/planned-sprints {"length_days": n}. Board managers only.
func (h System) Plan(w http.ResponseWriter, r *http.Request) {
	if !h.admins.RequireResourceBoardManager(w, r, "board", r.PathValue("id")) {
		return
	}
	var in struct {
		LengthDays int `json:"length_days"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	out, err := h.store.Plan(r.Context(), auth.OrgID(r.Context()), r.PathValue("id"), in.LengthDays)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, out)
}

// Unplan: DELETE /api/planned-sprints/{id}. Board managers only.
func (h System) Unplan(w http.ResponseWriter, r *http.Request) {
	if !h.admins.RequireResourceBoardManager(w, r, "planned_sprint", r.PathValue("id")) {
		return
	}
	if err := h.store.Unplan(r.Context(), auth.OrgID(r.Context()), r.PathValue("id")); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Approve: POST /api/planned-sprints/{id}/approve. Org admins and the board team's leaders.
func (h System) Approve(w http.ResponseWriter, r *http.Request) {
	if !h.admins.RequireResourceBoardManager(w, r, "planned_sprint", r.PathValue("id")) {
		return
	}
	p, err := h.store.Approve(r.Context(), auth.OrgID(r.Context()), auth.ActorID(r.Context()), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, p)
}

// PlanTicket: PUT /api/tickets/{id}/planned-sprint {"planned_sprint_id": id | null, "estimate": "5"}.
// Anyone who works the board can plan.
func (h System) PlanTicket(w http.ResponseWriter, r *http.Request) {
	var in struct {
		PlannedSprintID *string `json:"planned_sprint_id"`
		Estimate        *string `json:"estimate"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	if err := h.store.PlanTicket(r.Context(), auth.OrgID(r.Context()), r.PathValue("id"), in.PlannedSprintID, in.Estimate); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
