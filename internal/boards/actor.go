package boards

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/tracklines/backend/internal/auth"
)

// SetActor names who is changing tickets in tx, for their history (TL-75): actor, or else the
// request's caller (a user id or agent name). The ticket_history trigger reads it; with nobody (a
// webhook, a background job) the entries have no actor. It lasts until tx ends.
func SetActor(ctx context.Context, tx pgx.Tx, actor string) error {
	if actor == "" {
		actor = auth.ActorID(ctx)
	}
	if actor == "" {
		return nil
	}
	_, err := tx.Exec(ctx, `SELECT set_config('tracklines.actor', $1, true)`, actor)
	return err
}
