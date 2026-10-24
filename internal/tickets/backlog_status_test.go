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

// The backlog can show its closed tickets: done, won't do, duplicates, or all closed, most recently
// closed first; open is the default; another org's backlog stays out of reach (owner, 2026-10-17).
func TestBacklogStatusFilter(t *testing.T) {
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
	if _, err := db.Exec(ctx, `DELETE FROM projects WHERE owner_clerk_id = 'bst1'`); err != nil {
		t.Fatal(err)
	}
	var pid string
	if err := db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('bst1', 'Status') RETURNING id`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	s := Store{DB: db}
	mk := func(title string) string {
		tk, err := s.CreateBacklog(ctx, "bst1", pid, "task", title, "")
		if err != nil {
			t.Fatal(err)
		}
		return tk.ID
	}
	open, done, wont, dup := mk("open"), mk("done"), mk("wont"), mk("dup")
	if err := s.ResolveAs(ctx, "bst1", "", done, ResolutionDone, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.ResolveAs(ctx, "bst1", "", wont, ResolutionWontDo, "not needed"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkDuplicate(ctx, "bst1", "", dup, open, DuplicateOptions{}); err != nil {
		t.Fatal(err)
	}
	// done was closed first, the duplicate last
	if _, err := db.Exec(ctx, `UPDATE tickets SET resolved_at = now() - interval '2 days' WHERE id = $1`, done); err != nil {
		t.Fatal(err)
	}
	titles := func(status string) string {
		p, err := s.PageBacklogStatus(ctx, "bst1", pid, "", "", status, nil, 1, 25, "")
		if err != nil {
			t.Fatalf("%s: %v", status, err)
		}
		var out []string
		for _, tk := range p.Tickets {
			out = append(out, tk.Title)
		}
		if p.Counts["all"] != len(out) {
			t.Errorf("%s: count %d for %d tickets", status, p.Counts["all"], len(out))
		}
		return strings.Join(out, ",")
	}
	for status, want := range map[string]string{"": "open", "open": "open", "done": "done", "wont_do": "wont", "duplicate": "dup", "closed": "dup,wont,done"} {
		if got := titles(status); got != want {
			t.Errorf("status %q: %s, want %s", status, got, want)
		}
	}
	if _, err := s.PageBacklogStatus(ctx, "bst1", pid, "", "", "archived", nil, 1, 25, ""); !errors.Is(err, ErrBadBacklogStatus) {
		t.Errorf("unknown status: %v", err)
	}

	// planned (TL-583): a ticket planned into an upcoming sprint can be hidden, or shown alone
	planned := mk("planned")
	var boardID, planID string
	if err := db.QueryRow(ctx, `INSERT INTO boards (owner_clerk_id, project_id, name) VALUES ('bst1', $1, 'B') RETURNING id`, pid).Scan(&boardID); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `INSERT INTO planned_sprints (board_id, position, length_days) VALUES ($1, 1, 14) RETURNING id`, boardID).Scan(&planID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `UPDATE tickets SET planned_sprint_id = $2 WHERE id = $1`, planned, planID); err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]string{"": "open,planned", "exclude": "open", "only": "planned"} {
		pg, err := s.PageBacklogPlanned(ctx, "bst1", pid, "", "", "", p, nil, 1, 25, "oldest")
		if err != nil {
			t.Fatalf("planned %q: %v", p, err)
		}
		var got []string
		for _, tk := range pg.Tickets {
			got = append(got, tk.Title)
		}
		if strings.Join(got, ",") != want || pg.Counts["all"] != len(got) {
			t.Errorf("planned %q: %v (count %d), want %s", p, got, pg.Counts["all"], want)
		}
	}
	if _, err := s.PageBacklogPlanned(ctx, "bst1", pid, "", "", "", "maybe", nil, 1, 25, ""); !errors.Is(err, ErrBadBacklogPlanned) {
		t.Errorf("unknown planned: %v", err)
	}

	// over HTTP: status passes through, a bad one is a 400, another org gets a 404
	h := NewSystem(db)
	get := func(org, query string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/?"+query, nil)
		r.SetPathValue("id", pid)
		r = r.WithContext(auth.WithAPIKey(ctx, "user_x", org, "k", "ai"))
		rec := httptest.NewRecorder()
		h.PageBacklog(rec, r)
		return rec
	}
	if rec := get("bst1", "status=wont_do"); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"title":"wont"`) || strings.Contains(rec.Body.String(), `"title":"open"`) {
		t.Errorf("http wont_do: %d %s", rec.Code, rec.Body)
	}
	if rec := get("bst1", "status=nope"); rec.Code != 400 {
		t.Errorf("http bad status: %d", rec.Code)
	}
	if rec := get("bst1", "planned=exclude"); rec.Code != 200 || strings.Contains(rec.Body.String(), `"title":"planned"`) || !strings.Contains(rec.Body.String(), `"title":"open"`) {
		t.Errorf("http planned=exclude: %d %s", rec.Code, rec.Body)
	}
	if rec := get("bst1", "planned=nope"); rec.Code != 400 {
		t.Errorf("http bad planned: %d", rec.Code)
	}
	if rec := get("bst-other", "status=closed"); rec.Code != 404 {
		t.Errorf("another org: %d", rec.Code)
	}
}
