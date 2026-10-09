package tickets

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
)

func TestNormalizeLabels(t *testing.T) {
	got, err := NormalizeLabels([]string{" agent:api ", "", "Bug", "AGENT:API", "bug"})
	if err != nil || strings.Join(got, "|") != "agent:api|Bug" {
		t.Fatalf("%v %v", got, err)
	}
	if _, err := NormalizeLabels([]string{strings.Repeat("x", 51)}); !errors.Is(err, ErrBadLabels) {
		t.Fatal("too long accepted")
	}
	if _, err := NormalizeLabels([]string{strings.Repeat("é", 50)}); err != nil {
		t.Fatal("50 runes should be fine:", err)
	}
	many := make([]string, 21)
	for i := range many {
		many[i] = string(rune('a' + i))
	}
	if _, err := NormalizeLabels(many); !errors.Is(err, ErrBadLabels) {
		t.Fatal("21 labels accepted")
	}
}

// PostgreSQL is provisioned by this package's TestMain.
func TestLabels(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Fatal("TEST_DATABASE_URL not set; package TestMain should provision PostgreSQL")
	}
	ctx := context.Background()
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
	if _, err := db.Exec(ctx, `DELETE FROM users WHERE clerk_id IN ('lb1', 'lb2');
		DELETE FROM roadmaps WHERE owner_clerk_id IN ('lb1', 'lb2'); DELETE FROM boards WHERE owner_clerk_id IN ('lb1', 'lb2'); DELETE FROM projects WHERE owner_clerk_id IN ('lb1', 'lb2'); -- org-owned rows no longer cascade from users
		INSERT INTO users (clerk_id, email) VALUES ('lb1', 'a@b.c'), ('lb2', 'd@e.f')`); err != nil {
		t.Fatal(err)
	}
	var pid, bid, col string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('lb1', 'p') RETURNING id`).Scan(&pid)
	_ = db.QueryRow(ctx, `INSERT INTO boards (project_id, owner_clerk_id, name) VALUES ($1, 'lb1', 'b') RETURNING id`, pid).Scan(&bid)
	_ = db.QueryRow(ctx, `INSERT INTO columns (board_id, name, position) VALUES ($1, 'To do', 0) RETURNING id`, bid).Scan(&col)
	h := NewSystem(db)
	s := Store{DB: db}
	as := auth.WithAPIKeyUser(ctx, "lb1", "bugfixes")
	do := func(method, path, id, body string, fn http.HandlerFunc) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(as)
		req.SetPathValue("id", id)
		rec := httptest.NewRecorder()
		fn(rec, req)
		return rec
	}

	// labels on create (board + backlog) come back on the ticket and on the backlog/detail reads
	if rec := do("POST", "/", col, `{"title":"a","labels":["agent:api","Bug","agent:API"]}`, h.Create); rec.Code != 201 || !strings.Contains(rec.Body.String(), `"labels":["agent:api","Bug"]`) {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	rec := do("POST", "/", pid, `{"title":"b","labels":["agent:web"]}`, h.CreateBacklog)
	if rec.Code != 201 {
		t.Fatalf("backlog create: %d %s", rec.Code, rec.Body)
	}
	bl, _ := s.Backlog(ctx, "lb1", pid, "")
	if len(bl) != 1 || len(bl[0].Labels) != 1 || bl[0].Labels[0] != "agent:web" {
		t.Fatalf("backlog read: %+v", bl)
	}
	id := bl[0].ID
	if d, _ := s.Get(ctx, "lb1", id); len(d.Labels) != 1 {
		t.Fatalf("detail: %+v", d.Labels)
	}
	if rec := do("POST", "/", col, `{"title":"c"}`, h.Create); !strings.Contains(rec.Body.String(), `"labels":[]`) {
		t.Fatalf("no labels should be [], got %s", rec.Body)
	}

	// priority on create is stored (board + backlog); unknown priorities are rejected
	if rec := do("POST", "/", pid, `{"title":"p","priority":"urgent"}`, h.CreateBacklog); rec.Code != 201 || !strings.Contains(rec.Body.String(), `"priority":"urgent"`) {
		t.Fatalf("backlog priority: %d %s", rec.Code, rec.Body)
	}
	if bl, _ := s.Backlog(ctx, "lb1", pid, ""); bl[len(bl)-1].Priority != "urgent" {
		t.Fatalf("priority not stored: %+v", bl[len(bl)-1])
	}
	if rec := do("POST", "/", col, `{"title":"p","priority":"high"}`, h.Create); rec.Code != 201 || !strings.Contains(rec.Body.String(), `"priority":"high"`) {
		t.Fatalf("board priority: %d %s", rec.Code, rec.Body)
	}
	if rec := do("POST", "/", col, `{"title":"p","priority":"meh"}`, h.Create); rec.Code != 400 {
		t.Fatalf("bad priority: %d", rec.Code)
	}

	// PATCH without labels leaves them; with labels replaces; PUT [] clears
	do("PATCH", "/", id, `{"title":"b2"}`, h.Update)
	if d, _ := s.Get(ctx, "lb1", id); len(d.Labels) != 1 {
		t.Fatal("PATCH without labels must not touch them")
	}
	do("PATCH", "/", id, `{"title":"b2","labels":["x","y"]}`, h.Update)
	if d, _ := s.Get(ctx, "lb1", id); strings.Join(d.Labels, ",") != "x,y" {
		t.Fatalf("PATCH labels: %v", d.Labels)
	}
	if rec := do("PUT", "/", id, `{"labels":[]}`, h.SetLabels); rec.Code != 200 {
		t.Fatalf("clear: %d", rec.Code)
	}
	if d, _ := s.Get(ctx, "lb1", id); len(d.Labels) != 0 {
		t.Fatalf("clear left %v", d.Labels)
	}

	// bad input and other owners
	if rec := do("PUT", "/", id, `{"labels":["`+strings.Repeat("x", 51)+`"]}`, h.SetLabels); rec.Code != 400 {
		t.Fatalf("too long: %d", rec.Code)
	}
	if rec := do("POST", "/", col, `{"title":"d","labels":["`+strings.Repeat("x", 51)+`"]}`, h.Create); rec.Code != 400 {
		t.Fatalf("create with bad labels must not create: %d", rec.Code)
	}
	if err := s.SetLabels(ctx, "lb2", id, []string{"z"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other owner: %v", err)
	}

	// suggestions: grouped case-insensitively, most used first
	_ = s.SetLabels(ctx, "lb1", id, []string{"AGENT:API"})
	got, err := s.ProjectLabels(ctx, "lb1", pid)
	if err != nil || len(got) != 2 || got[0].Count != 2 || !strings.EqualFold(got[0].Label, "agent:api") || got[1].Label != "Bug" {
		t.Fatalf("project labels: %+v %v", got, err)
	}
	if _, err := s.ProjectLabels(ctx, "lb2", pid); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other owner's project: %v", err)
	}
}
