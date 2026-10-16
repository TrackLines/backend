package attachments

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/notifications"
)

// Old attachments (TL-259): a year after a file was added, or last kept, it's warned about; the org's
// admins and its uploader are told, and either can keep it (another year). Unkept, it's deleted at 13
// months, and never sooner than NoticeDays after the warning (a missed run mustn't cut the notice
// short: 28 days). Deleting it puts its file in that day's deleted batch, gone from storage 30 days later.
const (
	WarnAfter = "1 year"
	DeleteAt  = "13 months"
)

// clockSQL is when a file's year started: when it was added, or last kept.
const clockSQL = `GREATEST(a.created_at, COALESCE(a.kept_at, a.created_at))`

// deleteOnSQL is the day a warned file goes.
const deleteOnSQL = `GREATEST(` + clockSQL + ` + interval '` + DeleteAt + `', a.warned_at + interval '28 days')`

// Actor is who retention notifications come from: "Tracklines will delete …".
const Actor = "TrackLin.es"

// WarnOld marks files whose year is up and tells the org's admins and each file's uploader, one
// notification per ticket. Returns how many files were newly warned about.
func WarnOld(ctx context.Context, db *pgxpool.Pool, n notifications.Notifier, now time.Time) (int, error) {
	rows, err := db.Query(ctx, `WITH due AS (
			UPDATE ticket_attachments a SET warned_at = $1
			WHERE a.warned_at IS NULL AND `+clockSQL+` <= $1::timestamptz - interval '`+WarnAfter+`'
			RETURNING a.id, a.ticket_id, a.created_by, `+deleteOnSQL+` AS delete_on
		)
		SELECT p.owner_clerk_id, t.id, p.key_prefix || '-' || t.number, t.title, count(*), array_agg(DISTINCT due.created_by),
			ARRAY(SELECT user_clerk_id FROM org_admins WHERE org_id = p.owner_clerk_id),
			to_char(min(due.delete_on) AT TIME ZONE 'UTC', 'FMDD Mon YYYY')
		FROM due JOIN tickets t ON t.id = due.ticket_id JOIN projects p ON p.id = t.project_id
		GROUP BY p.owner_clerk_id, t.id, p.key_prefix, t.number, t.title`, now)
	if err != nil {
		return 0, err
	}
	type warning struct {
		org, ticket, key, title, on string
		files                       int
		uploaders, admins           []string
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (warning, error) {
		var w warning
		return w, r.Scan(&w.org, &w.ticket, &w.key, &w.title, &w.files, &w.uploaders, &w.admins, &w.on)
	})
	if err != nil {
		return 0, err
	}
	total := 0
	for _, w := range list {
		total += w.files
		what, it := "a year-old file", "it"
		if w.files > 1 {
			what, it = fmt.Sprintf("%d year-old files", w.files), "them"
		}
		notifications.Send(ctx, n, w.org, append(w.admins, w.uploaders...), notifications.Event{
			Kind: "retention", Actor: Actor, TicketID: w.ticket,
			Text: fmt.Sprintf("will delete %s from %s: %s on %s unless someone keeps %s", what, w.key, w.title, w.on, it),
		})
	}
	return total, nil
}

// DeleteExpired deletes warned files nobody kept, once they're 13 months old and their notice is up.
// Each one is logged in expired_attachments for the org's file clean-up page (TL-260).
func DeleteExpired(ctx context.Context, db *pgxpool.Pool, now time.Time) (int64, error) {
	tag, err := db.Exec(ctx, `WITH gone AS (
			DELETE FROM ticket_attachments a WHERE a.warned_at IS NOT NULL AND `+deleteOnSQL+` <= $1
			RETURNING a.ticket_id, a.name, a.size_bytes, a.created_by
		)
		INSERT INTO expired_attachments (org_id, ticket_id, ticket_key, ticket_title, name, size_bytes, created_by, deleted_at)
		SELECT p.owner_clerk_id, t.id, p.key_prefix || '-' || t.number, t.title, gone.name, gone.size_bytes, gone.created_by, $1
		FROM gone JOIN tickets_all t ON t.id = gone.ticket_id JOIN projects p ON p.id = t.project_id`, now)
	return tag.RowsAffected(), err
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
