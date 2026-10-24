package tickets

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/tracklines/backend/internal/boards"
	"github.com/tracklines/backend/internal/httpx"
)

// Epics (TL-213): large pieces of work. An epic lives in the backlog (never on a board or in a sprint),
// takes no estimate, can't sit anywhere inside another epic, and gets its progress and size from its
// sub-tickets. Any ticket can be made an epic; one on a board goes to the backlog.

const TypeEpic = "epic"

var (
	ErrEpicOffBoard = httpx.Code("epic_on_board", "epics stay in the backlog: they can't go on a board or into a sprint")
	ErrEpicEstimate = httpx.Code("epic_estimate", "epics take no estimate: their size comes from their sub-tickets")
	ErrEpicInEpic   = httpx.Code("epic_in_epic", "an epic can't be inside another epic")
)

// EpicProgress: how far an epic's sub-tickets have got (duplicates left out), and their size: the sum
// of their estimates as points, and how many have no estimate yet.
type EpicProgress struct {
	Done        int     `json:"done"`
	Total       int     `json:"total"`
	Points      float64 `json:"points"`
	Unestimated int     `json:"unestimated"`
}

// kidsSQL is one row per epic t: its sub-tickets' done count, total, estimates and unestimated count.
var kidsSQL = fmt.Sprintf(`LEFT JOIN LATERAL (SELECT count(*) FILTER (WHERE %s) AS done, count(*) AS total,
		coalesce(array_agg(x.estimate) FILTER (WHERE x.estimate IS NOT NULL), '{}') AS estimates,
		count(*) FILTER (WHERE x.estimate IS NULL) AS unestimated
	FROM tickets x WHERE x.parent_id = t.id AND x.duplicate_of IS NULL) kids ON true`, boards.DoneSQL("x"))

func progress(done, total, unestimated int, estimates []string) EpicProgress {
	p := EpicProgress{Done: done, Total: total, Unestimated: unestimated}
	for i := range estimates {
		p.Points += boards.Points(&estimates[i])
	}
	return p
}

// epicProgress is ticket id's progress when it's an epic, else nil.
func (s Store) epicProgress(ctx context.Context, id string) (*EpicProgress, error) {
	var isEpic bool
	var done, total, unestimated int
	var estimates []string
	err := s.DB.QueryRow(ctx, `SELECT t.type = 'epic', kids.done, kids.total, kids.unestimated, kids.estimates
		FROM tickets t `+kidsSQL+` WHERE t.id = $1`, id).Scan(&isEpic, &done, &total, &unestimated, &estimates)
	if err != nil || !isEpic {
		return nil, err
	}
	p := progress(done, total, unestimated, estimates)
	return &p, nil
}

// EpicSummary is one epic in a project's list.
type EpicSummary struct {
	ID         string       `json:"id"`
	Key        *string      `json:"key"`
	Title      string       `json:"title"`
	Priority   string       `json:"priority"`
	Done       bool         `json:"done"`
	Resolution *string      `json:"resolution"`
	Progress   EpicProgress `json:"progress"`
}

// Epics lists project projectID's epics: open ones first (by backlog position), then finished ones,
// newest first.
func (s Store) Epics(ctx context.Context, owner, projectID string) ([]EpicSummary, error) {
	var ok bool
	if err := s.DB.QueryRow(ctx, `SELECT true FROM projects WHERE id::text = $1 AND owner_clerk_id = $2`, projectID, owner).Scan(&ok); err != nil {
		return nil, notFound(err)
	}
	rows, err := s.DB.Query(ctx, `SELECT t.id, pr.key_prefix || '-' || t.number, t.title, t.priority::text, t.resolved_at IS NOT NULL, t.resolution,
			kids.done, kids.total, kids.unestimated, kids.estimates
		FROM tickets t JOIN projects pr ON pr.id = t.project_id `+kidsSQL+`
		WHERE t.project_id::text = $1 AND t.type = 'epic' AND t.duplicate_of IS NULL
		ORDER BY t.resolved_at IS NOT NULL, CASE WHEN t.resolved_at IS NULL THEN t.position END, t.resolved_at DESC`, projectID)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (EpicSummary, error) {
		var e EpicSummary
		var done, total, unestimated int
		var estimates []string
		err := row.Scan(&e.ID, &e.Key, &e.Title, &e.Priority, &e.Done, &e.Resolution, &done, &total, &unestimated, &estimates)
		e.Progress = progress(done, total, unestimated, estimates)
		return e, err
	})
	if out == nil {
		out = []EpicSummary{}
	}
	return out, err
}

// inEpic is a condition on ticket t: it sits under epic param (a $n holding the epic's id, ” for any
// ticket), at any depth (TL-215).
func inEpic(param string) string {
	return `(` + param + ` = '' OR t.id IN (WITH RECURSIVE d AS (SELECT x.id FROM tickets x WHERE x.parent_id::text = ` + param + `
		UNION SELECT x.id FROM tickets x JOIN d ON x.parent_id = d.id) SELECT id FROM d))`
}

// epicNesting refuses a tree where an epic sits inside another: child (with its sub-tickets) under
// parent, when child or any of its sub-tickets is an epic and parent or any of its ancestors is one.
func epicNesting(ctx context.Context, q pgx.Tx, child, parent string) error {
	var clash bool
	err := q.QueryRow(ctx, `WITH RECURSIVE up AS (
			SELECT id, parent_id, type FROM tickets WHERE id = $2
			UNION SELECT t.id, t.parent_id, t.type FROM tickets t JOIN up ON t.id = up.parent_id),
		down AS (
			SELECT id, type FROM tickets WHERE id = $1
			UNION SELECT t.id, t.type FROM tickets t JOIN down ON t.parent_id = down.id)
		SELECT EXISTS (SELECT 1 FROM up WHERE type = 'epic') AND EXISTS (SELECT 1 FROM down WHERE type = 'epic')`, child, parent).Scan(&clash)
	if err == nil && clash {
		return ErrEpicInEpic
	}
	return err
}

// canBecomeEpic: making id an epic mustn't put an epic inside another (one above it or below it).
func canBecomeEpic(ctx context.Context, q pgx.Tx, id string) error {
	var clash bool
	err := q.QueryRow(ctx, `WITH RECURSIVE up AS (
			SELECT id, parent_id, type FROM tickets WHERE id = (SELECT parent_id FROM tickets WHERE id = $1)
			UNION SELECT t.id, t.parent_id, t.type FROM tickets t JOIN up ON t.id = up.parent_id),
		down AS (
			SELECT id, type FROM tickets WHERE parent_id = $1
			UNION SELECT t.id, t.type FROM tickets t JOIN down ON t.parent_id = down.id)
		SELECT EXISTS (SELECT 1 FROM up WHERE type = 'epic') OR EXISTS (SELECT 1 FROM down WHERE type = 'epic')`, id).Scan(&clash)
	if err == nil && clash {
		return ErrEpicInEpic
	}
	return err
}
