package boards

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Flow contains finished tickets in a rolling number of UTC calendar weeks, cycle-time
// percentiles for tickets with a recorded start, and throughput including empty weeks.
type Flow struct {
	BoardName       string             `json:"-"` // for the Markdown report
	Weeks           int                `json:"weeks"`
	Tickets         []FlowTicket       `json:"tickets"`
	MedianCycleDays *float64           `json:"median_cycle_days"`
	P85CycleDays    *float64           `json:"p85_cycle_days"`
	Throughput      []WeeklyThroughput `json:"throughput"`
	WIP             []ColumnWIP        `json:"wip"` // TL-621
	Mix             []TypeCount        `json:"mix"` // TL-621
}

type FlowTicket struct {
	ID        string   `json:"id"`
	Key       string   `json:"key"`
	Title     string   `json:"title"`
	StartedAt *string  `json:"started_at"`
	DoneAt    string   `json:"done_at"`
	CycleDays *float64 `json:"cycle_days"`
}

type WeeklyThroughput struct {
	WeekStart       string   `json:"week_start"`
	Tickets         int      `json:"tickets"`
	MedianCycleDays *float64 `json:"median_cycle_days"` // of that week's done tickets with a start; null without any
	P85CycleDays    *float64 `json:"p85_cycle_days"`
}

// ColumnWIP: open tickets in a column now (every column but the last, Done).
type ColumnWIP struct {
	ColumnID string `json:"column_id"`
	Column   string `json:"column"`
	Tickets  int    `json:"tickets"`
}

// TypeCount: tickets of one type done in the window.
type TypeCount struct {
	Type    string `json:"type"`
	Tickets int    `json:"tickets"`
}

// Flow returns completed work whose done_at falls in the requested UTC weeks. A week's window
// starts on Monday and includes the current week. Historical tickets without started_at remain
// in throughput and the ticket list, but don't contribute to cycle-time percentiles.
func (s Store) Flow(ctx context.Context, owner, boardID string, weeks int) (*Flow, error) {
	var name string
	if err := s.DB.QueryRow(ctx, `SELECT b.name FROM boards b JOIN projects p ON p.id = b.project_id
		WHERE b.id = $1 AND p.owner_clerk_id = $2`, boardID, owner).Scan(&name); err != nil {
		return nil, notFound(err)
	}

	now := time.Now().UTC()
	weekStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	weekStart = weekStart.AddDate(0, 0, -((int(weekStart.Weekday()) + 6) % 7))
	firstWeek := weekStart.AddDate(0, 0, -7*(weeks-1))
	end := weekStart.AddDate(0, 0, 7)

	f := &Flow{BoardName: name, Weeks: weeks, Tickets: []FlowTicket{}, Throughput: make([]WeeklyThroughput, weeks)}
	for i := range f.Throughput {
		f.Throughput[i].WeekStart = firstWeek.AddDate(0, 0, 7*i).Format("2006-01-02")
	}

	rows, err := s.DB.Query(ctx, `SELECT t.id::text,
			(SELECT kp.key_prefix FROM projects kp WHERE kp.id = t.project_id) || '-' || t.number,
			t.title,
			to_char(t.started_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
			to_char(t.done_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
			CASE WHEN t.started_at IS NULL THEN NULL ELSE EXTRACT(EPOCH FROM (t.done_at - t.started_at)) / 86400 END
		FROM tickets t WHERE t.board_id = $1 AND t.done_at >= $2 AND t.done_at < $3
		ORDER BY t.done_at, t.position`, boardID, firstWeek, end)
	if err != nil {
		return nil, err
	}
	f.Tickets, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (FlowTicket, error) {
		var t FlowTicket
		if err := row.Scan(&t.ID, &t.Key, &t.Title, &t.StartedAt, &t.DoneAt, &t.CycleDays); err != nil {
			return FlowTicket{}, err
		}
		return t, nil
	})
	if err != nil {
		return nil, err
	}

	var median, p85 pgtype.Float8
	if err := s.DB.QueryRow(ctx, `SELECT percentile_cont(0.5) WITHIN GROUP (ORDER BY EXTRACT(EPOCH FROM (t.done_at - t.started_at)) / 86400),
			percentile_cont(0.85) WITHIN GROUP (ORDER BY EXTRACT(EPOCH FROM (t.done_at - t.started_at)) / 86400)
		FROM tickets t WHERE t.board_id = $1 AND t.done_at >= $2 AND t.done_at < $3 AND t.started_at IS NOT NULL`, boardID, firstWeek, end).Scan(&median, &p85); err != nil {
		return nil, err
	}
	if median.Valid {
		f.MedianCycleDays = &median.Float64
		f.P85CycleDays = &p85.Float64
	}

	// percentile_cont skips the NULL cycle of tickets without a start; a week with none of those gets NULL
	counts, err := s.DB.Query(ctx, `SELECT date_trunc('week', t.done_at AT TIME ZONE 'UTC')::date::text, count(*),
			percentile_cont(0.5) WITHIN GROUP (ORDER BY EXTRACT(EPOCH FROM (t.done_at - t.started_at)) / 86400),
			percentile_cont(0.85) WITHIN GROUP (ORDER BY EXTRACT(EPOCH FROM (t.done_at - t.started_at)) / 86400)
		FROM tickets t WHERE t.board_id = $1 AND t.done_at >= $2 AND t.done_at < $3
		GROUP BY 1`, boardID, firstWeek, end)
	if err != nil {
		return nil, err
	}
	defer counts.Close()
	for counts.Next() {
		var start string
		var n int
		var median, p85 *float64
		if err := counts.Scan(&start, &n, &median, &p85); err != nil {
			return nil, err
		}
		for i := range f.Throughput {
			if f.Throughput[i].WeekStart == start {
				f.Throughput[i].Tickets, f.Throughput[i].MedianCycleDays, f.Throughput[i].P85CycleDays = n, median, p85
				break
			}
		}
	}
	if err := counts.Err(); err != nil {
		return nil, err
	}

	wip, err := s.DB.Query(ctx, `SELECT c.id::text, c.name, count(t.id) FROM columns c
		LEFT JOIN tickets t ON t.column_id = c.id AND t.resolved_at IS NULL AND t.duplicate_of IS NULL
		WHERE c.board_id = $1 AND c.position < (SELECT max(position) FROM columns WHERE board_id = $1)
		GROUP BY c.id ORDER BY c.position`, boardID)
	if err != nil {
		return nil, err
	}
	if f.WIP, err = pgx.CollectRows(wip, pgx.RowToStructByPos[ColumnWIP]); err != nil {
		return nil, err
	}
	mix, err := s.DB.Query(ctx, `SELECT t.type::text, count(*) FROM tickets t
		WHERE t.board_id = $1 AND t.done_at >= $2 AND t.done_at < $3
		GROUP BY 1 ORDER BY 2 DESC, 1`, boardID, firstWeek, end)
	if err != nil {
		return nil, err
	}
	if f.Mix, err = pgx.CollectRows(mix, pgx.RowToStructByPos[TypeCount]); err != nil {
		return nil, err
	}
	return f, nil
}
