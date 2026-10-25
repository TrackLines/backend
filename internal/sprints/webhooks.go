package sprints

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/tracklines/backend/internal/webhooks"
)

// emitSprint queues sprint.started or sprint.closed (TL-635) in the transaction that did it.
func emitSprint(ctx context.Context, tx pgx.Tx, event string, sp Sprint) error {
	var org string
	if err := tx.QueryRow(ctx, `SELECT p.owner_clerk_id FROM boards b JOIN projects p ON p.id = b.project_id WHERE b.id = $1`, sp.BoardID).Scan(&org); err != nil {
		return err
	}
	return webhooks.Emit(ctx, tx, org, event, func() (any, error) {
		return map[string]any{"board_id": sp.BoardID, "sprint": map[string]any{
			"id": sp.ID, "number": sp.Number, "starts_at": sp.StartsAt, "ends_at": sp.EndsAt, "goal": sp.Goal}}, nil
	})
}
