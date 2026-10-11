package sprints

import (
	"context"
	"fmt"
	"net/http"

	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/notifications"
)

// WithNotifier tells board managers when a planned sprint needs their approval; nil: nobody.
func (h System) WithNotifier(n notifications.Notifier) System { h.notify = n; return h }

// approvalState is whether planned sprint id needs approval now (and its board and number).
func (h System) approvalState(ctx context.Context, org, id string) (board string, number int, needs bool) {
	if h.notify == nil || id == "" {
		return "", 0, false
	}
	if err := h.store.DB.QueryRow(ctx, `SELECT board_id FROM planned_sprints WHERE id::text = $1`, id).Scan(&board); err != nil {
		return "", 0, false
	}
	plans, err := h.store.Planned(ctx, org, board)
	if err != nil {
		return "", 0, false
	}
	for _, p := range plans {
		if p.ID == id {
			return board, p.Number, p.Capacity.NeedsApproval
		}
	}
	return "", 0, false
}

// overCapacity tells the board's managers once a planned sprint goes over capacity and needs approval.
func (h System) overCapacity(r *http.Request, board string, number int) {
	org := auth.OrgID(r.Context())
	to, err := notifications.Store{DB: h.store.DB}.BoardManagers(r.Context(), org, board)
	if err != nil {
		return
	}
	var name string
	_ = h.store.DB.QueryRow(r.Context(), `SELECT name FROM boards WHERE id::text = $1`, board).Scan(&name)
	notifications.Send(r.Context(), h.notify, org, to, notifications.Event{
		Kind: "approval", Text: fmt.Sprintf("planned sprint %d on %s over capacity, and it needs your approval", number, name),
		Actor: auth.ActorID(r.Context()), BoardID: board,
	})
}
