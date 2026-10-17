package tickets

import (
	"context"

	"github.com/jackc/pgx/v5"
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
