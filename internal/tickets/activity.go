package tickets

import (
	"context"
	"encoding/json"
	"sort"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/jackc/pgx/v5"
)

// ActivityEntry is one thing that happened to a ticket (TL-679): a stage it went through, an edit (which
// fields; never the text), an assignment, a label, a comment or file added or removed, a pull request…
// Action names it; data holds the details (see the actions in the OpenAPI).
type ActivityEntry struct {
	ID        string          `json:"id"`
	At        string          `json:"at"` // UTC
	Actor     *string         `json:"actor"`
	ActorKind string          `json:"actor_kind"` // person, agent, service or system
	Action    string          `json:"action"`
	Data      json.RawMessage `json:"data"`
}

// ActivitySource reads a ticket's activity from the auditor.
type ActivitySource func(ctx context.Context, org, ticket string) ([]ActivityEntry, error)

const activityCols = `to_char(at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), actor, actor_kind, action, data`

func scanActivity(rows pgx.Rows) ([]ActivityEntry, error) {
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (ActivityEntry, error) {
		var e ActivityEntry
		return e, row.Scan(&e.ID, &e.At, &e.Actor, &e.ActorKind, &e.Action, &e.Data)
	})
}

// Activity is everything that happened to ticket id, oldest first, for owner's organization. With remote
// (the auditor, while the "audit-service" flag is on) it's the auditor's activity plus the ticket's events
// still queued for it, each once; without, the stages in ticket_history, in the same shape.
func (s Store) Activity(ctx context.Context, owner, id string, remote ActivitySource) ([]ActivityEntry, error) {
	var ok bool
	if err := s.DB.QueryRow(ctx, `SELECT true FROM tickets t JOIN projects p ON p.id = t.project_id AND p.owner_clerk_id = $2
		WHERE t.id::text = $1`, id, owner).Scan(&ok); err != nil {
		return nil, notFound(err)
	}
	if remote == nil {
		// the same event the queue gives each stage (audit_stage_id, migration 000096); who did it as the
		// auditor names them (a user id is a person, any other name an agent)
		rows, err := s.DB.Query(ctx, `SELECT audit_stage_id(h.id)::text, to_char(h.at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), h.actor,
				CASE WHEN h.actor IS NULL THEN 'system' WHEN h.actor LIKE 'user\_%' THEN 'person' ELSE 'agent' END, 'ticket.stage',
				jsonb_strip_nulls(jsonb_build_object('stage', h.stage, 'outcome', h.outcome, 'stage_order', h.stage_order,
					'board_id', h.board_id, 'board_name', h.board_name, 'column_id', h.column_id,
					'column_name', h.column_name, 'resolution', h.resolution)) || jsonb_build_object('done', h.done)
			FROM ticket_history h WHERE h.ticket_id::text = $1 ORDER BY h.id`, id)
		if err != nil {
			return nil, err
		}
		out, err := scanActivity(rows)
		if out == nil {
			out = []ActivityEntry{}
		}
		return out, err
	}
	out, err := remote(ctx, owner, id)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		logs.Warnf("tickets: activity from the auditor: %v", err)
		return nil, ErrHistoryUnavailable
	}
	rows, err := s.DB.Query(ctx, `SELECT id::text, `+activityCols+` FROM audit_outbox
		WHERE org_id = $1 AND target_type = 'ticket' AND target_id = $2 ORDER BY at, queued`, owner, id)
	if err != nil {
		return nil, err
	}
	queued, err := scanActivity(rows)
	if err != nil {
		return nil, err
	}
	// an event the auditor has just stored may still be queued until the relay clears it: once each
	seen := make(map[string]bool, len(out))
	for _, e := range out {
		seen[e.ID] = true
	}
	for _, e := range queued {
		if !seen[e.ID] {
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At < out[j].At }) // UTC, one format: sorts as text
	if out == nil {
		out = []ActivityEntry{}
	}
	return out, nil
}
