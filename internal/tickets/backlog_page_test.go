package tickets

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"slices"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
)

// Needs a throwaway db: TEST_DATABASE_URL=postgres://... go test ./internal/tickets/
func TestPageBacklog(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
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
	if _, err := db.Exec(ctx, `DELETE FROM users WHERE clerk_id IN ('bp1', 'bp2');
		INSERT INTO users (clerk_id, email) VALUES ('bp1', 'a@b.c'), ('bp2', 'd@e.f')`); err != nil {
		t.Fatal(err)
	}
	var pid, bid, col string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('bp1', 'p') RETURNING id`).Scan(&pid)
	_ = db.QueryRow(ctx, `INSERT INTO boards (project_id, owner_clerk_id, name) VALUES ($1, 'bp1', 'b') RETURNING id`, pid).Scan(&bid)
	_ = db.QueryRow(ctx, `INSERT INTO columns (board_id, name, position) VALUES ($1, 'To do', 0), ($1, 'Done', 1) RETURNING id`, bid).Scan(&col)
	s := Store{DB: db}

	// backlog in position order: b0 (blocked), b1, f2 [UI], b3 [ui], t4
	var ids []string
	for i, typ := range []string{"bug", "bug", "feature", "bug", "task"} {
		tk, err := s.CreateBacklog(ctx, "bp1", pid, typ, typ+string(rune('0'+i)), "")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, tk.ID)
	}
	_ = s.SetLabels(ctx, "bp1", ids[2], []string{"UI"})
	_ = s.SetLabels(ctx, "bp1", ids[3], []string{"ui"})
	blocker, _ := s.CreateAs(ctx, "bp1", "bp1", col, "task", "blocker", "") // on the board, not done
	if err := s.SetBlockedBy(ctx, "bp1", ids[0], []string{blocker.ID}); err != nil {
		t.Fatal(err)
	}

	titles := func(p *BacklogPage) (out []string) {
		for _, tk := range p.Tickets {
			out = append(out, tk.Title)
		}
		return out
	}

	p1, err := s.PageBacklog(ctx, "bp1", pid, "", nil, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(titles(p1), []string{"bug1", "feature2"}) || p1.Counts["all"] != 5 || p1.Counts["bug"] != 3 || p1.Counts["feature"] != 1 || p1.Counts["task"] != 1 {
		t.Fatalf("page 1: %v %v", titles(p1), p1.Counts)
	}
	// labels: whole backlog, merged case-insensitively (UI + ui), board-only labels excluded
	_ = s.SetLabels(ctx, "bp1", blocker.ID, []string{"board-only"})
	if bl, _ := s.PageBacklog(ctx, "bp1", pid, "task", []string{"nope"}, 1, 2); len(bl.Labels) != 1 || bl.Labels[0].Count != 2 {
		t.Fatalf("labels: %+v", bl.Labels)
	}
	if p3, _ := s.PageBacklog(ctx, "bp1", pid, "", nil, 3, 2); !slices.Equal(titles(p3), []string{"bug0"}) { // blocked sorts last
		t.Fatalf("page 3: %v", titles(p3))
	}
	if p4, _ := s.PageBacklog(ctx, "bp1", pid, "", nil, 4, 2); p4 == nil || len(p4.Tickets) != 0 {
		t.Fatal("past the end should be empty")
	}
	// labels match case-insensitively; counts follow the label filter but not the type filter
	ui, _ := s.PageBacklog(ctx, "bp1", pid, "bug", []string{"Ui"}, 1, 25)
	if !slices.Equal(titles(ui), []string{"bug3"}) || ui.Counts["all"] != 2 || ui.Counts["feature"] != 1 || ui.Counts["task"] != 0 {
		t.Fatalf("labels: %v %v", titles(ui), ui.Counts)
	}
	if _, err := s.PageBacklog(ctx, "bp2", pid, "", nil, 1, 25); !errors.Is(err, ErrNotFound) {
		t.Fatal("other owner should 404:", err)
	}
	if _, err := s.PageBacklog(ctx, "bp1", pid, "epic", nil, 1, 25); !errors.Is(err, ErrInvalidType) {
		t.Fatal("bad type:", err)
	}

	// HTTP: query parsing
	h := NewSystem(db)
	as := auth.WithAPIKeyUser(ctx, "bp1", "claude")
	get := func(q string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/?"+q, nil).WithContext(as)
		req.SetPathValue("id", pid)
		rec := httptest.NewRecorder()
		h.PageBacklog(rec, req)
		return rec
	}
	for _, bad := range []string{"page=0", "page=x", "per_page=101"} {
		if rec := get(bad); rec.Code != 400 {
			t.Fatalf("%s: %d", bad, rec.Code)
		}
	}
	rec := get("label=ui&label=nope&per_page=1")
	var got BacklogPage
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &got) != nil || len(got.Tickets) != 1 || got.Counts["all"] != 2 {
		t.Fatalf("http: %d %s", rec.Code, rec.Body)
	}
}
