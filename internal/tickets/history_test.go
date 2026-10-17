package tickets

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgconn"
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

// TL-74: the history table only takes well-formed entries, can't be edited or deleted, goes when the
// ticket is purged, and reads back in the order things happened, for the ticket's organization only.
func TestHistoryModel(t *testing.T) {
	ctx, db := historyDB(t)
	if _, err := db.Exec(ctx, `DELETE FROM projects WHERE owner_clerk_id = 'hm1'`); err != nil {
		t.Fatal(err)
	}
	var pid, tid string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('hm1', 'p') RETURNING id`).Scan(&pid)
	_ = db.QueryRow(ctx, `INSERT INTO tickets (project_id, title, position, created_by) VALUES ($1, 't', 0, 'u') RETURNING id`, pid).Scan(&tid)
	s := Store{DB: db}
	before, err := s.History(ctx, "hm1", tid)
	if err != nil {
		t.Fatal(err)
	}
	add := func(sql string, args ...any) error {
		_, err := db.Exec(ctx, `INSERT INTO ticket_history (ticket_id, project_id, stage, outcome, stage_order, column_id, column_name, board_name, resolution, actor) `+sql, append([]any{tid, pid}, args...)...)
		return err
	}
	// stage rules: a column stage names its column; resolved has a resolution and is never skipped
	for name, sql := range map[string]string{
		"unknown stage":           `VALUES ($1, $2, 'limbo', 'entered', 0, NULL, NULL, NULL, NULL, NULL)`,
		"unknown outcome":         `VALUES ($1, $2, 'backlog', 'maybe', 0, NULL, NULL, NULL, NULL, NULL)`,
		"column without a column": `VALUES ($1, $2, 'column', 'entered', 1, NULL, 'To do', 'B', NULL, NULL)`,
		"backlog with a column":   `VALUES ($1, $2, 'backlog', 'entered', 0, gen_random_uuid(), 'x', 'B', NULL, NULL)`,
		"resolved without how":    `VALUES ($1, $2, 'resolved', 'entered', 9, NULL, NULL, NULL, NULL, NULL)`,
		"resolved skipped":        `VALUES ($1, $2, 'resolved', 'skipped', 9, NULL, NULL, NULL, 'done', NULL)`,
	} {
		if err := add(sql); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	for _, sql := range []string{
		`VALUES ($1, $2, 'backlog', 'skipped', 0, NULL, NULL, NULL, NULL, 'u')`,
		`VALUES ($1, $2, 'column', 'entered', 1, gen_random_uuid(), 'To do', 'Backend', NULL, 'u')`,
		`VALUES ($1, $2, 'resolved', 'entered', 1000000, NULL, NULL, NULL, 'wont_do', NULL)`,
	} {
		if err := add(sql); err != nil {
			t.Fatal(err)
		}
	}
	h, err := s.History(ctx, "hm1", tid)
	if err != nil || len(h) != len(before)+3 {
		t.Fatalf("history: %d entries (%v)", len(h), err)
	}
	got := h[len(before):]
	if got[0].Stage != "backlog" || got[0].Outcome != "skipped" || got[1].Stage != "column" || *got[1].ColumnName != "To do" ||
		got[2].Stage != "resolved" || *got[2].Resolution != "wont_do" || got[2].Actor != nil || got[0].At == "" {
		t.Fatalf("in order: %+v", got)
	}
	if _, err := s.History(ctx, "someone-else", tid); !errors.Is(err, ErrNotFound) {
		t.Fatal("another organization read it:", err)
	}

	// append-only: no edits, no deletes
	var pg *pgconn.PgError
	if _, err := db.Exec(ctx, `UPDATE ticket_history SET actor = 'mallory' WHERE ticket_id = $1`, tid); !errors.As(err, &pg) || pg.Code != "23001" {
		t.Fatalf("edited: %v", err)
	}
	if _, err := db.Exec(ctx, `DELETE FROM ticket_history WHERE ticket_id = $1`, tid); !errors.As(err, &pg) || pg.Code != "23001" {
		t.Fatalf("deleted: %v", err)
	}
	// binned tickets aren't readable; purged ones take their history with them
	_, _ = db.Exec(ctx, `UPDATE tickets_all SET deleted_at = now() WHERE id = $1`, tid)
	if _, err := s.History(ctx, "hm1", tid); !errors.Is(err, ErrNotFound) {
		t.Fatal("binned ticket's history read:", err)
	}
	if _, err := db.Exec(ctx, `DELETE FROM tickets_all WHERE id = $1`, tid); err != nil {
		t.Fatal("purge:", err)
	}
	var left int
	_ = db.QueryRow(ctx, `SELECT count(*) FROM ticket_history WHERE ticket_id = $1`, tid).Scan(&left)
	if left != 0 {
		t.Fatalf("%d entries outlived the ticket", left)
	}
}
