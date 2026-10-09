package tickets

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgreSQL is provisioned by this package's TestMain.
func TestAssign(t *testing.T) {
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
	if _, err := db.Exec(ctx, `DELETE FROM users WHERE clerk_id IN ('as1', 'as2');
		DELETE FROM roadmaps WHERE owner_clerk_id IN ('as1', 'as2'); DELETE FROM boards WHERE owner_clerk_id IN ('as1', 'as2'); DELETE FROM projects WHERE owner_clerk_id IN ('as1', 'as2'); -- org-owned rows no longer cascade from users
		INSERT INTO users (clerk_id, email) VALUES ('as1', 'a@b.c'), ('as2', 'd@e.f');
		INSERT INTO api_keys (owner_clerk_id, name, prefix, hash) VALUES
			('as1', 'codex', 'tl_a', 'h-as1-codex'), ('as1', 'gone', 'tl_b', 'h-as1-gone'), ('as2', 'stranger', 'tl_c', 'h-as2');
		INSERT INTO api_keys (owner_clerk_id, name, kind, prefix, hash) VALUES ('as1', 'deploy', 'service', 'tl_d', 'h-as1-deploy');
		UPDATE api_keys SET revoked_at = now() WHERE hash = 'h-as1-gone';
		UPDATE api_keys SET org_id = owner_clerk_id WHERE owner_clerk_id IN ('as1', 'as2')`); err != nil {
		t.Fatal(err)
	}
	var pid, tid string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('as1', 'p') RETURNING id`).Scan(&pid)
	_ = db.QueryRow(ctx, `INSERT INTO tickets (project_id, title, position) VALUES ($1, 'x', 0) RETURNING id`, pid).Scan(&tid)
	s := Store{DB: db}
	str := func(v string) *string { return &v }

	if list, _ := s.Assignees(ctx, "as1", "as1"); len(list) != 2 || list[0].Label != "You" || list[0].Kind != "person" || list[1].ID != "codex" || list[1].Kind != "ai" {
		t.Fatalf("assignees: %+v", list)
	}
	for _, who := range []string{"as1", "codex"} {
		got, err := s.Assign(ctx, "as1", "as1", tid, str(who))
		if err != nil || *got.AssignedTo != who {
			t.Fatalf("assign %s: %v %+v", who, err, got)
		}
	}
	// owner override: reassign even though codex holds it
	if _, err := s.Assign(ctx, "as1", "as1", tid, str("as1")); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"gone", "deploy", "stranger", "as2", "anyone"} {
		if _, err := s.Assign(ctx, "as1", "as1", tid, str(bad)); !errors.Is(err, ErrUnknownAssignee) {
			t.Fatalf("%s: %v", bad, err)
		}
	}
	if _, err := s.Assign(ctx, "as2", "as2", tid, str("as2")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other user's ticket: %v", err)
	}
	if got, err := s.Assign(ctx, "as1", "as1", tid, nil); err != nil || got.AssignedTo != nil {
		t.Fatalf("unassign: %v %+v", err, got)
	}
}
