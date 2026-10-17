package tickets

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/boards"
)

var (
	ErrNotFound            = errors.New("ticket not found")
	ErrInvalidType         = errors.New("type must be bug, feature or task")
	ErrBadBacklogSort      = errors.New("sort must be manual, newest or oldest")
	ErrAlreadyAssigned     = errors.New("ticket is already assigned")
	ErrNotAssignedToCaller = errors.New("ticket is not assigned to caller")
	ErrSelfParent          = errors.New("a ticket can't be its own parent")
	ErrParentLoop          = errors.New("that parent is already a sub-ticket of this ticket")
	ErrOtherProject        = errors.New("tickets must belong to the same project")
	ErrIsDuplicate         = errors.New("a duplicate ticket is already closed")
)

type Ticket = boards.Ticket

// scaleSQL is ticket t's estimate scale: its board's, or for a backlog ticket planned into an
// upcoming sprint, that sprint's board's. NULL in the plain backlog (no estimates there).
const scaleSQL = `COALESCE((SELECT estimate_scale FROM boards WHERE id = t.board_id),
	(SELECT pb.estimate_scale FROM planned_sprints ps JOIN boards pb ON pb.id = ps.board_id WHERE ps.id = t.planned_sprint_id))`

type Store struct{ DB *pgxpool.Pool }

// ValidType reports whether t is a ticket type; "" means "unchanged" on update.
func ValidType(t string) bool { return t == "bug" || t == "feature" || t == "task" || t == TypeEpic }

// notFound maps "no rows" and malformed uuids (22P02) to ErrNotFound.
func notFound(err error) error {
	var pg *pgconn.PgError
	if errors.Is(err, pgx.ErrNoRows) || (errors.As(err, &pg) && pg.Code == "22P02") {
		return ErrNotFound
	}
	return err
}

// Create appends a ticket to columnID; legacy/internal callers are attributed to owner.
func (s Store) Create(ctx context.Context, owner, columnID, typ, title, desc string) (*Ticket, error) {
	return s.CreateAs(ctx, owner, owner, columnID, typ, title, desc)
}

// CreateAs appends a ticket and records the authenticated actor separately from its owner.
// It joins the board's open sprint, if any. On that sprint's last day the sprint is locked, so the
// ticket goes to the bottom of the project backlog instead (ColumnID nil tells the caller).
// No estimate is asked for (internal callers and tests); people and agents use CreateEstimated.
func (s Store) CreateAs(ctx context.Context, owner, actor, columnID, typ, title, desc string) (*Ticket, error) {
	return s.create(ctx, owner, actor, columnID, typ, title, desc, "", false)
}

// CreateEstimated is CreateAs for people and agents: on a board with an estimate scale the
// estimate is required (EstimateRequiredError) and must be on the scale.
func (s Store) CreateEstimated(ctx context.Context, owner, actor, columnID, typ, title, desc, estimate string) (*Ticket, error) {
	return s.create(ctx, owner, actor, columnID, typ, title, desc, estimate, true)
}

func (s Store) create(ctx context.Context, owner, actor, columnID, typ, title, desc, estimate string, require bool) (*Ticket, error) {
	if typ == TypeEpic {
		return nil, ErrEpicOffBoard // made in the backlog (TL-213)
	}
	if !ValidType(typ) {
		return nil, ErrInvalidType
	}
	var boardID, projectID, scale string
	var archived bool
	if err := s.DB.QueryRow(ctx, `SELECT b.id, b.project_id, b.estimate_scale, b.archived_at IS NOT NULL FROM columns c JOIN boards b ON b.id = c.board_id
		JOIN projects p ON p.id = b.project_id WHERE c.id = $1 AND p.owner_clerk_id = $2`, columnID, owner).Scan(&boardID, &projectID, &scale, &archived); err != nil {
		return nil, notFound(err)
	}
	if archived {
		return nil, boards.ErrBoardArchived
	}
	if locked, err := lockedSprint(ctx, s.DB, boardID); err != nil {
		return nil, err
	} else if locked > 0 {
		return s.CreateBacklogAs(ctx, owner, actor, projectID, typ, title, desc) // the backlog has no scale
	}
	if !boards.ValidEstimate(scale, estimate) {
		return nil, boards.ErrBadEstimate
	}
	if require && scale != "none" && estimate == "" {
		return nil, EstimateRequiredError{Scale: scale}
	}
	t, err := boards.ScanTicket(s.DB.QueryRow(ctx, `WITH placed AS (
			SELECT b.project_id, b.id AS board_id, c.id AS column_id,
				(SELECT id FROM sprints WHERE board_id = b.id AND closed_at IS NULL) AS sprint_id
			FROM columns c JOIN boards b ON b.id = c.board_id JOIN projects p ON p.id = b.project_id
			WHERE c.id = $1 AND p.owner_clerk_id = $2)
		INSERT INTO tickets AS t (project_id, board_id, column_id, sprint_id, type, title, description, priority, position, created_by, estimate)
		SELECT project_id, board_id, column_id, sprint_id, $3::ticket_type, $4, $5, 'medium'::priority, COALESCE((SELECT max(position) + 1 FROM tickets x WHERE x.column_id = placed.column_id
				AND x.sprint_id IS NOT DISTINCT FROM placed.sprint_id), 0), $6, NULLIF($7, '')
		FROM placed RETURNING `+boards.TicketCols, columnID, owner, typ, title, desc, actor, estimate))
	if err != nil {
		return nil, notFound(err)
	}
	return &t, nil
}

// CreateBacklog adds a ticket to the bottom of the project's backlog.
func (s Store) CreateBacklog(ctx context.Context, owner, projectID, typ, title, desc string) (*Ticket, error) {
	return s.CreateBacklogAs(ctx, owner, owner, projectID, typ, title, desc)
}

// CreateBacklogAs adds a ticket and records the authenticated actor.
func (s Store) CreateBacklogAs(ctx context.Context, owner, actor, projectID, typ, title, desc string) (*Ticket, error) {
	if !ValidType(typ) {
		return nil, ErrInvalidType
	}
	t, err := boards.ScanTicket(s.DB.QueryRow(ctx, `INSERT INTO tickets AS t (project_id, type, title, description, priority, position, created_by)
		SELECT p.id, $3::ticket_type, $4, $5, 'medium'::priority, COALESCE((SELECT max(position) + 1 FROM tickets x WHERE x.project_id = p.id AND x.board_id IS NULL), 0), $6
		FROM projects p WHERE p.id = $1 AND p.owner_clerk_id = $2
		RETURNING `+boards.TicketCols, projectID, owner, typ, title, desc, actor))
	if err != nil {
		return nil, notFound(err)
	}
	return &t, nil
}

// Backlog lists the project's backlog; typ "" means all types.
func (s Store) Backlog(ctx context.Context, owner, projectID, typ string) ([]Ticket, error) {
	if typ != "" && !ValidType(typ) {
		return nil, ErrInvalidType
	}
	var ok bool
	if err := s.DB.QueryRow(ctx, `SELECT true FROM projects WHERE id = $1 AND owner_clerk_id = $2`, projectID, owner).Scan(&ok); err != nil {
		return nil, notFound(err)
	}
	rows, err := s.DB.Query(ctx, `SELECT `+boards.TicketCols+` FROM tickets t
		WHERE t.project_id = $1 AND t.board_id IS NULL AND t.resolved_at IS NULL AND ($2 = '' OR t.type::text = $2)
		ORDER BY t.position, t.created_at`, projectID, typ)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Ticket, error) { return boards.ScanTicket(row) })
	if out == nil {
		out = []Ticket{}
	}
	return out, err
}

// BacklogPage is one page of a project's backlog plus how many tickets match each type tab.
type BacklogPage struct {
	Tickets []Ticket       `json:"tickets"`
	Counts  map[string]int `json:"counts"` // all, bug, feature, task: label and priority filters applied, type filter not
	Labels  []LabelCount   `json:"labels"` // labels in the whole backlog (no filters), for the label filter
}

const MaxPerPage = 100

// PageBacklog returns page (1-based) of the backlog filtered by typ ("" = all), labels (any match,
// case-insensitive) and priorities (any of; none = all), ready tickets before blocked ones. Backlog
// stays the unpaged list.
func (s Store) PageBacklog(ctx context.Context, owner, projectID, typ string, labels []string, page, perPage int, priorities ...string) (*BacklogPage, error) {
	return s.PageBacklogSorted(ctx, owner, projectID, typ, labels, page, perPage, "manual", priorities...)
}

// PageBacklogSorted orders the filtered backlog before paging. Date sorts include every ticket;
// manual order keeps ready tickets before blocked ones, then follows the user's backlog positions.
func (s Store) PageBacklogSorted(ctx context.Context, owner, projectID, typ string, labels []string, page, perPage int, sortBy string, priorities ...string) (*BacklogPage, error) {
	return s.PageBacklogIn(ctx, owner, projectID, "", typ, labels, page, perPage, sortBy, priorities...)
}

// PageBacklogIn is PageBacklogSorted narrowed to epic's sub-tickets at any depth ("" for all, TL-215).
func (s Store) PageBacklogIn(ctx context.Context, owner, projectID, epic, typ string, labels []string, page, perPage int, sortBy string, priorities ...string) (*BacklogPage, error) {
	if typ != "" && !ValidType(typ) {
		return nil, ErrInvalidType
	}
	if sortBy == "" {
		sortBy = "manual"
	}
	if sortBy != "manual" && sortBy != "newest" && sortBy != "oldest" {
		return nil, ErrBadBacklogSort
	}
	if !ValidPriorities(priorities) {
		return nil, ErrBadPriority
	}
	priorities = nonNil(priorities)
	var ok bool
	if err := s.DB.QueryRow(ctx, `SELECT true FROM projects WHERE id = $1 AND owner_clerk_id = $2`, projectID, owner).Scan(&ok); err != nil {
		return nil, notFound(err)
	}
	lower := make([]string, len(labels))
	for i, l := range labels {
		lower[i] = strings.ToLower(l)
	}
	where := `t.project_id = $1 AND t.board_id IS NULL AND t.resolved_at IS NULL AND (cardinality($2::text[]) = 0 OR EXISTS (
		SELECT 1 FROM ticket_labels l WHERE l.ticket_id = t.id AND lower(l.label) = ANY($2)))
		AND (cardinality($3::text[]) = 0 OR t.priority::text = ANY($3)) AND ` + inEpic("$4::text")

	out := &BacklogPage{Counts: map[string]int{"all": 0, "bug": 0, "feature": 0, "task": 0, "epic": 0}}
	rows, err := s.DB.Query(ctx, `SELECT t.type::text, count(*) FROM tickets t WHERE `+where+` GROUP BY 1`, projectID, lower, priorities, epic)
	if err != nil {
		return nil, err
	}
	var typed string
	var n int
	if _, err := pgx.ForEachRow(rows, []any{&typed, &n}, func() error {
		out.Counts[typed] = n
		out.Counts["all"] += n
		return nil
	}); err != nil {
		return nil, err
	}

	rows, err = s.DB.Query(ctx, `SELECT min(l.label), count(*) FROM ticket_labels l JOIN tickets t ON t.id = l.ticket_id
		WHERE t.project_id = $1 AND t.board_id IS NULL AND t.resolved_at IS NULL GROUP BY lower(l.label) ORDER BY count(*) DESC, lower(min(l.label))`, projectID)
	if err != nil {
		return nil, err
	}
	if out.Labels, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (LabelCount, error) {
		var c LabelCount
		return c, row.Scan(&c.Label, &c.Count)
	}); err != nil {
		return nil, err
	}
	if out.Labels == nil {
		out.Labels = []LabelCount{}
	}

	order := boards.TicketBlockedSQL + `, t.position, t.created_at, t.id`
	switch sortBy {
	case "newest":
		order = `t.created_at DESC, t.position, t.id`
	case "oldest":
		order = `t.created_at, t.position, t.id`
	}
	rows, err = s.DB.Query(ctx, `SELECT `+boards.TicketCols+` FROM tickets t
		WHERE `+where+` AND ($5 = '' OR t.type::text = $5)
		ORDER BY `+order+` LIMIT $6 OFFSET $7`,
		projectID, lower, priorities, epic, typ, perPage, (page-1)*perPage)
	if err != nil {
		return nil, err
	}
	if out.Tickets, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (Ticket, error) { return boards.ScanTicket(row) }); err != nil {
		return nil, err
	}
	if out.Tickets == nil {
		out.Tickets = []Ticket{}
	}
	return out, nil
}

// Update edits a ticket; typ "" keeps the current type; priority "" keeps the current priority.
func (s Store) Update(ctx context.Context, owner, id, typ, title, desc, priority string) error {
	if typ != "" && !ValidType(typ) {
		return ErrInvalidType
	}
	if typ == TypeEpic {
		if err := s.becomeEpic(ctx, owner, id); err != nil {
			return err
		}
	}
	return exec(s.DB.Exec(ctx, `UPDATE tickets t SET title = $3, description = $4,
		type = COALESCE(NULLIF($5, '')::ticket_type, t.type), priority = COALESCE(NULLIF($6, '')::priority, t.priority), updated_at = now()
		FROM projects p WHERE t.id = $1 AND p.id = t.project_id AND p.owner_clerk_id = $2`, id, owner, title, desc, typ, priority))
}

// becomeEpic gets ticket id ready to be an epic (TL-213): no epic above or below it, off its board (to
// the backlog) and out of any planned sprint, with no estimate.
// ponytail: the move to the backlog commits before the type changes; if that update then fails, the
// ticket is just a backlog ticket, which is harmless.
func (s Store) becomeEpic(ctx context.Context, owner, id string) error {
	var onBoard bool
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT t.board_id IS NOT NULL FROM tickets t JOIN projects p ON p.id = t.project_id
			WHERE t.id::text = $1 AND p.owner_clerk_id = $2`, id, owner).Scan(&onBoard); err != nil {
			return notFound(err)
		}
		return canBecomeEpic(ctx, tx, id)
	})
	if err != nil {
		return err
	}
	if onBoard { // just the epic: its sub-tickets are the work, and stay on their boards (not TL-218's group move)
		if err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error { _, err := toBacklogTx(ctx, tx, owner, id, false); return err }); err != nil {
			return err
		}
	}
	_, err = s.DB.Exec(ctx, `UPDATE tickets SET planned_sprint_id = NULL, estimate = NULL WHERE id::text = $1`, id)
	return err
}

// SetEstimate sets ("" clears) the ticket's estimate; it must be on its board's scale, so a
// backlog ticket (no board) can only be cleared.
func (s Store) SetEstimate(ctx context.Context, owner, id, estimate string) error {
	return s.setEstimate(ctx, owner, id, estimate, false)
}

// SetRequiredEstimate is SetEstimate for people and agents: an estimate can't be cleared on a board
// with an estimate scale.
func (s Store) SetRequiredEstimate(ctx context.Context, owner, id, estimate string) error {
	return s.setEstimate(ctx, owner, id, estimate, true)
}

func (s Store) setEstimate(ctx context.Context, owner, id, estimate string, require bool) error {
	var scale string
	if err := s.DB.QueryRow(ctx, `SELECT COALESCE(`+scaleSQL+`, 'none') FROM tickets t
		JOIN projects p ON p.id = t.project_id AND p.owner_clerk_id = $2 WHERE t.id = $1`, id, owner).Scan(&scale); err != nil {
		return notFound(err)
	}
	if estimate != "" {
		var epic bool
		if err := s.DB.QueryRow(ctx, `SELECT type = 'epic' FROM tickets WHERE id::text = $1`, id).Scan(&epic); err == nil && epic {
			return ErrEpicEstimate
		}
	}
	if !boards.ValidEstimate(scale, estimate) {
		return boards.ErrBadEstimate
	}
	if require && scale != "none" && estimate == "" {
		return EstimateRequiredError{Scale: scale}
	}
	return exec(s.DB.Exec(ctx, `UPDATE tickets SET estimate = NULLIF($2, ''), updated_at = now() WHERE id = $1`, id, estimate))
}

// Claim assigns an unassigned ticket to actor atomically and returns the updated ticket.
func (s Store) Claim(ctx context.Context, owner, actor, id string) (*Ticket, error) {
	t, err := boards.ScanTicket(s.DB.QueryRow(ctx, `UPDATE tickets t SET assigned_to = $3, updated_at = now()
		FROM projects p WHERE t.id = $1 AND p.id = t.project_id AND p.owner_clerk_id = $2
		AND t.assigned_to IS NULL AND NOT `+boards.TicketBlockedSQL+` RETURNING `+boards.TicketCols, id, owner, actor))
	if err == nil {
		return &t, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, notFound(err)
	}
	current, err := boards.ScanTicket(s.DB.QueryRow(ctx, `SELECT `+boards.TicketCols+` FROM tickets t
		JOIN projects p ON p.id = t.project_id WHERE t.id = $1 AND p.owner_clerk_id = $2`, id, owner))
	if err != nil {
		return nil, notFound(err)
	}
	if current.AssignedTo != nil && *current.AssignedTo == actor {
		return &current, nil // idempotent retry by the current assignee
	}
	if current.Blocked && current.AssignedTo == nil {
		return nil, ErrBlocked // can't be picked up until its blockers are done
	}
	return nil, ErrAlreadyAssigned
}

// Release clears an assignment only when the caller currently owns it.
func (s Store) Release(ctx context.Context, owner, actor, id string) error {
	tag, err := s.DB.Exec(ctx, `UPDATE tickets t SET assigned_to = NULL, updated_at = now()
		FROM projects p WHERE t.id = $1 AND p.id = t.project_id AND p.owner_clerk_id = $2 AND t.assigned_to = $3`, id, owner, actor)
	if err != nil {
		return notFound(err)
	}
	if tag.RowsAffected() == 0 {
		var ok bool
		if err := s.DB.QueryRow(ctx, `SELECT true FROM tickets t JOIN projects p ON p.id = t.project_id
			WHERE t.id = $1 AND p.owner_clerk_id = $2`, id, owner).Scan(&ok); err != nil {
			return notFound(err)
		}
		return ErrNotAssignedToCaller
	}
	return nil
}

// Delete removes a ticket; if it was its parent's last unfinished sub-ticket, the parent completes.
func (s Store) Delete(ctx context.Context, owner, id string) error {
	// into the bin, not gone (TL-256): hidden everywhere now, purged with its comments and the rest
	// after Retention; its files go in today's deleted batch, deleted from storage on the same clock
	var parent *string
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `UPDATE tickets_all t SET deleted_at = now() FROM projects p
			WHERE t.id = $1 AND p.id = t.project_id AND p.owner_clerk_id = $2 AND t.deleted_at IS NULL RETURNING t.parent_id`, id, owner).Scan(&parent); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO deleted_files (file_key) SELECT file_key FROM ticket_attachments WHERE ticket_id = $1 ON CONFLICT DO NOTHING`, id)
		return err
	})
	if err != nil {
		return notFound(err)
	}
	if parent == nil {
		return nil
	}
	return s.completeIfFinished(ctx, owner, *parent)
}

func exec(tag pgconn.CommandTag, err error) error {
	if err != nil {
		return notFound(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Move puts ticket id at position pos (clamped) in toColumn. The column's board must be in
// the ticket's project; the ticket can come from another column, another board or the
// backlog, and joins the target board's open sprint. Same column = reorder. A resolved ticket
// reopens; finishing the parent's last unfinished sub-ticket completes the parent. Entering a
// sprint on its last day is refused (SprintLockedError); moves inside the sprint, to Done, or out
// of it are fine.
func (s Store) Move(ctx context.Context, owner, id, toColumn string, pos int) error {
	_, err := s.MoveEstimated(ctx, owner, id, toColumn, pos, nil, false, false)
	return err
}

// MoveEstimated is Move that can size the ticket on the way (estimate, on the target board's scale).
// With require, a ticket entering a board with an estimate scale must end up estimated
// (EstimateRequiredError); moves within the board aren't checked.
// With agent (an API key moved it) on a board with agent review, a move into Done lands in the
// column before Done waiting for review instead (review is true).
func (s Store) MoveEstimated(ctx context.Context, owner, id, toColumn string, pos int, estimate *string, require, agent bool) (review bool, err error) {
	review, _, err = s.MoveGroup(ctx, owner, id, toColumn, pos, estimate, require, agent)
	return review, err
}

// MoveGroup is MoveEstimated that also reports the open sub-tickets that came with it when it left its
// board for another (TL-218).
func (s Store) MoveGroup(ctx context.Context, owner, id, toColumn string, pos int, estimate *string, require, agent bool) (review bool, alsoMoved []Moved, err error) {
	o := moveOpts{lock: true, require: require, estimate: estimate, agent: agent}
	if err := s.move(ctx, owner, id, toColumn, pos, &o); err != nil {
		return false, nil, err
	}
	if o.alsoMoved == nil {
		o.alsoMoved = []Moved{}
	}
	return o.review, o.alsoMoved, s.completeParent(ctx, owner, id)
}

// moveOpts: lock refuses entering a sprint on its last day (off for completing a parent, which
// isn't adding scope); require and estimate as in MoveEstimated.
type moveOpts struct {
	lock, require bool
	estimate      *string
	agent         bool // set by: an API key; review is set when its move into Done became a review request
	review        bool
	left          *string // set by the move: the board it left for another one (its sub-tickets follow, TL-218)
	toBoard       string
	alsoMoved     []Moved // its open sub-tickets that came with it
}

func (s Store) move(ctx context.Context, owner, id, toColumn string, pos int, o *moveOpts) error {
	return pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		if err := s.moveTx(ctx, tx, owner, id, toColumn, pos, o); err != nil || o.left == nil {
			return err
		}
		// it left its board for another: its open sub-tickets there come along, to the new board's first
		// column, by the same rules; one that can't stops the whole move (TL-218)
		var first string
		if err := tx.QueryRow(ctx, `SELECT id FROM columns WHERE board_id = $1 ORDER BY position LIMIT 1`, o.toBoard).Scan(&first); err != nil {
			return err
		}
		subs, err := openSubTickets(ctx, tx, id, *o.left)
		for _, sub := range subs {
			if err != nil {
				break
			}
			so := moveOpts{lock: o.lock, require: o.require, agent: o.agent}
			if err = s.moveTx(ctx, tx, owner, sub.ID, first, 1<<30, &so); err != nil {
				err = GroupMoveError{Sub: sub, Err: err}
			}
		}
		o.alsoMoved = subs
		return err
	})
}

func (s Store) moveTx(ctx context.Context, tx pgx.Tx, owner, id, toColumn string, pos int, o *moveOpts) error {
	{
		if err := boards.SetActor(ctx, tx, ""); err != nil { // for the ticket's history
			return err
		}
		var epic bool
		if err := tx.QueryRow(ctx, `SELECT type = 'epic' FROM tickets WHERE id::text = $1`, id).Scan(&epic); err == nil && epic {
			return ErrEpicOffBoard
		}
		var boardID, scale string
		var archived, duplicate bool
		var from, fromSprint, sprint, fromBoard, fromScale, estimate *string
		// lock the target board: moves on one board are serialised
		err := tx.QueryRow(ctx, `SELECT b.id, b.estimate_scale, b.archived_at IS NOT NULL, t.column_id, t.sprint_id,
				(SELECT id FROM sprints WHERE board_id = b.id AND closed_at IS NULL),
			t.board_id, `+scaleSQL+`, t.estimate, t.duplicate_of IS NOT NULL
			FROM tickets t JOIN projects p ON p.id = t.project_id AND p.owner_clerk_id = $2
			JOIN columns c ON c.id = $3 JOIN boards b ON b.id = c.board_id AND b.project_id = t.project_id
			WHERE t.id = $1 FOR UPDATE OF b`, id, owner, toColumn).Scan(&boardID, &scale, &archived, &from, &fromSprint, &sprint, &fromBoard, &fromScale, &estimate, &duplicate)
		if err != nil {
			return notFound(err)
		}
		if archived {
			return boards.ErrBoardArchived
		}
		if duplicate {
			return ErrIsDuplicate
		}
		// an estimate only survives a move to a board on the same scale, unless one is given
		if fromScale == nil || *fromScale != scale {
			estimate = nil
		}
		if o.estimate != nil {
			if !boards.ValidEstimate(scale, *o.estimate) {
				return boards.ErrBadEstimate
			}
			estimate = o.estimate
			if *estimate == "" {
				estimate = nil
			}
		}
		// Needs review: an agent's move into Done on a review board stops one column short (a board
		// with a single column has nowhere to wait, so it goes through)
		if o.agent {
			var reviewCol *string
			if err := tx.QueryRow(ctx, `SELECT (SELECT c.id FROM columns c WHERE c.board_id = b.id ORDER BY c.position DESC OFFSET 1 LIMIT 1)
				FROM boards b WHERE b.id = $1 AND b.agent_review
					AND $2 = (SELECT d.id FROM columns d WHERE d.board_id = b.id ORDER BY d.position DESC LIMIT 1)`, boardID, toColumn).Scan(&reviewCol); err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			if reviewCol != nil {
				toColumn, o.review = *reviewCol, true
			}
		}
		entering := fromBoard == nil || *fromBoard != boardID
		if o.require && entering && scale != "none" && estimate == nil {
			return EstimateRequiredError{Scale: scale}
		}
		if o.lock && sprint != nil && (fromSprint == nil || *fromSprint != *sprint) {
			if locked, err := lockedSprint(ctx, tx, boardID); err != nil {
				return err
			} else if locked > 0 {
				return SprintLockedError{Number: locked}
			}
		}
		ids, err := columnOrder(ctx, tx, toColumn, sprint, id)
		if err != nil {
			return err
		}
		pos = max(0, min(pos, len(ids)))
		ids = append(ids[:pos], append([]string{id}, ids[pos:]...)...)
		if _, err := tx.Exec(ctx, `UPDATE tickets t SET board_id = $2, column_id = $3, sprint_id = $4, resolved_at = NULL, resolution = NULL, updated_at = now(),
				planned_sprint_id = NULL, -- on a board now: out of any plan
				estimate = $5,
				-- waiting for review only while an agent's finish is held; any other move clears it
				review_requested_at = CASE WHEN $6 THEN COALESCE(t.review_requested_at, now()) END,
				agent_review_at = CASE WHEN $6 THEN COALESCE(t.agent_review_at, now()) ELSE t.agent_review_at END,
				-- The first move out of the source board's first column starts cycle time.
				started_at = CASE WHEN t.column_id = (SELECT fc.id FROM columns fc WHERE fc.board_id = t.board_id ORDER BY fc.position LIMIT 1)
					AND $3 <> (SELECT fc.id FROM columns fc WHERE fc.board_id = t.board_id ORDER BY fc.position LIMIT 1)
					THEN COALESCE(t.started_at, now()) ELSE t.started_at END,
				-- into Done stamps the time (kept when already done); anywhere else clears it.
				-- ponytail: reordering columns doesn't restamp; Done is still decided by DoneSQL
				done_at = CASE WHEN $3 = (SELECT dc.id FROM columns dc WHERE dc.board_id = $2 ORDER BY dc.position DESC LIMIT 1)
					THEN COALESCE(t.done_at, now()) END
			WHERE t.id = $1`, id, boardID, toColumn, sprint, estimate, o.review); err != nil {
			return err
		}
		if fromBoard != nil && *fromBoard != boardID {
			o.left, o.toBoard = fromBoard, boardID
		}
		if err := renumber(ctx, tx, ids); err != nil {
			return err
		}
		if from != nil && *from == toColumn {
			return nil
		}
		return renumberSource(ctx, tx, id, from, fromSprint)
	}
}

// ToBacklog takes a ticket off its board/sprint and appends it to the project backlog. Its estimate
// goes too: the backlog has no scale.
func (s Store) ToBacklog(ctx context.Context, owner, id string) error {
	return s.ToBacklogAs(ctx, owner, id, false)
}

// ToBacklogAs moves a ticket to the backlog, refusing to let an agent bypass requested review.
func (s Store) ToBacklogAs(ctx context.Context, owner, id string, agent bool) error {
	_, err := s.ToBacklogGroup(ctx, owner, id, agent)
	return err
}

// ToBacklogGroup is ToBacklogAs that also takes the ticket's open sub-tickets on its board to the
// backlog (TL-218), returning them; one that can't go stops the whole move.
func (s Store) ToBacklogGroup(ctx context.Context, owner, id string, agent bool) ([]Moved, error) {
	var moved []Moved
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		left, err := toBacklogTx(ctx, tx, owner, id, agent)
		if err != nil || left == nil {
			return err
		}
		subs, err := openSubTickets(ctx, tx, id, *left)
		for _, sub := range subs {
			if err != nil {
				break
			}
			if _, err = toBacklogTx(ctx, tx, owner, sub.ID, agent); err != nil {
				err = GroupMoveError{Sub: sub, Err: err}
			}
		}
		moved = subs
		return err
	})
	if err != nil {
		return nil, err
	}
	if moved == nil {
		moved = []Moved{}
	}
	return moved, nil
}

// toBacklogTx sends one ticket to the backlog in tx; left is the board it was on (nil: already there).
func toBacklogTx(ctx context.Context, tx pgx.Tx, owner, id string, agent bool) (left *string, err error) {
	{
		if err := boards.SetActor(ctx, tx, ""); err != nil { // for the ticket's history
			return nil, err
		}
		var from, fromSprint, board *string
		var duplicate bool
		var reviewPending, reviewBoard bool
		err := tx.QueryRow(ctx, `SELECT t.column_id, t.sprint_id, t.board_id::text, t.duplicate_of IS NOT NULL, t.review_requested_at IS NOT NULL,
				COALESCE(b.agent_review, false) FROM tickets t
			JOIN projects p ON p.id = t.project_id AND p.owner_clerk_id = $2
			LEFT JOIN boards b ON b.id = t.board_id
			WHERE t.id = $1 FOR UPDATE OF t`, id, owner).Scan(&from, &fromSprint, &board, &duplicate, &reviewPending, &reviewBoard)
		if err != nil {
			return nil, notFound(err)
		}
		if duplicate {
			return nil, ErrIsDuplicate
		}
		// on a board where a person reviews agent work, an agent can't take a ticket off it: from the
		// backlog it could resolve the ticket as done with nobody looking (TL-249)
		if agent && (reviewPending || reviewBoard) {
			return nil, ErrAgentReviewBypass
		}
		if from == nil {
			return nil, nil // already in the backlog
		}
		if _, err := tx.Exec(ctx, `UPDATE tickets t SET board_id = NULL, column_id = NULL, sprint_id = NULL, estimate = NULL, done_at = NULL, review_requested_at = NULL, updated_at = now(),
				position = COALESCE((SELECT max(position) + 1 FROM tickets x WHERE x.project_id = t.project_id AND x.board_id IS NULL), 0)
			WHERE t.id = $1`, id); err != nil {
			return nil, err
		}
		return board, renumberSource(ctx, tx, id, from, fromSprint)
	}
}

// renumberSource closes the gap a ticket left in its old column (or the backlog when from is nil).
func renumberSource(ctx context.Context, tx pgx.Tx, id string, from, fromSprint *string) error {
	if from != nil {
		rest, err := columnOrder(ctx, tx, *from, fromSprint, id)
		if err != nil {
			return err
		}
		return renumber(ctx, tx, rest)
	}
	rows, err := tx.Query(ctx, `SELECT x.id FROM tickets x JOIN tickets t ON t.id = $1
		WHERE x.project_id = t.project_id AND x.board_id IS NULL AND x.id <> $1 ORDER BY x.position, x.created_at`, id)
	if err != nil {
		return err
	}
	rest, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	return renumber(ctx, tx, rest)
}

// columnOrder lists a column's ticket ids in one sprint (closed sprints' Done tickets stay
// in the column but are hidden, so they mustn't count towards positions), excluding skip.
func columnOrder(ctx context.Context, tx pgx.Tx, columnID string, sprint *string, skip string) ([]string, error) {
	rows, err := tx.Query(ctx, `SELECT id FROM tickets WHERE column_id = $1 AND sprint_id IS NOT DISTINCT FROM $2 AND id <> $3
		ORDER BY position, created_at`, columnID, sprint, skip)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

func renumber(ctx context.Context, tx pgx.Tx, ids []string) error {
	_, err := tx.Exec(ctx, `UPDATE tickets t SET position = o.n - 1
		FROM unnest($1::uuid[]) WITH ORDINALITY AS o(id, n) WHERE t.id = o.id`, ids)
	return err
}

// Detail is one ticket plus where it lives, for linking to it directly. Board, column and
// sprint are nil for backlog tickets.
type Detail struct {
	Ticket
	ProjectName   string         `json:"project_name"`
	BoardID       *string        `json:"board_id"`
	BoardName     *string        `json:"board_name"`
	EstimateScale string         `json:"estimate_scale"` // the board's; "none" in the backlog
	ColumnName    *string        `json:"column_name"`
	SprintNumber  *int           `json:"sprint_number"`
	Done          bool           `json:"done"`             // in the board's last column: locked for attachments
	Attachments   int            `json:"attachment_count"` // files on it (list_attachments): designs, screenshots, logs
	BlockedBy     []Dep          `json:"blocked_by"`
	Blocks        []Dep          `json:"blocks"`
	Parent        *Dep           `json:"parent"`   // the ticket this one is a sub-ticket of, if any
	Children      []Child        `json:"children"` // immediate sub-tickets, with done status
	Duplicates    []DuplicateRef `json:"duplicates"`
	Epic          *EpicProgress  `json:"epic,omitempty"` // an epic's progress and size, from its sub-tickets (TL-213)
}

type DuplicateRef struct {
	ID    string `json:"id"`
	Key   string `json:"key"`
	Title string `json:"title"`
}

type Child struct {
	ID        string  `json:"id"`
	Key       *string `json:"key"`
	Title     string  `json:"title"`
	Done      bool    `json:"done"`
	Type      string  `json:"type"`
	Assign    *string `json:"assigned_to"`
	Estimate  *string `json:"estimate"`
	BoardID   *string `json:"board_id"`   // nil: in the backlog
	BoardName *string `json:"board_name"` // an epic's page groups its sub-tickets by board (TL-214)
}

// SetParent clears (parentID nil) or sets the parent of ticket id. Both must be in the same
// project, and the parent can't be the ticket itself or one of its descendants.
func (s Store) SetParent(ctx context.Context, owner, id string, parentID *string) error {
	if parentID != nil && *parentID == id {
		return ErrSelfParent
	}
	var old *string
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		var project string
		if err := tx.QueryRow(ctx, `SELECT t.project_id FROM tickets t JOIN projects p ON p.id = t.project_id
			WHERE t.id = $1 AND p.owner_clerk_id = $2`, id, owner).Scan(&project); err != nil {
			return notFound(err)
		}
		// one parent change at a time per project, so two concurrent edits can't form a loop
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('ticket-parent:' || $1))`, project); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT parent_id FROM tickets WHERE id = $1`, id).Scan(&old); err != nil {
			return err
		}
		if parentID != nil {
			var parentProject string
			var loop bool
			// walk up from the proposed parent; reaching id means id is its ancestor (UNION stops on any old loop)
			if err := tx.QueryRow(ctx, `WITH RECURSIVE up AS (
					SELECT id, parent_id FROM tickets WHERE id = $1
					UNION SELECT t.id, t.parent_id FROM tickets t JOIN up ON t.id = up.parent_id)
				SELECT t.project_id, EXISTS (SELECT 1 FROM up WHERE up.id = $2)
				FROM tickets t JOIN projects p ON p.id = t.project_id WHERE t.id = $1 AND p.owner_clerk_id = $3`,
				*parentID, id, owner).Scan(&parentProject, &loop); err != nil {
				return notFound(err)
			}
			if parentProject != project {
				return ErrOtherProject
			}
			if loop {
				return ErrParentLoop
			}
			if err := epicNesting(ctx, tx, id, *parentID); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `UPDATE tickets SET parent_id = $2, updated_at = now() WHERE id = $1`, id, parentID)
		return err
	})
	// taking a sub-ticket away can leave the old parent with only finished ones
	if err != nil || old == nil || (parentID != nil && *parentID == *old) {
		return err
	}
	return s.completeIfFinished(ctx, owner, *old)
}

// Get returns the owner's ticket with its project/board/column/sprint context.
func (s Store) Get(ctx context.Context, owner, id string) (*Detail, error) {
	var d Detail
	var parentID *string
	dest := append(boards.TicketDest(&d.Ticket), &d.ProjectName, &d.BoardID, &d.BoardName, &d.EstimateScale, &d.ColumnName, &d.SprintNumber, &d.Done, &parentID, &d.Attachments)
	err := s.DB.QueryRow(ctx, `SELECT `+boards.TicketCols+`, p.name, t.board_id, b.name, COALESCE(`+scaleSQL+`, 'none'), c.name, sp.number, `+boards.TicketDoneSQL+`, t.parent_id,
		(SELECT count(*) FROM ticket_attachments a WHERE a.ticket_id = t.id)
		FROM tickets t JOIN projects p ON p.id = t.project_id AND p.owner_clerk_id = $2
		LEFT JOIN boards b ON b.id = t.board_id
		LEFT JOIN columns c ON c.id = t.column_id
		LEFT JOIN sprints sp ON sp.id = t.sprint_id
		WHERE t.id = $1`, id, owner).Scan(dest...)
	if err != nil {
		return nil, notFound(err)
	}
	if d.BlockedBy, err = s.deps(ctx, id, true); err != nil {
		return nil, err
	}
	if d.Blocks, err = s.deps(ctx, id, false); err != nil {
		return nil, err
	}
	if parentID != nil {
		// a parent in the bin isn't shown: the child reads as top-level, as when deletes were final (TL-256)
		var p Dep
		if err := s.DB.QueryRow(ctx, `SELECT t.id, t.title, `+boards.TicketDoneSQL+` FROM tickets t WHERE t.id = $1`, *parentID).Scan(&p.ID, &p.Title, &p.Done); err == nil {
			d.Parent = &p
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
	}
	if d.Epic, err = s.epicProgress(ctx, id); err != nil {
		return nil, err
	}
	if d.Children, err = s.ChildrenFor(ctx, id); err != nil {
		return nil, err
	}
	if d.Duplicates, err = s.DuplicatesFor(ctx, id); err != nil {
		return nil, err
	}
	return &d, nil
}

func (s Store) DuplicatesFor(ctx context.Context, id string) ([]DuplicateRef, error) {
	rows, err := s.DB.Query(ctx, `SELECT t.id, p.key_prefix || '-' || t.number, t.title FROM tickets t
		JOIN projects p ON p.id = t.project_id WHERE t.duplicate_of = $1 ORDER BY t.created_at, t.number`, id)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (DuplicateRef, error) {
		var d DuplicateRef
		return d, row.Scan(&d.ID, &d.Key, &d.Title)
	})
	if out == nil {
		out = []DuplicateRef{}
	}
	return out, err
}

// ChildrenFor returns the ticket's immediate children with done status.
func (s Store) ChildrenFor(ctx context.Context, id string) ([]Child, error) {
	rows, err := s.DB.Query(ctx, `SELECT t.id, pr.key_prefix || '-' || t.number, t.title, t.type::text, t.assigned_to, t.estimate, t.board_id::text, b.name, `+boards.TicketDoneSQL+`
		FROM tickets t JOIN projects pr ON pr.id = t.project_id LEFT JOIN boards b ON b.id = t.board_id
		WHERE t.parent_id = $1 ORDER BY t.created_at`, id)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Child, error) {
		var c Child
		err := row.Scan(&c.ID, &c.Key, &c.Title, &c.Type, &c.Assign, &c.Estimate, &c.BoardID, &c.BoardName, &c.Done)
		return c, err
	})
	if out == nil {
		out = []Child{}
	}
	return out, err
}

// keyPattern is a ticket key, e.g. TL-123 (case-insensitive).
var keyPattern = regexp.MustCompile(`^(?i)([a-z][a-z0-9]{1,5})-([0-9]+)$`)

// IsKey reports whether s looks like a ticket key rather than an id.
func IsKey(s string) bool { return keyPattern.MatchString(s) }

// IDByKey resolves a ticket key (TL-123) in the owner's org to the ticket's id.
func (s Store) IDByKey(ctx context.Context, owner, key string) (string, error) {
	m := keyPattern.FindStringSubmatch(strings.TrimSpace(key))
	if m == nil {
		return "", ErrNotFound
	}
	var id string
	err := s.DB.QueryRow(ctx, `SELECT t.id FROM tickets t JOIN projects p ON p.id = t.project_id
		WHERE p.owner_clerk_id = $1 AND p.key_prefix = upper($2) AND t.number = $3::int`, owner, m[1], m[2]).Scan(&id)
	return id, notFound(err)
}

// External is where a ticket made by another tool came from, e.g. a Bugfixes report.
type External struct {
	Source  string `json:"source"`  // bugfixes
	Project string `json:"project"` // the source's project id
	ID      string `json:"id"`      // e.g. the report id
	URL     string `json:"url"`
}

var ErrBadExternal = errors.New("external needs source (bugfixes), project and id; url must be http(s)")

func (e External) valid() bool {
	ok := func(s string, max int) bool { return s != "" && len(s) <= max && !strings.ContainsAny(s, "/?# ") }
	return e.Source == "bugfixes" && ok(e.Project, 64) && ok(e.ID, 64) &&
		(e.URL == "" || (len(e.URL) <= 500 && (strings.HasPrefix(e.URL, "https://") || strings.HasPrefix(e.URL, "http://"))))
}

// SetExternal records where ticket id came from (nil clears it).
func (s Store) SetExternal(ctx context.Context, owner, id string, e *External) error {
	if e != nil && !e.valid() {
		return ErrBadExternal
	}
	var src, project, ext, url *string
	if e != nil {
		src, project, ext, url = &e.Source, &e.Project, &e.ID, &e.URL
		if e.URL == "" {
			url = nil
		}
	}
	return exec(s.DB.Exec(ctx, `UPDATE tickets t SET external_source = $3, external_project = $4, external_id = $5, external_url = $6, updated_at = now()
		FROM projects p WHERE t.id = $1 AND p.id = t.project_id AND p.owner_clerk_id = $2`, id, owner, src, project, ext, url))
}

// ExternalOf returns where ticket id came from, its done time (if done) and nil when it's our own.
func (s Store) ExternalOf(ctx context.Context, owner, id string) (*External, *time.Time, error) {
	var src, project, ext, url *string
	var done *time.Time
	err := s.DB.QueryRow(ctx, `SELECT t.external_source, t.external_project, t.external_id, t.external_url,
			CASE WHEN `+boards.TicketDoneSQL+` THEN COALESCE(t.done_at, t.resolved_at, t.updated_at) END
		FROM tickets t JOIN projects p ON p.id = t.project_id AND p.owner_clerk_id = $2 WHERE t.id = $1`, id, owner).
		Scan(&src, &project, &ext, &url, &done)
	if err != nil {
		return nil, nil, notFound(err)
	}
	if src == nil { // our own ticket: nothing to link to
		return nil, nil, ErrNotFound
	}
	e := &External{Source: *src, Project: deref(project), ID: deref(ext), URL: deref(url)}
	return e, done, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// Retention is how long a deleted ticket stays in the bin before it's purged for good (TL-256).
const Retention = 30 * 24 * time.Hour

// PurgeBin permanently deletes tickets binned more than Retention before now. Their comments,
// labels, links and attachments go with them (cascade); their files were queued when binned.
func PurgeBin(ctx context.Context, db *pgxpool.Pool, now time.Time) (int64, error) {
	tag, err := db.Exec(ctx, `DELETE FROM tickets_all WHERE deleted_at < $1`, now.Add(-Retention))
	return tag.RowsAffected(), err
}
