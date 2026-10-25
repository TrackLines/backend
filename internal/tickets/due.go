package tickets

import (
	"context"
	"time"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/jackc/pgx/v5"
	"github.com/tracklines/backend/internal/boards"
	"github.com/tracklines/backend/internal/notifications"
)

// DueReminders tells people about due dates (TL-631): the day before and on the day, the assignee
// and the ticket's watchers, once each (due_reminders), for open tickets only.
// ponytail: "the day" is UTC's, not each person's; move to per-person zones if reminders land at odd hours.
type DueReminders struct {
	Store  Store
	Notify notifications.Notifier
}

type dueTicket struct {
	id, org, key, title string
	assignee, board     *string
}

// Send sends whatever is due on now's day and the next, and says how many reminders went out.
func (d DueReminders) Send(ctx context.Context, now time.Time) (int, error) {
	today := now.UTC().Format(time.DateOnly)
	sent := 0
	for _, kind := range []string{"today", "tomorrow"} {
		// claimed in the same statement that finds them, so two instances never both send one
		rows, err := d.Store.DB.Query(ctx, `WITH due AS (
				SELECT t.id, p.owner_clerk_id AS org, p.key_prefix || '-' || t.number AS key, t.title, t.assigned_to, t.board_id
				FROM tickets t JOIN projects p ON p.id = t.project_id
				WHERE t.due_date = $1::date + CASE WHEN $2 = 'tomorrow' THEN 1 ELSE 0 END AND NOT `+boards.TicketDoneSQL+`),
			claimed AS (
				INSERT INTO due_reminders (ticket_id, due_date, kind)
				SELECT id, $1::date + CASE WHEN $2 = 'tomorrow' THEN 1 ELSE 0 END, $2 FROM due
				ON CONFLICT DO NOTHING RETURNING ticket_id)
			SELECT due.id, due.org, due.key, due.title, due.assigned_to, due.board_id::text FROM due JOIN claimed ON claimed.ticket_id = due.id`, today, kind)
		if err != nil {
			return sent, err
		}
		due, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (dueTicket, error) {
			var t dueTicket
			return t, row.Scan(&t.id, &t.org, &t.key, &t.title, &t.assignee, &t.board)
		})
		if err != nil {
			return sent, err
		}
		for _, t := range due {
			text := t.key + ": " + t.title + " is due today"
			if kind == "tomorrow" {
				text = t.key + ": " + t.title + " is due tomorrow"
			}
			e := notifications.Event{Kind: "due", Text: text, TicketID: t.id}
			if t.board != nil {
				e.BoardID = *t.board
			}
			var told []string
			if t.assignee != nil {
				told = []string{*t.assignee}
				notifications.Send(ctx, d.Notify, t.org, told, e)
			}
			notifications.Watch(ctx, d.Notify, d.Store.DB, t.org, t.id, told, e)
			sent++
		}
	}
	return sent, nil
}

// Run sends reminders every interval until ctx is cancelled.
func (d DueReminders) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if n, err := d.Send(ctx, time.Now()); err != nil && ctx.Err() == nil {
			logs.Errorf("tickets: due reminders: %v", err)
		} else if n > 0 {
			logs.Logf("tickets: sent %d due-date reminder(s)", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
