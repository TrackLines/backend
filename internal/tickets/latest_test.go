package tickets

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
)

// Latest: newest first across projects, capped per project, only the caller's org, no archived projects.
func TestLatest(t *testing.T) {
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
	if _, err := db.Exec(ctx, `DELETE FROM projects WHERE owner_clerk_id IN ('lt1', 'lt2')`); err != nil {
		t.Fatal(err)
	}
	var busy, quiet, archived, foreign string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('lt1', 'Busy') RETURNING id`).Scan(&busy)
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('lt1', 'Quiet') RETURNING id`).Scan(&quiet)
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name, archived_at) VALUES ('lt1', 'Old', now()) RETURNING id`).Scan(&archived)
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('lt2', 'Theirs') RETURNING id`).Scan(&foreign)
	s := Store{DB: db}
	mk := func(owner, project, title string, age int) string {
		tk, err := s.CreateBacklog(ctx, owner, project, "task", title, "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(ctx, `UPDATE tickets SET created_at = now() - make_interval(mins => $2) WHERE id = $1`, tk.ID, age); err != nil {
			t.Fatal(err)
		}
		return tk.ID
	}
	b1, b2, b3 := mk("lt1", busy, "b1", 1), mk("lt1", busy, "b2", 2), mk("lt1", busy, "b3", 3)
	mk("lt1", busy, "b4", 4) // over the per-project cap
	q1 := mk("lt1", quiet, "q1", 10)
	mk("lt1", archived, "old", 0)
	mk("lt2", foreign, "theirs", 0)

	got, err := s.Latest(ctx, "lt1", 20, 3)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{b1, b2, b3, q1}
	if len(got) != len(want) {
		t.Fatalf("got %d: %v", len(got), got)
	}
	for i, id := range want {
		if got[i].ID != id {
			t.Fatalf("%d: got %s (%s), want %s", i, got[i].Title, got[i].ID, id)
		}
	}
	if got[3].ProjectName != "Quiet" {
		t.Fatalf("project name: %q", got[3].ProjectName)
	}
	if got, _ := s.Latest(ctx, "lt1", 2, 3); len(got) != 2 || got[0].ID != b1 {
		t.Fatalf("limit 2: %v", got)
	}
	if got, _ := s.Latest(ctx, "lt1", 20, 1); len(got) != 2 || got[0].ID != b1 || got[1].ID != q1 {
		t.Fatalf("per project 1: %v", got)
	}

	h := NewSystem(db)
	call := func(query string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.Latest(rec, httptest.NewRequest("GET", "/api/tickets/latest?"+query, nil).WithContext(auth.WithAPIKeyUser(ctx, "lt1", "ui")))
		return rec
	}
	if rec := call(""); rec.Code != 200 || !strings.Contains(rec.Body.String(), b1) || !strings.Contains(rec.Body.String(), q1) { // defaults 5 and 3
		t.Fatalf("default: %d %s", rec.Code, rec.Body)
	}
	if rec := call("limit=20&per_project=20"); strings.Contains(rec.Body.String(), "theirs") || strings.Contains(rec.Body.String(), "\"old\"") {
		t.Fatalf("another org's or an archived project's ticket: %s", rec.Body)
	}
	for _, q := range []string{"limit=0", "limit=21", "per_project=0", "per_project=x"} {
		if rec := call(q); rec.Code != 400 {
			t.Fatalf("%q: %d", q, rec.Code)
		}
	}
}
