package imports

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TL-668: dropping ticket_history changes how a ticket's import fingerprint is made. Migration 118 moves
// tickets still as they were imported onto the new fingerprint, so their import can still be scrapped,
// and leaves one that's been changed since reading as changed.
func TestDropTicketHistoryKeepsImportsScrappable(t *testing.T) {
	ctx := context.Background()
	url := os.Getenv("TEST_DATABASE_URL")
	m, err := migrate.New("file://../migrations", "pgx5"+url[len("postgres"):])
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { // whatever happens, the next test gets the current schema
		if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
			t.Error(err)
		}
	})
	if err := m.Migrate(117); err != nil && !errors.Is(err, migrate.ErrNoChange) { // ticket_history is back (118's down)
		t.Fatal(err)
	}
	db, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(ctx, `DELETE FROM imports WHERE org_id = 'fm1'; DELETE FROM projects WHERE owner_clerk_id = 'fm1'`); err != nil {
		t.Fatal(err)
	}
	var project, kept, changed, imp string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('fm1', 'p') RETURNING id`).Scan(&project)
	_ = db.QueryRow(ctx, `INSERT INTO tickets (project_id, title, position, created_by) VALUES ($1, 'kept', 0, 'u') RETURNING id`, project).Scan(&kept)
	_ = db.QueryRow(ctx, `INSERT INTO tickets (project_id, title, position, created_by) VALUES ($1, 'changed', 1, 'u') RETURNING id`, project).Scan(&changed)
	if err := db.QueryRow(ctx, `INSERT INTO imports (org_id, source, state, plan, created_by) VALUES ('fm1', 'jira', 'done', '{}', 'u') RETURNING id`).Scan(&imp); err != nil {
		t.Fatal(err)
	}
	// the fingerprint as it was made before the cutover: it counted the ticket's history rows
	if _, err := db.Exec(ctx, `INSERT INTO import_items (import_id, kind, source_id, target_id, fingerprint)
		SELECT $1, 'ticket', t.title, t.id, md5(row_to_json(t)::text
			|| coalesce((SELECT string_agg(lower(l.label), ',' ORDER BY lower(l.label)) FROM ticket_labels l WHERE l.ticket_id = t.id), '')
			|| coalesce((SELECT string_agg(c.id::text || md5(c.body), ',' ORDER BY c.id) FROM ticket_comments c WHERE c.ticket_id = t.id), '')
			|| (SELECT count(*) FROM ticket_attachments a WHERE a.ticket_id = t.id)
			|| '/' || (SELECT count(*) FROM ticket_history h WHERE h.ticket_id = t.id)
			|| '/' || (SELECT count(*) FROM import_reviews r WHERE r.ticket_id = t.id))
		FROM tickets_all t WHERE t.project_id = $2`, imp, project); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `UPDATE tickets SET title = 'someone edited this' WHERE id = $1`, changed); err != nil {
		t.Fatal(err)
	}

	if err := m.Up(); err != nil {
		t.Fatal(err)
	}
	asImported := func(ticket string) (same bool) {
		t.Helper()
		if err := db.QueryRow(ctx, `SELECT i.fingerprint IS NOT DISTINCT FROM `+fingerprint+`
			FROM import_items i JOIN tickets_all t ON t.id = i.target_id WHERE i.import_id = $1 AND t.id = $2`, imp, ticket).Scan(&same); err != nil {
			t.Fatal(err)
		}
		return same
	}
	if !asImported(kept) {
		t.Error("an untouched imported ticket reads as changed after the migration: its import can't be scrapped")
	}
	if asImported(changed) {
		t.Error("a ticket edited since its import reads as untouched after the migration")
	}
}
