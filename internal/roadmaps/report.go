package roadmaps

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/tracklines/backend/internal/httpx"
	"github.com/tracklines/backend/internal/workweek"
)

// Roadmap reports (TL-616): the burn-up as JSON for the website's PDF, or the whole report as
// Markdown (?format=md) for wikis and the MCP tool, charts as Mermaid with a plain table of the same
// numbers under each, like the sprint reports (TL-399).

// Week is one point of the burn-up: tickets counted towards the roadmap that existed, and were done,
// by the end of the week starting WeekStart (a Monday, in the caller's zone).
type Week struct {
	WeekStart string `json:"week_start"`
	Done      int    `json:"done"`
	Total     int    `json:"total"`
}

// stamp: when a counted ticket was created and, if it's done, done.
type stamp struct {
	created time.Time
	done    *time.Time
}

// stamps returns the roadmap's counted tickets (the progress rule), each once.
func (s Store) stamps(ctx context.Context, id string) ([]stamp, error) {
	rows, err := s.DB.Query(ctx, `SELECT DISTINCT ON (c.id) c.created_at, c.done_at FROM `+countedSQL+` c`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []stamp
	for rows.Next() {
		var st stamp
		if err := rows.Scan(&st.created, &st.done); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// burnWeeks is the most a burn-up looks back, so an old ticket doesn't stretch it over years.
const burnWeeks = 12

// weekOf is the first day (at midnight, in loc) of the week t is in, as the project's working week starts it (TL-702).
func weekOf(t time.Time, loc *time.Location, week workweek.Week) time.Time {
	return week.StartOfWeek(t.In(loc))
}

// burnup: weekly from the earliest ticket's week (at most burnWeeks back) to now's week; none
// without tickets.
func burnup(stamps []stamp, now time.Time, loc *time.Location, week workweek.Week) []Week {
	out := []Week{}
	if len(stamps) == 0 {
		return out
	}
	this := weekOf(now, loc, week)
	start := this.AddDate(0, 0, -7*burnWeeks)
	earliest := stamps[0].created
	for _, s := range stamps {
		if s.created.Before(earliest) {
			earliest = s.created
		}
	}
	if first := weekOf(earliest, loc, week); first.After(start) {
		start = first
	}
	for w := start; !w.After(this); w = w.AddDate(0, 0, 7) {
		end := w.AddDate(0, 0, 7)
		week := Week{WeekStart: w.Format(time.DateOnly)}
		for _, s := range stamps {
			if s.created.Before(end) {
				week.Total++
				if s.done != nil && s.done.Before(end) {
					week.Done++
				}
			}
		}
		out = append(out, week)
	}
	return out
}

// Report: the burn-up as JSON, or ?format=md the Markdown report. For those who manage the roadmap.
func (h System) Report(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !h.manager(w, r, id, "") {
		return
	}
	ctx := r.Context()
	loc, now := httpx.Zone(ctx), time.Now()
	stamps, err := h.store.stamps(ctx, id)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	ww := workweek.Default() // the roadmap's project's week, where it has one
	var project *string
	if h.store.DB.QueryRow(ctx, `SELECT project_id::text FROM roadmaps WHERE id::text = $1`, id).Scan(&project) == nil && project != nil {
		if got, err := (workweek.Store{DB: h.store.DB}).ForProject(ctx, *project); err == nil {
			ww = got
		}
	}
	weeks := burnup(stamps, now, loc, ww)
	if f := r.URL.Query().Get("format"); f != "md" && f != "markdown" {
		httpx.JSON(w, http.StatusOK, map[string]any{"burnup": weeks})
		return
	}
	// ponytail: manager already read the roadmap; read again rather than thread it out
	rm, err := h.store.Get(ctx, id)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	updates, err := h.store.ListUpdates(ctx, id)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	file := fmt.Sprintf("%s-roadmap-%s.md", slug(rm.Title), now.In(loc).Format(time.DateOnly))
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+file+`"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write([]byte(Markdown(rm, updates, weeks, now, loc)))
}

// Item statuses as the report shows them; late: past its target date and not done.
const (
	shownDone       = "Done"
	shownInProgress = "In progress"
	shownNotStarted = "Not started"
	shownLate       = "Late"
)

func shownStatus(i Item, today string) string {
	switch {
	case i.Status == StatusDone:
		return shownDone
	case i.TargetDate != nil && *i.TargetDate != "" && *i.TargetDate < today:
		return shownLate
	case i.Status == StatusInProgress:
		return shownInProgress
	}
	return shownNotStarted
}

func healthName(h string) string {
	switch h {
	case HealthOnTrack:
		return "On track"
	case HealthAtRisk:
		return "At risk"
	case HealthOffTrack:
		return "Off track"
	}
	return h
}

func date(s *string) string {
	if s == nil || *s == "" {
		return "–"
	}
	return *s
}

func pct(p Progress) string {
	if p.Total == 0 {
		return "–"
	}
	return fmt.Sprintf("%d of %d (%.0f%%)", p.Done, p.Total, float64(p.Done)*100/float64(p.Total))
}

// published: an update's date in loc.
func published(at string, loc *time.Location) string {
	t, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return cell(at)
	}
	return t.In(loc).Format("2 Jan 2006")
}

// Markdown is the roadmap's report as a Markdown document.
func Markdown(rm *Roadmap, updates []Update, weeks []Week, now time.Time, loc *time.Location) string {
	var d mdDoc
	today := now.In(loc).Format(time.DateOnly)
	d.line("# Roadmap report: %s", cell(rm.Title))
	d.line("")
	d.line("Generated %s", now.In(loc).Format("2 Jan 2006 15:04"))
	d.line("")
	if desc := strings.TrimSpace(rm.Description); desc != "" {
		d.line("%s", desc)
		d.line("")
	}
	d.line("**Overall progress: %s tickets done**", pct(rm.Progress))
	d.line("")
	if len(updates) > 0 {
		u := updates[len(updates)-1]
		d.line("## Latest update")
		d.line("")
		d.table([]string{"Date", "Health", "Note", "Author"}, [][]string{{published(u.PublishedAt, loc), healthName(u.Health), cell(u.Note), cell(u.Author)}})
	}

	if len(rm.Items) > 0 {
		d.line("## Items")
		d.line("")
		for n, i := range rm.Items {
			d.line("### %d. %s", n+1, cell(i.Title))
			d.line("")
			d.line("%s → %s · %s · progress %s", date(i.StartDate), date(i.TargetDate), shownStatus(i, today), pct(i.Progress))
			d.line("")
			if desc := strings.TrimSpace(i.Description); desc != "" {
				d.line("%s", desc)
				d.line("")
			}
			if len(i.Tickets) > 0 {
				rows := make([][]string, len(i.Tickets))
				for k, t := range i.Tickets {
					done := "No"
					if t.Done {
						done = "Yes"
					}
					key := cell(t.Key)
					if key == "" {
						key = "–"
					}
					rows[k] = []string{key, cell(t.Title), done}
				}
				d.table([]string{"Key", "Ticket", "Done"}, rows)
			}
		}

		d.line("## Items by status")
		d.line("")
		counts := map[string]int{}
		for _, i := range rm.Items {
			counts[shownStatus(i, today)]++
		}
		order := []string{shownDone, shownInProgress, shownNotStarted, shownLate}
		d.line("```mermaid")
		d.line("pie title Items by status")
		for _, s := range order {
			if counts[s] > 0 {
				d.line(`    "%s" : %d`, s, counts[s])
			}
		}
		d.line("```")
		d.line("")
		rows := make([][]string, len(order))
		for k, s := range order {
			rows[k] = []string{s, strconv.Itoa(counts[s])}
		}
		d.table([]string{"Status", "Items"}, rows)

		d.line("## Timeline")
		d.line("")
		var tasks []string
		rows = nil
		for n, i := range rm.Items {
			status := shownStatus(i, today)
			rows = append(rows, []string{cell(i.Title), date(i.StartDate), date(i.TargetDate), status})
			if i.TargetDate == nil || *i.TargetDate == "" {
				continue // no dates: in the table only
			}
			name := mermaidLabel(i.Title)
			if name == "" {
				name = fmt.Sprintf("Item %d", n+1)
			}
			var tags []string
			switch status {
			case shownDone:
				tags = append(tags, "done")
			case shownLate:
				tags = append(tags, "crit")
			case shownInProgress:
				tags = append(tags, "active")
			}
			start, end := date(i.StartDate), *i.TargetDate
			if start == "–" {
				tags, start, end = append(tags, "milestone"), end, "0d" // a target alone is a point in time
			}
			tags = append(tags, fmt.Sprintf("i%d", n+1))
			tasks = append(tasks, fmt.Sprintf("    %s :%s, %s, %s", name, strings.Join(tags, ", "), start, end))
		}
		if len(tasks) > 0 {
			d.line("```mermaid")
			d.line("gantt")
			d.line("    title %s", mermaidLabel(rm.Title))
			d.line("    dateFormat YYYY-MM-DD")
			d.line("    section Items")
			for _, t := range tasks {
				d.line("%s", t)
			}
			d.line("```")
			d.line("")
		}
		d.table([]string{"Item", "Start", "Target", "Status"}, rows)

		d.line("## Tickets per item")
		d.line("")
		xs := make([]string, len(rm.Items))
		var done, total []float64
		rows = nil
		for k, i := range rm.Items {
			xs[k] = i.Title
			done, total = append(done, float64(i.Progress.Done)), append(total, float64(i.Progress.Total))
			rows = append(rows, []string{cell(i.Title), strconv.Itoa(i.Progress.Done), strconv.Itoa(i.Progress.Total)})
		}
		d.chart("Total (back) and done (front) tickets", "tickets", xs, [][]float64{total, done}, nil)
		d.table([]string{"Item", "Done", "Total"}, rows)
	}

	if len(weeks) > 0 {
		d.line("## Burn-up")
		d.line("")
		xs := make([]string, len(weeks))
		var done, total []float64
		rows := make([][]string, len(weeks))
		for k, wk := range weeks {
			xs[k] = wk.WeekStart
			done, total = append(done, float64(wk.Done)), append(total, float64(wk.Total))
			rows[k] = []string{wk.WeekStart, strconv.Itoa(wk.Done), strconv.Itoa(wk.Total)}
		}
		d.chart("Done and total tickets by week", "tickets", xs, nil, [][]float64{done, total})
		d.table([]string{"Week of", "Done", "Total"}, rows)
	}

	if len(updates) > 0 {
		d.line("## Health over time")
		d.line("")
		rows := make([][]string, len(updates))
		for k, u := range updates {
			rows[k] = []string{published(u.PublishedAt, loc), healthName(u.Health), cell(u.Note), cell(u.Author)}
		}
		d.table([]string{"Date", "Health", "Note", "Author"}, rows)
	}
	return d.b.String()
}

// Markdown helpers, as in sprints/markdown.go (a copy: too small to share a package for).

var notSlug = regexp.MustCompile(`[^a-z0-9]+`)

func slug(s string) string {
	out := strings.Trim(notSlug.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if out == "" {
		return "roadmap"
	}
	return out
}

// cell: text safe in a Markdown table cell: no breaking the table (|, newlines), no code spans or HTML.
func cell(s string) string {
	s = strings.NewReplacer("\\", "\\\\", "|", "\\|", "`", "\\`", "<", "&lt;", ">", "&gt;", "\r", " ", "\n", " ").Replace(s)
	return strings.TrimSpace(s)
}

// mermaidLabel: text safe as a Mermaid label, quoted or not (gantt task names aren't): no quotes,
// colons (gantt's separator), semicolons (statement ends), # (entity codes), HTML or newlines.
func mermaidLabel(s string) string {
	s = strings.NewReplacer(`"`, "'", "`", "'", ":", " -", ";", ",", "#", "", "<", "", ">", "", "\r", " ", "\n", " ").Replace(s)
	return strings.Join(strings.Fields(s), " ")
}

type mdDoc struct{ b strings.Builder }

func (d *mdDoc) line(format string, args ...any) { fmt.Fprintf(&d.b, format+"\n", args...) }

// table writes a header and rows, cells already safe.
func (d *mdDoc) table(head []string, rows [][]string) {
	d.line("| %s |", strings.Join(head, " | "))
	seps := make([]string, len(head))
	for i := range seps {
		seps[i] = "---"
	}
	d.line("| %s |", strings.Join(seps, " | "))
	for _, r := range rows {
		d.line("| %s |", strings.Join(r, " | "))
	}
	d.line("")
}

// chart writes a Mermaid xychart: bars and/or lines over the same x labels.
func (d *mdDoc) chart(title, yLabel string, xs []string, bars, lines [][]float64) {
	d.line("```mermaid")
	d.line("xychart-beta")
	d.line(`    title "%s"`, mermaidLabel(title))
	quoted := make([]string, len(xs))
	for i, x := range xs {
		quoted[i] = `"` + mermaidLabel(x) + `"`
	}
	d.line("    x-axis [%s]", strings.Join(quoted, ", "))
	d.line(`    y-axis "%s"`, mermaidLabel(yLabel))
	series := func(kind string, list [][]float64) {
		for _, values := range list {
			vs := make([]string, len(values))
			for i, v := range values {
				vs[i] = strconv.FormatFloat(v, 'f', -1, 64)
			}
			d.line("    %s [%s]", kind, strings.Join(vs, ", "))
		}
	}
	series("bar", bars)
	series("line", lines)
	d.line("```")
	d.line("")
}
