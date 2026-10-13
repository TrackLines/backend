package sprints

import (
	"context"
	"errors"
	"github.com/clerk/clerk-sdk-go/v2"
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
		INSERT INTO org_admins (org_id, user_clerk_id, granted_by) VALUES ('an1', 'user_lead', 'test'), ('an1', 'user_planner', 'test')`); err != nil {
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
	as := func(user string) context.Context {
		c := &clerk.SessionClaims{RegisteredClaims: clerk.RegisteredClaims{Subject: user}}
		c.ActiveOrganizationID = "an1"
		return clerk.ContextWithSessionClaims(ctx, c)
	}
	planAs := func(who context.Context, ticket, body string) int {
		req := httptest.NewRequest("PUT", "/", strings.NewReader(body)).WithContext(who)
		req.SetPathValue("id", ticket)
		rec := httptest.NewRecorder()
		h.PlanTicket(rec, req)
		return rec.Code
	}
	planOne := func(estimate string) {
		tk, _ := ts.CreateBacklog(ctx, "an1", pid, "task", "work", "")
		if code := planAs(as("user_planner"), tk.ID, `{"planned_sprint_id":"`+plan.ID+`","estimate":"`+estimate+`"}`); code != 204 {
			t.Fatalf("plan: %d", code)
		}
	}

	// planning is for org admins and the board's team leaders, not any member or an API key (TL-241)
	tk, _ := ts.CreateBacklog(ctx, "an1", pid, "task", "access", "")
	if code := planAs(as("user_member"), tk.ID, `{"planned_sprint_id":"`+plan.ID+`","estimate":"1"}`); code != 403 {
		t.Fatalf("a plain member planned a ticket: %d", code)
	}
	if code := planAs(auth.WithAPIKeyUser(ctx, "an1", "Claude"), tk.ID, `{"planned_sprint_id":"`+plan.ID+`","estimate":"1"}`); code != 403 {
		t.Fatalf("an API key planned a ticket: %d", code)
	}
	if code := planAs(as("user_lead"), tk.ID, `{"planned_sprint_id":"`+plan.ID+`","estimate":"1"}`); code != 204 {
		t.Fatalf("admin plan: %d", code)
	}
	if code := planAs(as("user_member"), tk.ID, `{"planned_sprint_id":null}`); code != 403 {
		t.Fatalf("a plain member took a ticket out of a planned sprint: %d", code)
	}
	if code := planAs(as("user_lead"), tk.ID, `{"planned_sprint_id":null}`); code != 204 {
		t.Fatalf("admin unplan: %d", code)
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
