package linear

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/tracklines/backend/internal/httpx"
	"github.com/tracklines/backend/internal/imports"
)

var ErrOptions = httpx.Code("linear_options", "the import options couldn't be read")

// Source is the Linear import (TL-654, decided 2026-10-20/26): the workspace becomes one project, its
// teams boards, its projects epics, and every issue a ticket. The team with most issues keeps its
// key and numbers; the others are numbered after it, each ticket keeping its link to Linear.
type Source struct {
	Store Store
	// Members is the org's people by email (lower case) → user id; nil: nobody is matched.
	Members func(ctx context.Context, org string) (map[string]string, error)
}

// Options: a prefix when the main team's key can't be used, and which of the org's teams runs each
// board ("team:<id>" → team id).
type Options struct {
	Prefix string            `json:"prefix"`
	Teams  map[string]string `json:"teams"`
}

type team struct {
	ID             string `json:"id"`
	Key            string `json:"key"`
	Name           string `json:"name"`
	EstimationType string `json:"issueEstimationType"`
	States         struct {
		Nodes []state `json:"nodes"`
	} `json:"states"`
	issues []issue
}

type state struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Type     string  `json:"type"` // triage, backlog, unstarted, started, completed, canceled
	Position float64 `json:"position"`
}

type person struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

type issue struct {
	ID          string   `json:"id"`
	Number      int      `json:"number"`
	Identifier  string   `json:"identifier"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Priority    int      `json:"priority"`
	Estimate    *float64 `json:"estimate"`
	DueDate     *string  `json:"dueDate"`
	URL         string   `json:"url"`
	CreatedAt   string   `json:"createdAt"`
	UpdatedAt   string   `json:"updatedAt"`
	CompletedAt *string  `json:"completedAt"`
	CanceledAt  *string  `json:"canceledAt"`
	State       state    `json:"state"`
	Parent      *struct {
		ID string `json:"id"`
	} `json:"parent"`
	Project *struct {
		ID string `json:"id"`
	} `json:"project"`
	Assignee *person `json:"assignee"`
	Labels   struct {
		Nodes []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	} `json:"labels"`
	Relations struct {
		Nodes []struct {
			Type    string `json:"type"`
			Related struct {
				ID string `json:"id"`
			} `json:"relatedIssue"`
		} `json:"nodes"`
	} `json:"relations"`
	Comments struct {
		Nodes    []comment `json:"nodes"`
		PageInfo struct {
			HasNextPage bool `json:"hasNextPage"`
		} `json:"pageInfo"`
	} `json:"comments"`
}

type comment struct {
	ID        string  `json:"id"`
	Body      string  `json:"body"`
	CreatedAt string  `json:"createdAt"`
	User      *person `json:"user"`
}

// The queries stay under Linear's 10,000-point limit: 25 issues a page, each with up to 25 comments
// (more are read separately), 20 labels and 20 relations.
const (
	teamsQuery = `query($after: String) { teams(first: 25, after: $after) { nodes { id key name issueEstimationType
		states(first: 50) { nodes { id name type position } } } pageInfo { hasNextPage endCursor } } }`
	projectsQuery = `query($after: String) { projects(first: 50, after: $after) { nodes { id name description url
		createdAt updatedAt completedAt canceledAt } pageInfo { hasNextPage endCursor } } }`
	issuesQuery = `query($team: String!, $after: String) { team(id: $team) { issues(first: 25, after: $after) { nodes {
		id number identifier title description priority estimate dueDate url createdAt updatedAt completedAt canceledAt
		state { id type } parent { id } project { id } assignee { name email }
		labels(first: 20) { nodes { name } }
		relations(first: 20) { nodes { type relatedIssue { id } } }
		comments(first: 25) { nodes { id body createdAt user { name email } } pageInfo { hasNextPage } }
	} pageInfo { hasNextPage endCursor } } } }`
	commentsQuery = `query($issue: String!, $after: String) { issue(id: $issue) { comments(first: 50, after: $after) {
		nodes { id body createdAt user { name email } } pageInfo { hasNextPage endCursor } } } }`
	orgQuery = `query { organization { id name } }`
)

func when(s string) *time.Time {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		t = t.UTC()
		return &t
	}
	return nil
}

func whenp(s *string) *time.Time {
	if s == nil {
		return nil
	}
	return when(*s)
}

// Linear's estimate scales → ours; t-shirt sizes are stored as numbers (ponytail: assumed 1, 2, 3, 5
// and 8 for XS–XL; any other value is reported as skipped).
var scales = map[string]string{"exponential": "powers", "fibonacci": "fibonacci", "linear": "linear", "tShirt": "tshirt"}
var tshirt = map[float64]string{1: "XS", 2: "S", 3: "M", 5: "L", 8: "XL"}

// priority: Linear's 0 (none) to 4 (low); "none" is medium, since every ticket here has one.
var priorities = []string{"medium", "urgent", "high", "medium", "low"}

// a type from labels: Linear has none of its own
func ticketType(labels []string) string {
	for _, l := range labels {
		switch strings.ToLower(l) {
		case "bug", "defect":
			return "bug"
		case "feature", "improvement", "enhancement":
			return "feature"
		case "spike":
			return "spike"
		}
	}
	return "task"
}

// columns: the states a board shows, unstarted then started then completed (Done last), each in
// Linear's order. Triage and backlog states are the backlog; cancelled ones are closed as won't do.
var columnRank = map[string]int{"unstarted": 0, "started": 1, "completed": 2}

var uploads = regexp.MustCompile(`https://uploads\.linear\.app/[^\s)"'\]>]+`)

// Plan reads the whole workspace from Linear and maps it.
func (s Source) Plan(ctx context.Context, org string, raw json.RawMessage) (imports.Plan, error) {
	var o Options
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &o); err != nil {
			return imports.Plan{}, ErrOptions
		}
	}
	c, err := s.Store.Client(ctx, org)
	if err != nil {
		return imports.Plan{}, err
	}
	members := map[string]string{}
	if s.Members != nil {
		if members, err = s.Members(ctx, org); err != nil {
			return imports.Plan{}, err
		}
	}
	taken, err := imports.Prefixes(ctx, s.Store.DB, org)
	if err != nil {
		return imports.Plan{}, err
	}
	var ws struct {
		Organization struct{ ID, Name string } `json:"organization"`
	}
	if err := c.Query(ctx, orgQuery, nil, &ws); err != nil {
		return imports.Plan{}, err
	}

	var teams []*team
	if err := c.Pages(ctx, teamsQuery, nil, []string{"teams"}, func(raw json.RawMessage) error {
		var t team
		if err := json.Unmarshal(raw, &t); err != nil {
			return err
		}
		teams = append(teams, &t)
		return nil
	}); err != nil {
		return imports.Plan{}, fmt.Errorf("linear teams: %w", err)
	}
	for _, t := range teams {
		if err := c.Pages(ctx, issuesQuery, map[string]any{"team": t.ID}, []string{"team", "issues"}, func(raw json.RawMessage) error {
			var is issue
			if err := json.Unmarshal(raw, &is); err != nil {
				return err
			}
			if is.Comments.PageInfo.HasNextPage { // more than the first page: read them all
				is.Comments.Nodes = nil
				if err := c.Pages(ctx, commentsQuery, map[string]any{"issue": is.ID}, []string{"issue", "comments"}, func(raw json.RawMessage) error {
					var cm comment
					if err := json.Unmarshal(raw, &cm); err != nil {
						return err
					}
					is.Comments.Nodes = append(is.Comments.Nodes, cm)
					return nil
				}); err != nil {
					return err
				}
			}
			t.issues = append(t.issues, is)
			return nil
		}); err != nil {
			return imports.Plan{}, fmt.Errorf("linear issues of %s: %w", t.Key, err)
		}
	}
	type project struct {
		ID, Name, Description, URL string
		CreatedAt, UpdatedAt       string
		CompletedAt, CanceledAt    *string
	}
	var projects []project
	if err := c.Pages(ctx, projectsQuery, nil, []string{"projects"}, func(raw json.RawMessage) error {
		var p project
		if err := json.Unmarshal(raw, &p); err != nil {
			return err
		}
		projects = append(projects, p)
		return nil
	}); err != nil {
		return imports.Plan{}, fmt.Errorf("linear projects: %w", err)
	}

	// the team with most issues keeps its key and numbers; the rest follow it
	slices.SortStableFunc(teams, func(a, b *team) int { return cmp.Compare(len(b.issues), len(a.issues)) })
	p := imports.Project{SourceID: ws.Organization.ID, Name: ws.Organization.Name, Boards: []imports.Board{}}
	plan := imports.Plan{}
	if len(teams) > 0 {
		p.Prefix = strings.ToUpper(strings.TrimSpace(o.Prefix))
		if p.Prefix == "" {
			p.Prefix = cmp.Or(imports.Prefix(teams[0].Key, taken), strings.ToUpper(teams[0].Key)) // the preview asks when it can't be used
		}
	}
	next := 0
	if len(teams) > 0 {
		for _, is := range teams[0].issues {
			next = max(next, is.Number)
		}
	}
	number := map[string]int{} // issue id → its number here
	for i, t := range teams {
		slices.SortFunc(t.issues, func(a, b issue) int { return cmp.Compare(a.Number, b.Number) })
		if i == 0 {
			for _, is := range t.issues {
				number[is.ID] = is.Number
			}
			continue
		}
		if len(t.issues) == 0 {
			continue
		}
		first := next + 1
		for _, is := range t.issues {
			next++
			number[is.ID] = next
		}
		plan.Notes = append(plan.Notes, imports.Note{Kind: "renumbered", SourceID: "team:" + t.ID,
			Message: fmt.Sprintf("%s's %d issues become %s-%d to %s-%d (each keeps its link to Linear)", t.Name, len(t.issues), p.Prefix, first, p.Prefix, next)})
	}

	// boards: one per team, its columns from its workflow
	blockedBy := map[string][]string{}
	for _, t := range teams {
		for _, is := range t.issues {
			for _, r := range is.Relations.Nodes {
				if r.Type == "blocks" {
					blockedBy[r.Related.ID] = append(blockedBy[r.Related.ID], is.ID)
				}
			}
		}
	}
	people := map[string]*imports.Person{}
	for _, t := range teams {
		b := imports.Board{SourceID: "team:" + t.ID, Name: t.Name, EstimateScale: scales[t.EstimationType]}
		if id := o.Teams[b.SourceID]; id != "" {
			b.TeamID = &id
		}
		states := slices.Clone(t.States.Nodes)
		states = slices.DeleteFunc(states, func(st state) bool { _, ok := columnRank[st.Type]; return !ok })
		slices.SortStableFunc(states, func(a, b state) int {
			return cmp.Or(cmp.Compare(columnRank[a.Type], columnRank[b.Type]), cmp.Compare(a.Position, b.Position))
		})
		onBoard := map[string]bool{}
		for _, st := range states {
			b.Columns = append(b.Columns, imports.Column{SourceID: st.ID, Name: st.Name})
			onBoard[st.ID] = true
		}
		p.Boards = append(p.Boards, b)

		for _, is := range t.issues {
			tk, skips := s.ticket(is, t, members, people)
			tk.Number = number[is.ID]
			tk.BlockedBy = blockedBy[is.ID]
			if onBoard[is.State.ID] {
				tk.Board, tk.Column = b.SourceID, is.State.ID
			}
			plan.Skipped = append(plan.Skipped, skips...)
			p.Tickets = append(p.Tickets, tk)
		}
	}
	// Linear projects become epics, numbered after every issue
	for _, pj := range projects {
		next++
		p.Tickets = append(p.Tickets, imports.Ticket{SourceID: pj.ID, Number: next, Type: "epic", Title: pj.Name, Description: pj.Description,
			Priority: "medium", URL: pj.URL, CreatedAt: when(pj.CreatedAt), UpdatedAt: when(pj.UpdatedAt),
			Done: pj.CompletedAt != nil, DoneAt: whenp(pj.CompletedAt), WontDo: pj.CanceledAt != nil && pj.CompletedAt == nil})
	}
	for _, pp := range people {
		plan.People = append(plan.People, *pp)
	}
	slices.SortFunc(plan.People, func(a, b imports.Person) int {
		return cmp.Or(cmp.Compare(b.Tickets, a.Tickets), strings.Compare(a.Name, b.Name))
	})
	plan.Projects = []imports.Project{p}
	return plan, nil
}

func (s Source) ticket(is issue, t *team, members map[string]string, people map[string]*imports.Person) (imports.Ticket, []imports.Skip) {
	var labels []string
	for _, l := range is.Labels.Nodes {
		labels = append(labels, l.Name)
	}
	tk := imports.Ticket{SourceID: is.ID, Type: ticketType(labels), Title: is.Title, Description: is.Description, Labels: labels,
		Priority: priorities[min(max(is.Priority, 0), 4)], URL: is.URL, CreatedAt: when(is.CreatedAt), UpdatedAt: when(is.UpdatedAt),
		DueDate: is.DueDate, Done: is.State.Type == "completed", DoneAt: whenp(is.CompletedAt), WontDo: is.State.Type == "canceled"}
	var skips []imports.Skip
	if is.Estimate != nil {
		switch scale := scales[t.EstimationType]; {
		case scale == "tshirt" && tshirt[*is.Estimate] != "":
			tk.Estimate = tshirt[*is.Estimate]
		case scale == "tshirt":
			skips = append(skips, imports.Skip{Kind: "estimate", SourceID: is.ID, Reason: fmt.Sprintf("%v isn't a t-shirt size here", *is.Estimate)})
		default:
			tk.Estimate = strconv.FormatFloat(*is.Estimate, 'f', -1, 64) // the framework skips any that aren't on the scale
		}
	}
	switch {
	case is.Parent != nil:
		tk.Parent = is.Parent.ID
	case is.Project != nil:
		tk.Parent = is.Project.ID // its epic
	}
	if a := is.Assignee; a != nil {
		if id, ok := members[strings.ToLower(a.Email)]; ok && a.Email != "" {
			tk.Assignee = &id
		}
		who := a.Name + "\x00" + strings.ToLower(a.Email)
		if people[who] == nil {
			people[who] = &imports.Person{Name: a.Name, Matched: tk.Assignee != nil}
		}
		people[who].Tickets++
	}
	texts := []string{is.Description}
	for _, cm := range is.Comments.Nodes {
		author := "Someone on Linear"
		if cm.User != nil {
			author = cm.User.Name
			if id, ok := members[strings.ToLower(cm.User.Email)]; ok && cm.User.Email != "" {
				author = id
			}
		}
		tk.Comments = append(tk.Comments, imports.Comment{SourceID: cm.ID, Author: author, Body: cm.Body, CreatedAt: when(cm.CreatedAt)})
		texts = append(texts, cm.Body)
	}
	// files people uploaded, linked in the text: copied while the import runs (the framework fetches them)
	seen := map[string]bool{}
	for _, text := range texts {
		for _, u := range uploads.FindAllString(text, -1) {
			if !seen[u] {
				seen[u] = true
				tk.Attachments = append(tk.Attachments, imports.Attachment{SourceID: "upload:" + u, Name: path.Base(strings.SplitN(u, "?", 2)[0]), URL: u})
			}
		}
	}
	return tk, skips
}

// Fetch downloads an uploaded file while an import runs, with the org's key.
func (s Source) Fetch(ctx context.Context, org, u string, limit int64) ([]byte, error) {
	c, err := s.Store.Client(ctx, org)
	if err != nil {
		return nil, err
	}
	return c.Download(ctx, u, limit)
}
