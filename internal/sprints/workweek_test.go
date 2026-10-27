package sprints

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tracklines/backend/internal/boards"
	"github.com/tracklines/backend/internal/workweek"
)

// TL-702: a sprint ends on a working day of its project's week, however it's made: started, its length
// changed, or the next one rolled over. Availability counts the same working days.
func TestSprintsEndOnWorkingDays(t *testing.T) {
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
	const org = "ww1"
	for _, q := range []string{`DELETE FROM boards WHERE owner_clerk_id = 'ww1'`, `DELETE FROM projects WHERE owner_clerk_id = 'ww1'`, `DELETE FROM working_weeks WHERE org_id = 'ww1'`} {
		if _, err := db.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	var pid string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ($1, 'Weds') RETURNING id`, org).Scan(&pid)
	// only Wednesdays are worked: every sprint end, whatever today is, lands on one
	if err := (workweek.Store{DB: db}).Set(ctx, org, pid, "user_admin", workweek.Week{Days: []int{3}, WeekStart: 1, From: 540, To: 1020}); err != nil {
		t.Fatal(err)
	}
	b, err := boards.Store{DB: db}.Create(ctx, org, pid, "Board", "")
	if err != nil {
		t.Fatal(err)
	}
	s := Store{DB: db}
	wednesday := func(what string, sp *Sprint) {
		t.Helper()
		ends, err := time.Parse(time.RFC3339, sp.EndsAt)
		if err != nil {
			t.Fatal(err)
		}
		if ends.UTC().Weekday() != time.Wednesday {
			t.Fatalf("%s ends on a %s (%s)", what, ends.UTC().Weekday(), sp.EndsAt)
		}
	}
	sp, err := s.Start(ctx, org, b.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	wednesday("started", sp)
	if sp, err = s.UpdateLength(ctx, org, sp.ID, 9); err != nil {
		t.Fatal(err)
	}
	wednesday("lengthened", sp)
	next, err := s.Close(ctx, org, sp.ID)
	if err != nil {
		t.Fatal(err)
	}
	wednesday("rolled over", next)

	// availability: a week of days counts only its working days (here, the one Wednesday)
	mon := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	if got := teamAvailability(1, nil, mon, 7, workweek.Week{Days: []int{3}, WeekStart: 1, From: 540, To: 1020}); got == nil || got.Percent != 100 {
		t.Fatalf("availability over one working day: %+v", got)
	}
	if got := teamAvailability(1, nil, mon, 2, workweek.Week{Days: []int{3}, WeekStart: 1, From: 540, To: 1020}); got != nil {
		t.Fatalf("Mon–Tue on a Wednesday week has no working days: %+v", got)
	}
}
