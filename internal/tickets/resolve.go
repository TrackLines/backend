package tickets

import (
	"context"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/boards"
)

var ErrOnBoard = errors.New("this ticket is on a board: move it to the board's Done column instead")

// Resolve closes a backlog ticket in place: it counts as done everywhere (boards.DoneSQL) and leaves
// the backlog, but keeps its links and stays readable by id. Resolving again is a no-op. Moving it
// onto a board reopens it. Then its parent is completed if this was its last unfinished sub-ticket.
func (s Store) Resolve(ctx context.Context, owner, id string) error {
	tag, err := s.DB.Exec(ctx, `UPDATE tickets t SET resolved_at = COALESCE(t.resolved_at, now()),
			done_at = COALESCE(t.done_at, now()), updated_at = now()
		FROM projects p WHERE t.id = $1 AND p.id = t.project_id AND p.owner_clerk_id = $2 AND t.board_id IS NULL`, id, owner)
	if err != nil {
		return notFound(err)
	}
	if tag.RowsAffected() == 0 { // not the owner's ticket, or it's on a board
		var onBoard bool
		if err := s.DB.QueryRow(ctx, `SELECT t.board_id IS NOT NULL FROM tickets t JOIN projects p ON p.id = t.project_id
			WHERE t.id = $1 AND p.owner_clerk_id = $2`, id, owner).Scan(&onBoard); err != nil {
			return notFound(err)
		}
		return ErrOnBoard
	}
	return s.completeParent(ctx, owner, id)
}

// completeParent finishes id's parent once every one of its sub-tickets is done or resolved: a
// backlog parent is resolved, a parent on a board moves to that board's Done column. Each of those
// completes its own parent in turn, so it carries up the chain. A parent that's already done, or
// whose children aren't all finished, is left alone.
func (s Store) completeParent(ctx context.Context, owner, id string) error {
	var parent, board *string
	var children, finished int
	var parentDone bool
	err := s.DB.QueryRow(ctx, `SELECT p.id, p.board_id, `+boards.DoneSQL("p")+`,
			(SELECT count(*) FROM tickets c WHERE c.parent_id = p.id),
			(SELECT count(*) FROM tickets c WHERE c.parent_id = p.id AND `+boards.DoneSQL("c")+`)
		FROM tickets t JOIN tickets p ON p.id = t.parent_id WHERE t.id = $1`, id).
		Scan(&parent, &board, &parentDone, &children, &finished)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (parentDone || children == 0 || finished < children)) {
		return nil
	}
	if err != nil {
		return err
	}
	if board == nil {
		return s.Resolve(ctx, owner, *parent)
	}
	var done string
	if err := s.DB.QueryRow(ctx, `SELECT id FROM columns WHERE board_id = $1 ORDER BY position DESC LIMIT 1`, *board).Scan(&done); err != nil {
		return err
	}
	if err := s.move(ctx, owner, *parent, done, 0, moveOpts{}); err != nil {
		return err
	}
	return s.completeParent(ctx, owner, *parent)
}

// Resolve: POST /api/tickets/{id}/resolve. 204; 409 for a ticket on a board.
func (h System) Resolve(w http.ResponseWriter, r *http.Request) {
	if err := h.store.Resolve(r.Context(), auth.OrgID(r.Context()), r.PathValue("id")); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
