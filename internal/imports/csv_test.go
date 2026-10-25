package imports

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/tracklines/backend/internal/httpx"
)

func csvPlan(t *testing.T, file string, mapping map[string]string) (Plan, error) {
	t.Helper()
	raw, _ := json.Marshal(CSVOptions{CSV: file, Name: "Ops", Prefix: "ops", Mapping: mapping})
	return CSV{}.Plan(context.Background(), "org", raw)
}

// A CSV becomes one new project (TL-637): columns mapped by header (any case), statuses become the
// board's columns with a done-like one last, other tools' types and priorities understood.
func TestCSVPlan(t *testing.T) {
	file := "\xef\xbb\xbfSummary,Notes,Issue Type,Priority,Tags,Status,Due date\n" +
		"Fix login,\"line one\nline two\",Defect,Blocker,\"auth; ui\",Done,2026-11-02\n" +
		"Add search,,Story,Lowest,,In Progress,02/12/2026\n" +
		"'=SUM(A1),,Weird,whatever,,To Do,\n" +
		",,,,,,\n" + // blank: left out
		"Short row\n"
	p, err := csvPlan(t, file, map[string]string{"title": "summary", "description": "Notes", "type": "issue type", "priority": "Priority", "labels": "Tags", "status": "Status", "due": "Due date"})
	if err != nil {
		t.Fatal(err)
	}
	pr := p.Projects[0]
	if pr.Name != "Ops" || pr.Prefix != "OPS" || len(pr.Tickets) != 4 {
		t.Fatalf("project: %+v", pr)
	}
	var cols []string
	for _, c := range pr.Boards[0].Columns {
		cols = append(cols, c.Name)
	}
	if strings.Join(cols, ",") != "In Progress,To Do,Done" {
		t.Fatalf("columns: %v (done last)", cols)
	}
	a, b, c, d := pr.Tickets[0], pr.Tickets[1], pr.Tickets[2], pr.Tickets[3]
	if a.Type != "bug" || a.Priority != "urgent" || strings.Join(a.Labels, "|") != "auth|ui" || a.Description != "line one\nline two" || a.Column != "done" || *a.DueDate != "2026-11-02" {
		t.Fatalf("row 1: %+v", a)
	}
	if b.Type != "feature" || b.Priority != "low" || b.DueDate == nil || *b.DueDate != "2026-12-02" || b.Number != 2 {
		t.Fatalf("row 2: %+v", b)
	}
	if c.Title != "=SUM(A1)" || c.Type != "task" || c.Priority != "" { // the export's formula guard taken off; unknowns defaulted
		t.Fatalf("row 3: %+v", c)
	}
	if len(p.Skipped) != 1 || !strings.Contains(p.Skipped[0].Reason, `"weird"`) {
		t.Fatalf("unknown type should be reported: %+v", p.Skipped)
	}
	if d.Title != "Short row" || d.Column != "in progress" { // no status: the board's first column
		t.Fatalf("short row: %+v", d)
	}
}

func TestCSVPlanWithoutStatusGoesToTheBacklog(t *testing.T) {
	p, err := csvPlan(t, "Title\nOne\nTwo\n", map[string]string{"title": "Title"})
	if err != nil || len(p.Projects[0].Boards) != 0 || p.Projects[0].Tickets[1].Board != "" {
		t.Fatalf("%+v %v", p, err)
	}
}

func TestCSVPlanRefusesWhatItCantUse(t *testing.T) {
	var ce *httpx.CodedError
	if _, err := csvPlan(t, "Title\nx\n", map[string]string{"title": "Nope"}); !errors.As(err, &ce) || ce.Code != "csv_column" {
		t.Fatalf("missing column: %v", err)
	}
	if _, err := csvPlan(t, "Title\nx\n", map[string]string{}); !errors.Is(err, ErrCSVOptions) {
		t.Fatalf("no title mapping: %v", err)
	}
	if _, err := csvPlan(t, "Title\n", map[string]string{"title": "Title"}); err == nil {
		t.Fatal("no rows should be refused")
	}
	if _, err := csvPlan(t, "a,\"b\nc", map[string]string{"title": "a"}); !errors.Is(err, ErrCSVRead) && err == nil {
		t.Fatalf("unreadable: %v", err)
	}
}
