package sprints

import (
	"context"
	"github.com/tracklines/backend/internal/workweek"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
)

// Availability is the board team's time away over a planned sprint (TL-408), from the Google Calendar
// sync's time_away. A team figure only: never who, never per person.
type Availability struct {
	Percent        float64 `json:"percent"`          // of the team's working days, not away (0–100)
	PersonDaysAway float64 `json:"person_days_away"` // over the sprint's working days
}

type away struct {
	user       *string // nil: a company day off
	start, end time.Time
	allDay     bool
}

// availability reads the team's time away for a sprint of days days from start. nil when the org
// hasn't connected Google Calendar or the board has no team: capacity stays as it was.
func availability(ctx context.Context, q querier, boardID string, start time.Time, days int) (*Availability, error) {
	var org *string
	var members []string
	if err := q.QueryRow(ctx, `SELECT c.org_id, COALESCE((SELECT array_agg(tm.user_clerk_id) FROM team_members tm WHERE tm.team_id = b.team_id), '{}')
		FROM boards b JOIN projects p ON p.id = b.project_id LEFT JOIN calendar_connections c ON c.org_id = p.owner_clerk_id
		WHERE b.id = $1`, boardID).Scan(&org, &members); err != nil {
		return nil, notFound(err)
	}
	if org == nil || len(members) == 0 {
		return nil, nil
	}
	from := start.UTC().Truncate(24 * time.Hour)
	rows, err := q.Query(ctx, `SELECT user_id, starts_at, ends_at, all_day FROM time_away
		WHERE org_id = $1 AND ends_at > $2 AND starts_at < $3 AND (user_id IS NULL OR user_id = ANY($4))`,
		*org, from, from.AddDate(0, 0, days), members)
	if err != nil {
		return nil, err
	}
	var list []away
	var a away
	if _, err := pgx.ForEachRow(rows, []any{&a.user, &a.start, &a.end, &a.allDay}, func() error { list = append(list, a); return nil }); err != nil {
		return nil, err
	}
	week := workweek.Default() // its project's working days (TL-702)
	var workDays []int
	if q.QueryRow(ctx, `SELECT array(SELECT unnest(working_days($1::uuid))::int)`, boardID).Scan(&workDays) == nil {
		week.Days = workDays
	}
	return teamAvailability(len(members), list, from, days, week), nil
}

// teamAvailability counts working days (the week's working days that aren't company days off) from from, and each
// member's days away on them: a whole day for an all-day absence, else hours away / 8 (up to a day).
// nil when there are no working days to count.
// ponytail: UTC days; the calendar's zone isn't stored, so an all-day absence counts on a UTC day it
// covers 12h+ of. Store the zone with time_away if teams far from UTC see days shift.
func teamAvailability(members int, list []away, from time.Time, days int, week workweek.Week) *Availability {
	overlap := func(a away, day time.Time) float64 {
		s, e := max(a.start.Unix(), day.Unix()), min(a.end.Unix(), day.Add(24*time.Hour).Unix())
		return max(0, float64(e-s)/3600)
	}
	working, awayDays := 0, 0.0
	for i := range days {
		day := from.AddDate(0, 0, i)
		if !week.Works(day.Weekday()) {
			continue
		}
		off := map[string]float64{} // member → share of the day away
		company := false
		for _, a := range list {
			h := overlap(a, day)
			switch {
			case a.user == nil:
				company = company || h >= 12
			case a.allDay && h >= 12:
				off[*a.user] = 1
			case !a.allDay:
				off[*a.user] = min(1, off[*a.user]+h/8)
			}
		}
		if company {
			continue
		}
		working++
		for _, d := range off {
			awayDays += d
		}
	}
	if working == 0 {
		return nil
	}
	total := float64(members * working)
	return &Availability{
		Percent:        math.Round((total-awayDays)/total*1000) / 10,
		PersonDaysAway: math.Round(awayDays*10) / 10,
	}
}
