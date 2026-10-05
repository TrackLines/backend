package boards

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNotFound = errors.New("board not found")
	ErrLimit    = errors.New("board limit reached for plan")
)

// DefaultColumns are created with every board so it's usable immediately.
var DefaultColumns = []string{"To do", "In progress", "Done"}

type Ticket struct {
	ID          string `json:"id"`
	ColumnID    string `json:"column_id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Position    int    `json:"position"`
}

type Column struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Position int      `json:"position"`
	Tickets  []Ticket `json:"tickets"`
}

type Board struct {
	ID           string   `json:"id"`
	OwnerClerkID string   `json:"owner_clerk_id"`
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	CreatedAt    string   `json:"created_at"`
	UpdatedAt    string   `json:"updated_at"`
	Columns      []Column `json:"columns,omitempty"`
}

type Store struct{ DB *pgxpool.Pool }

const cols = `id, owner_clerk_id, name, description,
	to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
	to_char(updated_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')`

func scan(row pgx.Row) (Board, error) {
	var b Board
	err := row.Scan(&b.ID, &b.OwnerClerkID, &b.Name, &b.Description, &b.CreatedAt, &b.UpdatedAt)
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

// Create inserts a board plus DefaultColumns. limit < 0 means unlimited; otherwise
// ErrLimit when the owner already has limit boards (checked under a per-owner lock, so
// concurrent creates can't both slip past the free tier).
func (s Store) Create(ctx context.Context, owner, name, desc string, limit int) (*Board, error) {
	var b Board
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		if limit >= 0 {
			var n int
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('boards:' || $1))`, owner); err != nil {
				return err
			}
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM boards WHERE owner_clerk_id = $1`, owner).Scan(&n); err != nil {
				return err
			}
			if n >= limit {
				return ErrLimit
			}
		}
		var err error
		if b, err = scan(tx.QueryRow(ctx, `INSERT INTO boards (owner_clerk_id, name, description)
			VALUES ($1, $2, $3) RETURNING `+cols, owner, name, desc)); err != nil {
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

func (s Store) ListByOwner(ctx context.Context, owner string) ([]Board, error) {
	rows, err := s.DB.Query(ctx, `SELECT `+cols+` FROM boards WHERE owner_clerk_id = $1 ORDER BY updated_at DESC`, owner)
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
	rows, err = s.DB.Query(ctx, `SELECT id, column_id, title, description, position
		FROM tickets WHERE board_id = $1 ORDER BY position`, id)
	if err != nil {
		return nil, err
	}
	tickets, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Ticket, error) {
		var t Ticket
		return t, row.Scan(&t.ID, &t.ColumnID, &t.Title, &t.Description, &t.Position)
	})
	if err != nil {
		return nil, err
	}
	for _, t := range tickets {
		c := &b.Columns[idx[t.ColumnID]]
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
