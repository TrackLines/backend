package roadmaps

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgreSQL is provisioned by this package's TestMain.
func TestStore(t *testing.T) {
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
	if _, err := db.Exec(ctx, `INSERT INTO users (clerk_id, email) VALUES ('u1', 'a@b.c'), ('u2', 'd@e.f') ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	var pid, otherPID string
	if err := db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('u1', 'p') RETURNING id`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('u2', 'p') RETURNING id`).Scan(&otherPID); err != nil {
		t.Fatal(err)
	}
	s := Store{DB: db}

	if _, err := s.Create(ctx, "u1", otherPID, "x", "", Public); !errors.Is(err, ErrNotFound) {
		t.Fatalf("roadmap in someone else's project: %v", err)
	}
	if _, err := s.Create(ctx, "u1", pid, "x", "", "bogus"); !errors.Is(err, ErrInvalidVisibility) {
		t.Fatalf("bad visibility: %v", err)
	}
	r, err := s.Create(ctx, "u1", pid, "Q4", "plan", Public)
	if err != nil {
		t.Fatal(err)
	}
	d := "2026-12-01"
	if err := s.ReplaceItems(ctx, r.ID, "u1", []Item{{Title: "a", TargetDate: &d}, {Title: "b"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceItems(ctx, r.ID, "u2", nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("non-owner replace: %v", err)
	}
	got, err := s.Get(ctx, r.ID)
	if err != nil || len(got.Items) != 2 || *got.Items[0].TargetDate != d || got.Items[1].TargetDate != nil || got.Items[1].Position != 1 {
		t.Fatalf("get: %+v %v", got, err)
	}
	if err := s.Update(ctx, r.ID, "u2", "hijack", "", Public); !errors.Is(err, ErrNotFound) {
		t.Fatalf("non-owner update: %v", err)
	}
	if err := s.Update(ctx, r.ID, "u1", "Q4", "plan", LoginOnly); err != nil {
		t.Fatal(err)
	}
	if inProject, _ := s.ListByProject(ctx, pid); len(inProject) != 1 || inProject[0].ProjectID != pid {
		t.Fatalf("project list: %+v", inProject)
	}
	if mine, _ := s.ListByOwner(ctx, "u1"); len(mine) != 1 {
		t.Fatalf("owner list: %+v", mine)
	}
	if _, err := s.Get(ctx, "not-a-uuid"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bad uuid: %v", err)
	}
	if err := s.Delete(ctx, r.ID, "u1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, r.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
}
