// Package inbox puts in-app notifications (the bell) in people's inboxes, and nothing else: no push,
// no Chat. It needs only Postgres, so the automation service's jobs can use it as well as the backend's
// notifications (TL-431).
package inbox

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// Execer is what Add writes with: a pool, a connection or a transaction.
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Notice is one notification: what happened (kind, text), who did it, and what it's about.
type Notice struct {
	Kind, Text, Actor string
	TicketID, BoardID string // either may be ""
}

// Add puts n in the inbox of each person in to: people only (user_…, not API keys or agents), each
// once, and never the actor's own.
func Add(ctx context.Context, db Execer, org string, to []string, n Notice) error {
	seen := map[string]bool{n.Actor: true}
	var users []string
	for _, u := range to {
		if strings.HasPrefix(u, "user_") && !seen[u] {
			seen[u] = true
			users = append(users, u)
		}
	}
	if len(users) == 0 {
		return nil
	}
	_, err := db.Exec(ctx, `INSERT INTO notifications (org_id, user_id, kind, text, actor, ticket_id, board_id)
		SELECT $1, u, $3, $4, $5, NULLIF($6, '')::uuid, NULLIF($7, '')::uuid FROM unnest($2::text[]) AS u`,
		org, users, n.Kind, n.Text, n.Actor, n.TicketID, n.BoardID)
	return err
}
