package imports

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/tracklines/backend/internal/httpx"
)

// CSV is a source for a spreadsheet of tickets (TL-637): any tool's export, or TrackLin.es's own. It
// makes one new project; a mapped status column becomes its board's columns (a done-like status
// last), and without one every ticket goes to the backlog.
// ponytail: the file comes inside the JSON request (≤ 1 MB, a few thousand tickets); upload it
// separately if bigger imports are wanted.
type CSV struct{}

// CSVOptions: the file, the new project, and which CSV column holds each field (by header).
type CSVOptions struct {
	CSV     string            `json:"csv"`
	Name    string            `json:"name"`
	Prefix  string            `json:"prefix"`
	Mapping map[string]string `json:"mapping"` // field → header: title (required), description, type, priority, labels, status, due, created
}

var (
	ErrCSVOptions = httpx.Code("csv_options", "send the CSV, the new project's name and key prefix, and which column is the title")
	ErrCSVRead    = httpx.Code("csv_unreadable", "that file couldn't be read as CSV")
)

// CSVFields is what a column can be mapped to.
var CSVFields = []string{"title", "description", "type", "priority", "labels", "status", "due", "created"}

var typeAliases = map[string]string{"bug": "bug", "defect": "bug", "feature": "feature", "story": "feature", "user story": "feature",
	"improvement": "feature", "enhancement": "feature", "task": "task", "chore": "task", "sub-task": "task", "subtask": "task",
	"epic": "epic", "spike": "spike", "research": "spike"}

var priorityAliases = map[string]string{"lowest": "low", "low": "low", "minor": "low", "trivial": "low", "medium": "medium", "normal": "medium",
	"high": "high", "major": "high", "highest": "urgent", "urgent": "urgent", "critical": "urgent", "blocker": "urgent"}

var doneWords = []string{"done", "closed", "complete", "completed", "resolved", "finished", "shipped", "released"}

// uncell undoes the export's formula guard ('=1+1 → =1+1) and trims.
func uncell(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 1 && s[0] == '\'' && strings.ContainsRune("=+-@\t\r", rune(s[1])) {
		return s[1:]
	}
	return s
}

func (CSV) Plan(_ context.Context, _ string, raw json.RawMessage) (Plan, error) {
	var o CSVOptions
	if err := json.Unmarshal(raw, &o); err != nil || strings.TrimSpace(o.CSV) == "" || strings.TrimSpace(o.Name) == "" || o.Mapping["title"] == "" {
		return Plan{}, ErrCSVOptions
	}
	r := csv.NewReader(strings.NewReader(strings.TrimPrefix(o.CSV, "\xef\xbb\xbf"))) // a spreadsheet's byte-order mark
	r.FieldsPerRecord = -1                                                           // ragged rows: missing cells read as empty
	r.LazyQuotes = true
	records, err := r.ReadAll()
	if err != nil || len(records) < 1 {
		return Plan{}, ErrCSVRead
	}
	header := records[0]
	col := map[string]int{} // field → index
	for field, name := range o.Mapping {
		if !slices.Contains(CSVFields, field) || name == "" {
			continue
		}
		i := slices.IndexFunc(header, func(h string) bool { return strings.EqualFold(strings.TrimSpace(uncell(h)), strings.TrimSpace(name)) })
		if i < 0 {
			return Plan{}, httpx.Code("csv_column", fmt.Sprintf("there's no column called %q", name))
		}
		col[field] = i
	}
	cell := func(row []string, field string) string {
		if i, ok := col[field]; ok && i < len(row) {
			return uncell(row[i])
		}
		return ""
	}

	pr := Project{SourceID: "csv", Name: strings.TrimSpace(o.Name), Prefix: strings.ToUpper(strings.TrimSpace(o.Prefix))}
	var plan Plan
	statuses := []string{} // in the order first seen; a done-like one goes last
	if _, ok := col["status"]; ok {
		for _, row := range records[1:] {
			if s := cell(row, "status"); s != "" && !slices.ContainsFunc(statuses, func(x string) bool { return strings.EqualFold(x, s) }) {
				statuses = append(statuses, s)
			}
		}
		done := slices.IndexFunc(statuses, func(s string) bool { return slices.Contains(doneWords, strings.ToLower(s)) })
		if done >= 0 {
			d := statuses[done]
			statuses = append(slices.Delete(statuses, done, done+1), d)
		} else {
			statuses = append(statuses, "Done")
		}
		if len(statuses) == 1 { // only a done status: give the board somewhere to start
			statuses = append([]string{"To do"}, statuses...)
		}
		b := Board{SourceID: "board", Name: pr.Name}
		for _, s := range statuses {
			b.Columns = append(b.Columns, Column{SourceID: strings.ToLower(s), Name: s})
		}
		pr.Boards = []Board{b}
	}
	for i, row := range records[1:] {
		if slices.IndexFunc(row, func(c string) bool { return strings.TrimSpace(c) != "" }) < 0 {
			continue // a blank line
		}
		t := Ticket{SourceID: fmt.Sprint("row ", i+2), Number: len(pr.Tickets) + 1, Title: cell(row, "title"), Description: cell(row, "description"), Type: "task"}
		if v := strings.ToLower(cell(row, "type")); v != "" {
			if known, ok := typeAliases[v]; ok {
				t.Type = known
			} else {
				plan.Skipped = append(plan.Skipped, Skip{Kind: "type", SourceID: t.SourceID, Reason: fmt.Sprintf("type %q isn't one TrackLin.es has: imported as a task", v)})
			}
		}
		if v := strings.ToLower(cell(row, "priority")); v != "" {
			t.Priority = priorityAliases[v] // unknown: the default, medium
		}
		for _, l := range strings.FieldsFunc(cell(row, "labels"), func(r rune) bool { return r == ';' || r == ',' || r == '|' }) {
			if l = strings.TrimSpace(l); l != "" {
				t.Labels = append(t.Labels, l)
			}
		}
		if s := cell(row, "status"); s != "" && len(pr.Boards) > 0 {
			t.Board, t.Column = "board", strings.ToLower(s)
		} else if len(pr.Boards) > 0 {
			t.Board, t.Column = "board", strings.ToLower(statuses[0]) // no status: the board's first column
		}
		if v := cell(row, "due"); v != "" {
			if d, ok := parseDay(v); ok {
				t.DueDate = &d
			}
		}
		if v := cell(row, "created"); v != "" {
			if at, ok := parseTime(v); ok {
				t.CreatedAt = &at
			}
		}
		pr.Tickets = append(pr.Tickets, t)
	}
	if len(pr.Tickets) == 0 {
		return Plan{}, errors.New("that file has no tickets in it")
	}
	plan.Projects = []Project{pr}
	return plan, nil
}

// parseDay reads a due date in the usual spreadsheet ways.
func parseDay(s string) (string, bool) {
	for _, layout := range []string{time.DateOnly, time.RFC3339, "2006-01-02 15:04:05", "02/01/2006", "2 Jan 2006", "Jan 2, 2006"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.Format(time.DateOnly), true
		}
	}
	return "", false
}

func parseTime(s string) (time.Time, bool) {
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", time.DateOnly, "02/01/2006", "2 Jan 2006"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}
