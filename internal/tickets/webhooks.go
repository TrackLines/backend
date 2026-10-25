package tickets

import (
	"context"

	"github.com/tracklines/backend/internal/boards"
	"github.com/tracklines/backend/internal/webhooks"
)

// emit queues a ticket webhook (TL-635) in q, the change's transaction where it has one: data is
// the ticket as the API returns it (bin included, for ticket.deleted).
func emit(ctx context.Context, q webhooks.Querier, org, event, id string) error {
	return webhooks.Emit(ctx, q, org, event, func() (any, error) { return readTicket(ctx, q, id) })
}

func readTicket(ctx context.Context, q webhooks.Querier, id string) (boards.Ticket, error) {
	return boards.ScanTicket(q.QueryRow(ctx, `SELECT `+boards.TicketCols+` FROM tickets_all t WHERE t.id::text = $1`, id))
}

// emitMove: ticket.moved with the columns it went between (to nil: the backlog), then ticket.done
// when this move finished it.
func emitMove(ctx context.Context, q webhooks.Querier, org, id string, from, to *string, done bool) error {
	err := webhooks.Emit(ctx, q, org, "ticket.moved", func() (any, error) {
		t, err := readTicket(ctx, q, id)
		return struct {
			boards.Ticket
			FromColumnID *string `json:"from_column_id"`
			ToColumnID   *string `json:"to_column_id"`
		}{t, from, to}, err
	})
	if err != nil || !done {
		return err
	}
	return emit(ctx, q, org, "ticket.done", id)
}
