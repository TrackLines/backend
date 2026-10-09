package sprints

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/tracklines/backend/internal/boards"
)

// Velocity is what a board finished per closed sprint, plus how the open sprint is burning down.
// Unit is "points" on a board with an estimate scale (unestimated tickets count 0), else "tickets".
type Velocity struct {
	Unit    string           `json:"unit"`
	Sprints []SprintVelocity `json:"sprints"` // closed, oldest first
	Current *Burn            `json:"current"` // the open sprint, if any
}

type SprintVelocity struct {
	Number    int     `json:"number"`
	StartsAt  string  `json:"starts_at"`
	ClosedAt  string  `json:"closed_at"`
	Completed float64 `json:"completed"`
}

// Burn is the open sprint's scope and when each piece of it got done. Scope is today's: tickets
// added or removed mid-sprint move the total for the whole chart (no history of scope changes).
type Burn struct {
	Number      int         `json:"number"`
	StartsAt    string      `json:"starts_at"`
	EndsAt      string      `json:"ends_at"`
	Total       float64     `json:"total"`
	Unestimated int         `json:"unestimated"` // open-sprint tickets with no estimate, on a points board
	Done        []BurnPoint `json:"done"`        // oldest first
}

type BurnPoint struct {
	At    string  `json:"at"`
	Value float64 `json:"value"`
}

const ts = `'YYYY-MM-DD"T"HH24:MI:SS"Z"'`

// Velocity reads the owner's board. A closed sprint's tickets are the ones it finished: closing
// carries everything else into the next sprint.
func (s Store) Velocity(ctx context.Context, owner, boardID string) (*Velocity, error) {
	var scale string
	if err := s.DB.QueryRow(ctx, `SELECT b.estimate_scale FROM boards b JOIN projects p ON p.id = b.project_id
		WHERE b.id = $1 AND p.owner_clerk_id = $2`, boardID, owner).Scan(&scale); err != nil {
		return nil, notFound(err)
	}
	v := &Velocity{Unit: "tickets", Sprints: []SprintVelocity{}}
	if scale != "none" {
		v.Unit = "points"
	}
	value := func(estimate *string) float64 {
		if v.Unit == "tickets" {
			return 1
		}
		return boards.Points(estimate)
	}

	rows, err := s.DB.Query(ctx, `SELECT sp.number, to_char(sp.starts_at AT TIME ZONE 'UTC', `+ts+`),
			to_char(sp.closed_at AT TIME ZONE 'UTC', `+ts+`), t.id, t.estimate
		FROM sprints sp LEFT JOIN tickets t ON t.sprint_id = sp.id AND `+boards.TicketDoneSQL+`
		WHERE sp.board_id = $1 AND sp.closed_at IS NOT NULL ORDER BY sp.number`, boardID)
	if err != nil {
		return nil, err
	}
	var sv SprintVelocity
	var ticketID, estimate *string
	if _, err := pgx.ForEachRow(rows, []any{&sv.Number, &sv.StartsAt, &sv.ClosedAt, &ticketID, &estimate}, func() error {
		if n := len(v.Sprints); n == 0 || v.Sprints[n-1].Number != sv.Number {
			v.Sprints = append(v.Sprints, sv)
		}
		if ticketID != nil {
			v.Sprints[len(v.Sprints)-1].Completed += value(estimate)
		}
		return nil
	}); err != nil {
		return nil, err
	}

	b := Burn{Done: []BurnPoint{}}
	var sprintID string
	err = s.DB.QueryRow(ctx, `SELECT id, number, to_char(starts_at AT TIME ZONE 'UTC', `+ts+`), to_char(ends_at AT TIME ZONE 'UTC', `+ts+`)
		FROM sprints WHERE board_id = $1 AND closed_at IS NULL`, boardID).Scan(&sprintID, &b.Number, &b.StartsAt, &b.EndsAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return v, nil
	}
	if err != nil {
		return nil, err
	}
	// done before done_at existed (or stamped oddly): count it from the sprint's start
	rows, err = s.DB.Query(ctx, `SELECT t.estimate, `+boards.TicketDoneSQL+`,
			to_char(greatest(COALESCE(t.done_at, sp.starts_at), sp.starts_at) AT TIME ZONE 'UTC', `+ts+`)
		FROM tickets t JOIN sprints sp ON sp.id = t.sprint_id WHERE t.sprint_id = $1 ORDER BY 3`, sprintID)
	if err != nil {
		return nil, err
	}
	var done bool
	var at string
	if _, err := pgx.ForEachRow(rows, []any{&estimate, &done, &at}, func() error {
		b.Total += value(estimate)
		if v.Unit == "points" && estimate == nil {
			b.Unestimated++
		}
		if done {
			b.Done = append(b.Done, BurnPoint{At: at, Value: value(estimate)})
		}
		return nil
	}); err != nil {
		return nil, err
	}
	v.Current = &b
	return v, nil
}
