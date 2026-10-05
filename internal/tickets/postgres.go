package tickets

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/boards"
)

var ErrNotFound = errors.New("ticket not found")

type Ticket = boards.Ticket

type Store struct{ DB *pgxpool.Pool }

// notFound maps "no rows" and malformed uuids (22P02) to ErrNotFound.
func notFound(err error) error {
	var pg *pgconn.PgError
	if errors.Is(err, pgx.ErrNoRows) || (errors.As(err, &pg) && pg.Code == "22P02") {
		return ErrNotFound
	}
	return err
}

// Create appends a ticket to the bottom of columnID; the column must belong to a board owner holds.
func (s Store) Create(ctx context.Context, owner, columnID, title, desc string) (*Ticket, error) {
	var t Ticket
	err := s.DB.QueryRow(ctx, `INSERT INTO tickets (board_id, column_id, title, description, position)
		SELECT b.id, c.id, $3, $4, COALESCE((SELECT max(position) + 1 FROM tickets WHERE column_id = c.id), 0)
		FROM columns c JOIN boards b ON b.id = c.board_id
		WHERE c.id = $1 AND b.owner_clerk_id = $2
		RETURNING id, column_id, title, description, position`, columnID, owner, title, desc).
		Scan(&t.ID, &t.ColumnID, &t.Title, &t.Description, &t.Position)
	if err != nil {
		return nil, notFound(err)
	}
	return &t, nil
}

func (s Store) Update(ctx context.Context, owner, id, title, desc string) error {
	return exec(s.DB.Exec(ctx, `UPDATE tickets t SET title = $3, description = $4, updated_at = now()
		FROM boards b WHERE t.id = $1 AND b.id = t.board_id AND b.owner_clerk_id = $2`, id, owner, title, desc))
}

func (s Store) Delete(ctx context.Context, owner, id string) error {
	return exec(s.DB.Exec(ctx, `DELETE FROM tickets t USING boards b
		WHERE t.id = $1 AND b.id = t.board_id AND b.owner_clerk_id = $2`, id, owner))
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

// Move puts ticket id at position pos (clamped) in toColumn, which must be on the same
// board. Both columns are renumbered 0..n-1. Same column = reorder.
func (s Store) Move(ctx context.Context, owner, id, toColumn string, pos int) error {
	return pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		var boardID, fromColumn string
		// lock the board row: moves on one board are serialised
		err := tx.QueryRow(ctx, `SELECT b.id, t.column_id FROM tickets t JOIN boards b ON b.id = t.board_id
			JOIN columns c ON c.id = $3 AND c.board_id = b.id
			WHERE t.id = $1 AND b.owner_clerk_id = $2 FOR UPDATE OF b`, id, owner, toColumn).Scan(&boardID, &fromColumn)
		if err != nil {
			return notFound(err)
		}
		ids, err := columnOrder(ctx, tx, toColumn, id)
		if err != nil {
			return err
		}
		pos = max(0, min(pos, len(ids)))
		ids = append(ids[:pos], append([]string{id}, ids[pos:]...)...)
		if _, err := tx.Exec(ctx, `UPDATE tickets SET column_id = $2, updated_at = now() WHERE id = $1`, id, toColumn); err != nil {
			return err
		}
		if err := renumber(ctx, tx, ids); err != nil {
			return err
		}
		if fromColumn == toColumn {
			return nil
		}
		rest, err := columnOrder(ctx, tx, fromColumn, id)
		if err != nil {
			return err
		}
		return renumber(ctx, tx, rest)
	})
}

// columnOrder lists ticket ids in a column by position, excluding skip.
func columnOrder(ctx context.Context, tx pgx.Tx, columnID, skip string) ([]string, error) {
	rows, err := tx.Query(ctx, `SELECT id FROM tickets WHERE column_id = $1 AND id <> $2 ORDER BY position, created_at`, columnID, skip)
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
