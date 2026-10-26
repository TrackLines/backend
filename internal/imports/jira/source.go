package jira

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/tracklines/backend/internal/boards"
	"github.com/tracklines/backend/internal/httpx"
	"github.com/tracklines/backend/internal/imports"
)

var ErrOptions = httpx.Code("jira_options", "pick at least one Jira project to import")

// Source is the Jira Cloud import (TL-538): each chosen Jira project becomes a project, its boards
// boards (their columns as they are in Jira), and every issue a ticket with its number kept.
type Source struct {
	Store Store
	// Members is the org's people by email (lower case) → user id: Jira people are matched by email,
	// anyone else is left unassigned. nil: nobody is matched.
	Members func(ctx context.Context, org string) (map[string]string, error)
}

// Options are what the preview asks: which Jira projects (by key), a prefix for any whose own key
// can't be used, and which of the org's teams runs each board ("board:<id>" → team id).
type Options struct {
	Projects []string          `json:"projects"`
	Prefixes map[string]string `json:"prefixes"`
	Teams    map[string]string `json:"teams"`
}

// issue is a Jira issue with the fields an import reads.
type issue struct {
	ID     string `json:"id"`
	Key    string `json:"key"`
	Fields struct {
		Summary     string          `json:"summary"`
		Description json.RawMessage `json:"description"`
		IssueType   struct {
			Name           string `json:"name"`
			Subtask        bool   `json:"subtask"`
			HierarchyLevel int    `json:"hierarchyLevel"`
		} `json:"issuetype"`
		Priority   *struct{ Name string }  `json:"priority"`
		Labels     []string                `json:"labels"`
		Components []struct{ Name string } `json:"components"`
		Status     struct {
			ID             string `json:"id"`
			StatusCategory struct {
				Key string `json:"key"`
			} `json:"statusCategory"`
		} `json:"status"`
		Parent   *struct{ ID string } `json:"parent"`
		Assignee *person              `json:"assignee"`
		Created  string               `json:"created"`
		Updated  string               `json:"updated"`
		Resolved string               `json:"resolutiondate"`
		DueDate  string               `json:"duedate"`
		Links    []struct {
			Type struct {
				Name, Inward string
			} `json:"type"`
			InwardIssue *struct{ ID string } `json:"inwardIssue"`
		} `json:"issuelinks"`
		Comment struct {
			Comments []comment `json:"comments"`
			Total    int       `json:"total"`
		} `json:"comment"`
		Attachment []struct {
			ID       string `json:"id"`
			Filename string `json:"filename"`
			Size     int64  `json:"size"`
			MimeType string `json:"mimeType"`
		} `json:"attachment"`
	} `json:"fields"`
	estimates map[string]float64 // story point fields by id
}

type person struct {
	DisplayName  string `json:"displayName"`
	EmailAddress string `json:"emailAddress"`
}

type comment struct {
	ID      string          `json:"id"`
	Author  *person         `json:"author"`
	Body    json.RawMessage `json:"body"`
	Created string          `json:"created"`
}

// Jira's timestamps: 2025-03-01T09:00:00.000+0000
func when(s string) *time.Time {
	for _, layout := range []string{"2006-01-02T15:04:05.000-0700", time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			t = t.UTC()
			return &t
		}
	}
	return nil
}

var jiraKey = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*$`)

// Plan reads each chosen project from Jira and maps it.
func (s Source) Plan(ctx context.Context, org string, raw json.RawMessage) (imports.Plan, error) {
	var o Options
	if err := json.Unmarshal(raw, &o); err != nil || len(o.Projects) == 0 {
		return imports.Plan{}, ErrOptions
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
	points, err := storyPoints(ctx, c)
	if err != nil {
		return imports.Plan{}, err
	}
	var plan imports.Plan
	people := map[string]*imports.Person{}
	for _, key := range o.Projects {
		if !jiraKey.MatchString(key) {
			return imports.Plan{}, ErrOptions
		}
		p, skips, err := s.project(ctx, c, key, points, members, o, taken, people)
		if err != nil {
			return imports.Plan{}, err
		}
		plan.Projects = append(plan.Projects, p)
		plan.Skipped = append(plan.Skipped, skips...)
	}
	for _, p := range people {
		plan.People = append(plan.People, *p)
	}
	slices.SortFunc(plan.People, func(a, b imports.Person) int {
		return cmp.Or(cmp.Compare(b.Tickets, a.Tickets), strings.Compare(a.Name, b.Name))
	})
	return plan, nil
}

// storyPoints is the ids of the fields Jira keeps story points in (a custom field, named differently
// on company- and team-managed projects).
func storyPoints(ctx context.Context, c Client) ([]string, error) {
	var fields []struct{ ID, Name string }
	if err := c.Get(ctx, "/rest/api/3/field", nil, &fields); err != nil {
		return nil, err
	}
	var ids []string
	for _, f := range fields {
		if n := strings.ToLower(f.Name); n == "story points" || n == "story point estimate" {
			ids = append(ids, f.ID)
		}
	}
	return ids, nil
}

func (s Source) project(ctx context.Context, c Client, key string, points []string, members map[string]string, o Options, taken map[string]bool, people map[string]*imports.Person) (imports.Project, []imports.Skip, error) {
	var jp struct{ ID, Key, Name, Description string }
	if err := c.Get(ctx, "/rest/api/3/project/"+url.PathEscape(key), nil, &jp); err != nil {
		return imports.Project{}, nil, fmt.Errorf("jira project %s: %w", key, err)
	}
	prefix := strings.ToUpper(strings.TrimSpace(o.Prefixes[jp.Key]))
	if prefix == "" {
		prefix = imports.Prefix(jp.Key, taken)
	}
	if prefix == "" {
		prefix = strings.ToUpper(jp.Key) // the preview says it can't be used, and asks for one
	}
	taken[prefix] = true // the next project in this import picks another
	p := imports.Project{SourceID: jp.ID, Name: jp.Name, Prefix: prefix, Description: jp.Description, Boards: []imports.Board{}}
	var skips []imports.Skip

	// every issue in the project
	var issues []issue
	fields := append([]string{"summary", "description", "issuetype", "priority", "labels", "components", "status", "parent", "assignee",
		"created", "updated", "resolutiondate", "duedate", "issuelinks", "comment", "attachment"}, points...)
	q := url.Values{"jql": {"project = \"" + jp.Key + "\" ORDER BY key"}, "fields": {strings.Join(fields, ",")}, "maxResults": {"100"}}
	err := c.Search(ctx, q, func(raw json.RawMessage) error {
		var is issue
		if err := json.Unmarshal(raw, &is); err != nil {
			return err
		}
		var all struct {
			Fields map[string]json.RawMessage `json:"fields"`
		}
		_ = json.Unmarshal(raw, &all)
		is.estimates = map[string]float64{}
		for _, f := range points {
			var v *float64
			if json.Unmarshal(all.Fields[f], &v) == nil && v != nil {
				is.estimates[f] = *v
			}
		}
		issues = append(issues, is)
		return nil
	})
	if err != nil {
		return p, nil, fmt.Errorf("jira issues of %s: %w", key, err)
	}

	// its boards: columns as in Jira, and which issues each shows (an issue on several is on the first)
	type place struct{ board, column string }
	placed := map[string]place{} // issue id → where
	estimateField := map[string]string{}
	err = c.List(ctx, "/rest/agile/1.0/board", url.Values{"projectKeyOrId": {jp.Key}}, "values", func(raw json.RawMessage) error {
		var jb struct {
			ID   int    `json:"id"`
			Name string `json:"name"`
		}
		if err := json.Unmarshal(raw, &jb); err != nil {
			return err
		}
		id := strconv.Itoa(jb.ID)
		var conf struct {
			ColumnConfig struct {
				Columns []struct {
					Name     string                `json:"name"`
					Statuses []struct{ ID string } `json:"statuses"`
				} `json:"columns"`
			} `json:"columnConfig"`
			Estimation struct {
				Field struct {
					FieldID string `json:"fieldId"`
				} `json:"field"`
			} `json:"estimation"`
		}
		if err := c.Get(ctx, "/rest/agile/1.0/board/"+id+"/configuration", nil, &conf); err != nil {
			return err
		}
		b := imports.Board{SourceID: "board:" + id, Name: jb.Name}
		if t, ok := o.Teams[b.SourceID]; ok && t != "" {
			b.TeamID = &t
		}
		column := map[string]string{} // status id → column source id
		for i, col := range conf.ColumnConfig.Columns {
			if len(col.Statuses) == 0 {
				continue // a column no status maps to holds nothing
			}
			cid := strconv.Itoa(i)
			b.Columns = append(b.Columns, imports.Column{SourceID: cid, Name: col.Name})
			for _, st := range col.Statuses {
				column[st.ID] = cid
			}
		}
		estimateField[b.SourceID] = conf.Estimation.Field.FieldID
		p.Boards = append(p.Boards, b)
		return c.List(ctx, "/rest/agile/1.0/board/"+id+"/issue", url.Values{"fields": {"status"}, "maxResults": {"100"}}, "issues", func(raw json.RawMessage) error {
			var bi struct {
				ID     string `json:"id"`
				Fields struct {
					Status struct{ ID string } `json:"status"`
				} `json:"fields"`
			}
			if err := json.Unmarshal(raw, &bi); err != nil {
				return err
			}
			if _, already := placed[bi.ID]; !already && column[bi.Fields.Status.ID] != "" {
				placed[bi.ID] = place{b.SourceID, column[bi.Fields.Status.ID]}
			}
			return nil
		})
	})
	if err != nil {
		return p, nil, fmt.Errorf("jira boards of %s: %w", key, err)
	}

	values := map[string][]string{} // estimates per board, to pick its scale
	for _, is := range issues {
		t, sk, err := s.ticket(ctx, c, is, members)
		if err != nil {
			return p, nil, err
		}
		skips = append(skips, sk...)
		if a := is.Fields.Assignee; a != nil { // who it's assigned to, for the preview
			who := a.DisplayName + "\x00" + strings.ToLower(a.EmailAddress)
			if people[who] == nil {
				people[who] = &imports.Person{Name: a.DisplayName, Matched: t.Assignee != nil}
			}
			people[who].Tickets++
		}
		if at, ok := placed[is.ID]; ok {
			t.Board, t.Column = at.board, at.column
			if v, ok := is.estimates[estimateField[at.board]]; ok {
				t.Estimate = strconv.FormatFloat(v, 'f', -1, 64)
				values[at.board] = append(values[at.board], t.Estimate)
			}
		} else if len(is.estimates) > 0 {
			skips = append(skips, imports.Skip{Kind: "estimate", SourceID: is.ID, Reason: "it's in the backlog, and backlog tickets aren't estimated"})
		}
		p.Tickets = append(p.Tickets, t)
	}
	for i, b := range p.Boards {
		p.Boards[i].EstimateScale = scaleFor(values[b.SourceID])
	}
	return p, skips, nil
}

// scaleFor is the first scale every estimate fits (Fibonacci when there are none or none fits: the
// misfits are reported as skipped estimates).
func scaleFor(estimates []string) string {
	for _, s := range []string{"fibonacci", "linear", "powers"} {
		if !slices.ContainsFunc(estimates, func(e string) bool { return !boards.ValidEstimate(s, e) }) {
			return s
		}
	}
	return "fibonacci"
}

var types = map[string]string{"bug": "bug", "defect": "bug", "story": "feature", "feature": "feature", "new feature": "feature",
	"improvement": "feature", "enhancement": "feature", "spike": "spike", "epic": "epic"}

var priorities = map[string]string{"highest": "urgent", "blocker": "urgent", "critical": "urgent", "high": "high", "major": "high",
	"medium": "medium", "low": "low", "lowest": "low", "minor": "low", "trivial": "low"}

func (s Source) ticket(ctx context.Context, c Client, is issue, members map[string]string) (imports.Ticket, []imports.Skip, error) {
	f := is.Fields
	n, _ := strconv.Atoi(is.Key[strings.LastIndex(is.Key, "-")+1:])
	t := imports.Ticket{SourceID: is.ID, Number: n, Title: f.Summary, Type: "task", Priority: "medium",
		URL: strings.TrimRight(c.Site, "/") + "/browse/" + is.Key, CreatedAt: when(f.Created), UpdatedAt: when(f.Updated),
		Done: f.Status.StatusCategory.Key == "done", DoneAt: when(f.Resolved)}
	switch typ := types[strings.ToLower(f.IssueType.Name)]; {
	case f.IssueType.HierarchyLevel >= 1:
		t.Type = "epic"
	case typ != "":
		t.Type = typ
	}
	if f.Priority != nil {
		if p := priorities[strings.ToLower(f.Priority.Name)]; p != "" {
			t.Priority = p
		}
	}
	t.Labels = append(t.Labels, f.Labels...)
	for _, comp := range f.Components {
		t.Labels = append(t.Labels, comp.Name)
	}
	if f.Parent != nil {
		t.Parent = f.Parent.ID // a sub-task's parent, or a story's epic
	}
	if f.Assignee != nil {
		if id, ok := members[strings.ToLower(f.Assignee.EmailAddress)]; ok && f.Assignee.EmailAddress != "" {
			t.Assignee = &id
		}
	}
	if f.DueDate != "" {
		d := f.DueDate
		t.DueDate = &d
	}
	for _, l := range f.Links {
		if l.InwardIssue != nil && (l.Type.Name == "Blocks" || strings.Contains(strings.ToLower(l.Type.Inward), "blocked by")) {
			t.BlockedBy = append(t.BlockedBy, l.InwardIssue.ID)
		}
	}
	for _, a := range f.Attachment { // copied while the import runs (TL-540): redirect=false streams it from Jira itself
		content := strings.TrimRight(c.Site, "/") + "/rest/api/3/attachment/content/" + url.PathEscape(a.ID)
		t.Attachments = append(t.Attachments, imports.Attachment{SourceID: "attachment:" + a.ID, Name: a.Filename, Size: a.Size, Type: a.MimeType,
			URL: content + "?redirect=false", Link: content})
	}
	var skips []imports.Skip
	var err error
	if t.Description, err = ToMarkdown(f.Description, nil); err != nil {
		skips = append(skips, imports.Skip{Kind: "description", SourceID: is.ID, Reason: "it couldn't be read"})
	}
	comments := f.Comment.Comments
	if f.Comment.Total > len(comments) { // the issue only carries the first few
		comments = nil
		err := c.List(ctx, "/rest/api/3/issue/"+is.ID+"/comment", url.Values{"maxResults": {"100"}}, "comments", func(raw json.RawMessage) error {
			var cm comment
			if err := json.Unmarshal(raw, &cm); err != nil {
				return err
			}
			comments = append(comments, cm)
			return nil
		})
		if err != nil {
			return t, nil, fmt.Errorf("jira comments of %s: %w", is.Key, err)
		}
	}
	for _, cm := range comments {
		body, err := ToMarkdown(cm.Body, nil)
		if err != nil {
			skips = append(skips, imports.Skip{Kind: "comment", SourceID: cm.ID, Reason: "it couldn't be read"})
			continue
		}
		author := "Someone on Jira"
		if cm.Author != nil {
			author = cm.Author.DisplayName
			if id, ok := members[strings.ToLower(cm.Author.EmailAddress)]; ok && cm.Author.EmailAddress != "" {
				author = id
			}
		}
		t.Comments = append(t.Comments, imports.Comment{SourceID: cm.ID, Author: author, Body: body, CreatedAt: when(cm.Created)})
	}
	return t, skips, nil
}

// Fetch downloads an attachment while an import runs, as the org's connection.
func (s Source) Fetch(ctx context.Context, org, u string, max int64) ([]byte, error) {
	c, err := s.Store.Client(ctx, org)
	if err != nil {
		return nil, err
	}
	return c.Download(ctx, u, max)
}

// Projects: GET /api/imports/jira/projects, the Jira projects the connection can see, to pick from.
func (h System) Projects(w http.ResponseWriter, r *http.Request) {
	org, _, ok := h.gate(w, r)
	if !ok {
		return
	}
	c, err := h.Store.Client(r.Context(), org)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	type project struct {
		ID   string `json:"id"`
		Key  string `json:"key"`
		Name string `json:"name"`
	}
	out := []project{}
	err = c.List(r.Context(), "/rest/api/3/project/search", url.Values{"maxResults": {"50"}, "orderBy": {"name"}}, "values", func(raw json.RawMessage) error {
		var p project
		if err := json.Unmarshal(raw, &p); err != nil {
			return err
		}
		out = append(out, p)
		return nil
	})
	if errors.Is(err, ErrUnauthorized) { // the token was revoked in Atlassian since it was saved
		writeErr(w, r, err)
		return
	}
	if err != nil {
		writeErr(w, r, fmt.Errorf("%w: %v", ErrUnreachable, err))
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}
