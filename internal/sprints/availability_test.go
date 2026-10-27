package sprints

import (
	"context"
	"errors"
	"github.com/tracklines/backend/internal/workweek"
	"os"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/boards"
)

func TestTeamAvailability(t *testing.T) {
	mon := time.Date(2030, 1, 7, 0, 0, 0, 0, time.UTC) // a Monday
	if mon.Weekday() != time.Monday {
		t.Fatal("not a Monday")
	}
	day := func(n int) time.Time { return mon.AddDate(0, 0, n) }
	u := func(s string) *string { return &s }
	allDay := func(user *string, from, to int) away { return away{user, day(from), day(to), true} }

	// 4 people, two weeks (10 working days): 6 person-days away → 85%
	got := teamAvailability(4, []away{allDay(u("a"), 0, 5), allDay(u("b"), 7, 8),
		allDay(u("a"), 5, 7)}, mon, 14, workweek.Default()) // a's weekend doesn't count
	if got == nil || got.Percent != 85 || got.PersonDaysAway != 6 {
		t.Fatalf("85%%: %+v", got)
	}
	// a company day off leaves the working days (and whoever's off that day isn't counted twice)
	got = teamAvailability(2, []away{allDay(nil, 0, 1), allDay(u("a"), 0, 1)}, mon, 7, workweek.Default())
	if got == nil || got.Percent != 100 || got.PersonDaysAway != 0 {
		t.Fatalf("company day: %+v", got)
	}
	// part days: 4 hours is half a day; an all-day absence from a zone east of UTC still counts once
	got = teamAvailability(1, []away{
		{u("a"), day(0).Add(9 * time.Hour), day(0).Add(13 * time.Hour), false},
		{u("a"), day(1).Add(-time.Hour), day(2).Add(-time.Hour), true},
	}, mon, 5, workweek.Default())
	if got == nil || got.PersonDaysAway != 1.5 || got.Percent != 70 {
		t.Fatalf("part days: %+v", got)
	}
	// nothing to count: a weekend-only sprint
	if got := teamAvailability(3, nil, day(5), 2, workweek.Default()); got != nil {
		t.Fatalf("weekend only: %+v", got)
	}
}

// TestPlannedAvailability: with Google Calendar connected, a planned sprint's cap is scaled by the
// board team's availability over it, which can make it need approval (TL-408).
func TestPlannedAvailability(t *testing.T) {
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
	if _, err := db.Exec(ctx, `DELETE FROM boards WHERE owner_clerk_id = 'pv1'; DELETE FROM projects WHERE owner_clerk_id = 'pv1';
		DELETE FROM teams WHERE org_id = 'pv1'; DELETE FROM calendar_connections WHERE org_id = 'pv1'`); err != nil {
		t.Fatal(err)
	}
	var pid, team string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('pv1', 'p') RETURNING id`).Scan(&pid)
	b, _ := boards.Store{DB: db}.Create(ctx, "pv1", pid, "team", "")
	_ = db.QueryRow(ctx, `INSERT INTO teams (org_id, name) VALUES ('pv1', 'Squad') RETURNING id`).Scan(&team)
	if _, err := db.Exec(ctx, `UPDATE boards SET estimate_scale = 'fibonacci', team_id = $2 WHERE id = $1;`, b.ID, team); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO team_members (team_id, user_clerk_id, leader) VALUES ($1, 'u_a', true), ($1, 'u_b', false)`, team); err != nil {
		t.Fatal(err)
	}
	s := Store{DB: db}
	// the current sprint ends Monday 2030-01-07, so the planned one is that and the next week
	sp, err := s.Start(ctx, "pv1", b.ID, 14)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `UPDATE sprints SET ends_at = '2030-01-07T00:00:00Z' WHERE id = $1`, sp.ID); err != nil {
		t.Fatal(err)
	}
	plan, err := s.Plan(ctx, "pv1", b.ID, 14, "")
	if err != nil {
		t.Fatal(err)
	}
	// velocity 10 (one closed sprint) → cap 11; 10 points planned
	var closed string
	_ = db.QueryRow(ctx, `INSERT INTO sprints (board_id, number, length_days, starts_at, ends_at, closed_at)
		VALUES ($1, -1, 14, now() - interval '30 days', now() - interval '16 days', now() - interval '16 days') RETURNING id`, b.ID).Scan(&closed)
	if _, err := db.Exec(ctx, `INSERT INTO tickets (project_id, board_id, column_id, sprint_id, type, title, position, created_by, estimate)
		VALUES ($1, $2, $3, $4, 'task', 'done', 0, 'pv1', '5'), ($1, $2, $3, $4, 'task', 'done', 1, 'pv1', '5')`, pid, b.ID, b.Columns[2].ID, closed); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO tickets (project_id, planned_sprint_id, type, title, position, created_by, estimate)
		VALUES ($1, $2, 'task', 'next', 0, 'pv1', '5'), ($1, $2, 'task', 'next', 1, 'pv1', '5')`, pid, plan[0].ID); err != nil {
		t.Fatal(err)
	}
	capacity := func() Capacity {
		t.Helper()
		ps, err := s.Planned(ctx, "pv1", b.ID)
		if err != nil {
			t.Fatal(err)
		}
		return ps[0].Capacity
	}

	// without Google Calendar: unchanged
	if c := capacity(); c.Availability != nil || c.Cap == nil || *c.Cap != 11 || c.Over {
		t.Fatalf("not connected: %+v", c)
	}
	// connected, nobody away: everyone's available, same cap
	if _, err := db.Exec(ctx, `INSERT INTO calendar_connections (org_id, domain, admin_email, connected_by) VALUES ('pv1', 'x.test', 'a@x.test', 'u_a')`); err != nil {
		t.Fatal(err)
	}
	if c := capacity(); c.Availability == nil || c.Availability.Percent != 100 || *c.Cap != 11 {
		t.Fatalf("nobody away: %+v", c)
	}
	// u_a books the first week off (5 of 20 person-days): 75%, cap 8.25, so 10 points now needs approval.
	// u_b's Saturday and someone outside the team don't count.
	if _, err := db.Exec(ctx, `INSERT INTO time_away (org_id, user_id, starts_at, ends_at, all_day, source_id) VALUES
		('pv1', 'u_a', '2030-01-07T00:00:00Z', '2030-01-12T00:00:00Z', true, 'e1'),
		('pv1', 'u_b', '2030-01-12T00:00:00Z', '2030-01-13T00:00:00Z', true, 'e2'),
		('pv1', 'u_other', '2030-01-07T00:00:00Z', '2030-01-21T00:00:00Z', true, 'e3')`); err != nil {
		t.Fatal(err)
	}
	c := capacity()
	if c.Availability == nil || c.Availability.Percent != 75 || c.Availability.PersonDaysAway != 5 || *c.Cap != 8.25 || !c.Over || !c.NeedsApproval {
		t.Fatalf("leave booked: %+v %+v", c, c.Availability)
	}
	if *c.Velocity != 10 {
		t.Fatalf("velocity itself is unscaled: %v", *c.Velocity)
	}
	// a company day off on the second Monday: 9 working days, 5 of 18 away → 72.2%
	if _, err := db.Exec(ctx, `INSERT INTO time_away (org_id, user_id, starts_at, ends_at, all_day, source_id) VALUES
		('pv1', NULL, '2030-01-14T00:00:00Z', '2030-01-15T00:00:00Z', true, 'h1')`); err != nil {
		t.Fatal(err)
	}
	if c := capacity(); c.Availability.Percent != 72.2 || *c.Cap != 7.94 {
		t.Fatalf("company day: %+v %+v", c, c.Availability)
	}
	// a board without a team: unchanged
	if _, err := db.Exec(ctx, `UPDATE boards SET team_id = NULL WHERE id = $1`, b.ID); err != nil {
		t.Fatal(err)
	}
	if c := capacity(); c.Availability != nil || *c.Cap != 11 {
		t.Fatalf("no team: %+v", c)
	}
}
