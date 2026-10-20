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
	// ErrDoneMoved: the last column is Done (what makes a ticket done), so it stays last (TL-469)
	ErrDoneMoved = errors.New("the last column is Done and stays last: move the other columns around it")
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
		// a new column goes before the last one, Done: a ticket is done when it's in the board's
		// last column (boards.DoneSQL), so a column after Done would un-finish everything in it (TL-469)
		var position int
		var last *int
		if err := tx.QueryRow(ctx, `SELECT max(position) FROM columns WHERE board_id = $1`, boardID).Scan(&last); err != nil {
			return err
		}
		if last != nil {
			position = *last
			if _, err := tx.Exec(ctx, `UPDATE columns SET position = position + 1 WHERE board_id = $1 AND position = $2`, boardID, *last); err != nil {
				return err
			}
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

// SetWIPLimit sets the column's advisory work-in-progress limit; 0 removes it.
func (s Store) SetWIPLimit(ctx context.Context, owner, id string, limit int) error {
	tag, err := s.DB.Exec(ctx, `UPDATE columns c SET wip_limit = NULLIF($3, 0) FROM boards b
		WHERE c.id = $1 AND b.id = c.board_id AND b.owner_clerk_id = $2`, id, owner, limit)
	return affected(tag, err)
}

// Delete refuses to discard tickets that are still visible (open-sprint or no-sprint),
// then compacts the remaining positions. Tickets in a closed sprint's Done column are
// already hidden by the board view, so they don't block deletion.
func (s Store) Delete(ctx context.Context, owner, id string) error {
	return pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		var boardID string
		var openSprint *string // nil: the board has no open sprint (kanban, or before the first one)
		if err := tx.QueryRow(ctx, `SELECT c.board_id,
				(SELECT id FROM sprints WHERE board_id = b.id AND closed_at IS NULL)
			FROM columns c JOIN boards b ON b.id = c.board_id
			WHERE c.id = $1 AND b.owner_clerk_id = $2 FOR UPDATE OF b, c`, id, owner).Scan(&boardID, &openSprint); err != nil {
			return notFound(err)
		}
		var count int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM tickets
			WHERE column_id = $1 AND sprint_id IS NOT DISTINCT FROM $2`, id, openSprint).Scan(&count); err != nil {
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
		if len(ids) > 0 && ids[len(ids)-1] != existing[len(existing)-1] {
			return ErrDoneMoved
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
