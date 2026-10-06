package boards

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("board not found")

// DefaultColumns are created with every board so it's usable immediately.
var DefaultColumns = []string{"To do", "In progress", "Done"}

type Ticket struct {
	ID          string  `json:"id"`
	CreatedBy   string  `json:"created_by"`
	AssignedTo  *string `json:"assigned_to"`
	ProjectID   string  `json:"project_id"`
	ColumnID    *string `json:"column_id"`
	SprintID    *string `json:"sprint_id"`
	Type        string  `json:"type"`
	Title       string  `json:"title"`
	Description string  `json:"description"`
	Priority    string  `json:"priority"`
	Position    int     `json:"position"`
	Blocked     bool    `json:"blocked"` // a ticket it depends on isn't done yet
}

// TicketCols + ScanTicket read a ticket row; shared with the tickets package. The last column
// is "blocked": some ticket it depends on isn't done yet.
var TicketCols = `t.id, t.created_by, t.assigned_to, t.project_id, t.column_id, t.sprint_id, t.type::text, t.title, t.description, t.priority::text, t.position, ` + TicketBlockedSQL

func ScanTicket(row pgx.Row) (Ticket, error) {
	var t Ticket
	err := row.Scan(TicketDest(&t)...)
	return t, err
}

// TicketDoneSQL is true when ticket t sits in its board's last column ("done") — the same rule
// sprint carry-over uses. Done tickets are locked against new attachments.
var TicketDoneSQL = DoneSQL("t")

// DoneSQL is TicketDoneSQL for a ticket table alias other than t.
func DoneSQL(a string) string {
	return `(` + a + `.column_id IS NOT NULL AND ` + a + `.column_id = (
	SELECT dc.id FROM columns dc WHERE dc.board_id = ` + a + `.board_id ORDER BY dc.position DESC LIMIT 1))`
}

// TicketBlockedSQL is true while any ticket that t depends on (ticket_dependencies) isn't done.
var TicketBlockedSQL = `EXISTS (SELECT 1 FROM ticket_dependencies td JOIN tickets bt ON bt.id = td.blocked_by_id
	WHERE td.ticket_id = t.id AND NOT ` + DoneSQL("bt") + `)`

// TicketDest lists scan targets matching TicketCols, for queries that select extra columns after them.
func TicketDest(t *Ticket) []any {
	return []any{&t.ID, &t.CreatedBy, &t.AssignedTo, &t.ProjectID, &t.ColumnID, &t.SprintID, &t.Type, &t.Title, &t.Description, &t.Priority, &t.Position, &t.Blocked}
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
}

const SprintCols = `id, board_id, number, length_days,
	to_char(starts_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
	to_char(ends_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
	to_char(closed_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')`

func ScanSprint(row pgx.Row) (Sprint, error) {
	var s Sprint
	err := row.Scan(&s.ID, &s.BoardID, &s.Number, &s.LengthDays, &s.StartsAt, &s.EndsAt, &s.ClosedAt)
	return s, err
}

type Column struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Position int      `json:"position"`
	Tickets  []Ticket `json:"tickets"`
}

type Board struct {
	ID           string   `json:"id"`
	ProjectID    string   `json:"project_id"`
	OwnerClerkID string   `json:"owner_clerk_id"`
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	CreatedAt    string   `json:"created_at"`
	UpdatedAt    string   `json:"updated_at"`
	Sprint       *Sprint  `json:"sprint,omitempty"` // open sprint, if the board runs sprints
	Columns      []Column `json:"columns,omitempty"`
}

type Store struct{ DB *pgxpool.Pool }

const cols = `id, project_id, owner_clerk_id, name, description,
	to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
	to_char(updated_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')`

func scan(row pgx.Row) (Board, error) {
	var b Board
	err := row.Scan(&b.ID, &b.ProjectID, &b.OwnerClerkID, &b.Name, &b.Description, &b.CreatedAt, &b.UpdatedAt)
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

// Create inserts a board (one per team) into the owner's project, plus DefaultColumns.
// ErrNotFound when the project isn't the owner's.
func (s Store) Create(ctx context.Context, owner, projectID, name, desc string) (*Board, error) {
	var b Board
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		var err error
		if b, err = scan(tx.QueryRow(ctx, `INSERT INTO boards (project_id, owner_clerk_id, name, description)
			SELECT id, owner_clerk_id, $3, $4 FROM projects WHERE id = $1 AND owner_clerk_id = $2
			RETURNING `+cols, projectID, owner, name, desc)); err != nil {
			return err
		}
		for pos, c := range DefaultColumns {
			var col Column
			if err := tx.QueryRow(ctx, `INSERT INTO columns (board_id, name, position) VALUES ($1, $2, $3)
				RETURNING id, name, position`, b.ID, c, pos).Scan(&col.ID, &col.Name, &col.Position); err != nil {
				return err
			}
			col.Tickets = []Ticket{}
			b.Columns = append(b.Columns, col)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &b, nil
}

// ListByProject lists a project's boards (without columns); callers check ownership.
func (s Store) ListByProject(ctx context.Context, projectID string) ([]Board, error) {
	rows, err := s.DB.Query(ctx, `SELECT `+cols+` FROM boards WHERE project_id = $1 ORDER BY created_at`, projectID)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Board, error) { return scan(row) })
	if out == nil {
		out = []Board{}
	}
	return out, err
}

// Get returns the board with ordered columns and tickets if owner holds it.
func (s Store) Get(ctx context.Context, id, owner string) (*Board, error) {
	b, err := scan(s.DB.QueryRow(ctx, `SELECT `+cols+` FROM boards WHERE id = $1 AND owner_clerk_id = $2`, id, owner))
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(ctx, `SELECT id, name, position FROM columns WHERE board_id = $1 ORDER BY position`, id)
	if err != nil {
		return nil, err
	}
	b.Columns, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (Column, error) {
		c := Column{Tickets: []Ticket{}}
		return c, row.Scan(&c.ID, &c.Name, &c.Position)
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
	rows, err = s.DB.Query(ctx, `SELECT `+TicketCols+` FROM tickets t
		WHERE t.board_id = $1 AND t.sprint_id IS NOT DISTINCT FROM $2 ORDER BY t.position, t.created_at`, id, sprintID)
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

func (s Store) Delete(ctx context.Context, id, owner string) error {
	tag, err := s.DB.Exec(ctx, `DELETE FROM boards WHERE id = $1 AND owner_clerk_id = $2`, id, owner)
	if err != nil {
		return notFound(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
