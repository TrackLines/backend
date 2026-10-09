package boards

import (
	"context"
	"errors"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

func TestStore(t *testing.T) {
	ctx := context.Background()
	container, err := postgres.Run(ctx, "postgres:14-alpine",
		postgres.WithDatabase("tracklines_test"),
		postgres.WithUsername("test"),
		postgres.WithPassword("test"),
		postgres.BasicWaitStrategies(),
	)
	if err != nil {
		t.Fatalf("start postgres container: %v", err)
	}
	t.Cleanup(func() {
		if err := container.Terminate(ctx); err != nil {
			t.Errorf("terminate postgres container: %v", err)
		}
	})
	url, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("get postgres connection string: %v", err)
	}
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
	if _, err := db.Exec(ctx, `INSERT INTO users (clerk_id, email) VALUES ('b1', 'a@b.c'), ('b2', 'd@e.f') ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	var pid, otherPID string
	if err := db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('b1', 'p') RETURNING id`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('b2', 'p') RETURNING id`).Scan(&otherPID); err != nil {
		t.Fatal(err)
	}
	s := Store{DB: db}

	if _, err := s.Create(ctx, "b1", otherPID, "x", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("board in someone else's project: %v", err)
	}
	for _, team := range []string{"Backend", "Design"} { // one board per team
		if _, err := s.Create(ctx, "b1", pid, team, ""); err != nil {
			t.Fatal(err)
		}
	}
	list, _ := s.ListByProject(ctx, pid)
	if len(list) != 2 || list[0].ProjectID != pid {
		t.Fatalf("list: %+v", list)
	}
	b := list[0]
	if _, err := s.Get(ctx, b.ID, "b2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("non-owner get: %v", err)
	}
	full, err := s.Get(ctx, b.ID, "b1")
	if err != nil || len(full.Columns) != 3 {
		t.Fatalf("get: %+v %v", full, err)
	}
	if err := s.UpdateSettings(ctx, b.ID, "b1", "Renamed", nil); err != nil {
		t.Fatal(err)
	}
	full, err = s.Get(ctx, b.ID, "b1")
	if err != nil || full.Name != "Renamed" {
		t.Fatalf("rename: %+v %v", full, err)
	}
	if err := s.UpdateSettings(ctx, b.ID, "b2", "not yours", nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rename board owned by another organization: %v", err)
	}
	col := full.Columns[1].ID
	if _, err := db.Exec(ctx, `INSERT INTO tickets (project_id, board_id, column_id, title, position) VALUES ($3, $1, $2, 't2', 1), ($3, $1, $2, 't1', 0)`, b.ID, col, pid); err != nil {
		t.Fatal(err)
	}
	full, _ = s.Get(ctx, b.ID, "b1")
	if ts := full.Columns[1].Tickets; len(ts) != 2 || ts[0].Title != "t1" {
		t.Fatalf("tickets: %+v", ts)
	}
	// stats: 2 in progress (column 1), plus an urgent ticket in To do and one in Done
	if _, err := db.Exec(ctx, `INSERT INTO tickets (project_id, board_id, column_id, title, position, priority)
		VALUES ($3, $1, $2, 'todo', 0, 'urgent'), ($3, $1, $4, 'shipped', 0, 'urgent')`, b.ID, full.Columns[0].ID, pid, full.Columns[2].ID); err != nil {
		t.Fatal(err)
	}
	list, _ = s.ListByProject(ctx, pid)
	if st := list[0].Stats; st == nil || st.Active == "" || st.SprintNumber != nil ||
		st.Open != 3 || st.InProgress != 2 || st.Done != 1 || st.Urgent != 1 {
		t.Fatalf("stats: %+v", st)
	}
	if st := list[1].Stats; st == nil || st.Open+st.Done+st.Urgent != 0 {
		t.Fatalf("empty board stats: %+v", st)
	}
	// an open sprint: only its tickets count (none yet)
	if _, err := db.Exec(ctx, `INSERT INTO sprints (board_id, number, length_days, ends_at) VALUES ($1, 4, 14, now() + interval '14 days')`, b.ID); err != nil {
		t.Fatal(err)
	}
	list, _ = s.ListByProject(ctx, pid)
	if st := list[0].Stats; st.SprintNumber == nil || *st.SprintNumber != 4 || st.SprintEndsAt == nil || st.Open != 0 || st.Done != 0 {
		t.Fatalf("sprint stats: %+v", st)
	}
	if err := s.Delete(ctx, b.ID, "b2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("non-owner delete: %v", err)
	}
	if err := s.Delete(ctx, b.ID, "b1"); err != nil {
		t.Fatal(err)
	}
}
