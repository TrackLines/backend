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
	"github.com/tracklines/backend/internal/boards"
)

// Won't do: a backlog ticket closed as decided against stays (readable, with its reason), so whoever
// raised it can be told; moving it onto a board reopens it.
func TestWontDo(t *testing.T) {
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
	if _, err := db.Exec(ctx, `DELETE FROM boards WHERE owner_clerk_id = 'wd1'; DELETE FROM projects WHERE owner_clerk_id = 'wd1'`); err != nil {
		t.Fatal(err)
	}
	var pid string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('wd1', 'Wont') RETURNING id`).Scan(&pid)
	b, _ := boards.Store{DB: db}.Create(ctx, "wd1", pid, "web", "")
	s := Store{DB: db}
	h := NewSystem(db)
	as := auth.WithAPIKeyUser(ctx, "wd1", "claude")
	resolve := func(id, body string) int {
		req := httptest.NewRequest("POST", "/", strings.NewReader(body)).WithContext(as)
		req.SetPathValue("id", id)
		rec := httptest.NewRecorder()
		h.Resolve(rec, req)
		return rec.Code
	}
	resolution := func(id string) (res *string, done bool) {
		tk, err := s.Get(ctx, "wd1", id)
		if err != nil {
			t.Fatal(err)
		}
		return tk.Resolution, tk.Done
	}

	idea, _ := s.CreateBacklog(ctx, "wd1", pid, "feature", "Dark mode for emails", "")
	if code := resolve(idea.ID, `{"resolution":"wont_do","reason":"Emails stay plain text"}`); code != 204 {
		t.Fatalf("won't do: %d", code)
	}
	if res, done := resolution(idea.ID); res == nil || *res != "wont_do" || !done {
		t.Fatalf("won't do state: %v %v", res, done)
	}
	var body, author string
	if err := db.QueryRow(ctx, `SELECT body, author FROM ticket_comments WHERE ticket_id = $1`, idea.ID).Scan(&body, &author); err != nil ||
		body != "Won't do: Emails stay plain text" || author != "claude" {
		t.Fatalf("reason comment: %q %q %v", body, author, err)
	}
	// it leaves the backlog but still exists
	if page, err := s.PageBacklog(ctx, "wd1", pid, "", nil, 1, 25); err != nil || page.Counts["all"] != 0 {
		t.Fatalf("still in the backlog: %+v %v", page, err)
	}

	// no body: a plain resolve, as before
	plain, _ := s.CreateBacklog(ctx, "wd1", pid, "task", "Tidy", "")
	if code := resolve(plain.ID, ""); code != 204 {
		t.Fatalf("plain resolve: %d", code)
	}
	if res, done := resolution(plain.ID); res == nil || *res != "done" || !done {
		t.Fatalf("plain state: %v %v", res, done)
	}
	// bad input; board tickets are finished by moving them
	if code := resolve(plain.ID, `{"resolution":"maybe"}`); code != 400 {
		t.Fatalf("bad resolution: %d", code)
	}
	onBoard, _ := s.Create(ctx, "wd1", b.Columns[0].ID, "task", "on a board", "")
	if code := resolve(onBoard.ID, `{"resolution":"wont_do"}`); code != 409 {
		t.Fatalf("board ticket: %d", code)
	}
	// changed our mind: onto a board reopens it and clears the resolution
	if err := s.Move(ctx, "wd1", idea.ID, b.Columns[0].ID, 0); err != nil {
		t.Fatal(err)
	}
	if res, done := resolution(idea.ID); res != nil || done {
		t.Fatalf("reopened: %v %v", res, done)
	}
}
