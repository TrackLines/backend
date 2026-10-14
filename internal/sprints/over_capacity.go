package sprints

import (
	"context"
	"fmt"
	"math"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/tracklines/backend/internal/boards"
	"github.com/tracklines/backend/internal/notifications"
)

// overCapacity is a sprint that started over its board's capacity (velocity + 10%), from what was
// carried over plus the refined plan (TL-149). Refinement guards a plan; carry-over has no limit.
type overCapacity struct {
	org, board, boardName string
	number                int
	used, cap             float64
	unit                  string
}

// startedOver checks sprint next as it starts. Nil when it's within capacity, the board has no
// velocity yet (nothing to compare with), or a lead approved the plan for at least this much.
func startedOver(ctx context.Context, tx pgx.Tx, next Sprint, approved *float64) (*overCapacity, error) {
	o := overCapacity{board: next.BoardID, number: next.Number}
	var scale string
	if err := tx.QueryRow(ctx, `SELECT b.estimate_scale, b.name, p.owner_clerk_id FROM boards b JOIN projects p ON p.id = b.project_id
		WHERE b.id = $1`, next.BoardID).Scan(&scale, &o.boardName, &o.org); err != nil {
		return nil, err
	}
	o.unit = unitOf(scale)
	v, err := velocity(ctx, tx, next.BoardID, o.unit)
	if err != nil || v == nil {
		return nil, err
	}
	o.cap = math.Round(*v*1.1*100) / 100
	rows, err := tx.Query(ctx, `SELECT t.estimate FROM tickets t WHERE t.sprint_id = $1 AND NOT `+boards.TicketDoneSQL, next.ID)
	if err != nil {
		return nil, err
	}
	value := valueIn(o.unit)
	var estimate *string
	if _, err := pgx.ForEachRow(rows, []any{&estimate}, func() error { o.used += value(estimate); return nil }); err != nil {
		return nil, err
	}
	if o.used <= o.cap || (approved != nil && o.used <= *approved) {
		return nil, nil
	}
	return &o, nil
}

// warnOver tells the board's managers (its org admins and team leaders) that the sprint started over
// capacity, so they can move something out. Nothing to say, or nobody to tell: nothing happens.
func (s Store) warnOver(ctx context.Context, o *overCapacity) {
	if o == nil || s.Notify == nil {
		return
	}
	to, err := notifications.Store{DB: s.DB}.BoardManagers(ctx, o.org, o.board)
	if err != nil || len(to) == 0 {
		return
	}
	notifications.Send(ctx, s.Notify, o.org, to, notifications.Event{
		Kind: "over_capacity", Actor: "Tracklines", BoardID: o.board,
		Text: fmt.Sprintf("started sprint %d on %s over capacity: %s %s against a limit of %s (velocity + 10%%), from unfinished work carried over",
			o.number, o.boardName, num(o.used), o.unit, num(o.cap)),
	})
}

// num writes 14 as "14" and 12.1 as "12.1".
func num(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }
