package tickets

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
)

func historyDB(t *testing.T) (context.Context, *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	url := os.Getenv("TEST_DATABASE_URL")
	m, err := migrate.New("file://../migrations", "pgx5"+url[len("postgres"):])
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatal(err)
	}
	db, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	return ctx, db
}

// TL-668: the backend keeps no history table of its own. A ticket's history is what's queued for the
// auditor (plus what the auditor holds), in the order things happened, for the ticket's organization only.
func TestHistoryModel(t *testing.T) {
	ctx, db := historyDB(t)
	if _, err := db.Exec(ctx, `DELETE FROM projects WHERE owner_clerk_id = 'hm1'`); err != nil {
		t.Fatal(err)
	}
	var table *string
	if err := db.QueryRow(ctx, `SELECT to_regclass('ticket_history')::text`).Scan(&table); err != nil || table != nil {
		t.Fatalf("ticket_history should be gone: %v %v", table, err)
	}
	var pid, tid string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('hm1', 'p') RETURNING id`).Scan(&pid)
	_ = db.QueryRow(ctx, `INSERT INTO tickets (project_id, title, position, created_by) VALUES ($1, 't', 0, 'u') RETURNING id`, pid).Scan(&tid)
	if _, err := db.Exec(ctx, `UPDATE tickets SET resolution = 'wont_do', resolved_at = now() WHERE id = $1`, tid); err != nil {
		t.Fatal(err)
	}
	s := Store{DB: db}
	h, err := s.History(ctx, "hm1", tid, nil)
	if err != nil || len(h) != 2 {
		t.Fatalf("history: %d entries (%v)", len(h), err)
	}
	if h[0].Stage != "backlog" || h[0].Outcome != "entered" || h[0].Actor == nil || *h[0].Actor != "u" || h[0].At == "" || h[0].BoardID != nil ||
		h[1].Stage != "resolved" || *h[1].Resolution != "wont_do" || h[1].Actor != nil || h[1].StageOrder != 1000000 {
		t.Fatalf("in order: %+v", h)
	}
	if _, err := s.History(ctx, "someone-else", tid, nil); !errors.Is(err, ErrNotFound) {
		t.Fatal("another organization read it:", err)
	}
	// binned tickets aren't readable; a purged one tells the auditor to delete its history
	_, _ = db.Exec(ctx, `UPDATE tickets_all SET deleted_at = now() WHERE id = $1`, tid)
	if _, err := s.History(ctx, "hm1", tid, nil); !errors.Is(err, ErrNotFound) {
		t.Fatal("binned ticket's history read:", err)
	}
	if _, err := db.Exec(ctx, `DELETE FROM tickets_all WHERE id = $1`, tid); err != nil {
		t.Fatal("purge:", err)
	}
	var purged int
	_ = db.QueryRow(ctx, `SELECT count(*) FROM audit_outbox WHERE target_id = $1 AND action = 'ticket.purged'`, tid).Scan(&purged)
	if purged != 1 {
		t.Fatalf("%d ticket.purged events queued, want 1", purged)
	}
}
