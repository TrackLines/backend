package sprints

import (
	"context"
	"errors"
	"time"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/boards"
)

var (
	ErrNotFound      = errors.New("sprint not found")
	ErrAlreadyOpen   = errors.New("board already has an open sprint")
	ErrInvalidLength = errors.New("length_days must be between 1 and 365")
	ErrKanban        = errors.New("kanban boards don't run sprints")
)

type Sprint = boards.Sprint

type Store struct{ DB *pgxpool.Pool }

func notFound(err error) error {
	var pg *pgconn.PgError
	if errors.Is(err, pgx.ErrNoRows) || (errors.As(err, &pg) && pg.Code == "22P02") {
		return ErrNotFound
	}
	return err
}

// Start opens sprint 1 (or the next number) on the owner's board and pulls in the
// board's tickets that aren't in any sprint yet.
func (s Store) Start(ctx context.Context, owner, boardID string, lengthDays int) (*Sprint, error) {
	if lengthDays < 1 || lengthDays > 365 {
		return nil, ErrInvalidLength
	}
	var sp Sprint
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		// the board lock pairs with boards.SetStyle: a board can't turn kanban while a sprint starts
		var style string
		if err := tx.QueryRow(ctx, `SELECT b.style FROM boards b JOIN projects p ON p.id = b.project_id
			WHERE b.id = $1 AND p.owner_clerk_id = $2 FOR UPDATE OF b`, boardID, owner).Scan(&style); err != nil {
			return notFound(err)
		}
		if style == "kanban" {
			return ErrKanban
		}
		var err error
		sp, err = boards.ScanSprint(tx.QueryRow(ctx, `INSERT INTO sprints (board_id, number, length_days, ends_at)
			SELECT b.id, COALESCE((SELECT max(number) FROM sprints WHERE board_id = b.id), 0) + 1, $3,
				now() + make_interval(days => $3)
			FROM boards b JOIN projects p ON p.id = b.project_id
			WHERE b.id = $1 AND p.owner_clerk_id = $2
			RETURNING `+boards.SprintCols, boardID, owner, lengthDays))
		var pg *pgconn.PgError
		if errors.As(err, &pg) && pg.Code == "23505" { // sprints_one_open_per_board
			return ErrAlreadyOpen
		}
		if err != nil {
			return notFound(err)
		}
		_, err = tx.Exec(ctx, `UPDATE tickets SET sprint_id = $1 WHERE board_id = $2 AND sprint_id IS NULL`, sp.ID, boardID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &sp, nil
}

// List returns the board's sprints, newest first.
func (s Store) List(ctx context.Context, owner, boardID string) ([]Sprint, error) {
	var ok bool
	if err := s.DB.QueryRow(ctx, `SELECT true FROM boards b JOIN projects p ON p.id = b.project_id
		WHERE b.id = $1 AND p.owner_clerk_id = $2`, boardID, owner).Scan(&ok); err != nil {
		return nil, notFound(err)
	}
	rows, err := s.DB.Query(ctx, `SELECT `+boards.SprintCols+` FROM sprints WHERE board_id = $1 ORDER BY number DESC`, boardID)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Sprint, error) { return boards.ScanSprint(row) })
	if out == nil {
		out = []Sprint{}
	}
	return out, err
}

// Close closes the owner's open sprint and opens the next one (see closeSprint).
func (s Store) Close(ctx context.Context, owner, sprintID string) (*Sprint, error) {
	var next Sprint
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		var id string
		err := tx.QueryRow(ctx, `SELECT s.id FROM sprints s JOIN boards b ON b.id = s.board_id
			JOIN projects p ON p.id = b.project_id
			WHERE s.id = $1 AND p.owner_clerk_id = $2 AND s.closed_at IS NULL FOR UPDATE OF s`, sprintID, owner).Scan(&id)
		if err != nil {
			return notFound(err)
		}
		next, err = closeSprint(ctx, tx, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &next, nil
}

// closeSprint (caller holds the sprint row lock) marks it closed, opens the next sprint
// with the same length starting now, and carries over every ticket that isn't in the
// board's last column ("done"), keeping its column and order.
func closeSprint(ctx context.Context, tx pgx.Tx, id string) (Sprint, error) {
	if _, err := tx.Exec(ctx, `UPDATE sprints SET closed_at = now() WHERE id = $1`, id); err != nil {
		return Sprint{}, err
	}
	next, err := boards.ScanSprint(tx.QueryRow(ctx, `INSERT INTO sprints (board_id, number, length_days, ends_at)
		SELECT board_id, number + 1, length_days, now() + make_interval(days => length_days)
		FROM sprints WHERE id = $1 RETURNING `+boards.SprintCols, id))
	if err != nil {
		return Sprint{}, err
	}
	_, err = tx.Exec(ctx, `UPDATE tickets SET sprint_id = $2 WHERE sprint_id = $1 AND column_id <> (
		SELECT c.id FROM columns c WHERE c.board_id = $3 ORDER BY c.position DESC LIMIT 1)`, id, next.ID, next.BoardID)
	return next, err
}

// AutoClose closes every open sprint past its end date. Rows are claimed with
// FOR UPDATE SKIP LOCKED, so several replicas can run it without double-closing.
func (s Store) AutoClose(ctx context.Context) (int, error) {
	closed := 0
	for {
		done := false
		err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
			var id string
			err := tx.QueryRow(ctx, `SELECT id FROM sprints WHERE closed_at IS NULL AND ends_at <= now()
				ORDER BY ends_at LIMIT 1 FOR UPDATE SKIP LOCKED`).Scan(&id)
			if errors.Is(err, pgx.ErrNoRows) {
				done = true
				return nil
			}
			if err != nil {
				return err
			}
			_, err = closeSprint(ctx, tx, id)
			return err
		})
		if err != nil || done {
			return closed, err
		}
		closed++
	}
}

// RunAutoClose checks for overdue sprints every interval until ctx is cancelled.
func (s Store) RunAutoClose(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if n, err := s.AutoClose(ctx); err != nil {
			logs.Errorf("sprints: auto-close: %v", err)
		} else if n > 0 {
			logs.Logf("sprints: auto-closed %d overdue sprint(s)", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
