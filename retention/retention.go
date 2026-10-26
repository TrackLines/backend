// Package retention is the nightly clean-up the privacy policy promises (TL-256, TL-259): tickets in
// the bin and deleted organizations purged after 30 days, year-old attachments warned about and the
// unkept ones deleted, expired Google Chat link codes dropped, and deleted files removed from
// UploadThing a month on. It needs only Postgres and HTTP, so the small automation service runs it
// (TL-431); the backend's own /server purge and /server sweep-files run the same code.
package retention

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tracklines/backend/inbox"
)

// Retention is how long a binned ticket or a deleted organization is kept before it's purged.
const Retention = 30 * 24 * time.Hour

// Result is what a nightly purge did.
type Result struct {
	Tickets, Orgs, Warned          int
	Expired, LinkCodes, JiraTokens int64
}

func (r Result) String() string {
	return fmt.Sprintf("%d ticket(s) from the bin, %d deleted organization(s), %d attachment(s) warned about, %d expired, %d chat link code(s), %d Jira/Linear token(s)",
		r.Tickets, r.Orgs, r.Warned, r.Expired, r.LinkCodes, r.JiraTokens)
}

// Purge is the nightly purge: the ticket bin, deleted organizations, old attachments (warning, then
// deleting), expired Chat link codes and expired Jira tokens, in that order, stopping at the first failure. send delivers
// the attachment warnings (ToInbox, or the backend's notifications).
func Purge(ctx context.Context, db *pgxpool.Pool, send Send, now time.Time) (Result, error) {
	var r Result
	tk, err := PurgeBin(ctx, db, now)
	if err != nil {
		return r, fmt.Errorf("purge the ticket bin: %w", err)
	}
	r.Tickets = int(tk)
	if r.Orgs, err = PurgeOrgs(ctx, db, now); err != nil {
		return r, fmt.Errorf("purge deleted organizations: %w", err)
	}
	if r.Warned, err = WarnOld(ctx, db, send, now); err != nil {
		return r, fmt.Errorf("warn about old attachments: %w", err)
	}
	if r.Expired, err = DeleteExpired(ctx, db, now); err != nil {
		return r, fmt.Errorf("delete expired attachments: %w", err)
	}
	if r.LinkCodes, err = PurgeLinkCodes(ctx, db, now); err != nil {
		return r, fmt.Errorf("purge expired chat link codes: %w", err)
	}
	if r.JiraTokens, err = PurgeJiraTokens(ctx, db, now); err != nil {
		return r, fmt.Errorf("purge expired jira tokens: %w", err)
	}
	return r, nil
}

// PurgeBin permanently deletes tickets binned more than Retention before now. Their comments, labels,
// links and attachments go with them (cascade); their files were queued for deletion when binned.
func PurgeBin(ctx context.Context, db *pgxpool.Pool, now time.Time) (int64, error) {
	tag, err := db.Exec(ctx, `DELETE FROM tickets_all WHERE deleted_at < $1`, now.Add(-Retention))
	return tag.RowsAffected(), err
}

// OrgData deletes everything an organization owns. Deleting its projects cascades to their boards,
// tickets, comments, attachments (whose files the Sweeper then deletes from UploadThing), roadmaps,
// intake and repo links; the rest is keyed by org directly. A new org-owned table belongs here.
var OrgData = []string{
	`DELETE FROM projects WHERE owner_clerk_id = $1`,
	`DELETE FROM boards WHERE owner_clerk_id = $1`,
	`DELETE FROM roadmaps WHERE owner_clerk_id = $1`,
	`DELETE FROM board_templates WHERE org_id = $1`,
	`DELETE FROM project_repos WHERE org_id = $1`,
	`DELETE FROM org_integrations WHERE org_id = $1`,
	`DELETE FROM invitations WHERE org_id = $1`,
	`DELETE FROM notifications WHERE org_id = $1`,
	`DELETE FROM ticket_mentions WHERE org_id = $1`,
	`DELETE FROM ticket_watch_mutes WHERE org_id = $1`,
	`DELETE FROM teams WHERE org_id = $1`,
	`DELETE FROM org_admins WHERE org_id = $1`,
	`DELETE FROM api_keys WHERE org_id = $1`,
	`DELETE FROM expired_attachments WHERE org_id = $1`,
	`DELETE FROM chat_webhooks WHERE org_id = $1`,
	`DELETE FROM jira_connections WHERE org_id = $1`,
	`DELETE FROM linear_connections WHERE org_id = $1`,
	`DELETE FROM deleted_orgs WHERE org_id = $1`,
}

// PurgeOrgs permanently deletes the data of organizations deleted more than Retention before now,
// each in its own transaction.
func PurgeOrgs(ctx context.Context, db *pgxpool.Pool, now time.Time) (int, error) {
	rows, err := db.Query(ctx, `SELECT org_id FROM deleted_orgs WHERE deleted_at < $1`, now.Add(-Retention))
	if err != nil {
		return 0, err
	}
	orgs, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, err
	}
	for _, org := range orgs {
		if err := pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
			for _, q := range OrgData {
				if _, err := tx.Exec(ctx, q, org); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return 0, err
		}
	}
	return len(orgs), nil
}

// Old attachments (TL-259): a year after a file was added, or last kept, it's warned about; the org's
// admins and its uploader are told, and either can keep it (another year). Unkept, it's deleted at 13
// months, and never sooner than 28 days after the warning (a missed run mustn't cut the notice short).
// Deleting it puts its file in that day's deleted batch, gone from storage 30 days later.
const (
	WarnAfter = "1 year"
	DeleteAt  = "13 months"
)

// ClockSQL is when a file's year started: when it was added, or last kept.
const ClockSQL = `GREATEST(a.created_at, COALESCE(a.kept_at, a.created_at))`

// DeleteOnSQL is the day a warned file goes.
const DeleteOnSQL = `GREATEST(` + ClockSQL + ` + interval '` + DeleteAt + `', a.warned_at + interval '28 days')`

// Actor is who retention notifications come from: "TrackLin.es will delete …".
const Actor = "TrackLin.es"

// Send delivers one warning to people in org. A failed one is returned, and the rest still go.
type Send func(ctx context.Context, org string, to []string, n inbox.Notice) error

// ToInbox sends warnings to people's in-app inboxes.
func ToInbox(db inbox.Execer) Send {
	return func(ctx context.Context, org string, to []string, n inbox.Notice) error {
		return inbox.Add(ctx, db, org, to, n)
	}
}

// WarnOld marks files whose year is up and tells the org's admins and each file's uploader, one
// notice per ticket. Returns how many files were newly warned about; a notice that failed to send is
// in the error (the files are still marked: the warning stands, and the next run won't repeat it).
func WarnOld(ctx context.Context, db *pgxpool.Pool, send Send, now time.Time) (int, error) {
	rows, err := db.Query(ctx, `WITH due AS (
			UPDATE ticket_attachments a SET warned_at = $1
			WHERE a.warned_at IS NULL AND `+ClockSQL+` <= $1::timestamptz - interval '`+WarnAfter+`'
			RETURNING a.id, a.ticket_id, a.created_by, `+DeleteOnSQL+` AS delete_on
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
	var failed []error
	for _, w := range list {
		total += w.files
		what, it := "a year-old file", "it"
		if w.files > 1 {
			what, it = fmt.Sprintf("%d year-old files", w.files), "them"
		}
		if err := send(ctx, w.org, append(w.admins, w.uploaders...), inbox.Notice{
			Kind: "retention", Actor: Actor, TicketID: w.ticket,
			Text: fmt.Sprintf("will delete %s from %s: %s on %s unless someone keeps %s", what, w.key, w.title, w.on, it),
		}); err != nil {
			failed = append(failed, fmt.Errorf("%s: %w", w.key, err))
		}
	}
	return total, errors.Join(failed...)
}

// DeleteExpired deletes warned files nobody kept, once they're 13 months old and their notice is up.
// Each one is logged in expired_attachments for the org's file clean-up page (TL-260).
func DeleteExpired(ctx context.Context, db *pgxpool.Pool, now time.Time) (int64, error) {
	tag, err := db.Exec(ctx, `WITH gone AS (
			DELETE FROM ticket_attachments a WHERE a.warned_at IS NOT NULL AND `+DeleteOnSQL+` <= $1
			RETURNING a.ticket_id, a.name, a.size_bytes, a.created_by
		)
		INSERT INTO expired_attachments (org_id, ticket_id, ticket_key, ticket_title, name, size_bytes, created_by, deleted_at)
		SELECT p.owner_clerk_id, t.id, p.key_prefix || '-' || t.number, t.title, gone.name, gone.size_bytes, gone.created_by, $1
		FROM gone JOIN tickets_all t ON t.id = gone.ticket_id JOIN projects p ON p.id = t.project_id`, now)
	return tag.RowsAffected(), err
}

// PurgeLinkCodes deletes Google Chat link codes that expired unused (TL-289).
func PurgeLinkCodes(ctx context.Context, db *pgxpool.Pool, now time.Time) (int64, error) {
	tag, err := db.Exec(ctx, `DELETE FROM chat_link_codes WHERE expires_at < $1`, now)
	return tag.RowsAffected(), err
}

// PurgeJiraTokens deletes Jira and Linear connections whose 30 days are up (TL-537, TL-654); reading
// one deletes it too.
func PurgeJiraTokens(ctx context.Context, db *pgxpool.Pool, now time.Time) (int64, error) {
	var n int64
	err := db.QueryRow(ctx, `WITH j AS (DELETE FROM jira_connections WHERE expires_at <= $1 RETURNING 1),
		l AS (DELETE FROM linear_connections WHERE expires_at <= $1 RETURNING 1)
		SELECT (SELECT count(*) FROM j) + (SELECT count(*) FROM l)`, now).Scan(&n)
	return n, err
}

// FileDeleter is UploadThing's delete API (a fake in tests).
type FileDeleter interface {
	DeleteFiles(ctx context.Context, keys ...string) error
}

// Sweeper deletes files from UploadThing once their attachment is gone, however it went: removed by
// hand, or with its ticket, project or organization. A database trigger queues each file key in
// deleted_files, so no delete path can forget its files (TL-206).
type Sweeper struct {
	DB    *pgxpool.Pool
	Files FileDeleter
}

// KeepDays is how long a deleted file waits in its day's batch before it's deleted from storage, so a
// mistake can still be undone (TL-256): the batch dated D goes on day D+30.
const KeepDays = 30

// Sweep deletes up to 100 queued files whose day's batch is due; a failed batch stays queued and is
// retried next time.
func (s Sweeper) Sweep(ctx context.Context) (int, error) {
	rows, err := s.DB.Query(ctx, `SELECT file_key FROM deleted_files WHERE deleted_on <= current_date - $1::int
		ORDER BY deleted_on, deleted_at LIMIT 100`, KeepDays)
	if err != nil {
		return 0, err
	}
	keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || len(keys) == 0 {
		return 0, err
	}
	if err := s.Files.DeleteFiles(ctx, keys...); err != nil {
		_, _ = s.DB.Exec(ctx, `UPDATE deleted_files SET attempts = attempts + 1 WHERE file_key = ANY($1)`, keys)
		return 0, err
	}
	_, err = s.DB.Exec(ctx, `DELETE FROM deleted_files WHERE file_key = ANY($1)`, keys)
	return len(keys), err
}

// SweepAll deletes every due file, a batch of 100 at a time, until none are left or one fails.
func (s Sweeper) SweepAll(ctx context.Context) (int, error) {
	total := 0
	for {
		n, err := s.Sweep(ctx)
		total += n
		if err != nil || n == 0 {
			return total, err
		}
	}
}
