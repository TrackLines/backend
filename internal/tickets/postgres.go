package tickets

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/boards"
)

var (
	ErrNotFound            = errors.New("ticket not found")
	ErrInvalidType         = errors.New("type must be bug, feature or task")
	ErrAlreadyAssigned     = errors.New("ticket is already assigned")
	ErrNotAssignedToCaller = errors.New("ticket is not assigned to caller")
	ErrSelfParent          = errors.New("a ticket can't be its own parent")
	ErrParentLoop          = errors.New("that parent is already a sub-ticket of this ticket")
	ErrOtherProject        = errors.New("tickets must belong to the same project")
)

type Ticket = boards.Ticket

type Store struct{ DB *pgxpool.Pool }

// ValidType reports whether t is a ticket type; "" means "unchanged" on update.
func ValidType(t string) bool { return t == "bug" || t == "feature" || t == "task" }

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
// It joins the board's open sprint, if any.
func (s Store) CreateAs(ctx context.Context, owner, actor, columnID, typ, title, desc string) (*Ticket, error) {
	if !ValidType(typ) {
		return nil, ErrInvalidType
	}
	t, err := boards.ScanTicket(s.DB.QueryRow(ctx, `WITH placed AS (
			SELECT b.project_id, b.id AS board_id, c.id AS column_id,
				(SELECT id FROM sprints WHERE board_id = b.id AND closed_at IS NULL) AS sprint_id
			FROM columns c JOIN boards b ON b.id = c.board_id JOIN projects p ON p.id = b.project_id
			WHERE c.id = $1 AND p.owner_clerk_id = $2)
		INSERT INTO tickets AS t (project_id, board_id, column_id, sprint_id, type, title, description, priority, position, created_by)
		SELECT project_id, board_id, column_id, sprint_id, $3::ticket_type, $4, $5, 'medium'::priority, COALESCE((SELECT max(position) + 1 FROM tickets x WHERE x.column_id = placed.column_id
				AND x.sprint_id IS NOT DISTINCT FROM placed.sprint_id), 0), $6
		FROM placed RETURNING `+boards.TicketCols, columnID, owner, typ, title, desc, actor))
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
		WHERE t.project_id = $1 AND t.board_id IS NULL AND ($2 = '' OR t.type::text = $2)
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
	Counts  map[string]int `json:"counts"` // all, bug, feature, task: label filter applied, type filter not
	Labels  []LabelCount   `json:"labels"` // labels in the whole backlog (no filters), for the label filter
}

const MaxPerPage = 100

// PageBacklog returns page (1-based) of the backlog filtered by typ ("" = all) and labels (any match,
// case-insensitive), ready tickets before blocked ones. Backlog stays the unpaged list.
func (s Store) PageBacklog(ctx context.Context, owner, projectID, typ string, labels []string, page, perPage int) (*BacklogPage, error) {
	if typ != "" && !ValidType(typ) {
		return nil, ErrInvalidType
	}
	var ok bool
	if err := s.DB.QueryRow(ctx, `SELECT true FROM projects WHERE id = $1 AND owner_clerk_id = $2`, projectID, owner).Scan(&ok); err != nil {
		return nil, notFound(err)
	}
	lower := make([]string, len(labels))
	for i, l := range labels {
		lower[i] = strings.ToLower(l)
	}
	const where = `t.project_id = $1 AND t.board_id IS NULL AND (cardinality($2::text[]) = 0 OR EXISTS (
		SELECT 1 FROM ticket_labels l WHERE l.ticket_id = t.id AND lower(l.label) = ANY($2)))`

	out := &BacklogPage{Counts: map[string]int{"all": 0, "bug": 0, "feature": 0, "task": 0}}
	rows, err := s.DB.Query(ctx, `SELECT t.type::text, count(*) FROM tickets t WHERE `+where+` GROUP BY 1`, projectID, lower)
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
		WHERE t.project_id = $1 AND t.board_id IS NULL GROUP BY lower(l.label) ORDER BY count(*) DESC, lower(min(l.label))`, projectID)
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

	rows, err = s.DB.Query(ctx, `SELECT `+boards.TicketCols+` FROM tickets t
		WHERE `+where+` AND ($3 = '' OR t.type::text = $3)
		ORDER BY `+boards.TicketBlockedSQL+`, t.position, t.created_at LIMIT $4 OFFSET $5`,
		projectID, lower, typ, perPage, (page-1)*perPage)
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
	return exec(s.DB.Exec(ctx, `UPDATE tickets t SET title = $3, description = $4,
		type = COALESCE(NULLIF($5, '')::ticket_type, t.type), priority = COALESCE(NULLIF($6, '')::priority, t.priority), updated_at = now()
		FROM projects p WHERE t.id = $1 AND p.id = t.project_id AND p.owner_clerk_id = $2`, id, owner, title, desc, typ, priority))
}

// SetEstimate sets ("" clears) the ticket's estimate; it must be on its board's scale, so a
// backlog ticket (no board) can only be cleared.
func (s Store) SetEstimate(ctx context.Context, owner, id, estimate string) error {
	var scale string
	if err := s.DB.QueryRow(ctx, `SELECT COALESCE(b.estimate_scale, 'none') FROM tickets t
		JOIN projects p ON p.id = t.project_id AND p.owner_clerk_id = $2
		LEFT JOIN boards b ON b.id = t.board_id WHERE t.id = $1`, id, owner).Scan(&scale); err != nil {
		return notFound(err)
	}
	if !boards.ValidEstimate(scale, estimate) {
		return boards.ErrBadEstimate
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

func (s Store) Delete(ctx context.Context, owner, id string) error {
	return exec(s.DB.Exec(ctx, `DELETE FROM tickets t USING projects p
		WHERE t.id = $1 AND p.id = t.project_id AND p.owner_clerk_id = $2`, id, owner))
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
// backlog, and joins the target board's open sprint. Same column = reorder.
func (s Store) Move(ctx context.Context, owner, id, toColumn string, pos int) error {
	return pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		var boardID string
		var from, fromSprint, sprint *string
		// lock the target board: moves on one board are serialised
		err := tx.QueryRow(ctx, `SELECT b.id, t.column_id, t.sprint_id,
				(SELECT id FROM sprints WHERE board_id = b.id AND closed_at IS NULL)
			FROM tickets t JOIN projects p ON p.id = t.project_id AND p.owner_clerk_id = $2
			JOIN columns c ON c.id = $3 JOIN boards b ON b.id = c.board_id AND b.project_id = t.project_id
			WHERE t.id = $1 FOR UPDATE OF b`, id, owner, toColumn).Scan(&boardID, &from, &fromSprint, &sprint)
		if err != nil {
			return notFound(err)
		}
		ids, err := columnOrder(ctx, tx, toColumn, sprint, id)
		if err != nil {
			return err
		}
		pos = max(0, min(pos, len(ids)))
		ids = append(ids[:pos], append([]string{id}, ids[pos:]...)...)
		// an estimate only survives a move to a board on the same scale
		if _, err := tx.Exec(ctx, `UPDATE tickets t SET board_id = $2, column_id = $3, sprint_id = $4, updated_at = now(),
				estimate = CASE WHEN (SELECT estimate_scale FROM boards WHERE id = t.board_id) =
					(SELECT estimate_scale FROM boards WHERE id = $2) THEN t.estimate END
			WHERE t.id = $1`, id, boardID, toColumn, sprint); err != nil {
			return err
		}
		if err := renumber(ctx, tx, ids); err != nil {
			return err
		}
		if from != nil && *from == toColumn {
			return nil
		}
		return renumberSource(ctx, tx, id, from, fromSprint)
	})
}

// ToBacklog takes a ticket off its board/sprint and appends it to the project backlog. Its estimate
// goes too: the backlog has no scale.
func (s Store) ToBacklog(ctx context.Context, owner, id string) error {
	return pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		var from, fromSprint *string
		err := tx.QueryRow(ctx, `SELECT t.column_id, t.sprint_id FROM tickets t
			JOIN projects p ON p.id = t.project_id AND p.owner_clerk_id = $2
			WHERE t.id = $1 FOR UPDATE OF t`, id, owner).Scan(&from, &fromSprint)
		if err != nil {
			return notFound(err)
		}
		if from == nil {
			return nil // already in the backlog
		}
		if _, err := tx.Exec(ctx, `UPDATE tickets t SET board_id = NULL, column_id = NULL, sprint_id = NULL, estimate = NULL, updated_at = now(),
				position = COALESCE((SELECT max(position) + 1 FROM tickets x WHERE x.project_id = t.project_id AND x.board_id IS NULL), 0)
			WHERE t.id = $1`, id); err != nil {
			return err
		}
		return renumberSource(ctx, tx, id, from, fromSprint)
	})
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
	ProjectName   string  `json:"project_name"`
	BoardID       *string `json:"board_id"`
	BoardName     *string `json:"board_name"`
	EstimateScale string  `json:"estimate_scale"` // the board's; "none" in the backlog
	ColumnName    *string `json:"column_name"`
	SprintNumber  *int    `json:"sprint_number"`
	Done          bool    `json:"done"` // in the board's last column: locked for attachments
	BlockedBy     []Dep   `json:"blocked_by"`
	Blocks        []Dep   `json:"blocks"`
	Parent        *Dep    `json:"parent"`   // the ticket this one is a sub-ticket of, if any
	Children      []Child `json:"children"` // immediate sub-tickets, with done status
}

type Child struct {
	ID     string  `json:"id"`
	Title  string  `json:"title"`
	Done   bool    `json:"done"`
	Type   string  `json:"type"`
	Assign *string `json:"assigned_to"`
}

// SetParent clears (parentID nil) or sets the parent of ticket id. Both must be in the same
// project, and the parent can't be the ticket itself or one of its descendants.
func (s Store) SetParent(ctx context.Context, owner, id string, parentID *string) error {
	if parentID != nil && *parentID == id {
		return ErrSelfParent
	}
	return pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		var project string
		if err := tx.QueryRow(ctx, `SELECT t.project_id FROM tickets t JOIN projects p ON p.id = t.project_id
			WHERE t.id = $1 AND p.owner_clerk_id = $2`, id, owner).Scan(&project); err != nil {
			return notFound(err)
		}
		// one parent change at a time per project, so two concurrent edits can't form a loop
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('ticket-parent:' || $1))`, project); err != nil {
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
		}
		_, err := tx.Exec(ctx, `UPDATE tickets SET parent_id = $2, updated_at = now() WHERE id = $1`, id, parentID)
		return err
	})
}

// Get returns the owner's ticket with its project/board/column/sprint context.
func (s Store) Get(ctx context.Context, owner, id string) (*Detail, error) {
	var d Detail
	var parentID *string
	dest := append(boards.TicketDest(&d.Ticket), &d.ProjectName, &d.BoardID, &d.BoardName, &d.EstimateScale, &d.ColumnName, &d.SprintNumber, &d.Done, &parentID)
	err := s.DB.QueryRow(ctx, `SELECT `+boards.TicketCols+`, p.name, t.board_id, b.name, COALESCE(b.estimate_scale, 'none'), c.name, sp.number, `+boards.TicketDoneSQL+`, t.parent_id
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
		var p Dep
		if err := s.DB.QueryRow(ctx, `SELECT t.id, t.title, `+boards.TicketDoneSQL+` FROM tickets t WHERE t.id = $1`, *parentID).Scan(&p.ID, &p.Title, &p.Done); err != nil {
			return nil, err
		}
		d.Parent = &p
	}
	if d.Children, err = s.ChildrenFor(ctx, id); err != nil {
		return nil, err
	}
	return &d, nil
}

// ChildrenFor returns the ticket's immediate children with done status.
func (s Store) ChildrenFor(ctx context.Context, id string) ([]Child, error) {
	rows, err := s.DB.Query(ctx, `SELECT t.id, t.title, t.type::text, t.assigned_to, `+boards.TicketDoneSQL+`
		FROM tickets t WHERE t.parent_id = $1 ORDER BY t.created_at`, id)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Child, error) {
		var c Child
		err := row.Scan(&c.ID, &c.Title, &c.Type, &c.Assign, &c.Done)
		return c, err
	})
	if out == nil {
		out = []Child{}
	}
	return out, err
}
