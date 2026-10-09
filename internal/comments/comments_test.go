package comments

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgreSQL is provisioned by this package's TestMain.
func TestComments(t *testing.T) {
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
	if _, err := db.Exec(ctx, `DELETE FROM users WHERE clerk_id IN ('c1', 'c2');
		DELETE FROM roadmaps WHERE owner_clerk_id IN ('c1', 'c2'); DELETE FROM boards WHERE owner_clerk_id IN ('c1', 'c2'); DELETE FROM projects WHERE owner_clerk_id IN ('c1', 'c2'); -- org-owned rows no longer cascade from users
		INSERT INTO users (clerk_id, email) VALUES ('c1', 'a@b.c'), ('c2', 'd@e.f')`); err != nil {
		t.Fatal(err)
	}
	newTicket := func(title string) string {
		var id string
		if err := db.QueryRow(ctx, `WITH p AS (INSERT INTO projects (owner_clerk_id, name) VALUES ('c1', 'p') RETURNING id)
			INSERT INTO tickets (project_id, title, position) SELECT id, $1, 0 FROM p RETURNING id`, title).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	ticket, other := newTicket("t"), newTicket("other")
	s := Store{DB: db}

	first, err := s.Create(ctx, "c1", "claude", ticket, "  started on this  ", nil)
	if err != nil || first.Body != "started on this" || first.Author != "claude" || first.ParentID != nil {
		t.Fatalf("comment: %+v %v", first, err)
	}
	reply, err := s.Create(ctx, "c1", "user_abc", ticket, "thanks", &first.ID)
	if err != nil || reply.ParentID == nil || *reply.ParentID != first.ID {
		t.Fatalf("reply: %+v %v", reply, err)
	}
	second, _ := s.Create(ctx, "c1", "codex", ticket, "done", nil)

	list, err := s.List(ctx, "c1", ticket)
	if err != nil || len(list) != 3 || list[0].ID != first.ID || list[1].ID != reply.ID || list[2].ID != second.ID {
		t.Fatalf("chronological list: %+v %v", list, err)
	}

	otherComment, _ := s.Create(ctx, "c1", "claude", other, "on another ticket", nil)
	ghost := "00000000-0000-0000-0000-000000000000"
	junk := "not-a-uuid"
	for name, parent := range map[string]*string{"parent on another ticket": &otherComment.ID, "unknown parent": &ghost, "malformed parent": &junk} {
		if _, err := s.Create(ctx, "c1", "claude", ticket, "x", parent); !errors.Is(err, ErrBadParent) {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, body := range map[string]string{"blank": "   ", "too long": strings.Repeat("a", MaxBody+1)} {
		if _, err := s.Create(ctx, "c1", "claude", ticket, body, nil); !errors.Is(err, ErrBadBody) {
			t.Errorf("%s body: %v", name, err)
		}
	}

	if _, err := s.Create(ctx, "c2", "x", ticket, "not mine", nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("non-owner comment: %v", err)
	}
	if _, err := s.List(ctx, "c2", ticket); !errors.Is(err, ErrNotFound) {
		t.Fatalf("non-owner list: %v", err)
	}
	if list, _ := s.List(ctx, "c1", ticket); len(list) != 3 {
		t.Fatalf("rejected comments leaked in: %d", len(list))
	}

	if _, err := db.Exec(ctx, `DELETE FROM tickets WHERE id = $1`, ticket); err != nil {
		t.Fatal(err)
	}
	var left int
	_ = db.QueryRow(ctx, `SELECT count(*) FROM ticket_comments WHERE ticket_id = $1`, ticket).Scan(&left)
	if left != 0 {
		t.Fatalf("deleting a ticket should remove its thread, %d left", left)
	}
}
