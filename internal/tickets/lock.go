package tickets

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tracklines/backend/internal/boards"
	"github.com/tracklines/backend/internal/httpx"
)

// SprintLockedError: on its last day a sprint takes no new tickets, even when everything in it is done.
type SprintLockedError struct{ Number int }

func (e SprintLockedError) Error() string {
	return fmt.Sprintf("sprint %d ends today: its scope is locked. Put this in the backlog or the next sprint", e.Number)
}

// EstimateRequiredError: a board with an estimate scale only takes estimated tickets.
type EstimateRequiredError struct{ Scale string }

func (e EstimateRequiredError) Error() string {
	return fmt.Sprintf("estimate required: this board estimates on the %s scale (%s)", e.Scale, strings.Join(boards.Scales[e.Scale], ", "))
}

// lastDay: now is on (or past) the calendar day the sprint ends, both seen in loc.
func lastDay(now, ends time.Time, loc *time.Location) bool {
	ny, nm, nd := now.In(loc).Date()
	ey, em, ed := ends.In(loc).Date()
	return !time.Date(ny, nm, nd, 0, 0, 0, 0, time.UTC).Before(time.Date(ey, em, ed, 0, 0, 0, 0, time.UTC))
}

// lockedSprint returns the number of the board's open sprint when it's on its last day in the
// caller's zone (httpx.Zone), else 0.
func lockedSprint(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, boardID string) (int, error) {
	var number int
	var ends time.Time
	err := q.QueryRow(ctx, `SELECT number, ends_at FROM sprints WHERE board_id = $1 AND closed_at IS NULL AND starts_at <= now()`, boardID).Scan(&number, &ends)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil || !lastDay(time.Now(), ends, httpx.Zone(ctx)) {
		return 0, err
	}
	return number, nil
}
