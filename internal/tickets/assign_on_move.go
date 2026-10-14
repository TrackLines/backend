package tickets

import (
	"context"
	"errors"
	"net/http"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/tracklines/backend/internal/auth"
)

// Assign on move (TL-266): whoever moves an unassigned ticket to another column takes it, the way
// claiming does (never a blocked ticket, never over someone else). Not:
//   - into a board's first column (To do): sending it back means nobody's on it;
//   - by an API key: agents claim before they work;
//   - by an admin or the board's team leader while a standup runs on the board: they're running it.

// moveFrom is the ticket's column before a move ("" in the backlog) and whether it's unassigned.
func (s Store) moveFrom(ctx context.Context, owner, id string) (column string, unassigned bool) {
	_ = s.DB.QueryRow(ctx, `SELECT coalesce(t.column_id::text, ''), t.assigned_to IS NULL FROM tickets t
		JOIN projects p ON p.id = t.project_id WHERE t.id = $1 AND p.owner_clerk_id = $2`, id, owner).Scan(&column, &unassigned)
	return column, unassigned
}

func (h System) assignMover(r *http.Request, org, id, toColumn string) {
	ctx := r.Context()
	user, signedIn := auth.UserID(ctx)
	if h.boardManager == nil || !signedIn || auth.ViaAPIKey(ctx) {
		return
	}
	var board string
	var first bool
	if err := h.store.DB.QueryRow(ctx, `SELECT c.board_id::text, c.position = (SELECT min(position) FROM columns WHERE board_id = c.board_id)
		FROM columns c WHERE c.id::text = $1`, toColumn).Scan(&board, &first); err != nil || first {
		return
	}
	if h.standupRunning != nil && h.standupRunning(ctx, board) {
		if manager, err := h.boardManager(ctx, org, user, board); err != nil || manager {
			return
		}
	}
	// someone got there first, or it's blocked: nothing to assign, nothing wrong
	if _, err := h.store.Claim(ctx, org, user, id); err != nil && !errors.Is(err, ErrAlreadyAssigned) && !errors.Is(err, ErrBlocked) {
		logs.Errorf("tickets: assign on move: %v", err)
	}
}
