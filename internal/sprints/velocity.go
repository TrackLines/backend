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
	v := &Velocity{Unit: unitOf(scale), Sprints: []SprintVelocity{}}
	value := valueIn(v.Unit)

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

	var open string
	err = s.DB.QueryRow(ctx, `SELECT id FROM sprints WHERE board_id = $1 AND closed_at IS NULL`, boardID).Scan(&open)
	if errors.Is(err, pgx.ErrNoRows) {
		return v, nil
	}
	if err != nil {
		return nil, err
	}
	v.Current, err = s.burn(ctx, open, v.Unit)
	return v, err
}

// unitOf: boards with an estimate scale count points, the rest count tickets.
func unitOf(scale string) string {
	if scale == "none" {
		return "tickets"
	}
	return "points"
}

func valueIn(unit string) func(*string) float64 {
	return func(estimate *string) float64 {
		if unit == "tickets" {
			return 1
		}
		return boards.Points(estimate)
	}
}

// burn is a sprint's scope and when each piece of it got done. An open sprint's scope is what's in
// it now. A closed sprint's is what it held at close (scope_*), or, for sprints closed before that
// was recorded, what it finished; it ends at its close time.
func (s Store) burn(ctx context.Context, sprintID, unit string) (*Burn, error) {
	b := Burn{Done: []BurnPoint{}}
	var closed bool
	var scopeTickets *int
	var scopePoints *float64
	if err := s.DB.QueryRow(ctx, `SELECT number, to_char(starts_at AT TIME ZONE 'UTC', `+ts+`),
			to_char(COALESCE(closed_at, ends_at) AT TIME ZONE 'UTC', `+ts+`), closed_at IS NOT NULL, scope_tickets, scope_points::float8
		FROM sprints WHERE id = $1`, sprintID).Scan(&b.Number, &b.StartsAt, &b.EndsAt, &closed, &scopeTickets, &scopePoints); err != nil {
		return nil, notFound(err)
	}
	value := valueIn(unit)
	// done before done_at existed (or stamped oddly): count it from the sprint's start
	rows, err := s.DB.Query(ctx, `SELECT t.estimate, `+boards.TicketDoneSQL+`,
			to_char(greatest(COALESCE(t.done_at, sp.starts_at), sp.starts_at) AT TIME ZONE 'UTC', `+ts+`)
		FROM tickets t JOIN sprints sp ON sp.id = t.sprint_id WHERE t.sprint_id = $1 ORDER BY 3`, sprintID)
	if err != nil {
		return nil, err
	}
	var estimate *string
	var done bool
	var at string
	if _, err := pgx.ForEachRow(rows, []any{&estimate, &done, &at}, func() error {
		b.Total += value(estimate)
		if unit == "points" && estimate == nil {
			b.Unestimated++
		}
		if done {
			b.Done = append(b.Done, BurnPoint{At: at, Value: value(estimate)})
		}
		return nil
	}); err != nil {
		return nil, err
	}
	switch {
	case closed && unit == "tickets" && scopeTickets != nil:
		b.Total = float64(*scopeTickets)
	case closed && unit == "points" && scopePoints != nil:
		b.Total = *scopePoints
	}
	return &b, nil
}

// SprintDetail is one sprint, read-only: its burn data and the tickets it holds (for a closed
// sprint, the ones it finished). CarriedOver is how many moved on to the next sprint when it
// closed; null when that wasn't recorded (closed before scope tracking) or the sprint is open.
type SprintDetail struct {
	Sprint
	Unit        string          `json:"unit"`
	Burn        *Burn           `json:"burn"`
	Tickets     []boards.Ticket `json:"tickets"`
	CarriedOver *int            `json:"carried_over"`
}

// Detail returns sprint id on one of the owner's boards.
func (s Store) Detail(ctx context.Context, owner, id string) (*SprintDetail, error) {
	var d SprintDetail
	var scale string
	var scopeTickets *int
	err := s.DB.QueryRow(ctx, `SELECT sp.id, sp.board_id, sp.number, sp.length_days,
			to_char(sp.starts_at AT TIME ZONE 'UTC', `+ts+`), to_char(sp.ends_at AT TIME ZONE 'UTC', `+ts+`),
			to_char(sp.closed_at AT TIME ZONE 'UTC', `+ts+`), b.estimate_scale, sp.scope_tickets
		FROM sprints sp JOIN boards b ON b.id = sp.board_id JOIN projects p ON p.id = b.project_id
		WHERE sp.id::text = $1 AND p.owner_clerk_id = $2`, id, owner).
		Scan(&d.ID, &d.BoardID, &d.Number, &d.LengthDays, &d.StartsAt, &d.EndsAt, &d.ClosedAt, &scale, &scopeTickets)
	if err != nil {
		return nil, notFound(err)
	}
	d.Unit = unitOf(scale)
	if d.Burn, err = s.burn(ctx, d.ID, d.Unit); err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(ctx, `SELECT `+boards.TicketCols+` FROM tickets t WHERE t.sprint_id = $1 ORDER BY t.done_at NULLS LAST, t.position`, d.ID)
	if err != nil {
		return nil, err
	}
	if d.Tickets, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (boards.Ticket, error) { return boards.ScanTicket(row) }); err != nil {
		return nil, err
	}
	if d.ClosedAt != nil && scopeTickets != nil {
		n := max(*scopeTickets-len(d.Tickets), 0)
		d.CarriedOver = &n
	}
	return &d, nil
}
