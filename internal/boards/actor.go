package boards

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
)

// SetActor names who is changing tickets in tx, for their history (TL-75): actor, or else the
// request's caller (a user id or agent name), and what kind of caller that is, for the auditor
// (TL-666). The ticket_history triggers read them; with nobody (a webhook, a background job) the
// entries have no actor. They last until tx ends.
func SetActor(ctx context.Context, tx pgx.Tx, actor string) error {
	if actor == "" {
		actor = auth.ActorID(ctx)
	}
	if actor == "" {
		return nil
	}
	_, err := tx.Exec(ctx, `SELECT set_config('tracklines.actor', $1, true), set_config('tracklines.actor_kind', $2, true)`, actor, ActorKind(ctx))
	return err
}

// ActorKind is the caller as the auditor names it: a person (signed in, or their personal key), an
// agent or a service key, or the system when nobody is (TL-666).
func ActorKind(ctx context.Context) string {
	if !auth.ViaAPIKey(ctx) {
		if _, signedIn := auth.UserID(ctx); signedIn {
			return "person"
		}
		return "system"
	}
	switch auth.APIKeyKind(ctx) {
	case "personal":
		return "person"
	case "service":
		return "service"
	}
	return "agent"
}

// ActorHook makes every connection checked out of the pool name its caller (TL-677): the request's actor
// and their kind become session settings, which the history and audit triggers read for every write,
// single statements included. SetActor still names someone else for one transaction. A connection only
// hears about a change of caller: a checkout by the same one costs nothing.
// ponytail: one round trip when the caller changes; cheaper per-statement routes would need every write rewritten.
func ActorHook(c *pgxpool.Config) {
	c.PrepareConn = func(ctx context.Context, conn *pgx.Conn) (bool, error) {
		actor, kind := auth.ActorID(ctx), ""
		if actor != "" {
			kind = ActorKind(ctx)
		}
		data := conn.PgConn().CustomData()
		if who := actor + "\x00" + kind; data["actor"] != who {
			if _, err := conn.Exec(ctx, `SELECT set_config('tracklines.actor', $1, false), set_config('tracklines.actor_kind', $2, false)`, actor, kind); err != nil {
				return false, err
			}
			data["actor"] = who
		}
		return true, nil
	}
}
