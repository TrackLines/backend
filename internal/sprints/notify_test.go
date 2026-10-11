package sprints

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/boards"
	"github.com/tracklines/backend/internal/notifications"
	"github.com/tracklines/backend/internal/organizations"
	"github.com/tracklines/backend/internal/tickets"
)

// Planning a ticket that tips a planned sprint over capacity tells the board's managers, once.
func TestApprovalNotification(t *testing.T) {
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
	defer db.Close()
	if _, err := db.Exec(ctx, `DELETE FROM notifications WHERE org_id = 'an1'; DELETE FROM org_admins WHERE org_id = 'an1';
		DELETE FROM boards WHERE owner_clerk_id = 'an1'; DELETE FROM projects WHERE owner_clerk_id = 'an1';
		INSERT INTO org_admins (org_id, user_clerk_id, granted_by) VALUES ('an1', 'user_lead', 'test')`); err != nil {
		t.Fatal(err)
	}
	var pid string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('an1', 'p') RETURNING id`).Scan(&pid)
	b, _ := boards.Store{DB: db}.Create(ctx, "an1", pid, "Web", "")
	_, _ = db.Exec(ctx, `UPDATE boards SET estimate_scale = 'fibonacci' WHERE id = $1`, b.ID)
	s := Store{DB: db}
	if _, err := s.Start(ctx, "an1", b.ID, 14); err != nil {
		t.Fatal(err)
	}
	plans, err := s.Plan(ctx, "an1", b.ID, 14, "")
	if err != nil {
		t.Fatal(err)
	}
	plan := plans[0]
	// velocity 8 (cap 8.8) from one closed sprint that finished 8 points
	var closed string
	_ = db.QueryRow(ctx, `INSERT INTO sprints (board_id, number, length_days, starts_at, ends_at, closed_at)
		VALUES ($1, -1, 14, now() - interval '60 days', now() - interval '50 days', now() - interval '50 days') RETURNING id`, b.ID).Scan(&closed)
	_, _ = db.Exec(ctx, `INSERT INTO tickets (project_id, board_id, column_id, sprint_id, type, title, position, created_by, estimate)
		VALUES ($1, $2, $3, $4, 'task', 'done', 0, 'an1', '8')`, pid, b.ID, b.Columns[2].ID, closed)

	h := NewSystem(db, organizations.Admins{DB: db}).WithNotifier(notifications.Store{DB: db})
	ts := tickets.Store{DB: db}
	planOne := func(estimate string) {
		tk, _ := ts.CreateBacklog(ctx, "an1", pid, "task", "work", "")
		req := httptest.NewRequest("PUT", "/", strings.NewReader(`{"planned_sprint_id":"`+plan.ID+`","estimate":"`+estimate+`"}`)).WithContext(auth.WithAPIKeyUser(ctx, "an1", "Claude"))
		req.SetPathValue("id", tk.ID)
		rec := httptest.NewRecorder()
		h.PlanTicket(rec, req)
		if rec.Code != 204 {
			t.Fatalf("plan: %d %s", rec.Code, rec.Body)
		}
	}
	told := func() int {
		var n int
		_ = db.QueryRow(ctx, `SELECT count(*) FROM notifications WHERE org_id = 'an1' AND user_id = 'user_lead' AND kind = 'approval'`).Scan(&n)
		return n
	}
	planOne("5") // 5 of 8.8
	if told() != 0 {
		t.Fatal("told while within capacity")
	}
	planOne("5") // 10: over
	if told() != 1 {
		t.Fatalf("over capacity: told %d", told())
	}
	planOne("1") // still over: not again
	if told() != 1 {
		t.Fatalf("told again: %d", told())
	}
	var text string
	_ = db.QueryRow(ctx, `SELECT text FROM notifications WHERE org_id = 'an1' AND kind = 'approval'`).Scan(&text)
	if !strings.Contains(text, "on Web over capacity") {
		t.Fatalf("text: %q", text)
	}
}
