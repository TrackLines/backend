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
	"github.com/tracklines/backend/internal/organizations"
	"github.com/tracklines/backend/internal/tickets"
)

// TL-838: a service key plans a backlog ticket into a planned sprint, with its estimate, as the admin who
// made it (retro-board's poker). Agent and personal keys still can't; nor can a key whose maker lost admin.
func TestServiceKeyPlansTickets(t *testing.T) {
	ctx := context.Background()
	url := os.Getenv("TEST_DATABASE_URL")
	m, err := migrate.New("file://../migrations", "pgx5"+url[len("postgres"):])
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	boards.ActorHook(cfg) // as the server does: history names the caller
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(ctx, `DELETE FROM org_admins WHERE org_id IN ('sk1', 'sk2');
		DELETE FROM boards WHERE owner_clerk_id IN ('sk1', 'sk2'); DELETE FROM projects WHERE owner_clerk_id IN ('sk1', 'sk2');
		INSERT INTO org_admins (org_id, user_clerk_id, granted_by) VALUES ('sk1', 'user_admin', 'test'), ('sk2', 'user_other', 'test')`); err != nil {
		t.Fatal(err)
	}
	var pid string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('sk1', 'p') RETURNING id`).Scan(&pid)
	b, _ := boards.Store{DB: db}.Create(ctx, "sk1", pid, "Web", "")
	_, _ = db.Exec(ctx, `UPDATE boards SET estimate_scale = 'fibonacci' WHERE id = $1`, b.ID)
	s := Store{DB: db}
	if _, err := s.Start(ctx, "sk1", b.ID, 14); err != nil {
		t.Fatal(err)
	}
	plans, err := s.Plan(ctx, "sk1", b.ID, 14, "")
	if err != nil {
		t.Fatal(err)
	}
	plan := plans[0].ID

	h := NewSystem(db, organizations.Admins{DB: db})
	ts := tickets.Store{DB: db}
	key := func(owner, org, kind string) context.Context {
		return auth.WithAPIKey(ctx, owner, org, "retro-board", kind)
	}
	planAs := func(who context.Context, ticket, body string) int {
		req := httptest.NewRequest("PUT", "/", strings.NewReader(body)).WithContext(who)
		req.SetPathValue("id", ticket)
		rec := httptest.NewRecorder()
		h.PlanTicket(rec, req)
		return rec.Code
	}
	body := func(estimate string) string {
		return `{"planned_sprint_id":"` + plan + `","estimate":"` + estimate + `"}`
	}

	// the future sprints to choose from: a service key can list them
	req := httptest.NewRequest("GET", "/", nil).WithContext(key("user_admin", "sk1", "service"))
	req.SetPathValue("id", b.ID)
	rec := httptest.NewRecorder()
	h.ListPlanned(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), plan) {
		t.Fatalf("list planned with a service key: %d %s", rec.Code, rec.Body)
	}

	tk, _ := ts.CreateBacklog(ctx, "sk1", pid, "task", "from poker", "")
	for name, who := range map[string]context.Context{
		"an agent key made by an admin":            key("user_admin", "sk1", "ai"),
		"a personal key made by an admin":          key("user_admin", "sk1", "personal"),
		"a service key whose maker isn't an admin": key("user_member", "sk1", "service"),
	} {
		if code := planAs(who, tk.ID, body("5")); code != 403 {
			t.Fatalf("%s planned a ticket: %d", name, code)
		}
	}
	if code := planAs(key("user_other", "sk2", "service"), tk.ID, body("5")); code != 404 {
		t.Fatalf("another org's service key: %d", code)
	}
	if code := planAs(key("user_admin", "sk1", "service"), tk.ID, body("4")); code != 400 {
		t.Fatalf("off-scale estimate: %d", code)
	}

	if code := planAs(key("user_admin", "sk1", "service"), tk.ID, body("5")); code != 204 {
		t.Fatalf("service key plan: %d", code)
	}
	var planned, estimate, kind string
	_ = db.QueryRow(ctx, `SELECT planned_sprint_id::text, estimate FROM tickets WHERE id = $1`, tk.ID).Scan(&planned, &estimate)
	if planned != plan || estimate != "5" {
		t.Fatalf("after planning: sprint %q estimate %q", planned, estimate)
	}
	_ = db.QueryRow(ctx, `SELECT actor_kind FROM audit_outbox WHERE target_id = $1 AND action = 'ticket.planned_changed' ORDER BY id DESC LIMIT 1`, tk.ID).Scan(&kind)
	if kind != "service" {
		t.Fatalf("history actor_kind: %q", kind)
	}

	// once its maker stops being an admin, the key can't plan any more
	_, _ = db.Exec(ctx, `DELETE FROM org_admins WHERE org_id = 'sk1' AND user_clerk_id = 'user_admin'; INSERT INTO org_admins (org_id, user_clerk_id, granted_by) VALUES ('sk1', 'user_new', 'test')`)
	other, _ := ts.CreateBacklog(ctx, "sk1", pid, "task", "later", "")
	if code := planAs(key("user_admin", "sk1", "service"), other.ID, body("3")); code != 403 {
		t.Fatalf("a former admin's service key planned: %d", code)
	}
}
