package tickets

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/boards"
)

// "My work" is search with assignee=me: every board and project in the org, open by default,
// blocked included, newest change first, paged.
func TestMyWork(t *testing.T) {
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
	if _, err := db.Exec(ctx, `DELETE FROM boards WHERE owner_clerk_id IN ('mw1', 'mw2'); DELETE FROM projects WHERE owner_clerk_id IN ('mw1', 'mw2')`); err != nil {
		t.Fatal(err)
	}
	s := Store{DB: db}
	project := func(org, name string) (string, *boards.Board) {
		var pid string
		_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ($1, $2) RETURNING id`, org, name).Scan(&pid)
		b, _ := boards.Store{DB: db}.Create(ctx, org, pid, name+" board", "")
		return pid, b
	}
	shop, shopBoard := project("mw1", "Shop")
	_, docsBoard := project("mw1", "Docs")
	_, foreignBoard := project("mw2", "Elsewhere")
	mine := func(org string, b *boards.Board, title, prio string) *Ticket {
		tk, err := s.Create(ctx, org, b.Columns[0].ID, "task", title, "")
		if err != nil {
			t.Fatal(err)
		}
		_ = s.Update(ctx, org, tk.ID, "", title, "", prio)
		if _, err := s.Claim(ctx, org, "ava", tk.ID); err != nil {
			t.Fatal(err)
		}
		return tk
	}
	a := mine("mw1", shopBoard, "shop urgent", "urgent")
	mine("mw1", shopBoard, "shop low", "low")
	c := mine("mw1", docsBoard, "docs medium", "medium")
	blocker, _ := s.Create(ctx, "mw1", docsBoard.Columns[0].ID, "task", "someone else's", "")
	_ = s.SetBlockedBy(ctx, "mw1", c.ID, []string{blocker.ID})
	finished := mine("mw1", docsBoard, "docs finished", "medium")
	_ = s.Move(ctx, "mw1", finished.ID, docsBoard.Columns[2].ID, 0)
	mine("mw2", foreignBoard, "other org", "urgent")
	// the most recently changed comes first
	if _, err := db.Exec(ctx, `UPDATE tickets SET updated_at = now() + interval '1 minute' WHERE id = $1`, a.ID); err != nil {
		t.Fatal(err)
	}

	h := NewSystem(db)
	get := func(query string) []SearchResult {
		t.Helper()
		req := httptest.NewRequest("GET", "/api/search?"+query, nil).WithContext(auth.WithAPIKeyUser(ctx, "mw1", "ava"))
		rec := httptest.NewRecorder()
		h.Search(rec, req)
		var out []SearchResult
		if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
			t.Fatalf("%s: %d %s", query, rec.Code, rec.Body)
		}
		return out
	}
	titles := func(rs []SearchResult) string {
		var out []string
		for _, r := range rs {
			out = append(out, r.Title)
		}
		return strings.Join(out, ", ")
	}

	// open work across both projects, blocked included, newest change first, never another org's
	open := get("assignee=me&status=open")
	if len(open) != 3 || open[0].ID != a.ID || strings.Contains(titles(open), "other org") || strings.Contains(titles(open), "finished") {
		t.Fatalf("my open work: %s", titles(open))
	}
	blocked := false
	for _, r := range open {
		if r.ID == c.ID {
			blocked = r.Blocked && r.BoardName != nil && *r.BoardName == "Docs board" && r.ProjectName == "Docs"
		}
	}
	if !blocked {
		t.Fatalf("blocked work with its context: %+v", open)
	}
	// done is included on request
	if all := get("assignee=me"); len(all) != 4 {
		t.Fatalf("with done: %s", titles(all))
	}
	// filters
	if got := get("assignee=me&status=open&project=" + shop); len(got) != 2 {
		t.Fatalf("project: %s", titles(got))
	}
	if got := get("assignee=me&status=open&board=" + docsBoard.ID); len(got) != 1 || got[0].ID != c.ID {
		t.Fatalf("board: %s", titles(got))
	}
	if got := get("assignee=me&status=open&priority=urgent&priority=low"); len(got) != 2 {
		t.Fatalf("priority: %s", titles(got))
	}
	// paging: stable, no overlap, ends
	p1, p2, p3 := get("assignee=me&status=open&limit=2&page=1"), get("assignee=me&status=open&limit=2&page=2"), get("assignee=me&status=open&limit=2&page=3")
	if len(p1) != 2 || len(p2) != 1 || len(p3) != 0 || p1[0].ID == p2[0].ID || p1[1].ID == p2[0].ID {
		t.Fatalf("pages: [%s] [%s] [%s]", titles(p1), titles(p2), titles(p3))
	}
	req := httptest.NewRequest("GET", "/api/search?page=0", nil).WithContext(auth.WithAPIKeyUser(ctx, "mw1", "ava"))
	rec := httptest.NewRecorder()
	h.Search(rec, req)
	if rec.Code != 400 {
		t.Fatalf("bad page: %d", rec.Code)
	}
}
