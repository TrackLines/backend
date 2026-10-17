package sprints

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/boards"
	"github.com/tracklines/backend/internal/httpx"
)

// recordStart notes the tickets a sprint starts with (TL-397), and the over-capacity total a lead
// approved for it, if any. Called once its carried-over and planned tickets have joined.
func recordStart(ctx context.Context, tx pgx.Tx, sprintID string, approved *float64) error {
	if _, err := tx.Exec(ctx, `INSERT INTO sprint_tickets (sprint_id, ticket_id, at_start, estimate, type)
		SELECT $1, id, true, estimate, type::text FROM tickets WHERE sprint_id = $1 AND duplicate_of IS NULL
		ON CONFLICT (sprint_id, ticket_id) DO NOTHING`, sprintID); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE sprints SET snapshot = true, approved_over = $2 WHERE id = $1`, sprintID, approved)
	return err
}

// recordClose notes the tickets a sprint ends with, and which were finished, before the unfinished
// ones move on. Tickets added along the way get their first row here.
func recordClose(ctx context.Context, tx pgx.Tx, sprintID string) error {
	_, err := tx.Exec(ctx, `INSERT INTO sprint_tickets (sprint_id, ticket_id, at_close, done, estimate, type)
		SELECT $1, t.id, true, `+boards.TicketDoneSQL+`, t.estimate, t.type::text FROM tickets t WHERE t.sprint_id = $1 AND t.duplicate_of IS NULL
		ON CONFLICT (sprint_id, ticket_id) DO UPDATE SET at_close = true, done = EXCLUDED.done, estimate = EXCLUDED.estimate, type = EXCLUDED.type`, sprintID)
	return err
}

// --- the report ---

// Report is how a board's sprints went over a period (TL-397): for a project manager to show
// leadership. Never per person (no-per-person-metrics): only the team's totals.
type Report struct {
	BoardID   string         `json:"board_id"`
	BoardName string         `json:"board_name"`
	Unit      string         `json:"unit"` // points, or tickets on a board that doesn't estimate
	From      string         `json:"from"`
	To        string         `json:"to"`
	Sprints   []SprintReport `json:"sprints"` // oldest first; the open sprint last, if it's in the period
	Totals    ReportTotals   `json:"totals"`
	Previous  ReportTotals   `json:"previous"` // the same length of time just before, for trends
	Summary   string         `json:"summary"`
}

type SprintReport struct {
	Number   int     `json:"number"`
	StartsAt string  `json:"starts_at"`
	EndsAt   string  `json:"ends_at"`
	ClosedAt *string `json:"closed_at"` // null: still open
	Goal     string  `json:"goal"`
	GoalMet  *bool   `json:"goal_met"` // closed with a goal: met when nothing carried over

	// Planned is what it started with. Sprints from before reports were recorded only know what they
	// held at close (Estimated: true), and nothing of what was added or removed (nulls).
	Planned        float64  `json:"planned"`
	Estimated      bool     `json:"estimated,omitempty"`
	Done           float64  `json:"done"`
	Predictability *float64 `json:"predictability"` // done ÷ planned, 0–1+; null with nothing planned
	Added          *float64 `json:"added"`
	Removed        *float64 `json:"removed"`
	Carried        *float64 `json:"carried"`  // unfinished at close, moved to the next sprint; null while open
	Velocity       *float64 `json:"velocity"` // average done over this and the two sprints before it
	ApprovedOver   *float64 `json:"approved_over"`
	Mix            []Mix    `json:"mix"` // what got done, by type
}

type Mix struct {
	Type  string  `json:"type"`
	Count int     `json:"count"`
	Value float64 `json:"value"` // in the report's unit
}

type ReportTotals struct {
	Sprints        int      `json:"sprints"` // closed sprints counted
	Planned        float64  `json:"planned"`
	Done           float64  `json:"done"`
	Predictability *float64 `json:"predictability"` // total done ÷ total planned
	Added          *float64 `json:"added"`          // null if no sprint in the period recorded it
	Carried        *float64 `json:"carried"`
	Velocity       *float64 `json:"velocity"` // average done per closed sprint
	Mix            []Mix    `json:"mix"`
}

// sprintRow is one sprint and its tickets as recorded.
type sprintRow struct {
	id, startsAt, endsAt string
	closedAt             *string
	number               int
	goal                 string
	snapshot             bool
	scopeTickets         *int
	scopePoints          *float64
	approved             *float64
}

// BoardReport reads the owner's board's sprints that closed in [from, to), plus the open sprint if it
// started before to, and the same for the period just before (for trends).
func (s Store) BoardReport(ctx context.Context, owner, boardID string, from, to time.Time) (*Report, error) {
	var name, scale string
	if err := s.DB.QueryRow(ctx, `SELECT b.name, b.estimate_scale FROM boards b JOIN projects p ON p.id = b.project_id
		WHERE b.id = $1 AND p.owner_clerk_id = $2`, boardID, owner).Scan(&name, &scale); err != nil {
		return nil, notFound(err)
	}
	unit := unitOf(scale)
	r := &Report{BoardID: boardID, BoardName: name, Unit: unit, From: from.UTC().Format(time.RFC3339), To: to.UTC().Format(time.RFC3339), Sprints: []SprintReport{}}
	// every closed sprint up to the period's end, so velocity can look back two sprints
	all, err := s.sprintReports(ctx, boardID, unit, to)
	if err != nil {
		return nil, err
	}
	length := to.Sub(from)
	var previous []SprintReport
	for _, sp := range all {
		switch {
		case sp.ClosedAt == nil: // open: in the period if it started before its end
			if start, _ := time.Parse(time.RFC3339, sp.StartsAt); start.Before(to) {
				r.Sprints = append(r.Sprints, sp)
			}
		default:
			closed, _ := time.Parse(time.RFC3339, *sp.ClosedAt)
			if !closed.Before(from) && closed.Before(to) {
				r.Sprints = append(r.Sprints, sp)
			} else if !closed.Before(from.Add(-length)) && closed.Before(from) {
				previous = append(previous, sp)
			}
		}
	}
	r.Totals, r.Previous = totals(r.Sprints), totals(previous)
	r.Summary = summary(r.Totals, r.Previous, unit)
	return r, nil
}

func (s Store) sprintReports(ctx context.Context, boardID, unit string, before time.Time) ([]SprintReport, error) {
	rows, err := s.DB.Query(ctx, `SELECT id, number, to_char(starts_at AT TIME ZONE 'UTC', `+ts+`), to_char(ends_at AT TIME ZONE 'UTC', `+ts+`),
			to_char(closed_at AT TIME ZONE 'UTC', `+ts+`), goal, snapshot, scope_tickets, scope_points::float8, approved_over::float8
		FROM sprints WHERE board_id = $1 AND starts_at < $2 ORDER BY number`, boardID, before)
	if err != nil {
		return nil, err
	}
	var sprints []sprintRow
	var sp sprintRow
	if _, err := pgx.ForEachRow(rows, []any{&sp.id, &sp.number, &sp.startsAt, &sp.endsAt, &sp.closedAt, &sp.goal, &sp.snapshot, &sp.scopeTickets, &sp.scopePoints, &sp.approved}, func() error {
		sprints = append(sprints, sp)
		return nil
	}); err != nil {
		return nil, err
	}
	value := valueIn(unit)
	out := make([]SprintReport, 0, len(sprints))
	for _, sp := range sprints {
		rep, err := s.sprintReport(ctx, sp, unit, value)
		if err != nil {
			return nil, err
		}
		out = append(out, rep)
	}
	for i := range out { // velocity: this and up to two closed sprints before it
		if out[i].ClosedAt == nil {
			continue
		}
		sum, n := 0.0, 0
		for j := i; j >= 0 && n < 3; j-- {
			if out[j].ClosedAt != nil {
				sum += out[j].Done
				n++
			}
		}
		v := sum / float64(n)
		out[i].Velocity = &v
	}
	return out, nil
}

// sprintTicket is a ticket as a report sees it.
type sprintTicket struct {
	atStart, atClose, done bool
	estimate               *string
	typ                    string
}

func (s Store) sprintReport(ctx context.Context, sp sprintRow, unit string, value func(*string) float64) (SprintReport, error) {
	rep := SprintReport{Number: sp.number, StartsAt: sp.startsAt, EndsAt: sp.endsAt, ClosedAt: sp.closedAt, Goal: sp.goal, ApprovedOver: sp.approved, Mix: []Mix{}}
	var list []sprintTicket
	var err error
	switch {
	case sp.closedAt != nil && sp.snapshot: // recorded at start and close
		list, err = s.recorded(ctx, sp.id)
	case sp.closedAt == nil: // open: its start (if recorded) against what's in it now
		list, err = s.openNow(ctx, sp.id)
	default: // closed before reports were recorded: what it finished, and what it held at close
		list, err = s.finished(ctx, sp.id)
	}
	if err != nil {
		return rep, err
	}
	var planned, done, added, removed, carried, current float64
	mix := map[string]*Mix{}
	for _, t := range list {
		v := value(t.estimate)
		if t.atStart {
			planned += v
		}
		if t.atStart && !t.atClose {
			removed += v
		}
		if !t.atStart && t.atClose {
			added += v
		}
		if t.atClose && t.done {
			done += v
			m := mix[t.typ]
			if m == nil {
				m = &Mix{Type: t.typ}
				mix[t.typ] = m
			}
			m.Count++
			m.Value += v
		}
		if t.atClose && !t.done {
			carried += v
		}
		if t.atClose {
			current += v
		}
	}
	rep.Done = done
	switch {
	case sp.snapshot:
		rep.Planned, rep.Added, rep.Removed = planned, &added, &removed
	default: // no record of its start: what it held at close stands in for the plan
		rep.Estimated = true
		if sp.closedAt == nil { // open, started before reports were recorded: what's in it now
			rep.Planned = current
		} else if sp.scopeTickets != nil && unit == "tickets" {
			rep.Planned = float64(*sp.scopeTickets)
		} else if sp.scopePoints != nil && unit == "points" {
			rep.Planned = *sp.scopePoints
		} else {
			rep.Planned = done
		}
	}
	if sp.closedAt != nil {
		if rep.Estimated {
			c := max(rep.Planned-done, 0)
			carried = c
		}
		rep.Carried = &carried
		if sp.goal != "" {
			met := carried == 0
			rep.GoalMet = &met
		}
	}
	if rep.Planned > 0 {
		p := done / rep.Planned
		rep.Predictability = &p
	}
	for _, typ := range []string{"bug", "feature", "task", "epic"} {
		if m := mix[typ]; m != nil {
			rep.Mix = append(rep.Mix, *m)
		}
	}
	return rep, nil
}

func scanTickets(rows pgx.Rows) ([]sprintTicket, error) {
	var out []sprintTicket
	var t sprintTicket
	_, err := pgx.ForEachRow(rows, []any{&t.atStart, &t.atClose, &t.done, &t.estimate, &t.typ}, func() error {
		out = append(out, t)
		return nil
	})
	return out, err
}

func (s Store) recorded(ctx context.Context, sprintID string) ([]sprintTicket, error) {
	rows, err := s.DB.Query(ctx, `SELECT at_start, at_close, done, estimate, type FROM sprint_tickets WHERE sprint_id = $1`, sprintID)
	if err != nil {
		return nil, err
	}
	return scanTickets(rows)
}

// openNow: an open sprint's recorded start, and what's in it now (at_close meaning "in it now").
func (s Store) openNow(ctx context.Context, sprintID string) ([]sprintTicket, error) {
	rows, err := s.DB.Query(ctx, `SELECT COALESCE(st.at_start, false), t.id IS NOT NULL, COALESCE(`+boards.TicketDoneSQL+`, false),
			COALESCE(t.estimate, st.estimate), COALESCE(t.type::text, st.type)
		FROM (SELECT * FROM sprint_tickets WHERE sprint_id = $1) st
		FULL JOIN (SELECT * FROM tickets WHERE sprint_id = $1 AND duplicate_of IS NULL) t ON t.id = st.ticket_id`, sprintID)
	if err != nil {
		return nil, err
	}
	return scanTickets(rows)
}

// finished: a sprint closed before reports were recorded keeps only the tickets it finished.
func (s Store) finished(ctx context.Context, sprintID string) ([]sprintTicket, error) {
	rows, err := s.DB.Query(ctx, `SELECT false, true, true, t.estimate, t.type::text FROM tickets t
		WHERE t.sprint_id = $1 AND t.duplicate_of IS NULL AND `+boards.TicketDoneSQL, sprintID)
	if err != nil {
		return nil, err
	}
	return scanTickets(rows)
}

// totals adds up closed sprints (an open one isn't finished, so it isn't counted).
func totals(list []SprintReport) ReportTotals {
	t := ReportTotals{Mix: []Mix{}}
	var added, carried float64
	recorded := false
	mix := map[string]*Mix{}
	for _, sp := range list {
		if sp.ClosedAt == nil {
			continue
		}
		t.Sprints++
		t.Planned += sp.Planned
		t.Done += sp.Done
		if sp.Added != nil {
			added += *sp.Added
			recorded = true
		}
		if sp.Carried != nil {
			carried += *sp.Carried
		}
		for _, m := range sp.Mix {
			if mix[m.Type] == nil {
				mix[m.Type] = &Mix{Type: m.Type}
			}
			mix[m.Type].Count += m.Count
			mix[m.Type].Value += m.Value
		}
	}
	if t.Sprints == 0 {
		return t
	}
	if t.Planned > 0 {
		p := t.Done / t.Planned
		t.Predictability = &p
	}
	v := t.Done / float64(t.Sprints)
	t.Velocity, t.Carried = &v, &carried
	if recorded {
		t.Added = &added
	}
	for _, typ := range []string{"bug", "feature", "task", "epic"} {
		if m := mix[typ]; m != nil {
			t.Mix = append(t.Mix, *m)
		}
	}
	return t
}

// summary: one line a project manager can read out, e.g. "Delivered 84% of planned work across 6
// sprints, up from 71%."
func summary(now, before ReportTotals, unit string) string {
	if now.Sprints == 0 {
		return "No sprints finished in this period."
	}
	sprints := fmt.Sprintf("%d sprints", now.Sprints)
	if now.Sprints == 1 {
		sprints = "1 sprint"
	}
	if now.Predictability == nil {
		return fmt.Sprintf("Delivered %s %s across %s.", trim(now.Done), unit, sprints)
	}
	line := fmt.Sprintf("Delivered %.0f%% of planned work across %s", *now.Predictability*100, sprints)
	if before.Predictability != nil {
		was := math.Round(*before.Predictability * 100)
		is := math.Round(*now.Predictability * 100)
		switch {
		case is > was:
			line += fmt.Sprintf(", up from %.0f%%", was)
		case is < was:
			line += fmt.Sprintf(", down from %.0f%%", was)
		default:
			line += ", the same as before"
		}
	}
	return line + "."
}

func trim(f float64) string { return strconv.FormatFloat(math.Round(f*10)/10, 'f', -1, 64) }

// --- HTTP ---

var errRange = errors.New("from and to are dates (YYYY-MM-DD), from before to, at most two years apart")

// ReportRange reads ?from=&to= (whole days in the caller's time zone, to inclusive). Default: the last
// 90 days up to today.
func ReportRange(r *http.Request) (time.Time, time.Time, error) {
	loc := httpx.Zone(r.Context())
	now := time.Now().In(loc)
	to := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, 1)
	from := to.AddDate(0, 0, -90)
	if v := r.URL.Query().Get("to"); v != "" {
		d, err := time.ParseInLocation(time.DateOnly, v, loc)
		if err != nil {
			return time.Time{}, time.Time{}, errRange
		}
		to = d.AddDate(0, 0, 1)
	}
	if v := r.URL.Query().Get("from"); v != "" {
		d, err := time.ParseInLocation(time.DateOnly, v, loc)
		if err != nil {
			return time.Time{}, time.Time{}, errRange
		}
		from = d
	}
	if !from.Before(to) || to.Sub(from) > 731*24*time.Hour {
		return time.Time{}, time.Time{}, errRange
	}
	return from, to, nil
}

// BoardReport: GET /api/boards/{id}/reports/sprints?from=&to=. Anyone in the board's organization.
func (h System) BoardReport(w http.ResponseWriter, r *http.Request) {
	from, to, err := ReportRange(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	rep, err := h.store.BoardReport(r.Context(), auth.OrgID(r.Context()), r.PathValue("id"), from, to)
	if err != nil {
		writeErr(w, err)
		return
	}
	if wantsMarkdown(r) { // TL-399
		loc := httpx.Zone(r.Context())
		sendMarkdown(w, rep.BoardName, from, to, loc, BoardMarkdown(rep, from, to, loc, time.Now()))
		return
	}
	httpx.JSON(w, http.StatusOK, rep)
}
