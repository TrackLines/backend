// Package workweek is the working week (TL-510, TL-702): which days are worked, which day a week starts on,
// and the working hours. An organization sets it, and a project can set its own. Hours are in each
// person's own timezone (owner, 27 Oct), so the zone comes from whoever's asking, not from here.
package workweek

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tracklines/backend/internal/httpx"
)

// Week is a working week. Days are 0 (Sunday) to 6 (Saturday), like time.Weekday; From and To are
// minutes from midnight.
type Week struct {
	Days      []int `json:"days"`
	WeekStart int   `json:"week_start"`
	From      int   `json:"start_minute"`
	To        int   `json:"end_minute"`
}

// Default is the week until one is set: Mon–Fri, Monday first, 09:00–17:00.
func Default() Week { return Week{Days: []int{1, 2, 3, 4, 5}, WeekStart: 1, From: 9 * 60, To: 17 * 60} }

var ErrInvalid = httpx.Code("bad_working_week", "a working week needs at least one working day, a first day of the week, and hours of at least one hour within the day")

// Valid reports whether w can be saved (the table's checks, said before it gets there).
func (w Week) Valid() bool {
	if len(w.Days) == 0 || len(w.Days) > 7 || w.WeekStart < 0 || w.WeekStart > 6 || w.From < 0 || w.To > 24*60 || w.To-w.From < 60 {
		return false
	}
	seen := map[int]bool{}
	for _, d := range w.Days {
		if d < 0 || d > 6 || seen[d] {
			return false
		}
		seen[d] = true
	}
	return true
}

// Works reports whether d is a working day.
func (w Week) Works(d time.Weekday) bool { return slices.Contains(w.Days, int(d)) }

// DayHours is how long a working day is: what a timebox's "1d" means.
func (w Week) DayHours() float64 { return float64(w.To-w.From) / 60 }

// BusinessMinutes is how much of [start, end] falls in working hours on working days, in start's location.
func (w Week) BusinessMinutes(start, end time.Time) float64 {
	total := 0.0
	for day := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, start.Location()); !day.After(end); day = day.AddDate(0, 0, 1) {
		if !w.Works(day.Weekday()) {
			continue
		}
		open := day.Add(time.Duration(w.From) * time.Minute)
		shut := day.Add(time.Duration(w.To) * time.Minute)
		if start.After(open) {
			open = start
		}
		if end.Before(shut) {
			shut = end
		}
		if shut.After(open) {
			total += shut.Sub(open).Minutes()
		}
	}
	return total
}

// StartOfWeek is the first day of the week t is in, at midnight in t's location.
func (w Week) StartOfWeek(t time.Time) time.Time {
	day := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
	return day.AddDate(0, 0, -((int(day.Weekday()) - w.WeekStart + 7) % 7))
}

// Store reads and saves working weeks.
type Store struct{ DB *pgxpool.Pool }

const cols = `array(SELECT unnest(days)::int), week_start, start_minute, end_minute`

func scan(row pgx.Row) (Week, error) {
	var w Week
	err := row.Scan(&w.Days, &w.WeekStart, &w.From, &w.To)
	return w, err
}

// ForProject is a project's week: its own, else its organization's, else Default.
func (s Store) ForProject(ctx context.Context, project string) (Week, error) {
	w, err := scan(s.DB.QueryRow(ctx, `SELECT `+cols+` FROM working_weeks w JOIN projects p ON p.id = $1::uuid
		WHERE w.project_id = p.id OR (w.project_id IS NULL AND w.org_id = p.owner_clerk_id)
		ORDER BY w.project_id NULLS LAST LIMIT 1`, project))
	if errors.Is(err, pgx.ErrNoRows) {
		return Default(), nil
	}
	return w, err
}

// ForBoard is a board's week (its project's).
func (s Store) ForBoard(ctx context.Context, board string) (Week, error) {
	var project string
	if err := s.DB.QueryRow(ctx, `SELECT project_id::text FROM boards WHERE id = $1::uuid`, board).Scan(&project); err != nil {
		return Default(), err
	}
	return s.ForProject(ctx, project)
}

// Weeks is an organization's week and its projects' own, for clients to resolve any ticket's week at once.
type Weeks struct {
	Org      Week            `json:"org"`
	Projects map[string]Week `json:"projects"` // only projects with their own
}

// All is org's week and every project's own.
func (s Store) All(ctx context.Context, org string) (Weeks, error) {
	out := Weeks{Org: Default(), Projects: map[string]Week{}}
	rows, err := s.DB.Query(ctx, `SELECT COALESCE(project_id::text, ''), `+cols+` FROM working_weeks WHERE org_id = $1`, org)
	if err != nil {
		return out, err
	}
	var project string
	var w Week
	_, err = pgx.ForEachRow(rows, []any{&project, &w.Days, &w.WeekStart, &w.From, &w.To}, func() error {
		if project == "" {
			out.Org = w
		} else {
			out.Projects[project] = w
		}
		w = Week{}
		return nil
	})
	return out, err
}

// Set saves org's week, or (project not "") a project's own, as who.
func (s Store) Set(ctx context.Context, org, project, who string, w Week) error {
	if !w.Valid() {
		return ErrInvalid
	}
	days := make([]int16, len(w.Days))
	for i, d := range w.Days {
		days[i] = int16(d)
	}
	slices.Sort(days)
	var pid *string
	if project != "" {
		pid = &project
	}
	_, err := s.DB.Exec(ctx, `INSERT INTO working_weeks (org_id, project_id, days, week_start, start_minute, end_minute, updated_by)
		VALUES ($1, $2::uuid, $3, $4, $5, $6, $7)
		ON CONFLICT (org_id, project_id) DO UPDATE SET days = EXCLUDED.days, week_start = EXCLUDED.week_start,
			start_minute = EXCLUDED.start_minute, end_minute = EXCLUDED.end_minute, updated_by = EXCLUDED.updated_by, updated_at = now()`,
		org, pid, days, w.WeekStart, w.From, w.To, who)
	return err
}

// Reset drops a project's own week, so it follows its organization's.
func (s Store) Reset(ctx context.Context, org, project, who string) error {
	_, err := s.DB.Exec(ctx, `DELETE FROM working_weeks WHERE org_id = $1 AND project_id = $2::uuid`, org, project) // who: the request's actor
	return err
}
