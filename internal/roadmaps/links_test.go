package roadmaps

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
)

// Needs a throwaway db: TEST_DATABASE_URL=postgres://... go test ./internal/roadmaps/
func TestItemTickets(t *testing.T) {
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
	if _, err := db.Exec(ctx, `DELETE FROM users WHERE clerk_id IN ('rl1', 'rl2');
		INSERT INTO users (clerk_id, email) VALUES ('rl1', 'a@b.c'), ('rl2', 'd@e.f')`); err != nil {
		t.Fatal(err)
	}
	var pid, otherPID, bid, todo, done string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('rl1', 'p') RETURNING id`).Scan(&pid)
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('rl1', 'other') RETURNING id`).Scan(&otherPID)
	_ = db.QueryRow(ctx, `INSERT INTO boards (project_id, owner_clerk_id, name) VALUES ($1, 'rl1', 'b') RETURNING id`, pid).Scan(&bid)
	_ = db.QueryRow(ctx, `INSERT INTO columns (board_id, name, position) VALUES ($1, 'To do', 0) RETURNING id`, bid).Scan(&todo)
	_ = db.QueryRow(ctx, `INSERT INTO columns (board_id, name, position) VALUES ($1, 'Done', 1) RETURNING id`, bid).Scan(&done)
	ticket := func(project, col, title string) string {
		var id string
		var c any
		if col != "" {
			c = col
		}
		var b any
		if col != "" {
			b = bid
		}
		if err := db.QueryRow(ctx, `INSERT INTO tickets (project_id, board_id, column_id, title, position) VALUES ($1, $2, $3, $4, 0) RETURNING id`, project, b, c, title).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	t1, t2 := ticket(pid, todo, "schema"), ticket(pid, done, "api")
	foreign := ticket(otherPID, "", "elsewhere")

	s := Store{DB: db}
	r, _ := s.Create(ctx, "rl1", pid, "Plan", "", Public)
	if err := s.ReplaceItems(ctx, r.ID, "rl1", []Item{
		{Title: "Launch", ManualStatus: StatusNotStarted},
		{Title: "Later", ManualStatus: StatusInProgress},
		{Title: "Shared ticket"},
	}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(ctx, r.ID)
	launch, later, shared := got.Items[0], got.Items[1], got.Items[2]
	if later.Status != StatusInProgress || later.ManualStatus != StatusInProgress {
		t.Fatalf("unlinked item should preserve manual status: %+v", later)
	}

	if err := s.SetItemTickets(ctx, "rl1", r.ID, launch.ID, []string{t1, t2, t1}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetItemTickets(ctx, "rl1", r.ID, shared.ID, []string{t1}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Get(ctx, r.ID)
	if p := got.Items[0].Progress; p.Done != 1 || p.Total != 2 || len(got.Items[0].Tickets) != 2 || got.Items[0].Status != StatusInProgress {
		t.Fatalf("progress: %+v %+v", p, got.Items[0].Tickets)
	}
	if got.Progress != (Progress{Done: 1, Total: 2}) {
		t.Fatalf("roadmap progress: %+v", got.Progress)
	}
	if _, err := db.Exec(ctx, `UPDATE tickets SET column_id = $2 WHERE id = $1`, t1, done); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.Get(ctx, r.ID); got.Items[0].Progress.Done != 2 || got.Items[0].Status != StatusDone || got.Progress != (Progress{Done: 2, Total: 2}) {
		t.Fatalf("finishing a ticket should advance progress: %+v", got.Items[0].Progress)
	}

	// rejected links
	if err := s.SetItemTickets(ctx, "rl1", r.ID, launch.ID, []string{foreign}); !errors.Is(err, ErrOtherProject) {
		t.Fatalf("other project: %v", err)
	}
	if err := s.SetItemTickets(ctx, "rl2", r.ID, launch.ID, []string{t1}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("non-owner: %v", err)
	}
	r2, _ := s.Create(ctx, "rl1", pid, "Other roadmap", "", Public)
	if err := s.SetItemTickets(ctx, "rl1", r2.ID, launch.ID, []string{t1}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("item from another roadmap: %v", err)
	}

	// saving items keeps identity + links; new items inserted; dropped ones removed
	if err := s.ReplaceItems(ctx, r.ID, "rl1", []Item{{Title: "New first"}, {ID: launch.ID, Title: "Launch (renamed)"}}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Get(ctx, r.ID)
	if len(got.Items) != 2 || got.Items[1].ID != launch.ID || got.Items[1].Title != "Launch (renamed)" || got.Items[1].Progress.Total != 2 {
		t.Fatalf("resave lost identity/links: %+v", got.Items)
	}
	var laterLeft int
	_ = db.QueryRow(ctx, `SELECT count(*) FROM roadmap_items WHERE id = $1`, later.ID).Scan(&laterLeft)
	if laterLeft != 0 {
		t.Fatal("dropped item should be deleted")
	}

	// privacy through the real handler: anon sees counts only, owner sees tickets
	h := NewSystem(db)
	var body string
	get := func(ctx context.Context) Roadmap {
		req := httptest.NewRequest("GET", "/api/roadmaps/"+r.ID, nil).WithContext(ctx)
		req.SetPathValue("id", r.ID)
		rec := httptest.NewRecorder()
		h.Get(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("get: %d", rec.Code)
		}
		body = rec.Body.String()
		var out Roadmap
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return out
	}
	anon := get(context.Background())
	if anon.Items[1].Progress.Total != 2 || len(anon.Items[1].Tickets) != 0 {
		t.Fatalf("anon view: %+v", anon.Items[1])
	}
	if strings.Contains(body, "schema") || strings.Contains(body, t1) {
		t.Fatal("public response leaked a linked ticket's title or id")
	}
	owner := get(auth.WithAPIKeyUser(context.Background(), "rl1", "test-agent"))
	if len(owner.Items[1].Tickets) != 2 {
		t.Fatalf("owner view should list tickets: %+v", owner.Items[1])
	}
}
