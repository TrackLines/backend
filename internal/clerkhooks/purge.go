package clerkhooks

import (
	"context"
	"time"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Retention is how long a deleted organization's data is kept before it's purged.
const Retention = 30 * 24 * time.Hour

// orgData deletes everything an organization owns. Deleting its projects cascades to their boards,
// tickets, comments, attachments (whose files the attachments sweeper then deletes from UploadThing),
// roadmaps, intake and repo links; the rest is keyed by org directly.
var orgData = []string{
	`DELETE FROM projects WHERE owner_clerk_id = $1`,
	`DELETE FROM boards WHERE owner_clerk_id = $1`,
	`DELETE FROM roadmaps WHERE owner_clerk_id = $1`,
	`DELETE FROM board_templates WHERE org_id = $1`,
	`DELETE FROM project_repos WHERE org_id = $1`,
	`DELETE FROM org_integrations WHERE org_id = $1`,
	`DELETE FROM invitations WHERE org_id = $1`,
	`DELETE FROM notifications WHERE org_id = $1`,
	`DELETE FROM ticket_mentions WHERE org_id = $1`,
	`DELETE FROM teams WHERE org_id = $1`,
	`DELETE FROM org_admins WHERE org_id = $1`,
	`DELETE FROM api_keys WHERE org_id = $1`,
	`DELETE FROM deleted_orgs WHERE org_id = $1`,
}

// Purge permanently deletes the data of organizations deleted more than Retention before now.
func Purge(ctx context.Context, db *pgxpool.Pool, now time.Time) (int, error) {
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
			for _, q := range orgData {
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

// RunPurge purges every interval until ctx ends.
func RunPurge(ctx context.Context, db *pgxpool.Pool, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if n, err := Purge(ctx, db, time.Now()); err != nil && ctx.Err() == nil {
			logs.Errorf("clerkhooks: purge deleted organizations: %v", err)
		} else if n > 0 {
			logs.Logf("clerkhooks: purged %d deleted organization(s)", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
