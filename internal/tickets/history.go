package tickets

import (
	"context"
	"encoding/json"
	"sort"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/jackc/pgx/v5"

	"github.com/tracklines/backend/internal/httpx"
)

// HistoryEntry is one step in a ticket's lifecycle (TL-74): a stage it entered, or skipped on the way
// to a later one. Board and column names are as they were then.
type HistoryEntry struct {
	At         string  `json:"at"`      // UTC
	Stage      string  `json:"stage"`   // backlog | column | resolved
	Outcome    string  `json:"outcome"` // entered | skipped
	StageOrder int     `json:"stage_order"`
	BoardID    *string `json:"board_id,omitempty"`
	BoardName  *string `json:"board_name,omitempty"`
	ColumnID   *string `json:"column_id,omitempty"`
	ColumnName *string `json:"column_name,omitempty"`
	Done       bool    `json:"done"`                 // the board's last column
	Resolution *string `json:"resolution,omitempty"` // done | wont_do | duplicate
	Actor      *string `json:"actor,omitempty"`      // user id or agent name; absent when the system did it
}

// History is ticket id's lifecycle, oldest first, for owner's organization. Resolved tickets keep
// theirs; binned ones (deleted, awaiting purge) are not found, like everywhere else.
func (s Store) History(ctx context.Context, owner, id string) ([]HistoryEntry, error) {
	var ok bool
	if err := s.DB.QueryRow(ctx, `SELECT true FROM tickets t JOIN projects p ON p.id = t.project_id AND p.owner_clerk_id = $2
		WHERE t.id::text = $1`, id, owner).Scan(&ok); err != nil {
		return nil, notFound(err)
	}
	rows, err := s.DB.Query(ctx, `SELECT to_char(at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), stage, outcome, stage_order,
			board_id::text, board_name, column_id::text, column_name, done, resolution, actor
		FROM ticket_history WHERE ticket_id::text = $1 ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (HistoryEntry, error) {
		var e HistoryEntry
		err := row.Scan(&e.At, &e.Stage, &e.Outcome, &e.StageOrder, &e.BoardID, &e.BoardName, &e.ColumnID, &e.ColumnName, &e.Done, &e.Resolution, &e.Actor)
		return e, err
	})
	if out == nil {
		out = []HistoryEntry{}
	}
	return out, err
}

// ErrHistoryUnavailable: the auditor holds the history (TL-667) and can't be reached; the ticket itself
// still loads.
var ErrHistoryUnavailable = httpx.Code("history_unavailable", "this ticket's history can't be loaded right now: try again shortly")

// HistorySource reads a ticket's history from the auditor.
type HistorySource func(ctx context.Context, org, ticket string) ([]HistoryEntry, error)

// AuditedHistory is History read from the auditor (TL-667), with the ticket's stages still queued for it
// in audit_outbox added, so a move shows at once and nothing's missing while older history is delivered.
// ponytail: a batch the auditor has just stored but the relay hasn't yet removed from the outbox shows
// twice for that moment (under a second); the auditor's history has no ids to tell them apart.
func (s Store) AuditedHistory(ctx context.Context, owner, id string, remote HistorySource) ([]HistoryEntry, error) {
	var ok bool
	if err := s.DB.QueryRow(ctx, `SELECT true FROM tickets t JOIN projects p ON p.id = t.project_id AND p.owner_clerk_id = $2
		WHERE t.id::text = $1`, id, owner).Scan(&ok); err != nil {
		return nil, notFound(err)
	}
	out, err := remote(ctx, owner, id)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		logs.Warnf("tickets: history from the auditor: %v", err)
		return nil, ErrHistoryUnavailable
	}
	rows, err := s.DB.Query(ctx, `SELECT to_char(at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), actor, data FROM audit_outbox
		WHERE org_id = $1 AND action = 'ticket.stage' AND target_type = 'ticket' AND target_id = $2 ORDER BY at, queued`, owner, id)
	if err != nil {
		return nil, err
	}
	queued, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (HistoryEntry, error) {
		var e HistoryEntry
		var at string
		var actor *string
		var data []byte
		if err := row.Scan(&at, &actor, &data); err != nil {
			return e, err
		}
		err := json.Unmarshal(data, &e)
		e.At, e.Actor = at, actor
		return e, err
	})
	if err != nil {
		return nil, err
	}
	out = append(out, queued...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].At < out[j].At }) // UTC, one format: sorts as text
	if out == nil {
		out = []HistoryEntry{}
	}
	return out, nil
}
