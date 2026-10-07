package tickets

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/tracklines/backend/internal/boards"
)

var (
	ErrBlocked   = errors.New("ticket is blocked by tickets that aren't done yet")
	ErrSelfBlock = errors.New("a ticket can't block itself")
	ErrCycle     = errors.New("that would make tickets block each other in a loop")
)

// Dep is a linked ticket shown under "blocked by" / "blocks".
type Dep struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Done  bool   `json:"done"`
}

// SetBlockedBy replaces the tickets that id depends on. All must be in id's project; a set that
// would create a loop (A waits on B waits on A) is refused. Edits are serialised per project so two
// concurrent changes can't create a loop between them.
func (s Store) SetBlockedBy(ctx context.Context, owner, id string, blockers []string) error {
	return pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		var projectID string
		if err := tx.QueryRow(ctx, `SELECT t.project_id FROM tickets t JOIN projects p ON p.id = t.project_id
			WHERE t.id = $1 AND p.owner_clerk_id = $2`, id, owner).Scan(&projectID); err != nil {
			return notFound(err)
		}
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('ticket-deps:' || $1))`, projectID); err != nil {
			return err
		}
		uniq := make([]string, 0, len(blockers))
		seen := map[string]bool{}
		for _, b := range blockers {
			if b == id {
				return ErrSelfBlock
			}
			if !seen[b] {
				seen[b] = true
				uniq = append(uniq, b)
			}
		}
		var inProject int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM tickets WHERE id = ANY($1::uuid[]) AND project_id = $2`, uniq, projectID).Scan(&inProject); err != nil {
			return notFound(err) // malformed id
		}
		if inProject != len(uniq) {
			return ErrOtherProject
		}
		if _, err := tx.Exec(ctx, `DELETE FROM ticket_dependencies WHERE ticket_id = $1`, id); err != nil {
			return err
		}
		// would id become reachable by following "blocked by" from its new blockers?
		var loops bool
		if err := tx.QueryRow(ctx, `WITH RECURSIVE chain(tid) AS (
				SELECT unnest($1::uuid[])
				UNION SELECT d.blocked_by_id FROM ticket_dependencies d JOIN chain c ON d.ticket_id = c.tid
			) SELECT EXISTS (SELECT 1 FROM chain WHERE tid = $2)`, uniq, id).Scan(&loops); err != nil {
			return err
		}
		if loops {
			return ErrCycle
		}
		_, err := tx.Exec(ctx, `INSERT INTO ticket_dependencies (ticket_id, blocked_by_id) SELECT $1, unnest($2::uuid[])`, id, uniq)
		return err
	})
}

// deps lists tickets on one side of id's dependencies: blockedBy=true → what id waits on, else what waits on id.
func (s Store) deps(ctx context.Context, id string, blockedBy bool) ([]Dep, error) {
	join, where := "d.blocked_by_id", "d.ticket_id"
	if !blockedBy {
		join, where = "d.ticket_id", "d.blocked_by_id"
	}
	rows, err := s.DB.Query(ctx, `SELECT t.id, t.title, `+boards.TicketDoneSQL+` FROM ticket_dependencies d
		JOIN tickets t ON t.id = `+join+` WHERE `+where+` = $1 ORDER BY t.title`, id)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Dep, error) {
		var d Dep
		return d, row.Scan(&d.ID, &d.Title, &d.Done)
	})
	if out == nil {
		out = []Dep{}
	}
	return out, err
}
