package columns

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNotFound     = errors.New("column not found")
	ErrNotEmpty     = errors.New("column contains tickets")
	ErrInvalidOrder = errors.New("column order must contain every board column exactly once")
)

type Column struct {
	ID       string `json:"id"`
	BoardID  string `json:"board_id"`
	Name     string `json:"name"`
	Position int    `json:"position"`
}

type Store struct{ DB *pgxpool.Pool }

func notFound(err error) error {
	var pg *pgconn.PgError
	if errors.Is(err, pgx.ErrNoRows) || (errors.As(err, &pg) && pg.Code == "22P02") {
		return ErrNotFound
	}
	return err
}

// Create appends a column to an owned board under a board row lock.
func (s Store) Create(ctx context.Context, owner, boardID, name string) (*Column, error) {
	var column Column
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT id FROM boards WHERE id = $1 AND owner_clerk_id = $2 FOR UPDATE`, boardID, owner).Scan(&column.BoardID); err != nil {
			return notFound(err)
		}
		var position int
		if err := tx.QueryRow(ctx, `SELECT COALESCE(max(position) + 1, 0) FROM columns WHERE board_id = $1`, boardID).Scan(&position); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `INSERT INTO columns (board_id, name, position) VALUES ($1, $2, $3)
			RETURNING id, name, position`, boardID, name, position).Scan(&column.ID, &column.Name, &column.Position)
	})
	if err != nil {
		return nil, err
	}
	return &column, nil
}

func (s Store) Rename(ctx context.Context, owner, id, name string) error {
	tag, err := s.DB.Exec(ctx, `UPDATE columns c SET name = $3 FROM boards b
		WHERE c.id = $1 AND b.id = c.board_id AND b.owner_clerk_id = $2`, id, owner, name)
	return affected(tag, err)
}

// Delete refuses to discard tickets that are still visible (open-sprint or no-sprint),
// then compacts the remaining positions. Tickets in a closed sprint's Done column are
// already hidden by the board view, so they don't block deletion.
func (s Store) Delete(ctx context.Context, owner, id string) error {
	return pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		var boardID, openSprintID string
		var openSprint bool
		if err := tx.QueryRow(ctx, `SELECT c.board_id,
				(SELECT id FROM sprints WHERE board_id = b.id AND closed_at IS NULL)
			FROM columns c JOIN boards b ON b.id = c.board_id
			WHERE c.id = $1 AND b.owner_clerk_id = $2 FOR UPDATE OF b, c`, id, owner).Scan(&boardID, &openSprintID); err != nil {
			return notFound(err)
		}
		openSprint = openSprintID != ""
		var count int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM tickets
			WHERE column_id = $1 AND sprint_id IS NOT DISTINCT FROM $2`, id, pickString(openSprint, &openSprintID, nil)).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return ErrNotEmpty
		}
		tag, err := tx.Exec(ctx, `DELETE FROM columns WHERE id = $1`, id)
		if err := affected(tag, err); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE columns SET position = position + 1000000 WHERE board_id = $1`, boardID)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `WITH ordered AS (
				SELECT id, row_number() OVER (ORDER BY position, id) - 1 AS position
				FROM columns WHERE board_id = $1
			) UPDATE columns c SET position = ordered.position FROM ordered WHERE c.id = ordered.id`, boardID)
		return err
	})
}

// pickString returns a when useA, otherwise b.
func pickString(useA bool, a, b *string) *string {
	if useA {
		return a
	}
	return b
}

// Reorder requires an exact permutation so columns cannot be silently omitted.
func (s Store) Reorder(ctx context.Context, owner, boardID string, ids []string) error {
	return pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT id FROM boards WHERE id = $1 AND owner_clerk_id = $2 FOR UPDATE`, boardID, owner).Scan(new(string)); err != nil {
			return notFound(err)
		}
		rows, err := tx.Query(ctx, `SELECT id FROM columns WHERE board_id = $1 ORDER BY position`, boardID)
		if err != nil {
			return err
		}
		existing, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		if len(ids) != len(existing) {
			return ErrInvalidOrder
		}
		allowed := make(map[string]bool, len(existing))
		for _, id := range existing {
			allowed[id] = true
		}
		for _, id := range ids {
			if !allowed[id] {
				return ErrInvalidOrder
			}
			delete(allowed, id)
		}
		if len(allowed) != 0 {
			return ErrInvalidOrder
		}
		if _, err := tx.Exec(ctx, `UPDATE columns SET position = position + 1000000 WHERE board_id = $1`, boardID); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE columns c SET position = ranked.position - 1
			FROM unnest($2::uuid[]) WITH ORDINALITY AS ranked(id, position)
			WHERE c.id = ranked.id AND c.board_id = $1`, boardID, ids)
		return err
	})
}

func affected(tag pgconn.CommandTag, err error) error {
	if err != nil {
		return notFound(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
