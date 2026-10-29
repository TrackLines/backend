package tickets

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/tracklines/backend/internal/boards"
	"github.com/tracklines/backend/internal/httpx"
)

var (
	ErrBlocked   = httpx.Code("blocked", "ticket is blocked by tickets that aren't done yet")
	ErrSelfBlock = httpx.Code("self_block", "a ticket can't block itself")
	ErrCycle     = httpx.Code("block_cycle", "that would make tickets block each other in a loop")
	ErrDoneBlock = httpx.Code("done_blocker", "a done ticket can't get new blockers; move it out of Done first")
	// a ticket that isn't there, or is another org's: they read the same
	ErrUnknownBlocker = httpx.Code("unknown_blocker", "blockers must be tickets in this organization")
)

// Dep is a linked ticket shown under "blocked by" / "blocks", with its project: blockers can be in
// another project of the org (TL-714), shown as "Project - Title".
type Dep struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Done        bool   `json:"done"`
	ProjectID   string `json:"project_id,omitempty"`
	ProjectName string `json:"project_name,omitempty"`
}

// SetBlockedBy replaces the tickets that id depends on. They can be in any of the org's projects
// (TL-714), never another org's; a set that would create a loop (A waits on B waits on A) is refused.
// Edits are serialised per org so two concurrent changes can't create a loop between them.
func (s Store) SetBlockedBy(ctx context.Context, owner, id string, blockers []string) error {
	return pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		var done bool
		if err := tx.QueryRow(ctx, `SELECT `+boards.TicketDoneSQL+` FROM tickets t JOIN projects p ON p.id = t.project_id
			WHERE t.id = $1 AND p.owner_clerk_id = $2`, id, owner).Scan(&done); err != nil {
			return notFound(err)
		}
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('ticket-deps:' || $1))`, owner); err != nil {
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
		var inOrg int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM tickets t JOIN projects p ON p.id = t.project_id
			WHERE t.id = ANY($1::uuid[]) AND p.owner_clerk_id = $2`, uniq, owner).Scan(&inOrg); err != nil {
			return notFound(err) // malformed id
		}
		if inOrg != len(uniq) {
			return ErrUnknownBlocker
		}
		var adds bool
		if err := tx.QueryRow(ctx, `SELECT $3 AND EXISTS (SELECT unnest($2::uuid[]) EXCEPT SELECT blocked_by_id FROM ticket_dependencies WHERE ticket_id = $1)`,
			id, uniq, done).Scan(&adds); err != nil {
			return err
		}
		// only the blockers that go, so the ticket's history says what really changed (TL-677)
		if _, err := tx.Exec(ctx, `DELETE FROM ticket_dependencies WHERE ticket_id = $1 AND blocked_by_id <> ALL($2::uuid[])`, id, uniq); err != nil {
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
		if adds { // TL-528: a done ticket can lose blockers, not gain them
			return ErrDoneBlock
		}
		_, err := tx.Exec(ctx, `INSERT INTO ticket_dependencies (ticket_id, blocked_by_id) SELECT $1, unnest($2::uuid[]) ON CONFLICT DO NOTHING`, id, uniq)
		return err
	})
}

// deps lists tickets on one side of id's dependencies: blockedBy=true → what id waits on, else what waits on id.
func (s Store) deps(ctx context.Context, id string, blockedBy bool) ([]Dep, error) {
	join, where := "d.blocked_by_id", "d.ticket_id"
	if !blockedBy {
		join, where = "d.ticket_id", "d.blocked_by_id"
	}
	rows, err := s.DB.Query(ctx, `SELECT t.id, t.title, `+boards.TicketDoneSQL+`, p.id, p.name FROM ticket_dependencies d
		JOIN tickets t ON t.id = `+join+` JOIN projects p ON p.id = t.project_id WHERE `+where+` = $1 ORDER BY t.title`, id)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Dep, error) {
		var d Dep
		return d, row.Scan(&d.ID, &d.Title, &d.Done, &d.ProjectID, &d.ProjectName)
	})
	if out == nil {
		out = []Dep{}
	}
	return out, err
}
