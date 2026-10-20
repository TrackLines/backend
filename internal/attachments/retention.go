package attachments

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tracklines/backend/inbox"
	"github.com/tracklines/backend/internal/notifications"
	"github.com/tracklines/backend/retention"
)

// Old attachments (TL-259): warned about a year after they were added or last kept, deleted at 13
// months unless someone keeps them. The rules live in package retention, which the automation service
// runs nightly (TL-431); these are the backend's names for them.
const (
	WarnAfter   = retention.WarnAfter
	DeleteAt    = retention.DeleteAt
	Actor       = retention.Actor
	deleteOnSQL = retention.DeleteOnSQL
)

// WarnOld marks files whose year is up and tells the org's admins and each file's uploader through n,
// one notification per ticket. Returns how many files were newly warned about.
func WarnOld(ctx context.Context, db *pgxpool.Pool, n notifications.Notifier, now time.Time) (int, error) {
	return retention.WarnOld(ctx, db, func(ctx context.Context, org string, to []string, w inbox.Notice) error {
		notifications.Send(ctx, n, org, to, notifications.Event{Kind: w.Kind, Actor: w.Actor, TicketID: w.TicketID, Text: w.Text})
		return nil // Send logs a failure itself
	}, now)
}

// DeleteExpired deletes warned files nobody kept (retention.DeleteExpired).
func DeleteExpired(ctx context.Context, db *pgxpool.Pool, now time.Time) (int64, error) {
	return retention.DeleteExpired(ctx, db, now)
}

// DueFile is a warned file: deleted on DeleteOn unless someone keeps it.
type DueFile struct {
	ID          string    `json:"id"`
	TicketID    string    `json:"ticket_id"`
	TicketKey   string    `json:"ticket_key"`
	TicketTitle string    `json:"ticket_title"`
	Name        string    `json:"name"`
	Size        int64     `json:"size"`
	CreatedBy   string    `json:"created_by"`
	DeleteOn    time.Time `json:"delete_on"`
}

// GoneFile is a file the clean-up deleted.
type GoneFile struct {
	TicketID    string    `json:"ticket_id"`
	TicketKey   string    `json:"ticket_key"`
	TicketTitle string    `json:"ticket_title"`
	Name        string    `json:"name"`
	Size        int64     `json:"size"`
	CreatedBy   string    `json:"created_by"`
	DeletedAt   time.Time `json:"deleted_at"`
}

// Retention is an org's file clean-up: what's due to go (soonest first) and what has gone (newest first).
type Retention struct {
	Due     []DueFile  `json:"due"`
	Deleted []GoneFile `json:"deleted"`
}

// ponytail: the deleted list is capped, page it if an org ever needs more than the latest 500.
const goneShown = 500

// Retention lists org's file clean-up. Files on binned tickets are left out: they go with the ticket.
func (s Store) Retention(ctx context.Context, org string) (Retention, error) {
	rows, err := s.DB.Query(ctx, `SELECT a.id, t.id, p.key_prefix || '-' || t.number, t.title, a.name, a.size_bytes, a.created_by, `+deleteOnSQL+`
		FROM ticket_attachments a JOIN tickets t ON t.id = a.ticket_id JOIN projects p ON p.id = t.project_id
		WHERE p.owner_clerk_id = $1 AND a.warned_at IS NOT NULL ORDER BY 8, t.number, a.name`, org)
	if err != nil {
		return Retention{}, err
	}
	due, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (DueFile, error) {
		var f DueFile
		return f, r.Scan(&f.ID, &f.TicketID, &f.TicketKey, &f.TicketTitle, &f.Name, &f.Size, &f.CreatedBy, &f.DeleteOn)
	})
	if err != nil {
		return Retention{}, err
	}
	rows, err = s.DB.Query(ctx, `SELECT ticket_id, ticket_key, ticket_title, name, size_bytes, created_by, deleted_at
		FROM expired_attachments WHERE org_id = $1 ORDER BY deleted_at DESC, name LIMIT $2`, org, goneShown)
	if err != nil {
		return Retention{}, err
	}
	gone, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (GoneFile, error) {
		var f GoneFile
		return f, r.Scan(&f.TicketID, &f.TicketKey, &f.TicketTitle, &f.Name, &f.Size, &f.CreatedBy, &f.DeletedAt)
	})
	return Retention{Due: due, Deleted: gone}, err
}

// Keep holds on to a file for another year: its clock restarts and any warning is cleared. Only the
// org's admins or whoever uploaded it may (canKeep decides, given the uploader).
func (s Store) Keep(ctx context.Context, owner, id string, canKeep func(uploader string) bool) error {
	var uploader string
	if err := s.DB.QueryRow(ctx, `SELECT a.created_by FROM ticket_attachments a JOIN tickets t ON t.id = a.ticket_id
		JOIN projects p ON p.id = t.project_id WHERE a.id = $1 AND p.owner_clerk_id = $2`, id, owner).Scan(&uploader); err != nil {
		return notFound(err)
	}
	if !canKeep(uploader) {
		return ErrCantKeep
	}
	_, err := s.DB.Exec(ctx, `UPDATE ticket_attachments SET kept_at = now(), warned_at = NULL WHERE id = $1`, id)
	return err
}
