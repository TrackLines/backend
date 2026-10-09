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

// PostgreSQL is provisioned by this package's TestMain.
func TestParent(t *testing.T) {
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
	if _, err := db.Exec(ctx, `DELETE FROM users WHERE clerk_id IN ('pa1', 'pa2');
		DELETE FROM roadmaps WHERE owner_clerk_id IN ('pa1', 'pa2'); DELETE FROM boards WHERE owner_clerk_id IN ('pa1', 'pa2'); DELETE FROM projects WHERE owner_clerk_id IN ('pa1', 'pa2'); -- org-owned rows no longer cascade from users
		INSERT INTO users (clerk_id, email) VALUES ('pa1', 'a@b.c'), ('pa2', 'd@e.f')`); err != nil {
		t.Fatal(err)
	}
	var pid, other, bid, todo, done string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('pa1', 'p') RETURNING id`).Scan(&pid)
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('pa1', 'o') RETURNING id`).Scan(&other)
	_ = db.QueryRow(ctx, `INSERT INTO boards (project_id, owner_clerk_id, name) VALUES ($1, 'pa1', 'b') RETURNING id`, pid).Scan(&bid)
	_ = db.QueryRow(ctx, `INSERT INTO columns (board_id, name, position) VALUES ($1, 'To do', 0) RETURNING id`, bid).Scan(&todo)
	_ = db.QueryRow(ctx, `INSERT INTO columns (board_id, name, position) VALUES ($1, 'Done', 1) RETURNING id`, bid).Scan(&done)
	s := Store{DB: db}
	mk := func(project, title string) string {
		tk, err := s.CreateBacklog(ctx, "pa1", project, "task", title, "")
		if err != nil {
			t.Fatal(err)
		}
		return tk.ID
	}
	epic, a, b, grand, foreign := mk(pid, "epic"), mk(pid, "a"), mk(pid, "b"), mk(pid, "grandchild"), mk(other, "elsewhere")
	if _, err := db.Exec(ctx, `UPDATE tickets SET board_id = $2, column_id = $3 WHERE id = $1`, b, bid, done); err != nil {
		t.Fatal(err)
	}
	str := func(v string) *string { return &v }

	// a top-level parent lists its children (with done) — even though it has no parent itself
	for _, c := range []string{a, b} {
		if err := s.SetParent(ctx, "pa1", c, str(epic)); err != nil {
			t.Fatal(err)
		}
	}
	d, err := s.Get(ctx, "pa1", epic)
	if err != nil || d.Parent != nil || len(d.Children) != 2 {
		t.Fatalf("epic: %v %+v", err, d)
	}
	if n := map[bool]int{}; true {
		for _, c := range d.Children {
			n[c.Done]++
		}
		if n[true] != 1 || n[false] != 1 {
			t.Fatalf("done counts: %+v", d.Children)
		}
	}
	if d, _ := s.Get(ctx, "pa1", a); d.Parent == nil || d.Parent.Title != "epic" || len(d.Children) != 0 {
		t.Fatalf("child: %+v", d)
	}

	// rejected: self, loops (direct and deeper), other project, other owner
	if err := s.SetParent(ctx, "pa1", a, str(a)); !errors.Is(err, ErrSelfParent) {
		t.Fatalf("self: %v", err)
	}
	if err := s.SetParent(ctx, "pa1", grand, str(a)); err != nil {
		t.Fatal(err)
	}
	if err := s.SetParent(ctx, "pa1", epic, str(a)); !errors.Is(err, ErrParentLoop) {
		t.Fatalf("direct loop: %v", err)
	}
	if err := s.SetParent(ctx, "pa1", epic, str(grand)); !errors.Is(err, ErrParentLoop) {
		t.Fatalf("deep loop: %v", err)
	}
	if err := s.SetParent(ctx, "pa1", a, str(foreign)); !errors.Is(err, ErrOtherProject) {
		t.Fatalf("other project: %v", err)
	}
	if err := s.SetParent(ctx, "pa2", a, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other owner: %v", err)
	}
	if err := s.SetParent(ctx, "pa1", a, str("00000000-0000-0000-0000-000000000000")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing parent: %v", err)
	}

	// handler: detach with null, 400 on a loop
	h := NewSystem(db)
	put := func(id, body string) int {
		req := httptest.NewRequest("PUT", "/", strings.NewReader(body)).WithContext(auth.WithAPIKeyUser(ctx, "pa1", "t"))
		req.SetPathValue("id", id)
		rec := httptest.NewRecorder()
		h.SetParent(rec, req)
		return rec.Code
	}
	if code := put(epic, `{"parent_id":"`+grand+`"}`); code != http.StatusBadRequest {
		t.Fatalf("loop via handler: %d", code)
	}
	if code := put(b, `{"parent_id":null}`); code != http.StatusNoContent {
		t.Fatalf("detach: %d", code)
	}
	// deleting a parent leaves its children as top-level tickets
	if err := s.Delete(ctx, "pa1", epic); err != nil {
		t.Fatal(err)
	}
	if d, _ := s.Get(ctx, "pa1", a); d.Parent != nil || len(d.Children) != 1 {
		t.Fatalf("after parent delete: %+v", d)
	}
}
