package boards

import (
	"fmt"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The Kanban flow report as Markdown (TL-621), like the sprint report (internal/sprints/markdown.go):
// Mermaid charts, each followed by a plain table of the same numbers. The few helpers are copied
// from there because sprints imports boards.
// ponytail: second copy of cell/label/mdDoc; move both into one package if a third report needs them.

// wantsMarkdown: ?format=md (or markdown).
func wantsMarkdown(r *http.Request) bool {
	f := r.URL.Query().Get("format")
	return f == "md" || f == "markdown"
}

// sendMarkdown sends doc as a download named after the board and the day.
func sendMarkdown(w http.ResponseWriter, board string, day time.Time, doc string) {
	file := fmt.Sprintf("%s-flow-%s.md", slug(board), day.Format(time.DateOnly))
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+file+`"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write([]byte(doc))
}

var notSlug = regexp.MustCompile(`[^a-z0-9]+`)

func slug(s string) string {
	out := strings.Trim(notSlug.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if out == "" {
		return "board"
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

func num(f float64) string { return strconv.FormatFloat(math.Round(f*10)/10, 'f', -1, 64) }

func optNum(f *float64) string {
	if f == nil {
		return "–"
	}
	return num(*f)
}

func deref(f *float64) float64 {
	if f == nil {
		return 0
	}
	return *f
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
				vs[i] = num(v)
			}
			d.line("    %s [%s]", kind, strings.Join(vs, ", "))
		}
	}
	series("bar", bars)
	series("line", lines)
	d.line("```")
	d.line("")
}

// pie writes a Mermaid pie of the non-zero slices; nothing when every value is zero.
func (d *mdDoc) pie(title string, names []string, values []int) {
	var slices []string
	for i, v := range values {
		if v > 0 {
			slices = append(slices, fmt.Sprintf(`    "%s" : %d`, label(names[i]), v))
		}
	}
	if len(slices) == 0 {
		return
	}
	d.line("```mermaid")
	d.line(`pie title %s`, label(title))
	d.line("%s", strings.Join(slices, "\n"))
	d.line("```")
	d.line("")
}

func typeName(t string) string {
	if t == "" {
		return t
	}
	return strings.ToUpper(t[:1]) + t[1:]
}

// FlowMarkdown is a board's flow report as a Markdown document. Weeks are UTC calendar weeks
// (as in the JSON); the generated time is in loc.
func FlowMarkdown(f *Flow, loc *time.Location, now time.Time) string {
	var d mdDoc
	d.line("# Flow report: %s", cell(f.BoardName))
	d.line("")
	period := ""
	if len(f.Throughput) > 0 {
		first, _ := time.Parse(time.DateOnly, f.Throughput[0].WeekStart)
		last, _ := time.Parse(time.DateOnly, f.Throughput[len(f.Throughput)-1].WeekStart)
		period = fmt.Sprintf(" (%s to %s)", first.Format("2 Jan 2006"), last.AddDate(0, 0, 6).Format("2 Jan 2006"))
	}
	d.line("Last %d weeks%s · generated %s", f.Weeks, period, now.In(loc).Format("2 Jan 2006 15:04"))
	d.line("")
	d.line("## Headlines")
	d.line("")
	d.table([]string{"Tickets done", "Median cycle time (days)", "85th percentile cycle time (days)"}, [][]string{{
		fmt.Sprint(len(f.Tickets)), optNum(f.MedianCycleDays), optNum(f.P85CycleDays),
	}})

	xs := make([]string, len(f.Throughput))
	done := make([]float64, len(f.Throughput))
	median := make([]float64, len(f.Throughput))
	p85 := make([]float64, len(f.Throughput))
	for i, w := range f.Throughput {
		day, _ := time.Parse(time.DateOnly, w.WeekStart)
		xs[i] = day.Format("2 Jan")
		done[i], median[i], p85[i] = float64(w.Tickets), deref(w.MedianCycleDays), deref(w.P85CycleDays)
	}
	d.line("## Throughput")
	d.line("")
	d.chart("Tickets done per week", "tickets", xs, [][]float64{done}, nil)
	rows := make([][]string, len(f.Throughput))
	for i, w := range f.Throughput {
		rows[i] = []string{w.WeekStart, fmt.Sprint(w.Tickets)}
	}
	d.table([]string{"Week of", "Tickets done"}, rows)

	d.line("## Cycle time")
	d.line("")
	d.line("Days from start to done, for tickets done each week. Weeks with nothing done chart as 0.")
	d.line("")
	d.chart("Median and 85th percentile cycle days", "days", xs, nil, [][]float64{median, p85})
	for i, w := range f.Throughput {
		rows[i] = []string{w.WeekStart, optNum(w.MedianCycleDays), optNum(w.P85CycleDays)}
	}
	d.table([]string{"Week of", "Median (days)", "85th percentile (days)"}, rows)

	d.line("## Work in progress")
	d.line("")
	names := make([]string, len(f.WIP))
	counts := make([]int, len(f.WIP))
	rows = make([][]string, len(f.WIP))
	for i, c := range f.WIP {
		names[i], counts[i] = c.Column, c.Tickets
		rows[i] = []string{cell(c.Column), fmt.Sprint(c.Tickets)}
	}
	d.pie("Open tickets by column", names, counts)
	d.table([]string{"Column", "Open tickets"}, rows)

	d.line("## Work mix")
	d.line("")
	names = make([]string, len(f.Mix))
	counts = make([]int, len(f.Mix))
	rows = make([][]string, len(f.Mix))
	for i, m := range f.Mix {
		names[i], counts[i] = typeName(m.Type), m.Tickets
		rows[i] = []string{cell(typeName(m.Type)), fmt.Sprint(m.Tickets)}
	}
	d.pie("Tickets done by type", names, counts)
	d.table([]string{"Type", "Tickets done"}, rows)
	return d.b.String()
}
