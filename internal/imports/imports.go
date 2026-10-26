// Package imports brings another tracker's work into TrackLin.es (TL-536): a source (Jira, later
// Linear) turns its data into a Plan; the framework previews it against the org's plan limits,
// runs it in the background once confirmed, reports what it created and skipped, and can scrap
// (undo) it while nobody has acted on what it imported. Import once, no sync.
package imports

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tracklines/backend/internal/boards"
	"github.com/tracklines/backend/internal/tickets"
)

// Source turns a tracker's data into a Plan; options are the source's own (which projects, credentials…).
type Source interface {
	Plan(ctx context.Context, org string, options json.RawMessage) (Plan, error)
}

// Plan is everything an import creates: new projects, with their boards and tickets.
type Plan struct {
	Projects []Project `json:"projects"`
	Skipped  []Skip    `json:"skipped,omitempty"` // what the source left out already, and why
	People   []Person  `json:"people,omitempty"`  // who the source's tickets are assigned to, and whether they matched a member
	Notes    []Note    `json:"notes,omitempty"`   // what the preview should say that isn't a problem or a skip (Linear's renumbered teams)
}

// Note is something worth knowing before running an import.
type Note struct {
	Kind     string `json:"kind"` // renumbered
	SourceID string `json:"source_id,omitempty"`
	Message  string `json:"message"`
}

// Person is someone tickets are assigned to in the source: matched to a member (by email), or not,
// and then their tickets are left unassigned.
type Person struct {
	Name    string `json:"name"`
	Matched bool   `json:"matched"`
	Tickets int    `json:"tickets"`
}

type Project struct {
	SourceID    string   `json:"source_id"`
	Name        string   `json:"name"`
	Prefix      string   `json:"prefix"` // its key prefix: the source's key where possible
	Description string   `json:"description,omitempty"`
	Boards      []Board  `json:"boards"`
	Tickets     []Ticket `json:"tickets"`
}

type Board struct {
	SourceID      string   `json:"source_id"`
	Name          string   `json:"name"`
	EstimateScale string   `json:"estimate_scale,omitempty"` // default none
	TeamID        *string  `json:"team_id,omitempty"`        // the org's team that runs it
	Columns       []Column `json:"columns"`                  // in order: the first is To do, the last Done
}

type Column struct {
	SourceID string `json:"source_id"` // unique within its board (Jira statuses repeat across boards)
	Name     string `json:"name"`
}

// Ticket goes on its board's column, or in the project backlog when it has no board.
type Ticket struct {
	SourceID    string     `json:"source_id"`
	Number      int        `json:"number"` // kept: TL-12 stays TL-12
	Type        string     `json:"type"`
	Title       string     `json:"title"`
	Description string     `json:"description,omitempty"`
	Priority    string     `json:"priority,omitempty"` // default medium
	Estimate    string     `json:"estimate,omitempty"`
	Labels      []string   `json:"labels,omitempty"`
	Board       string     `json:"board,omitempty"`    // source id
	Column      string     `json:"column,omitempty"`   // source id, on Board
	Parent      string     `json:"parent,omitempty"`   // source id of its epic or parent ticket
	Assignee    *string    `json:"assignee,omitempty"` // a member's user id; unmatched people are left unassigned
	URL         string     `json:"url,omitempty"`      // the original, for external_url
	CreatedAt   *time.Time `json:"created_at,omitempty"`
	UpdatedAt   *time.Time `json:"updated_at,omitempty"`
	DueDate     *string    `json:"due_date,omitempty"` // YYYY-MM-DD (TL-637)
	Comments    []Comment  `json:"comments,omitempty"`
	BlockedBy   []string   `json:"blocked_by,omitempty"` // source ids of the tickets it waits on (TL-538)
	// Done: finished in the source. On a board it's done by being in the last column; in the backlog
	// it's resolved as done. DoneAt is when (default UpdatedAt).
	WontDo      bool         `json:"wont_do,omitempty"` // decided against in the source (Linear's cancelled): resolved as won't do in the backlog
	Done        bool         `json:"done,omitempty"`
	DoneAt      *time.Time   `json:"done_at,omitempty"`
	Attachments []Attachment `json:"attachments,omitempty"` // copied when they run (TL-540)
}

// Attachment is a file on a source ticket. Up to MaxCopy it's copied to UploadThing while the import
// runs (the source fetches it); a bigger one is linked in the ticket's description instead.
type Attachment struct {
	SourceID string `json:"source_id"`
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	Type     string `json:"type,omitempty"` // its content type
	URL      string `json:"url"`            // where the source fetches it from
	Link     string `json:"link,omitempty"` // where a person opens it in the source (default URL)
}

// MaxCopy is the largest attachment copied: ticket uploads' own limit (frontend app/api/uploadthing/core.ts).
const MaxCopy = 16 << 20

// Files stores copied attachments (retention.UploadThing).
type Files interface {
	Upload(ctx context.Context, name, contentType string, data []byte) (key, url string, err error)
}

// Fetcher is a source that can download its attachments while an import runs (at most max bytes).
type Fetcher interface {
	Fetch(ctx context.Context, org, url string, max int64) ([]byte, error)
}

type Comment struct {
	SourceID  string     `json:"source_id"`
	Author    string     `json:"author"` // a member's user id, or a non-member's name as plain text
	Body      string     `json:"body"`
	CreatedAt *time.Time `json:"created_at,omitempty"`
}

// Skip is something an import leaves out, and why.
type Skip struct {
	Kind     string `json:"kind"` // project, board, ticket, comment, estimate…
	SourceID string `json:"source_id"`
	Reason   string `json:"reason"`
}

// Problem stops an import from running until it's fixed (upgrade, trim the import, pick a prefix).
type Problem struct {
	Code     string `json:"code"` // project_limit, bad_prefix, prefix_taken, board_columns, ticket_number
	SourceID string `json:"source_id,omitempty"`
	Message  string `json:"message"`
}

// Counts is how much an import creates (planned) or created (report).
type Counts struct {
	Projects    int `json:"projects"`
	Boards      int `json:"boards"`
	Columns     int `json:"columns"`
	Tickets     int `json:"tickets"`
	Comments    int `json:"comments"`
	Attachments int `json:"attachments"` // copied (planned: those small enough to copy)
}

var prefixFormat = regexp.MustCompile(`^[A-Z][A-Z0-9]{1,5}$`) // projects_key_prefix_format

// org is what a plan is checked against: how many projects it has and may have, and the prefixes taken.
type org struct {
	projects, limit int // limit -1: unlimited
	prefixes        []string
}

// check finds what stops p from running (problems) and what a run will leave out (skips: the
// source's own plus anything the framework can't place), and counts what it will create.
func check(p Plan, o org) (problems []Problem, skips []Skip, n Counts) {
	skips = append(skips, p.Skipped...)
	if o.limit >= 0 && o.projects+len(p.Projects) > o.limit {
		problems = append(problems, Problem{Code: "project_limit",
			Message: fmt.Sprintf("this import creates %d projects; your plan allows %d and you have %d", len(p.Projects), o.limit, o.projects)})
	}
	seen := slices.Clone(o.prefixes)
	for _, pr := range p.Projects {
		n.Projects++
		switch {
		case !prefixFormat.MatchString(pr.Prefix):
			problems = append(problems, Problem{Code: "bad_prefix", SourceID: pr.SourceID,
				Message: fmt.Sprintf("%q: a prefix is 2 to 6 capital letters or digits, starting with a letter", pr.Prefix)})
		case slices.Contains(seen, pr.Prefix):
			problems = append(problems, Problem{Code: "prefix_taken", SourceID: pr.SourceID,
				Message: fmt.Sprintf("the prefix %s is already used", pr.Prefix)})
		}
		seen = append(seen, pr.Prefix)
		columns := map[string]bool{} // board/column source ids
		for _, b := range pr.Boards {
			if len(b.Columns) < 2 {
				problems = append(problems, Problem{Code: "board_columns", SourceID: b.SourceID,
					Message: fmt.Sprintf("board %q needs at least two columns (To do and Done)", b.Name)})
			}
			n.Boards++
			for _, c := range b.Columns {
				n.Columns++
				columns[b.SourceID+"/"+c.SourceID] = true
			}
		}
		numbers := map[int]bool{}
		for _, t := range pr.Tickets {
			if t.Number < 1 || numbers[t.Number] {
				problems = append(problems, Problem{Code: "ticket_number", SourceID: t.SourceID,
					Message: fmt.Sprintf("%s-%d: ticket numbers must be positive and unique in a project", pr.Prefix, t.Number)})
			}
			numbers[t.Number] = true
			if reason := placeable(t, columns); reason != "" {
				skips = append(skips, Skip{Kind: "ticket", SourceID: t.SourceID, Reason: reason})
				continue
			}
			n.Tickets++
			n.Comments += len(t.Comments)
			for _, a := range t.Attachments {
				if a.Size <= MaxCopy {
					n.Attachments++
				}
			}
		}
	}
	return problems, skips, n
}

// placeable is why t can't be imported, or "".
func placeable(t Ticket, columns map[string]bool) string {
	switch {
	case !tickets.ValidType(t.Type):
		return fmt.Sprintf("type %q isn't bug, feature, task, epic or spike", t.Type)
	case t.Title == "":
		return "no title"
	case t.Board != "" && !columns[t.Board+"/"+t.Column]:
		return "its board or column isn't in the import"
	case t.Priority != "" && !slices.Contains([]string{"low", "medium", "high", "urgent"}, t.Priority):
		return fmt.Sprintf("priority %q isn't low, medium, high or urgent", t.Priority)
	}
	return ""
}

// scale is a board's estimate scale, none unless the source names a known one.
func scale(b Board) string {
	if _, ok := boards.Scales[b.EstimateScale]; ok {
		return b.EstimateScale
	}
	return "none"
}

// Prefix is the key prefix for a source's key (Jira's SHOP, Linear's ENG): the key itself, or with a
// number added when it's taken (SHOP → SHOP2 → SHOP3…), up to 6 characters; "" when neither fits,
// and the preview asks for one.
func Prefix(key string, taken map[string]bool) string {
	key = strings.ToUpper(strings.ReplaceAll(key, "_", ""))
	if len(key) < 2 || len(key) > 6 {
		return ""
	}
	if !taken[key] {
		return key
	}
	for n := 2; ; n++ {
		p := key + strconv.Itoa(n)
		if len(p) > 6 {
			return ""
		}
		if !taken[p] {
			return p
		}
	}
}

// Prefixes is the key prefixes org's projects already use.
func Prefixes(ctx context.Context, db *pgxpool.Pool, org string) (map[string]bool, error) {
	rows, err := db.Query(ctx, `SELECT key_prefix FROM projects WHERE owner_clerk_id = $1`, org)
	if err != nil {
		return nil, err
	}
	list, err := pgx.CollectRows(rows, pgx.RowTo[string])
	taken := map[string]bool{}
	for _, p := range list {
		taken[p] = true
	}
	return taken, err
}
