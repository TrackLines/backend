package staff

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The system in numbers, and nothing that names or identifies anyone (TL-574).
func TestStats(t *testing.T) {
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
	var p1, p2, b1, b2, t1 string
	for _, q := range []struct {
		sql  string
		args []any
		into *string
	}{
		{`INSERT INTO projects (owner_clerk_id, name) VALUES ('org_secret_pro', 'Secret Project') RETURNING id`, nil, &p1},
		{`INSERT INTO projects (owner_clerk_id, name) VALUES ('org_secret_free', 'Other') RETURNING id`, nil, &p2},
		{`INSERT INTO org_billing (org_id, billing_email, stripe_status, plan) VALUES ('org_secret_pro', 'billing@secret.example', 'active', 'pro') RETURNING org_id`, nil, new(string)},
		{`INSERT INTO org_billing (org_id, billing_email, stripe_status) VALUES ('org_lapsed', 'x@secret.example', 'canceled') RETURNING org_id`, nil, new(string)},
		{`INSERT INTO boards (owner_clerk_id, name, project_id) VALUES ('org_secret_pro', 'Board', $1) RETURNING id`, []any{&p1}, &b1},
		{`INSERT INTO boards (owner_clerk_id, name, project_id, archived_at) VALUES ('org_secret_pro', 'Old', $1, now()) RETURNING id`, []any{&p1}, &b2},
		{`INSERT INTO tickets (project_id, type, title, position, created_by) VALUES ($1, 'task', 'Secret ticket', 0, 'u') RETURNING id`, []any{&p1}, &t1},
		{`INSERT INTO tickets (project_id, type, title, position, created_by, done_at) VALUES ($1, 'task', 'Done one', 0, 'u', now()) RETURNING id`, []any{&p2}, new(string)},
		{`INSERT INTO ticket_comments (ticket_id, body, author) VALUES ($1, 'secret words', 'u') RETURNING id`, []any{&t1}, new(string)},
		{`INSERT INTO ticket_attachments (ticket_id, file_key, url, name, size_bytes, content_type, created_by) VALUES ($1, 'k', 'https://x', 'secret.pdf', 2048, 'application/pdf', 'u') RETURNING id`, []any{&t1}, new(string)},
		{`INSERT INTO sprints (board_id, number, length_days, starts_at, ends_at) VALUES ($1, 1, 14, now(), now() + interval '14 days') RETURNING id`, []any{&b1}, new(string)},
	} {
		args := make([]any, len(q.args))
		for i, a := range q.args {
			args[i] = *(a.(*string))
		}
		if err := db.QueryRow(ctx, q.sql, args...).Scan(q.into); err != nil {
			t.Fatalf("%s: %v", q.sql, err)
		}
	}
	_ = p2

	rec := httptest.NewRecorder()
	Stats{DB: db}.Get(rec, httptest.NewRequest("GET", "/api/staff/stats", nil))
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var s SystemStats
	if err := json.Unmarshal(rec.Body.Bytes(), &s); err != nil {
		t.Fatal(err)
	}
	want := Count{Total: 3, Day: 3, Week: 3}
	if s.Organizations != want || s.Plans["pro"] != 1 || s.Plans["free"] != 2 || s.Plans["enterprise"] != 0 {
		t.Errorf("orgs %+v plans %v", s.Organizations, s.Plans)
	}
	if s.Projects.Total != 2 || s.Boards.Total != 1 || s.ArchivedBoards != 1 || s.Tickets.Total != 2 || s.DoneTickets != 1 ||
		s.Comments.Total != 1 || s.Attachments.Total != 1 || s.AttachmentBytes != 2048 || s.OpenSprints != 1 || s.DatabaseBytes == 0 {
		t.Errorf("counts %+v", s)
	}
	for _, leak := range []string{"secret", "Secret", "org_", p1, t1, b1} {
		if strings.Contains(rec.Body.String(), leak) {
			t.Errorf("leaks %q: %s", leak, rec.Body)
		}
	}
}

func TestLiveSessionKeys(t *testing.T) {
	for key, want := range map[string]bool{
		"tracklines:live:session:b1":              true,
		"tracklines:live:standup:board:b1":        true,
		"tracklines:live:refinement:project:p1":   true,
		"tracklines:live:standup:board:b1:people": false,
		"tracklines:live:events:b1":               false,
		"tracklines:live:events:standup:board:b1": false,
		"tracklines:live:events:project:p1":       false,
	} {
		if session(key) != want {
			t.Errorf("%s: %v", key, !want)
		}
	}
}
