package sprints

import (
	"context"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tracklines/backend/internal/boards"
	"github.com/tracklines/backend/internal/httpx"
	"github.com/tracklines/backend/internal/workweek"
)

// Spikes is how a board's timeboxed spikes finished in a report's period (TL-508): within their
// timebox or over it, counted in business hours of the project's working week (TL-702; Mon–Fri
// 09:00–17:00 unless set, a timebox day being one working day) in the caller's zone.
type Spikes struct {
	Done      int     `json:"done"`       // timeboxed spikes finished in the period
	Within    int     `json:"within"`     // took no more business time than their timebox
	Over      int     `json:"over"`       // took more
	OverHours float64 `json:"over_hours"` // business hours over, summed across those that went over
}

// spikeOutcomes reads the board's spikes with a timebox that were started and finished in [from, to).
// Spikes without a timebox, or never moved into In Progress, have nothing to compare and are left out.
func (s Store) spikeOutcomes(ctx context.Context, boardID string, from, to time.Time) (Spikes, error) {
	rows, err := s.DB.Query(ctx, `SELECT t.started_at, t.done_at, COALESCE(t.timebox_days, 0), COALESCE(t.timebox_hours, 0) FROM tickets t
		WHERE t.board_id = $1 AND t.type = 'spike' AND t.duplicate_of IS NULL AND t.started_at IS NOT NULL
			AND (t.timebox_days > 0 OR t.timebox_hours > 0) AND t.done_at >= $2 AND t.done_at < $3 AND `+boards.TicketDoneSQL,
		boardID, from, to)
	if err != nil {
		return Spikes{}, err
	}
	loc := httpx.Zone(ctx)
	week, err := workweek.Store{DB: s.DB}.ForBoard(ctx, boardID)
	if err != nil {
		rows.Close()
		return Spikes{}, err
	}
	var out Spikes
	var started, done time.Time
	var days, hours int
	_, err = pgx.ForEachRow(rows, []any{&started, &done, &days, &hours}, func() error {
		out.Done++
		budget := (float64(days)*week.DayHours() + float64(hours)) * 60
		if over := week.BusinessMinutes(started.In(loc), done.In(loc)) - budget; over > 0 {
			out.Over++
			out.OverHours += over / 60
		} else {
			out.Within++
		}
		return nil
	})
	out.OverHours = math.Round(out.OverHours*10) / 10
	return out, err
}
