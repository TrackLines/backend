package sprints

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// Sprint reports as Markdown (TL-399), for wikis, Notion, GitHub and Confluence: charts as Mermaid
// xychart blocks (drawn where Mermaid is), each followed by a plain table of the same numbers
// (readable anywhere). The server writes it, so the app's download and the MCP tools say the same.

// wantsMarkdown: ?format=md (or markdown).
func wantsMarkdown(r *http.Request) bool {
	f := r.URL.Query().Get("format")
	return f == "md" || f == "markdown"
}

// sendMarkdown sends doc as a download named after the report and its days (to inclusive).
func sendMarkdown(w http.ResponseWriter, name string, from, to time.Time, loc *time.Location, doc string) {
	file := fmt.Sprintf("%s-sprints-%s-to-%s.md", slug(name), from.In(loc).Format(time.DateOnly), to.In(loc).AddDate(0, 0, -1).Format(time.DateOnly))
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+file+`"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write([]byte(doc))
}

var notSlug = regexp.MustCompile(`[^a-z0-9]+`)

func slug(s string) string {
	out := strings.Trim(notSlug.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if out == "" {
		return "report"
	}
	return out
}

// cell: text safe in a Markdown table cell: no breaking the table (|, newlines), no code spans or HTML.
func cell(s string) string {
	s = strings.NewReplacer("\\", "\\\\", "|", "\\|", "`", "\\`", "<", "&lt;", ">", "&gt;", "\r", " ", "\n", " ").Replace(s)
	return strings.TrimSpace(s)
}

// label: text safe inside a Mermaid quoted label.
func label(s string) string {
	return strings.TrimSpace(strings.NewReplacer(`"`, "'", "\n", " ", "\r", " ", "`", "'").Replace(s))
}

func n(f float64) string { return trim(f) }

func optN(f *float64) string {
	if f == nil {
		return "–"
	}
	return trim(*f)
}

func optPct(f *float64) string {
	if f == nil {
		return "–"
	}
	return fmt.Sprintf("%.0f%%", *f*100)
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
	d.line(`    title "%s"`, label(title))
	quoted := make([]string, len(xs))
	for i, x := range xs {
		quoted[i] = `"` + label(x) + `"`
	}
	d.line("    x-axis [%s]", strings.Join(quoted, ", "))
	d.line(`    y-axis "%s"`, label(yLabel))
	series := func(kind string, list [][]float64) {
		for _, values := range list {
			vs := make([]string, len(values))
			for i, v := range values {
				vs[i] = trim(v)
			}
			d.line("    %s [%s]", kind, strings.Join(vs, ", "))
		}
	}
	series("bar", bars)
	series("line", lines)
	d.line("```")
	d.line("")
}

type slice struct {
	name  string
	value float64
}

// pie writes a Mermaid pie of shares (TL-618). Zero slices are dropped (they draw nothing), and with
// nothing left there's no pie: the table under it still says so. Labels are quoted, so label() is
// enough: a colon inside the quotes doesn't end it.
func (d *mdDoc) pie(title string, slices []slice) {
	var lines []string
	for _, s := range slices {
		if s.value > 0 {
			lines = append(lines, fmt.Sprintf(`    "%s" : %s`, label(s.name), trim(s.value)))
		}
	}
	if len(lines) == 0 {
		return
	}
	d.line("```mermaid")
	d.line("pie showData")
	d.line("    title %s", label(title))
	for _, l := range lines {
		d.line("%s", l)
	}
	d.line("```")
	d.line("")
}

func sprintName(s SprintReport) string {
	if s.ClosedAt == nil {
		return fmt.Sprintf("Sprint %d (so far)", s.Number)
	}
	return fmt.Sprintf("Sprint %d", s.Number)
}

func period(from, to time.Time, loc *time.Location) string {
	return fmt.Sprintf("%s to %s", from.In(loc).Format("2 Jan 2006"), to.In(loc).AddDate(0, 0, -1).Format("2 Jan 2006"))
}

// BoardMarkdown is a board's report as a Markdown document.
func BoardMarkdown(r *Report, from, to time.Time, loc *time.Location, now time.Time) string {
	var d mdDoc
	d.line("# Sprint report: %s", cell(r.BoardName))
	d.line("")
	d.line("%s · generated %s · in %s", period(from, to, loc), now.In(loc).Format("2 Jan 2006 15:04"), r.Unit)
	d.line("")
	d.line("**%s**", cell(r.Summary))
	d.line("")
	t := r.Totals
	var change *float64
	if t.Added != nil && t.Planned > 0 {
		c := *t.Added / t.Planned
		change = &c
	}
	d.line("## Headlines")
	d.line("")
	d.table([]string{"Delivered", "Predictability", "Velocity", "Scope change", "Carried over"}, [][]string{{
		fmt.Sprintf("%s of %s %s", n(t.Done), n(t.Planned), r.Unit),
		optPct(t.Predictability) + trendWord(t.Predictability, r.Previous.Predictability),
		optN(t.Velocity) + " per sprint" + trendWord(t.Velocity, r.Previous.Velocity),
		optPct(change),
		optN(t.Carried),
	}})
	if len(r.Sprints) == 0 {
		d.line("No sprints in this period.")
		return d.b.String()
	}
	var xs, closedXs []string
	var planned, done, closedDone, velocity, predictability []float64
	for _, s := range r.Sprints {
		xs = append(xs, sprintName(s))
		planned = append(planned, s.Planned)
		done = append(done, s.Done)
		if s.ClosedAt != nil {
			closedXs = append(closedXs, sprintName(s))
			closedDone = append(closedDone, s.Done)
			velocity = append(velocity, deref(s.Velocity))
			predictability = append(predictability, deref(s.Predictability)*100)
		}
	}
	d.line("## Planned vs delivered")
	d.line("")
	d.chart("Planned (bars) vs delivered (line)", r.Unit, xs, [][]float64{planned}, [][]float64{done})
	rows := make([][]string, len(r.Sprints))
	for i, s := range r.Sprints {
		rows[i] = []string{sprintName(s), n(s.Planned), n(s.Done)}
	}
	d.table([]string{"Sprint", "Planned", "Delivered"}, rows)
	if len(closedXs) > 0 {
		d.line("## Velocity")
		d.line("")
		d.chart("Delivered (bars) and 3-sprint average (line)", r.Unit, closedXs, [][]float64{closedDone}, [][]float64{velocity})
		rows = nil
		for i := range closedXs {
			rows = append(rows, []string{closedXs[i], n(closedDone[i]), n(velocity[i])})
		}
		d.table([]string{"Sprint", "Delivered", "3-sprint average"}, rows)
		d.line("## Predictability")
		d.line("")
		d.line("Share of planned work done. 80–100% is on target.")
		d.line("")
		d.chart("Predictability %", "%", closedXs, nil, [][]float64{predictability})
		rows = nil
		for i := range closedXs {
			rows = append(rows, []string{closedXs[i], fmt.Sprintf("%.0f%%", predictability[i])})
		}
		d.table([]string{"Sprint", "Predictability"}, rows)
	}
	var scopeXs []string
	var added, removed, carried []float64
	for _, s := range r.Sprints {
		if s.ClosedAt != nil && s.Added != nil {
			scopeXs = append(scopeXs, sprintName(s))
			added, removed, carried = append(added, *s.Added), append(removed, deref(s.Removed)), append(carried, deref(s.Carried))
		}
	}
	if len(scopeXs) > 0 {
		d.line("## Scope change")
		d.line("")
		d.chart("Added, removed and carried over", r.Unit, scopeXs, [][]float64{added, removed, carried}, nil)
		rows = nil
		for i := range scopeXs {
			rows = append(rows, []string{scopeXs[i], n(added[i]), n(removed[i]), n(carried[i])})
		}
		d.table([]string{"Sprint", "Added", "Removed", "Carried over"}, rows)
	}
	if len(t.Mix) > 0 {
		d.line("## Work mix")
		d.line("")
		rows = nil
		var mix []slice
		for _, m := range t.Mix {
			rows = append(rows, []string{typeName(m.Type), fmt.Sprint(m.Count), n(m.Value)})
			mix = append(mix, slice{typeName(m.Type), m.Value})
		}
		d.pie("Delivered "+r.Unit+" by type", mix)
		d.table([]string{"Type", "Tickets", strings.ToUpper(r.Unit[:1]) + r.Unit[1:]}, rows)
	}
	var goals [][]string
	var met, missed float64
	for _, s := range r.Sprints {
		if s.Goal == "" {
			continue
		}
		status := "In progress"
		if s.GoalMet != nil && *s.GoalMet {
			status, met = "Met", met+1
		} else if s.GoalMet != nil {
			status, missed = "Not met", missed+1
		}
		goals = append(goals, []string{sprintName(s), cell(s.Goal), status})
	}
	if len(goals) > 0 {
		d.line("## Sprint goals")
		d.line("")
		// closed sprints only: one still running hasn't met or missed its goal yet
		d.pie("Sprint goals met", []slice{{"Met", met}, {"Not met", missed}})
		d.table([]string{"Sprint", "Goal", ""}, goals)
	}
	d.line("## Sprint by sprint")
	d.line("")
	rows = nil
	estimated := false
	for _, s := range r.Sprints {
		p := n(s.Planned)
		if s.Estimated {
			p += "*"
			estimated = true
		}
		rows = append(rows, []string{sprintName(s), p, optN(s.Added), optN(s.Removed), n(s.Done), optN(s.Carried), optPct(s.Predictability)})
	}
	d.table([]string{"Sprint", "Planned", "Added", "Removed", "Done", "Carried over", "Predictability"}, rows)
	if estimated {
		d.line("\\* From before reports were recorded: planned is what the sprint held when it closed; added and removed aren't known.")
		d.line("")
	}
	return d.b.String()
}

// RollupMarkdown is a project's or the organization's report as a Markdown document.
func RollupMarkdown(r *Rollup, from, to time.Time, loc *time.Location, now time.Time) string {
	var d mdDoc
	d.line("# Sprint report: %s", cell(r.Name))
	d.line("")
	d.line("%s · generated %s", period(from, to, loc), now.In(loc).Format("2 Jan 2006 15:04"))
	d.line("")
	d.line("**%s**", cell(r.Summary))
	d.line("")
	goals := "–"
	if r.Overall.GoalsSet > 0 {
		goals = fmt.Sprintf("%d of %d", r.Overall.GoalsMet, r.Overall.GoalsSet)
	}
	delivered := func(t *ReportTotals) string {
		if t == nil {
			return "–"
		}
		return fmt.Sprintf("%s of %s", n(t.Done), n(t.Planned))
	}
	d.line("## Headlines")
	d.line("")
	d.table([]string{"Predictability", "Sprint goals met", "Points delivered", "Tickets delivered"}, [][]string{{
		optPct(r.Overall.Predictability) + trendWord(r.Overall.Predictability, r.Previous.Predictability), goals, delivered(r.Points), delivered(r.Tickets),
	}})
	type part struct {
		name, delivered string
		sprints         int
		predictability  *float64
		points, tickets float64 // delivered, for the share pies
	}
	var parts []part
	what := "Board"
	if r.Boards != nil {
		for _, b := range r.Boards {
			pt := part{name: b.BoardName, delivered: fmt.Sprintf("%s %s", n(b.Totals.Done), b.Unit), sprints: b.Totals.Sprints, predictability: b.Totals.Predictability}
			if b.Unit == "points" {
				pt.points = b.Totals.Done
			} else {
				pt.tickets = b.Totals.Done
			}
			parts = append(parts, pt)
		}
	} else {
		what = "Project"
		for _, p := range r.Projects {
			pt := part{name: p.Name, sprints: p.Overall.Sprints, predictability: p.Overall.Predictability}
			var list []string
			if p.Points != nil {
				list = append(list, n(p.Points.Done)+" points")
				pt.points = p.Points.Done
			}
			if p.Tickets != nil {
				list = append(list, n(p.Tickets.Done)+" tickets")
				pt.tickets = p.Tickets.Done
			}
			pt.delivered = strings.Join(list, ", ")
			parts = append(parts, pt)
		}
	}
	if len(parts) > 0 {
		d.line("## %ss side by side", what)
		d.line("")
		xs := make([]string, len(parts))
		ps := make([]float64, len(parts))
		rows := make([][]string, len(parts))
		var points, tickets []slice
		for i, p := range parts {
			xs[i], ps[i] = p.name, deref(p.predictability)*100
			rows[i] = []string{cell(p.name), fmt.Sprint(p.sprints), optPct(p.predictability), cell(p.delivered)}
			points, tickets = append(points, slice{p.name, p.points}), append(tickets, slice{p.name, p.tickets})
		}
		d.chart("Predictability % by "+strings.ToLower(what), "%", xs, [][]float64{ps}, nil)
		// points and tickets don't add up together, so a pie each
		d.pie("Points delivered by "+strings.ToLower(what), points)
		d.pie("Tickets delivered by "+strings.ToLower(what), tickets)
		d.table([]string{what, "Sprints", "Predictability", "Delivered"}, rows)
	}
	if o := r.Overall; o.GoalsSet > 0 {
		d.line("## Sprint goals")
		d.line("")
		d.pie("Sprint goals met", []slice{{"Met", float64(o.GoalsMet)}, {"Not met", float64(o.GoalsSet - o.GoalsMet)}})
		d.table([]string{"Met", "Not met"}, [][]string{{fmt.Sprint(o.GoalsMet), fmt.Sprint(o.GoalsSet - o.GoalsMet)}})
	}
	if len(r.Months) > 0 {
		d.line("## By month")
		d.line("")
		xs := make([]string, len(r.Months))
		ps := make([]float64, len(r.Months))
		rows := make([][]string, len(r.Months))
		for i, m := range r.Months {
			xs[i], ps[i] = m.Month, deref(m.Predictability)*100
			rows[i] = []string{m.Month, fmt.Sprint(m.Sprints), optPct(m.Predictability), n(m.PointsDone), n(m.TicketsDone)}
		}
		d.chart("Predictability % by month", "%", xs, nil, [][]float64{ps})
		d.table([]string{"Month", "Sprints", "Predictability", "Points done", "Tickets done"}, rows)
	}
	return d.b.String()
}

func deref(f *float64) float64 {
	if f == nil {
		return 0
	}
	return *f
}

// trendWord: " (up from 71%)" style notes; empty with nothing to compare.
func trendWord(now, before *float64) string {
	if now == nil || before == nil {
		return ""
	}
	a, b := int(*now*100+0.5), int(*before*100+0.5)
	switch {
	case a > b:
		return " ↑"
	case a < b:
		return " ↓"
	}
	return " →"
}

func typeName(t string) string {
	switch t {
	case "bug":
		return "Bugs"
	case "feature":
		return "Features"
	case "task":
		return "Tasks"
	}
	return "Epics"
}
