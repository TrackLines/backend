package tickets

import (
	"context"
	"time"
)

// Away: someone is on holiday now (TL-407), from Google Calendar's time away (TL-406). Until: when
// they're back (UTC; touching or overlapping leave is joined up). Company: a company day off.
type Away struct {
	Until   string `json:"until"`
	Company bool   `json:"company,omitempty"`
}

// AwayNext: leave starting within the next week, so work isn't handed to someone about to go.
type AwayNext struct {
	From  string `json:"from"`
	Until string `json:"until"`
}

const awayHorizon = 7 * 24 * time.Hour

type awaySpan struct{ start, end time.Time }

// awayNow reads org's time away around now: per person, and the company's (everyone).
func (s Store) awayNow(ctx context.Context, org string, now time.Time) (people map[string][]awaySpan, company []awaySpan, err error) {
	rows, err := s.DB.Query(ctx, `SELECT COALESCE(user_id, ''), starts_at, ends_at FROM time_away
		WHERE org_id = $1 AND ends_at > $2 AND starts_at < $3 ORDER BY starts_at`, org, now, now.Add(awayHorizon))
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	people = map[string][]awaySpan{}
	for rows.Next() {
		var user string
		var sp awaySpan
		if err := rows.Scan(&user, &sp.start, &sp.end); err != nil {
			return nil, nil, err
		}
		if user == "" {
			company = append(company, sp)
		} else {
			people[user] = append(people[user], sp)
		}
	}
	return people, company, rows.Err()
}

// awayStatus: whether spans (sorted by start) have someone away at now and until when, else their
// next leave within the horizon.
func awayStatus(spans []awaySpan, now time.Time) (until *time.Time, next *[2]time.Time) {
	for i, sp := range spans {
		if !sp.start.After(now) && sp.end.After(now) {
			end := sp.end
			for _, more := range spans[i+1:] { // back-to-back leave reads as one stretch
				if more.start.After(end) {
					break
				}
				if more.end.After(end) {
					end = more.end
				}
			}
			return &end, nil
		}
		if sp.start.After(now) {
			end := sp.end
			for _, more := range spans[i+1:] {
				if more.start.After(end) {
					break
				}
				if more.end.After(end) {
					end = more.end
				}
			}
			return nil, &[2]time.Time{sp.start, end}
		}
	}
	return nil, nil
}

// markAway fills in who's away on people (not agents): their own leave, or a company day off.
func (s Store) markAway(ctx context.Context, org string, people []Assignee, now time.Time) error {
	own, company, err := s.awayNow(ctx, org, now)
	if err != nil {
		return err
	}
	for i := range people {
		if people[i].Kind != "person" {
			continue
		}
		spans := append(append([]awaySpan{}, own[people[i].ID]...), company...)
		sortSpans(spans)
		until, next := awayStatus(spans, now)
		switch {
		case until != nil:
			ownUntil, _ := awayStatus(own[people[i].ID], now) // not their own leave: a company day off
			people[i].Away = &Away{Until: until.UTC().Format(time.RFC3339), Company: ownUntil == nil}
		case next != nil:
			people[i].AwayNext = &AwayNext{From: next[0].UTC().Format(time.RFC3339), Until: next[1].UTC().Format(time.RFC3339)}
		}
	}
	return nil
}

func sortSpans(spans []awaySpan) {
	for i := 1; i < len(spans); i++ { // a handful at most: insertion sort
		for j := i; j > 0 && spans[j].start.Before(spans[j-1].start); j-- {
			spans[j], spans[j-1] = spans[j-1], spans[j]
		}
	}
}
