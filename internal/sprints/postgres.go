package sprints

import (
	"context"
	"errors"
	"fmt"
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
	ErrInvalidStart  = errors.New("next_starts_at must be in the future")
	ErrKanban        = errors.New("kanban boards don't run sprints")
)

type Sprint = boards.Sprint

type CloseOptions struct {
	NextLengthDays int
	NextStartsAt   *time.Time
}

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

// UpdateLength changes the length of an unclosed sprint and keeps its start time fixed.
func (s Store) UpdateLength(ctx context.Context, owner, sprintID string, lengthDays int) (*Sprint, error) {
	if lengthDays < 1 || lengthDays > 365 {
		return nil, ErrInvalidLength
	}
	sp, err := boards.ScanSprint(s.DB.QueryRow(ctx, `UPDATE sprints sp SET length_days = $3,
		ends_at = sp.starts_at + make_interval(days => $3)
		WHERE sp.id = $1 AND sp.closed_at IS NULL AND EXISTS (
			SELECT 1 FROM boards b JOIN projects p ON p.id = b.project_id
			WHERE b.id = sp.board_id AND p.owner_clerk_id = $2)
		RETURNING `+boards.SprintCols, sprintID, owner, lengthDays))
	if err != nil {
		return nil, notFound(err)
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
	return s.CloseWithOptions(ctx, owner, sprintID, CloseOptions{})
}

// CloseWithOptions closes the current sprint and creates its next sprint, optionally scheduled
// for a future start date and with a different length. Omitted options preserve today's behavior.
func (s Store) CloseWithOptions(ctx context.Context, owner, sprintID string, options CloseOptions) (*Sprint, error) {
	if options.NextLengthDays != 0 && (options.NextLengthDays < 1 || options.NextLengthDays > 365) {
		return nil, ErrInvalidLength
	}
	if options.NextStartsAt != nil && !options.NextStartsAt.After(time.Now()) {
		return nil, ErrInvalidStart
	}
	var next Sprint
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		var id string
		err := tx.QueryRow(ctx, `SELECT s.id FROM sprints s JOIN boards b ON b.id = s.board_id
			JOIN projects p ON p.id = b.project_id
			WHERE s.id = $1 AND p.owner_clerk_id = $2 AND s.closed_at IS NULL FOR UPDATE OF s`, sprintID, owner).Scan(&id)
		if err != nil {
			return notFound(err)
		}
		next, err = closeSprint(ctx, tx, id, options)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &next, nil
}

// closeSprint (caller holds the sprint row lock) marks it closed, opens the next sprint
// with the same length starting now, and carries over every ticket that isn't in the
// board's last column ("done"), keeping its column and order. If the board planned its next
// sprint (refinement) and that plan is within capacity or approved, the plan's tickets join it.
func closeSprint(ctx context.Context, tx pgx.Tx, id string, options CloseOptions) (Sprint, error) {
	if err := boards.RecordScope(ctx, tx, id); err != nil {
		return Sprint{}, err
	}
	var boardID string
	if err := tx.QueryRow(ctx, `UPDATE sprints SET closed_at = now() WHERE id = $1 RETURNING board_id`, id).Scan(&boardID); err != nil {
		return Sprint{}, err
	}
	// refinement: planned sprint 1 becomes the next sprint (its length, unless the closer chose one)
	plan, err := nextPlan(ctx, tx, boardID)
	if err != nil {
		return Sprint{}, err
	}
	if plan != nil && options.NextLengthDays == 0 {
		options.NextLengthDays = plan.LengthDays
	}
	next, err := boards.ScanSprint(tx.QueryRow(ctx, `INSERT INTO sprints (board_id, number, length_days, starts_at, ends_at)
		SELECT board_id, number + 1, COALESCE(NULLIF($2, 0), length_days), COALESCE($3::timestamptz, now()),
			COALESCE($3::timestamptz, now()) + make_interval(days => COALESCE(NULLIF($2, 0), length_days))
		FROM sprints WHERE id = $1 RETURNING `+boards.SprintCols, id, options.NextLengthDays, options.NextStartsAt))
	if err != nil {
		return Sprint{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE tickets SET sprint_id = $2 WHERE sprint_id = $1 AND column_id <> (
		SELECT c.id FROM columns c WHERE c.board_id = $3 ORDER BY c.position DESC LIMIT 1)`, id, next.ID, next.BoardID); err != nil {
		return Sprint{}, err
	}
	if plan != nil {
		err = movePlanned(ctx, tx, plan, next)
	}
	return next, err
}

// AutoClose closes every open sprint past its end date. Rows are claimed with
// FOR UPDATE SKIP LOCKED, so several replicas can run it without double-closing.
// A sprint that fails to close is skipped for the rest of the run, so it can't hold up
// the others; its error (with the sprint id) is returned alongside the count.
func (s Store) AutoClose(ctx context.Context) (int, error) {
	closed, failed := 0, []string{}
	var errs []error
	for {
		var id string
		err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
			err := tx.QueryRow(ctx, `SELECT id FROM sprints WHERE closed_at IS NULL AND starts_at <= now() AND ends_at <= now()
				AND NOT (id::text = ANY($1)) ORDER BY ends_at LIMIT 1 FOR UPDATE SKIP LOCKED`, failed).Scan(&id)
			if err != nil {
				return err
			}
			_, err = closeSprint(ctx, tx, id, CloseOptions{})
			return err
		})
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return closed, errors.Join(errs...)
		case err != nil && id == "": // couldn't even pick a sprint (e.g. database unreachable)
			return closed, errors.Join(append(errs, err)...)
		case err != nil:
			errs = append(errs, fmt.Errorf("sprint %s: %w", id, err))
			failed = append(failed, id)
		default:
			closed++
		}
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
