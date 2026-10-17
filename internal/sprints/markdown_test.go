package sprints

import (
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

func fp(f float64) *float64 { return &f }
func sp(s string) *string   { return &s }
func bp(b bool) *bool       { return &b }

var mdReport = &Report{
	BoardName: "Back|end <b>", Unit: "points",
	Summary: "Delivered 67% of planned work across 2 sprints, up from 25%.",
	Sprints: []SprintReport{
		{Number: 1, ClosedAt: sp("2026-08-15T00:00:00Z"), Goal: "Old `goal`", GoalMet: bp(false), Planned: 4, Estimated: true, Done: 1, Predictability: fp(0.25), Carried: fp(3), Velocity: fp(1)},
		{Number: 2, ClosedAt: sp("2026-08-29T00:00:00Z"), Goal: "Ship | search <script>x</script>\nnow", GoalMet: bp(true), Planned: 3, Done: 2, Predictability: fp(0.667), Added: fp(1), Removed: fp(1), Carried: fp(0), Velocity: fp(1.5),
			Mix: []Mix{{Type: "bug", Count: 1, Value: 1}}},
		{Number: 3, Goal: "Billing", Planned: 5, Done: 1, Predictability: fp(0.2), Added: fp(0), Removed: fp(0)},
	},
	Totals:   ReportTotals{Sprints: 2, Planned: 7, Done: 3, Predictability: fp(0.43), Added: fp(1), Carried: fp(3), Velocity: fp(1.5), Mix: []Mix{{Type: "bug", Count: 2, Value: 2}, {Type: "feature", Count: 1, Value: 1}}},
	Previous: ReportTotals{Sprints: 1, Predictability: fp(0.25), Velocity: fp(1)},
}

var (
	mdFrom = time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	mdTo   = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	mdNow  = time.Date(2026, 10, 17, 9, 30, 0, 0, time.UTC)
)

// TL-399: the board report as Markdown: every section, in order; charts as Mermaid with the same
// numbers in a table under each; nothing in a title or goal can break a table or add markup.
func TestBoardMarkdown(t *testing.T) {
	md := BoardMarkdown(mdReport, mdFrom, mdTo, time.UTC, mdNow)
	order := []string{"# Sprint report: Back\\|end &lt;b&gt;", "1 Aug 2026 to 30 Sep 2026 · generated 17 Oct 2026 09:30 · in points",
		"**Delivered 67% of planned work across 2 sprints, up from 25%.**", "## Headlines", "## Planned vs delivered", "## Velocity",
		"## Predictability", "## Scope change", "## Work mix", "## Sprint goals", "## Sprint by sprint", "\\* From before reports were recorded"}
	at := 0
	for _, s := range order {
		i := strings.Index(md[at:], s)
		if i < 0 {
			t.Fatalf("missing or out of order: %q\n%s", s, md)
		}
		at += i
	}
	// headlines with trends against the period before
	if !strings.Contains(md, "| 3 of 7 points | 43% ↑ | 1.5 per sprint ↑ | 14% | 3 |") {
		t.Errorf("headlines:\n%s", md)
	}
	// the planned-vs-delivered chart holds the table's numbers
	if !strings.Contains(md, `x-axis ["Sprint 1", "Sprint 2", "Sprint 3 (so far)"]`) || !strings.Contains(md, "bar [4, 3, 5]") || !strings.Contains(md, "line [1, 2, 1]") {
		t.Errorf("planned vs delivered chart:\n%s", md)
	}
	for _, row := range []string{"| Sprint 1 | 4 | 1 |", "| Sprint 2 | 3 | 2 |", "| Sprint 3 (so far) | 5 | 1 |"} {
		if !strings.Contains(md, row) {
			t.Errorf("planned vs delivered table missing %q", row)
		}
	}
	// scope change only for sprints that recorded it; predictability for closed sprints
	if !strings.Contains(md, "bar [1]\n    bar [1]\n    bar [0]") || !strings.Contains(md, "line [25, 66.7]") {
		t.Errorf("scope/predictability charts:\n%s", md)
	}
	// goals escaped: no table break, no HTML, no code span, one line
	if !strings.Contains(md, "| Sprint 2 | Ship \\| search &lt;script&gt;x&lt;/script&gt; now | Met |") || !strings.Contains(md, "| Sprint 1 | Old \\`goal\\` | Not met |") || !strings.Contains(md, "| Sprint 3 (so far) | Billing | In progress |") {
		t.Errorf("goals:\n%s", md)
	}
	if strings.Contains(md, "<script") || strings.Contains(md, "<b>") {
		t.Error("raw HTML got through")
	}
	// every table row has the same number of cells as its header
	for _, block := range strings.Split(md, "\n\n") {
		lines := strings.Split(strings.TrimSpace(block), "\n")
		if !strings.HasPrefix(lines[0], "| ") {
			continue
		}
		cells := func(l string) int { return len(regexp.MustCompile(`(^|[^\\])\|`).FindAllString(l, -1)) }
		for _, l := range lines[1:] {
			if cells(l) != cells(lines[0]) {
				t.Errorf("table row with the wrong number of cells: %q (header %q)", l, lines[0])
			}
		}
	}
	for _, banned := range []string{"assign", "user_", "author"} {
		if strings.Contains(strings.ToLower(md), banned) {
			t.Errorf("mentions people (%q)", banned)
		}
	}
	if got := BoardMarkdown(&Report{BoardName: "Empty", Unit: "tickets", Summary: "No sprints finished in this period."}, mdFrom, mdTo, time.UTC, mdNow); !strings.Contains(got, "No sprints in this period.") || strings.Contains(got, "```mermaid") {
		t.Errorf("empty report:\n%s", got)
	}
}

func TestRollupMarkdown(t *testing.T) {
	r := &Rollup{Name: "Shop", Summary: "Across 2 boards, delivered 75% of planned work in 2 sprints.",
		Boards: []Report{{BoardName: "Backend", Unit: "points", Totals: ReportTotals{Sprints: 1, Done: 8, Predictability: fp(1)}}, {BoardName: "Design", Unit: "tickets", Totals: ReportTotals{Sprints: 1, Done: 1, Predictability: fp(0.5)}}},
		Points: &ReportTotals{Done: 8, Planned: 8}, Tickets: &ReportTotals{Done: 1, Planned: 2},
		Overall: OverallTotals{Sprints: 2, Predictability: fp(0.75), GoalsSet: 2, GoalsMet: 1}, Previous: OverallTotals{Predictability: fp(0.8)},
		Months: []MonthTotals{{Month: "2026-08", Sprints: 1, Predictability: fp(1), PointsDone: 8}, {Month: "2026-09", Sprints: 1, Predictability: fp(0.5), TicketsDone: 1}},
	}
	md := RollupMarkdown(r, mdFrom, mdTo, time.UTC, mdNow)
	for _, s := range []string{"# Sprint report: Shop", "| 75% ↓ | 1 of 2 | 8 of 8 | 1 of 2 |", "## Boards side by side", `x-axis ["Backend", "Design"]`, "bar [100, 50]",
		"| Backend | 1 | 100% | 8 points |", "| Design | 1 | 50% | 1 tickets |", "## By month", "line [100, 50]", "| 2026-09 | 1 | 50% | 0 | 1 |"} {
		if !strings.Contains(md, s) {
			t.Errorf("missing %q\n%s", s, md)
		}
	}
	r.Boards, r.Projects = nil, []Rollup{{Name: "Shop", Points: &ReportTotals{Done: 8}, Overall: OverallTotals{Sprints: 2, Predictability: fp(0.75)}}}
	if md := RollupMarkdown(r, mdFrom, mdTo, time.UTC, mdNow); !strings.Contains(md, "## Projects side by side") || !strings.Contains(md, "| Shop | 2 | 75% | 8 points |") {
		t.Errorf("org markdown:\n%s", md)
	}
}

func TestSendMarkdown(t *testing.T) {
	rec := httptest.NewRecorder()
	sendMarkdown(rec, "Back|end <b>", mdFrom, mdTo, time.UTC, "# x")
	if rec.Header().Get("Content-Type") != "text/markdown; charset=utf-8" || rec.Header().Get("Content-Disposition") != `attachment; filename="back-end-b-sprints-2026-08-01-to-2026-09-30.md"` || rec.Body.String() != "# x" {
		t.Errorf("headers: %v %q", rec.Header(), rec.Body)
	}
	if slug("!!!") != "report" || cell(" a|b\n`c` ") != "a\\|b \\`c\\`" || label(`say "hi"`) != "say 'hi'" {
		t.Error("slug, cell or label")
	}
}
