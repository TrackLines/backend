package boards

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tracklines/backend/internal/httpx"
)

var ErrNotFound = errors.New("board not found")

type Ticket struct {
	ID          string   `json:"id"`
	CreatedBy   string   `json:"created_by"`
	AssignedTo  *string  `json:"assigned_to"`
	ProjectID   string   `json:"project_id"`
	ColumnID    *string  `json:"column_id"`
	SprintID    *string  `json:"sprint_id"`
	Type        string   `json:"type"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Priority    string   `json:"priority"`
	Estimate    *string  `json:"estimate"` // on the board's scale; null = not estimated
	Position    int      `json:"position"`
	Blocked     bool     `json:"blocked"`     // a ticket it depends on isn't done yet
	Labels      []string `json:"labels"`      // free-form, sorted case-insensitively
	ResolvedAt  *string  `json:"resolved_at"` // closed in the backlog without a board; counts as done
	CreatedAt   string   `json:"created_at"`
	UpdatedAt   string   `json:"updated_at"`
	DoneAt      *string  `json:"done_at"`    // when it reached Done (or was resolved); null while open
	StartedAt   *string  `json:"started_at"` // first move out of the board's first column; null when unknown
	// PlannedSprintID: a backlog ticket planned into a board's upcoming sprint (refinement)
	PlannedSprintID *string `json:"planned_sprint_id"`
	Key             string  `json:"key"` // e.g. TL-123: the project's key prefix and the ticket's number in it
	// where a ticket made by another tool came from (e.g. a Bugfixes report); nil for our own
	ExternalSource *string `json:"external_source"`
	ExternalURL    *string `json:"external_url"`
	// an agent finished it on a board with agent review: it waits (in the review column) for a person
	ReviewRequestedAt *string `json:"review_requested_at"`
	Resolution        *string `json:"resolution"` // a resolved backlog ticket: done (or null) or wont_do
	DuplicateOfID     *string `json:"duplicate_of_id"`
	DuplicateOfKey    *string `json:"duplicate_of_key"`
	DuplicateOfTitle  *string `json:"duplicate_of_title"`
	EpicID            *string `json:"epic_id"` // the epic it's part of, at any depth (epics don't nest): boards filter by it (TL-215)
	// a spike's timebox (TL-502): days + hours as entered (hours 0–23); both null when not a spike or not yet timed
	TimeboxDays  *int `json:"timebox_days"`
	TimeboxHours *int `json:"timebox_hours"`
	// a planned backlog ticket's sprint, to show "Sprint 6 · Backend" (TL-582); both null when unplanned
	PlannedSprintNumber *int    `json:"planned_sprint_number"`
	PlannedBoardName    *string `json:"planned_board_name"`
	DueDate             *string `json:"due_date"` // YYYY-MM-DD, or null (TL-631)
}

// TicketCols + ScanTicket read a ticket row; shared with the tickets package. The last column
// is "blocked": some ticket it depends on isn't done yet.
var TicketCols = `t.id, t.created_by, t.assigned_to, t.project_id, t.column_id, t.sprint_id, t.type::text, t.title, t.description, t.priority::text, t.estimate, t.position, ` + TicketBlockedSQL +
	`, ARRAY(SELECT l.label FROM ticket_labels l WHERE l.ticket_id = t.id ORDER BY lower(l.label)),
	to_char(t.resolved_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
	to_char(t.created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
	to_char(t.updated_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
	to_char(t.done_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
	to_char(t.started_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), t.planned_sprint_id,
	(SELECT kp.key_prefix FROM projects kp WHERE kp.id = t.project_id) || '-' || t.number,
	t.external_source, t.external_url,
	to_char(t.review_requested_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), t.resolution,
	t.duplicate_of,
	(SELECT kp.key_prefix || '-' || kt.number FROM tickets kt JOIN projects kp ON kp.id = kt.project_id WHERE kt.id = t.duplicate_of),
	(SELECT kt.title FROM tickets kt WHERE kt.id = t.duplicate_of),
	(WITH RECURSIVE up AS (SELECT e.id, e.parent_id, e.type FROM tickets e WHERE e.id = t.parent_id
		UNION SELECT e.id, e.parent_id, e.type FROM tickets e JOIN up ON e.id = up.parent_id)
	SELECT up.id::text FROM up WHERE up.type = 'epic' LIMIT 1),
	t.timebox_days, t.timebox_hours,
	(SELECT ps.position + COALESCE((SELECT s.number FROM sprints s WHERE s.board_id = ps.board_id AND s.closed_at IS NULL),
		(SELECT max(s.number) FROM sprints s WHERE s.board_id = ps.board_id), 0) FROM planned_sprints ps WHERE ps.id = t.planned_sprint_id),
	(SELECT pb.name FROM planned_sprints ps JOIN boards pb ON pb.id = ps.board_id WHERE ps.id = t.planned_sprint_id),
	to_char(t.due_date, 'YYYY-MM-DD')`

func ScanTicket(row pgx.Row) (Ticket, error) {
	var t Ticket
	err := row.Scan(TicketDest(&t)...)
	return t, err
}

// TicketDoneSQL is true when ticket t sits in its board's last column ("done"), the same rule
// sprint carry-over uses, was resolved in the backlog, or is marked as a duplicate. Done tickets are locked against new attachments.
var TicketDoneSQL = DoneSQL("t")

// DoneSQL is TicketDoneSQL for a ticket table alias other than t.
func DoneSQL(a string) string {
	return `(` + a + `.duplicate_of IS NOT NULL OR ` + a + `.resolved_at IS NOT NULL OR (` + a + `.column_id IS NOT NULL AND ` + a + `.column_id = (
	SELECT dc.id FROM columns dc WHERE dc.board_id = ` + a + `.board_id ORDER BY dc.position DESC LIMIT 1)))`
}

// TicketBlockedSQL is true while any ticket that t depends on (ticket_dependencies) isn't done.
var TicketBlockedSQL = `EXISTS (SELECT 1 FROM ticket_dependencies td JOIN tickets bt ON bt.id = td.blocked_by_id
	WHERE td.ticket_id = t.id AND NOT ` + DoneSQL("bt") + `)`

// TicketDest lists scan targets matching TicketCols, for queries that select extra columns after them.
func TicketDest(t *Ticket) []any {
	return []any{&t.ID, &t.CreatedBy, &t.AssignedTo, &t.ProjectID, &t.ColumnID, &t.SprintID, &t.Type, &t.Title, &t.Description, &t.Priority, &t.Estimate, &t.Position, &t.Blocked, &t.Labels, &t.ResolvedAt, &t.CreatedAt, &t.UpdatedAt, &t.DoneAt, &t.StartedAt, &t.PlannedSprintID, &t.Key, &t.ExternalSource, &t.ExternalURL, &t.ReviewRequestedAt, &t.Resolution, &t.DuplicateOfID, &t.DuplicateOfKey, &t.DuplicateOfTitle, &t.EpicID, &t.TimeboxDays, &t.TimeboxHours, &t.PlannedSprintNumber, &t.PlannedBoardName, &t.DueDate}
}

// Sprint is a board's time box; only the open one is shown on the board.
type Sprint struct {
	ID         string  `json:"id"`
	BoardID    string  `json:"board_id"`
	Number     int     `json:"number"`
	LengthDays int     `json:"length_days"`
	StartsAt   string  `json:"starts_at"`
	EndsAt     string  `json:"ends_at"`
	ClosedAt   *string `json:"closed_at"`
	Goal       string  `json:"goal"` // one line the sprint is for; "" = none
}

const SprintCols = `id, board_id, number, length_days,
	to_char(starts_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
	to_char(ends_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
	to_char(closed_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), goal`

func ScanSprint(row pgx.Row) (Sprint, error) {
	var s Sprint
	err := row.Scan(&s.ID, &s.BoardID, &s.Number, &s.LengthDays, &s.StartsAt, &s.EndsAt, &s.ClosedAt, &s.Goal)
	return s, err
}

type Column struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Position int      `json:"position"`
	WIPLimit *int     `json:"wip_limit"` // advisory work-in-progress limit; null = none
	Tickets  []Ticket `json:"tickets"`
}

type Board struct {
	ID            string   `json:"id"`
	ProjectID     string   `json:"project_id"`
	OwnerClerkID  string   `json:"owner_clerk_id"`
	Name          string   `json:"name"`
	Description   string   `json:"description"`
	EstimateScale string   `json:"estimate_scale"`
	Style         string   `json:"style"`
	AgentReview   bool     `json:"agent_review"`           // agents' moves into Done wait for a person's review
	DevComplete   *string  `json:"dev_complete_column_id"` // set: merged PRs land here, a release moves them to Done (TL-531)
	ReviewColumn  *string  `json:"review_column_id"`       // agent review waits here (TL-592); null: the column before Done
	ArchivedAt    *string  `json:"archived_at"`            // archived: off the project\'s boards, history kept, no new work
	Deletable     bool     `json:"deletable"`              // never used (no tickets, no sprints): can be deleted outright
	TeamID        *string  `json:"team_id"`                // the team whose leaders run this board                 // sprints | kanban
	HiddenDone    int      `json:"hidden_done,omitempty"`  // kanban: Done tickets older than KanbanDoneDays, left off the view // a key of Scales
	CreatedAt     string   `json:"created_at"`
	UpdatedAt     string   `json:"updated_at"`
	Sprint        *Sprint  `json:"sprint,omitempty"` // open sprint, if the board runs sprints
	Columns       []Column `json:"columns,omitempty"`
	Stats         *Stats   `json:"stats,omitempty"` // ListByProject only
}

// Stats summarises a board for its project page. It counts the tickets the board shows:
// the open sprint's, or all of them if it has never run one. In progress = neither the first
// column nor done; Urgent = open urgent tickets.
type Stats struct {
	Open         int     `json:"open"`
	InProgress   int     `json:"in_progress"`
	Done         int     `json:"done"`
	Urgent       int     `json:"urgent"`
	Active       string  `json:"active"` // latest ticket change, or the board's own
	SprintNumber *int    `json:"sprint_number"`
	SprintEndsAt *string `json:"sprint_ends_at"`
}

type Store struct{ DB *pgxpool.Pool }

const cols = `id, project_id, owner_clerk_id, name, description, estimate_scale, style, team_id, agent_review, dev_complete_column_id, review_column_id,
	to_char(archived_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
	NOT EXISTS (SELECT 1 FROM tickets t WHERE t.board_id = boards.id) AND NOT EXISTS (SELECT 1 FROM sprints sp WHERE sp.board_id = boards.id),
	to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
	to_char(updated_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')`

func scan(row pgx.Row) (Board, error) {
	var b Board
	err := row.Scan(&b.ID, &b.ProjectID, &b.OwnerClerkID, &b.Name, &b.Description, &b.EstimateScale, &b.Style, &b.TeamID, &b.AgentReview, &b.DevComplete, &b.ReviewColumn, &b.ArchivedAt, &b.Deletable, &b.CreatedAt, &b.UpdatedAt)
	return b, notFound(err)
}

// notFound maps "no rows" and malformed uuids (22P02) to ErrNotFound.
func notFound(err error) error {
	var pg *pgconn.PgError
	if errors.Is(err, pgx.ErrNoRows) || (errors.As(err, &pg) && pg.Code == "22P02") {
		return ErrNotFound
	}
	return err
}

// Create inserts a board (one per team) into the owner's project, set up like the Simple template.
// ErrNotFound when the project isn't the owner's.
func (s Store) Create(ctx context.Context, owner, projectID, name, desc string) (*Board, error) {
	return s.CreateFrom(ctx, owner, projectID, name, desc, Builtins[0])
}

// ListByProject lists a project's boards (without columns); callers check ownership.
func (s Store) ListByProject(ctx context.Context, projectID string) ([]Board, error) {
	rows, err := s.DB.Query(ctx, `SELECT boards.id, boards.project_id, boards.owner_clerk_id, boards.name, boards.description, boards.estimate_scale, boards.style, boards.team_id, boards.agent_review, boards.dev_complete_column_id, boards.review_column_id,
		to_char(boards.archived_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
		NOT EXISTS (SELECT 1 FROM tickets ut WHERE ut.board_id = boards.id) AND NOT EXISTS (SELECT 1 FROM sprints us WHERE us.board_id = boards.id),
		to_char(boards.created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
		to_char(boards.updated_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
		count(t.tid) FILTER (WHERE NOT t.done),
		count(t.tid) FILTER (WHERE NOT t.done AND t.column_id <> (
			SELECT fc.id FROM columns fc WHERE fc.board_id = boards.id ORDER BY fc.position LIMIT 1)),
		count(t.tid) FILTER (WHERE t.done),
		count(t.tid) FILTER (WHERE t.priority = 'urgent' AND NOT t.done),
		to_char(greatest(boards.updated_at, max(t.changed)) AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
		sp.number, to_char(sp.ends_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
		FROM boards
		LEFT JOIN sprints sp ON sp.board_id = boards.id AND sp.closed_at IS NULL
		LEFT JOIN LATERAL (SELECT t.id AS tid, t.column_id, t.priority, t.updated_at AS changed, `+TicketDoneSQL+` AS done
			FROM tickets t WHERE t.board_id = boards.id AND t.sprint_id IS NOT DISTINCT FROM sp.id) t ON true
		WHERE boards.project_id = $1 AND boards.archived_at IS NULL GROUP BY boards.id, sp.id ORDER BY boards.created_at`, projectID)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Board, error) {
		b, st := Board{}, Stats{}
		err := row.Scan(&b.ID, &b.ProjectID, &b.OwnerClerkID, &b.Name, &b.Description, &b.EstimateScale, &b.Style, &b.TeamID, &b.AgentReview, &b.DevComplete, &b.ReviewColumn, &b.ArchivedAt, &b.Deletable, &b.CreatedAt, &b.UpdatedAt,
			&st.Open, &st.InProgress, &st.Done, &st.Urgent, &st.Active, &st.SprintNumber, &st.SprintEndsAt)
		b.Stats = &st
		return b, err
	})
	if out == nil {
		out = []Board{}
	}
	return out, err
}

// UpdateSettings changes the board name and/or estimate scale. Estimates on the old scale mean
// nothing on the new one, so they're cleared, except on closed sprints' tickets, which velocity reads.
func (s Store) UpdateSettings(ctx context.Context, id, owner, name string, scale *string) error {
	return pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		var old string
		if err := tx.QueryRow(ctx, `SELECT estimate_scale FROM boards WHERE id = $1 AND owner_clerk_id = $2 FOR UPDATE`, id, owner).Scan(&old); err != nil {
			return notFound(err)
		}
		updatedScale := old
		if scale != nil {
			updatedScale = *scale
		}
		if name == "" && old == updatedScale {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE boards SET name = COALESCE(NULLIF($3, ''), name), estimate_scale = $4, updated_at = now()
			WHERE id = $1 AND owner_clerk_id = $2`, id, owner, name, updatedScale); err != nil {
			return err
		}
		if scale != nil && old != updatedScale {
			// open estimates on the board and on tickets planned into its upcoming sprints
			if _, err := tx.Exec(ctx, `UPDATE tickets t SET estimate = NULL, updated_at = now()
				WHERE (t.board_id = $1 OR t.planned_sprint_id IN (SELECT ps.id FROM planned_sprints ps WHERE ps.board_id = $1)) AND t.estimate IS NOT NULL
				AND NOT EXISTS (SELECT 1 FROM sprints sp WHERE sp.id = t.sprint_id AND sp.closed_at IS NOT NULL)`, id); err != nil {
				return err
			}
		}
		return nil
	})
}

// SetEstimateScale changes how the board estimates. Estimates on the old scale mean nothing on
// the new one, so they're cleared, except on closed sprints' tickets, which velocity history reads.
func (s Store) SetEstimateScale(ctx context.Context, id, owner, scale string) error {
	return s.UpdateSettings(ctx, id, owner, "", &scale)
}

// KanbanDoneDays is how long a finished ticket stays in a kanban board's Done column.
const KanbanDoneDays = 14

// SetStyle switches the board between sprints and kanban. Going kanban ends the open sprint, if
// any: it closes now with no successor, its done tickets stay with it (velocity history) and the
// rest leave it, so they show on the kanban board.
// SetAgentReview turns agent review on or off. Tickets already waiting keep waiting (a person settles them).
func (s Store) SetAgentReview(ctx context.Context, id, owner string, on bool) error {
	tag, err := s.DB.Exec(ctx, `UPDATE boards SET agent_review = $3, updated_at = now() WHERE id = $1 AND owner_clerk_id = $2`, id, owner, on)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ErrBadDevComplete: the Dev complete column must be one of the board's own, between To do and Done.
var ErrBadDevComplete = httpx.Code("bad_dev_complete", "the Dev complete column must be one of this board's columns, other than To do and Done")

// ErrBadReviewColumn: the review column must be one of the board's own, between To do and Done (TL-592).
var ErrBadReviewColumn = httpx.Code("bad_review_column", "the review column must be one of this board's columns, other than To do and Done")

// SetDevComplete picks the column merged PRs move tickets to ("" turns it off: merging finishes them).
func (s Store) SetDevComplete(ctx context.Context, id, owner, column string) error {
	return s.setMiddleColumn(ctx, id, owner, column, "dev_complete_column_id", ErrBadDevComplete)
}

// SetReviewColumn picks the column agents' finished work waits in for review ("": the column before Done).
func (s Store) SetReviewColumn(ctx context.Context, id, owner, column string) error {
	return s.setMiddleColumn(ctx, id, owner, column, "review_column_id", ErrBadReviewColumn)
}

// setMiddleColumn sets the board's field (a column setting; a constant, never input) to one of its
// columns between To do and Done, or clears it for "". bad when column isn't one of those.
func (s Store) setMiddleColumn(ctx context.Context, id, owner, column, field string, bad error) error {
	tag, err := s.DB.Exec(ctx, `WITH pick AS (SELECT c.id FROM columns c WHERE c.board_id = $1 AND c.id::text = $3
			AND c.position > (SELECT min(position) FROM columns WHERE board_id = $1)
			AND c.position < (SELECT max(position) FROM columns WHERE board_id = $1))
		UPDATE boards SET `+field+` = (SELECT id FROM pick), updated_at = now()
		WHERE id = $1 AND owner_clerk_id = $2 AND ($3 = '' OR EXISTS (SELECT 1 FROM pick))`,
		id, owner, column)
	if err != nil {
		return notFound(err)
	}
	if tag.RowsAffected() == 0 {
		if err := s.DB.QueryRow(ctx, `SELECT id FROM boards WHERE id = $1 AND owner_clerk_id = $2`, id, owner).Scan(&id); err != nil {
			return notFound(err)
		}
		return bad
	}
	return nil
}

func (s Store) SetStyle(ctx context.Context, id, owner, style string) error {
	return pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT id FROM boards WHERE id = $1 AND owner_clerk_id = $2 FOR UPDATE`, id, owner).Scan(&id); err != nil {
			return notFound(err)
		}
		var open *string
		if err := tx.QueryRow(ctx, `SELECT id FROM sprints WHERE board_id = $1 AND closed_at IS NULL`, id).Scan(&open); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if style == "kanban" && open != nil {
			if err := RecordScope(ctx, tx, *open); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE sprints SET closed_at = now() WHERE id = $1`, *open); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE tickets t SET sprint_id = NULL WHERE t.sprint_id = $1 AND NOT `+TicketDoneSQL, *open); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `UPDATE boards SET style = $2, updated_at = now() WHERE id = $1 AND style <> $2`, id, style)
		return err
	})
}

// Get returns the board with ordered columns and tickets if owner holds it.
func (s Store) Get(ctx context.Context, id, owner string) (*Board, error) {
	b, err := scan(s.DB.QueryRow(ctx, `SELECT `+cols+` FROM boards WHERE id = $1 AND owner_clerk_id = $2`, id, owner))
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(ctx, `SELECT id, name, position, wip_limit FROM columns WHERE board_id = $1 ORDER BY position`, id)
	if err != nil {
		return nil, err
	}
	b.Columns, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (Column, error) {
		c := Column{Tickets: []Ticket{}}
		return c, row.Scan(&c.ID, &c.Name, &c.Position, &c.WIPLimit)
	})
	if err != nil {
		return nil, err
	}
	idx := make(map[string]int, len(b.Columns))
	for i, c := range b.Columns {
		idx[c.ID] = i
	}
	// only the open sprint's tickets (or all, if the board has never run a sprint)
	var sprintID *string
	if sp, err := ScanSprint(s.DB.QueryRow(ctx, `SELECT `+SprintCols+` FROM sprints WHERE board_id = $1 AND closed_at IS NULL`, id)); err == nil {
		b.Sprint, sprintID = &sp, &sp.ID
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	// kanban boards never run sprints; their Done column only keeps recent work
	recent := `true`
	if b.Style == "kanban" {
		recent = `(t.done_at IS NULL OR t.done_at > now() - make_interval(days => ` + strconv.Itoa(KanbanDoneDays) + `))`
		if err := s.DB.QueryRow(ctx, `SELECT count(*) FROM tickets t WHERE t.board_id = $1 AND NOT `+recent, id).Scan(&b.HiddenDone); err != nil {
			return nil, err
		}
	}
	rows, err = s.DB.Query(ctx, `SELECT `+TicketCols+` FROM tickets t
		WHERE t.board_id = $1 AND t.sprint_id IS NOT DISTINCT FROM $2 AND `+recent+` ORDER BY t.position, t.created_at`, id, sprintID)
	if err != nil {
		return nil, err
	}
	tickets, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Ticket, error) { return ScanTicket(row) })
	if err != nil {
		return nil, err
	}
	for _, t := range tickets {
		c := &b.Columns[idx[*t.ColumnID]]
		c.Tickets = append(c.Tickets, t)
	}
	return &b, nil
}

// Delete removes a board that was never used (no tickets, no sprints). A used one is archived instead
// (ErrBoardUsed), so its history isn't lost.
func (s Store) Delete(ctx context.Context, id, owner string) error {
	b, err := s.Get(ctx, id, owner)
	if err != nil {
		return err
	}
	if !b.Deletable {
		return ErrBoardUsed
	}
	_, err = s.DB.Exec(ctx, `DELETE FROM boards WHERE id = $1 AND owner_clerk_id = $2
		AND NOT EXISTS (SELECT 1 FROM tickets t WHERE t.board_id = boards.id)`, id, owner)
	return err
}

var (
	ErrBoardUsed     = httpx.Code("board_used", "this board has been used; archive it instead, so its history is kept")
	ErrOpenTickets   = httpx.Code("open_tickets", "this board still has unfinished tickets; finish them or move them first")
	ErrBoardArchived = httpx.Code("board_archived", "this board is archived; restore it first")
)

// Archive takes a used board off the project: refused while it has unfinished tickets. Its open sprint
// closes with no successor (finished tickets stay with it, for velocity history), its planned sprints
// go and their tickets return to the backlog unplanned.
func (s Store) Archive(ctx context.Context, id, owner string) error {
	return pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		var archived *time.Time
		if err := tx.QueryRow(ctx, `SELECT archived_at FROM boards WHERE id = $1 AND owner_clerk_id = $2 FOR UPDATE`, id, owner).Scan(&archived); err != nil {
			return notFound(err)
		}
		if archived != nil {
			return nil
		}
		var open int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM tickets t WHERE t.board_id = $1 AND NOT `+TicketDoneSQL, id).Scan(&open); err != nil {
			return err
		}
		if open > 0 {
			return ErrOpenTickets
		}
		for _, q := range []string{
			`UPDATE sprints SET closed_at = now() WHERE board_id = $1 AND closed_at IS NULL`,
			`UPDATE tickets SET planned_sprint_id = NULL, estimate = NULL, updated_at = now()
				WHERE planned_sprint_id IN (SELECT ps.id FROM planned_sprints ps WHERE ps.board_id = $1)`,
			`DELETE FROM planned_sprints WHERE board_id = $1`,
			`UPDATE boards SET archived_at = now(), updated_at = now() WHERE id = $1`,
		} {
			if _, err := tx.Exec(ctx, q, id); err != nil {
				return err
			}
		}
		return nil
	})
}

// Restore puts an archived board back on the project. A sprint board starts its next sprint when someone starts one.
func (s Store) Restore(ctx context.Context, id, owner string) error {
	tag, err := s.DB.Exec(ctx, `UPDATE boards SET archived_at = NULL, updated_at = now() WHERE id = $1 AND owner_clerk_id = $2`, id, owner)
	if err != nil {
		return notFound(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Archived lists a project's archived boards, most recently archived first.
func (s Store) Archived(ctx context.Context, owner, projectID string) ([]Board, error) {
	rows, err := s.DB.Query(ctx, `SELECT `+cols+` FROM boards WHERE project_id::text = $1 AND owner_clerk_id = $2 AND archived_at IS NOT NULL
		ORDER BY archived_at DESC`, projectID, owner)
	if err != nil {
		return nil, notFound(err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Board, error) { return scan(row) })
	if out == nil {
		out = []Board{}
	}
	return out, err
}
