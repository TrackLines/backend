package tickets

import (
	"context"
	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/boards"
	"github.com/tracklines/backend/internal/httpx"
)

// A parent leaving its board takes its open sub-tickets there with it (TL-218): to the backlog, or to
// the new board's first column. Finished ones stay as the old board's history; ones on other boards
// belong to those teams and stay. Nested sub-tickets follow the same rule all the way down.

// Moved is a sub-ticket that moved with its parent.
type Moved struct {
	ID    string  `json:"id"`
	Key   *string `json:"key"`
	Title string  `json:"title"`
}

// GroupMoveError: a sub-ticket couldn't make the move, so nothing moved. It unwraps to why (sprint
// locked, estimate needed, agent review…), so the status code is that rule's.
type GroupMoveError struct {
	Sub Moved
	Err error
}

func (e GroupMoveError) Error() string {
	name := e.Sub.Title
	if e.Sub.Key != nil {
		name = *e.Sub.Key + " (" + e.Sub.Title + ")"
	}
	return fmt.Sprintf("its sub-ticket %s can't move with it: %v", name, e.Err)
}

func (e GroupMoveError) Unwrap() error { return e.Err }

// openSubTickets: id's sub-tickets at any depth that are on board and not done, parents first.
func openSubTickets(ctx context.Context, q pgx.Tx, id, board string) ([]Moved, error) {
	rows, err := q.Query(ctx, `WITH RECURSIVE d AS (
			SELECT x.id, 1 AS depth FROM tickets x WHERE x.parent_id = $1
			UNION SELECT x.id, d.depth + 1 FROM tickets x JOIN d ON x.parent_id = d.id WHERE d.depth < 50)
		SELECT t.id, pr.key_prefix || '-' || t.number, t.title FROM d JOIN tickets t ON t.id = d.id JOIN projects pr ON pr.id = t.project_id
		WHERE t.board_id::text = $2 AND NOT `+boards.TicketDoneSQL+`
		GROUP BY t.id, pr.key_prefix, t.number, t.title, t.position ORDER BY min(d.depth), t.position, t.id`, id, board)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Moved, error) {
		var m Moved
		return m, row.Scan(&m.ID, &m.Key, &m.Title)
	})
}

// GroupPreview: GET /api/tickets/{id}/group, the open sub-tickets that would move with ticket {id} if
// it left its board (TL-218), so the app can say so first. [] when it's in the backlog or has none.
func (h System) GroupPreview(w http.ResponseWriter, r *http.Request) {
	var out []Moved
	err := pgx.BeginFunc(r.Context(), h.store.DB, func(tx pgx.Tx) error {
		var board *string
		if err := tx.QueryRow(r.Context(), `SELECT t.board_id::text FROM tickets t JOIN projects p ON p.id = t.project_id
			WHERE t.id::text = $1 AND p.owner_clerk_id = $2`, r.PathValue("id"), auth.OrgID(r.Context())).Scan(&board); err != nil {
			return notFound(err)
		}
		if board == nil {
			return nil
		}
		var err error
		out, err = openSubTickets(r.Context(), tx, r.PathValue("id"), *board)
		return err
	})
	if err != nil {
		writeErr(w, r, err)
		return
	}
	if out == nil {
		out = []Moved{}
	}
	httpx.JSON(w, http.StatusOK, out)
}
