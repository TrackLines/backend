package sprints

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/httpx"
)

// Roll-ups (TL-398): a project's sprint boards, or every project in the organization, together.
// Points add up only with points, tickets with tickets; predictability is a share, so it compares
// across both. Boards' sprints don't line up, so the trend is by calendar month, not sprint number.

// Rollup is a project's or the organization's report.
type Rollup struct {
	ID       string        `json:"id,omitempty"` // the project; empty for the organization
	Name     string        `json:"name"`
	From     string        `json:"from"`
	To       string        `json:"to"`
	Boards   []Report      `json:"boards,omitempty"`   // a project's sprint boards
	Projects []Rollup      `json:"projects,omitempty"` // the organization's projects
	Points   *ReportTotals `json:"points"`             // boards that estimate; null if none
	Tickets  *ReportTotals `json:"tickets"`            // boards that count tickets; null if none
	Overall  OverallTotals `json:"overall"`
	Previous OverallTotals `json:"previous"`
	Months   []MonthTotals `json:"months"` // oldest first, every month in the period
	Summary  string        `json:"summary"`
}

// OverallTotals is what compares across units.
type OverallTotals struct {
	Sprints        int      `json:"sprints"`
	Predictability *float64 `json:"predictability"` // planned work done, each board's share weighted by its sprints
	GoalsSet       int      `json:"goals_set"`
	GoalsMet       int      `json:"goals_met"`
}

// MonthTotals is the sprints that closed in a calendar month (the caller's).
type MonthTotals struct {
	Month          string   `json:"month"` // 2026-08
	Sprints        int      `json:"sprints"`
	Predictability *float64 `json:"predictability"`
	PointsDone     float64  `json:"points_done"`
	TicketsDone    float64  `json:"tickets_done"`
}

// ProjectReport reads the owner's project: each sprint board (not kanban, not archived) and them together.
func (s Store) ProjectReport(ctx context.Context, owner, projectID string, from, to time.Time, loc *time.Location) (*Rollup, error) {
	var name string
	if err := s.DB.QueryRow(ctx, `SELECT name FROM projects WHERE id = $1 AND owner_clerk_id = $2`, projectID, owner).Scan(&name); err != nil {
		return nil, notFound(err)
	}
	rows, err := s.DB.Query(ctx, `SELECT id FROM boards WHERE project_id = $1 AND style <> 'kanban' AND archived_at IS NULL ORDER BY created_at`, projectID)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	r := &Rollup{ID: projectID, Name: name, From: from.UTC().Format(time.RFC3339), To: to.UTC().Format(time.RFC3339), Boards: []Report{}}
	for _, id := range ids {
		b, err := s.BoardReport(ctx, owner, id, from, to)
		if err != nil {
			return nil, err
		}
		r.Boards = append(r.Boards, *b)
	}
	r.fill(r.Boards, from, to, loc)
	return r, nil
}

// OrgReport reads every project in the owner's organization.
func (s Store) OrgReport(ctx context.Context, owner string, from, to time.Time, loc *time.Location) (*Rollup, error) {
	rows, err := s.DB.Query(ctx, `SELECT id FROM projects WHERE owner_clerk_id = $1 ORDER BY name`, owner)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	r := &Rollup{Name: "Organization", From: from.UTC().Format(time.RFC3339), To: to.UTC().Format(time.RFC3339), Projects: []Rollup{}}
	var all []Report
	for _, id := range ids {
		p, err := s.ProjectReport(ctx, owner, id, from, to, loc)
		if err != nil {
			return nil, err
		}
		all = append(all, p.Boards...)
		p.Boards = nil // the org view is by project; a project's own report has its boards
		p.Months = nil
		r.Projects = append(r.Projects, *p)
	}
	r.fill(all, from, to, loc)
	return r, nil
}

// fill works out the roll-up's totals, months and summary from its boards' reports.
func (r *Rollup) fill(boards []Report, from, to time.Time, loc *time.Location) {
	var points, tickets []SprintReport
	var pointsBoards, ticketsBoards []Report
	for _, b := range boards {
		closed := closedOnly(b.Sprints)
		if b.Unit == "points" {
			points = append(points, closed...)
			pointsBoards = append(pointsBoards, b)
		} else {
			tickets = append(tickets, closed...)
			ticketsBoards = append(ticketsBoards, b)
		}
	}
	if len(pointsBoards) > 0 {
		t := totals(points)
		r.Points = &t
	}
	if len(ticketsBoards) > 0 {
		t := totals(tickets)
		r.Tickets = &t
	}
	r.Overall = overall(boards, func(b Report) ReportTotals { return b.Totals }, func(b Report) []SprintReport { return b.Sprints })
	r.Previous = overall(boards, func(b Report) ReportTotals { return b.Previous }, nil)
	r.Months = months(boards, from, to, loc)
	r.Summary = rollupSummary(r.Overall, r.Previous, len(boards))
}

func closedOnly(list []SprintReport) []SprintReport {
	var out []SprintReport
	for _, s := range list {
		if s.ClosedAt != nil {
			out = append(out, s)
		}
	}
	return out
}

// overall: predictability across boards, each board's share weighted by its closed sprints; goals.
func overall(boards []Report, pick func(Report) ReportTotals, sprints func(Report) []SprintReport) OverallTotals {
	var o OverallTotals
	sum := 0.0
	for _, b := range boards {
		t := pick(b)
		if t.Predictability != nil {
			sum += *t.Predictability * float64(t.Sprints)
			o.Sprints += t.Sprints
		}
		if sprints != nil {
			for _, s := range sprints(b) {
				if s.GoalMet != nil {
					o.GoalsSet++
					if *s.GoalMet {
						o.GoalsMet++
					}
				}
			}
		}
	}
	if o.Sprints > 0 {
		p := sum / float64(o.Sprints)
		o.Predictability = &p
	}
	return o
}

// months buckets closed sprints by the calendar month (in loc) they closed in; every month in the
// period is there, empty or not, so a chart's axis doesn't skip.
func months(boards []Report, from, to time.Time, loc *time.Location) []MonthTotals {
	type acc struct {
		m      MonthTotals
		share  float64
		shareN int
	}
	byMonth := map[string]*acc{}
	var order []string
	for m := time.Date(from.In(loc).Year(), from.In(loc).Month(), 1, 0, 0, 0, 0, loc); m.Before(to); m = m.AddDate(0, 1, 0) {
		key := m.Format("2006-01")
		byMonth[key] = &acc{m: MonthTotals{Month: key}}
		order = append(order, key)
	}
	for _, b := range boards {
		for _, s := range closedOnly(b.Sprints) {
			closed, _ := time.Parse(time.RFC3339, *s.ClosedAt)
			a := byMonth[closed.In(loc).Format("2006-01")]
			if a == nil {
				continue
			}
			a.m.Sprints++
			if b.Unit == "points" {
				a.m.PointsDone += s.Done
			} else {
				a.m.TicketsDone += s.Done
			}
			if s.Predictability != nil {
				a.share += *s.Predictability
				a.shareN++
			}
		}
	}
	out := make([]MonthTotals, 0, len(order))
	for _, key := range order {
		a := byMonth[key]
		if a.shareN > 0 {
			p := a.share / float64(a.shareN)
			a.m.Predictability = &p
		}
		out = append(out, a.m)
	}
	return out
}

func rollupSummary(now, before OverallTotals, boards int) string {
	if now.Sprints == 0 {
		return "No sprints finished in this period."
	}
	where := fmt.Sprintf("%d boards", boards)
	if boards == 1 {
		where = "1 board"
	}
	sprints := fmt.Sprintf("%d sprints", now.Sprints)
	if now.Sprints == 1 {
		sprints = "1 sprint"
	}
	line := fmt.Sprintf("Across %s, delivered %.0f%% of planned work in %s", where, *now.Predictability*100, sprints)
	if before.Predictability != nil {
		was, is := int(*before.Predictability*100+0.5), int(*now.Predictability*100+0.5)
		switch {
		case is > was:
			line += fmt.Sprintf(", up from %d%%", was)
		case is < was:
			line += fmt.Sprintf(", down from %d%%", was)
		default:
			line += ", the same as before"
		}
	}
	if now.GoalsSet > 0 {
		line += fmt.Sprintf(". %d of %d sprint goals met", now.GoalsMet, now.GoalsSet)
	}
	return line + "."
}

// --- HTTP ---

// ProjectReport: GET /api/projects/{id}/reports/sprints?from=&to=. Anyone in the project's organization.
func (h System) ProjectReport(w http.ResponseWriter, r *http.Request) {
	from, to, err := ReportRange(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	loc := httpx.Zone(r.Context())
	rep, err := h.store.ProjectReport(r.Context(), auth.OrgID(r.Context()), r.PathValue("id"), from, to, loc)
	if err != nil {
		writeErr(w, err)
		return
	}
	if wantsMarkdown(r) { // TL-399
		sendMarkdown(w, rep.Name, from, to, loc, RollupMarkdown(rep, from, to, loc, time.Now()))
		return
	}
	httpx.JSON(w, http.StatusOK, rep)
}

// OrgReport: GET /api/org/reports/sprints?from=&to=. Organization admins only.
func (h System) OrgReport(w http.ResponseWriter, r *http.Request) {
	org, _, ok := h.admins.RequireAdmin(w, r)
	if !ok {
		return
	}
	from, to, err := ReportRange(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	loc := httpx.Zone(r.Context())
	rep, err := h.store.OrgReport(r.Context(), org, from, to, loc)
	if err != nil {
		writeErr(w, err)
		return
	}
	if wantsMarkdown(r) { // TL-399
		sendMarkdown(w, "organization", from, to, loc, RollupMarkdown(rep, from, to, loc, time.Now()))
		return
	}
	httpx.JSON(w, http.StatusOK, rep)
}
