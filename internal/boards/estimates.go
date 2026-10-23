package boards

import (
	"context"
	"slices"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/tracklines/backend/internal/httpx"
)

// Scales are the ways a board can estimate tickets, smallest first. "none" turns estimates off.
// The frontend keeps the same list in lib/estimates.ts.
var Scales = map[string][]string{
	"none":      nil,
	"fibonacci": {"1", "2", "3", "5", "8", "13", "21"},
	"tshirt":    {"XS", "S", "M", "L", "XL"},
	"powers":    {"1", "2", "4", "8", "16"},
	"linear":    {"1", "2", "3", "4", "5"},
}

var ErrBadEstimate = httpx.Code("bad_estimate", "estimate isn't on this board's scale")

// ValidEstimate: "" (no estimate) always is; anything else must be on the scale.
func ValidEstimate(scale, estimate string) bool {
	return estimate == "" || slices.Contains(Scales[scale], estimate)
}

// tshirtPoints turns sizes into numbers so velocity can add them up.
var tshirtPoints = map[string]float64{"XS": 1, "S": 2, "M": 3, "L": 5, "XL": 8}

// Points is an estimate as a number, whatever scale it came from; 0 when not estimated.
func Points(estimate *string) float64 {
	if estimate == nil {
		return 0
	}
	if p, ok := tshirtPoints[*estimate]; ok {
		return p
	}
	p, _ := strconv.ParseFloat(*estimate, 64)
	return p
}

// RecordScope stores what an open sprint holds (excluding duplicate tickets) just before it closes.
// Past burn charts start from this.
func RecordScope(ctx context.Context, tx pgx.Tx, sprintID string) error {
	rows, err := tx.Query(ctx, `SELECT estimate FROM tickets WHERE sprint_id = $1 AND duplicate_of IS NULL`, sprintID)
	if err != nil {
		return err
	}
	estimates, err := pgx.CollectRows(rows, pgx.RowTo[*string])
	if err != nil {
		return err
	}
	points := 0.0
	for _, e := range estimates {
		points += Points(e)
	}
	_, err = tx.Exec(ctx, `UPDATE sprints SET scope_tickets = $2, scope_points = $3 WHERE id = $1`, sprintID, len(estimates), points)
	return err
}
