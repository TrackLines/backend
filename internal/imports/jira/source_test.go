package jira

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/clerk/clerk-sdk-go/v2"
	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tracklines/backend/internal/imports"
	"github.com/tracklines/backend/internal/organizations"
	"github.com/tracklines/backend/internal/secrets"
)

// fakeJira is a Jira Cloud site with project SHOP (board 7, four issues over two search pages) and
// LONGKEY (a key too long for a prefix, and board 8 with a single column).
func fakeJira(t *testing.T) *httptest.Server {
	adf := func(text string) json.RawMessage {
		return json.RawMessage(`{"type":"doc","version":1,"content":[{"type":"paragraph","content":[{"type":"text","text":"` + text + `"}]}]}`)
	}
	sam := map[string]any{"displayName": "Sam Doe", "emailAddress": "Sam@Acme.io"}
	issue := func(id, key, typ string, level int, status, category string, fields map[string]any) map[string]any {
		f := map[string]any{"summary": key + " title", "issuetype": map[string]any{"name": typ, "hierarchyLevel": level, "subtask": level < 0},
			"status":  map[string]any{"id": status, "statusCategory": map[string]any{"key": category}},
			"created": "2025-03-01T09:00:00.000+0000", "updated": "2025-03-02T09:00:00.000+0000", "comment": map[string]any{"comments": []any{}, "total": 0}}
		for k, v := range fields {
			f[k] = v
		}
		return map[string]any{"id": id, "key": key, "fields": f}
	}
	shop1 := []any{
		issue("101", "SHOP-1", "Epic", 1, "1", "new", nil),
		issue("112", "SHOP-12", "Bug", 0, "3", "indeterminate", map[string]any{
			"description": adf("Card declined"), "priority": map[string]any{"name": "Highest"}, "labels": []string{"pay"},
			"components": []any{map[string]any{"name": "Checkout"}}, "parent": map[string]any{"id": "101"}, "assignee": sam,
			"duedate": "2025-04-01", "customfield_10016": 5,
			"issuelinks": []any{map[string]any{"type": map[string]any{"name": "Blocks", "inward": "is blocked by"}, "inwardIssue": map[string]any{"id": "113"}},
				map[string]any{"type": map[string]any{"name": "Relates", "inward": "relates to"}, "inwardIssue": map[string]any{"id": "101"}}},
			"comment": map[string]any{"comments": []any{map[string]any{"id": "c1", "body": adf("first")}}, "total": 2}}),
	}
	shop2 := []any{
		issue("113", "SHOP-13", "Sub-task", -1, "10001", "done", map[string]any{"parent": map[string]any{"id": "112"}, "customfield_10016": 4,
			"resolutiondate": "2025-03-05T10:00:00.000+0000"}),
		issue("114", "SHOP-14", "Story", 0, "10001", "done", map[string]any{"customfield_10016": 3}),
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, _ := r.BasicAuth(); u != "sam@acme.io" || p != "tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var out any
		switch q := r.URL.Query(); r.URL.Path {
		case "/rest/api/3/myself":
			out = sam
		case "/rest/api/3/field":
			out = []any{map[string]any{"id": "summary", "name": "Summary"}, map[string]any{"id": "customfield_10016", "name": "Story point estimate"}}
		case "/rest/api/3/project/SHOP":
			out = map[string]any{"id": "10000", "key": "SHOP", "name": "Shop"}
		case "/rest/api/3/project/LONGKEY":
			out = map[string]any{"id": "10001", "key": "LONGKEY", "name": "Long"}
		case "/rest/api/3/project/search":
			out = map[string]any{"values": []any{map[string]any{"id": "10000", "key": "SHOP", "name": "Shop"}}, "isLast": true}
		case "/rest/api/3/search/jql":
			switch {
			case strings.Contains(q.Get("jql"), "LONGKEY"):
				out = map[string]any{"issues": []any{}, "isLast": true}
			case !strings.Contains(q.Get("fields"), "customfield_10016"):
				t.Errorf("story points not asked for: %s", q.Get("fields"))
			case q.Get("nextPageToken") == "":
				out = map[string]any{"issues": shop1, "nextPageToken": "p2"}
			default:
				out = map[string]any{"issues": shop2, "isLast": true}
			}
		case "/rest/api/3/issue/112/comment":
			out = map[string]any{"comments": []any{
				map[string]any{"id": "c1", "author": sam, "body": adf("first"), "created": "2025-03-01T10:00:00.000+0000"},
				map[string]any{"id": "c2", "author": map[string]any{"displayName": "Old Name", "emailAddress": "gone@else.where"}, "body": adf("second")},
			}, "total": 2}
		case "/rest/agile/1.0/board":
			id, name := 7, "Shop board"
			if q.Get("projectKeyOrId") == "LONGKEY" {
				id, name = 8, "Thin"
			}
			out = map[string]any{"values": []any{map[string]any{"id": id, "name": name}}, "isLast": true}
		case "/rest/agile/1.0/board/7/configuration":
			col := func(name string, statuses ...string) map[string]any {
				var ss []any
				for _, s := range statuses {
					ss = append(ss, map[string]any{"id": s})
				}
				return map[string]any{"name": name, "statuses": ss}
			}
			out = map[string]any{"columnConfig": map[string]any{"columns": []any{col("To Do", "1"), col("Empty"), col("In Progress", "3"), col("Done", "10001")}},
				"estimation": map[string]any{"field": map[string]any{"fieldId": "customfield_10016"}}}
		case "/rest/agile/1.0/board/8/configuration":
			out = map[string]any{"columnConfig": map[string]any{"columns": []any{map[string]any{"name": "All", "statuses": []any{map[string]any{"id": "1"}}}}}}
		case "/rest/agile/1.0/board/7/issue":
			st := func(id, status string) map[string]any {
				return map[string]any{"id": id, "fields": map[string]any{"status": map[string]any{"id": status}}}
			}
			out = map[string]any{"issues": []any{st("101", "1"), st("112", "3"), st("113", "10001"), st("999", "1")}, "total": 4}
		case "/rest/agile/1.0/board/8/issue":
			out = map[string]any{"issues": []any{}, "total": 0}
		default:
			t.Errorf("unexpected %s", r.URL)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TL-538: Jira's projects, boards and issues map into an import as decided, and the preview reports
// what stops it (a prefix that can't be used, a board without To do and Done).
func TestSource(t *testing.T) {
	ctx := context.Background()
	url := os.Getenv("TEST_DATABASE_URL")
	m, err := migrate.New("file://../../migrations", "pgx5"+url[len("postgres"):])
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatal(err)
	}
	db, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(ctx, `DELETE FROM jira_connections WHERE org_id = 'js1'; DELETE FROM imports WHERE org_id = 'js1';
		DELETE FROM projects WHERE owner_clerk_id = 'js1'; DELETE FROM org_admins WHERE org_id = 'js1';
		INSERT INTO org_admins (org_id, user_clerk_id, granted_by) VALUES ('js1', 'user_admin', 'test');
		INSERT INTO projects (owner_clerk_id, name, key_prefix) VALUES ('js1', 'Ours', 'SHOP')`); err != nil {
		t.Fatal(err)
	}
	jira := fakeJira(t)
	box, _ := secrets.New(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{3}, 32)))
	store := Store{DB: db, Box: box, Now: time.Now, Dial: func(site, email, token string) Client {
		return Client{Site: jira.URL, Email: email, Token: token, HTTP: jira.Client()}
	}}
	src := Source{Store: store, Members: func(context.Context, string) (map[string]string, error) {
		return map[string]string{"sam@acme.io": "user_sam"}, nil
	}}
	if _, err := src.Plan(ctx, "js1", json.RawMessage(`{"projects":["SHOP"]}`)); err == nil || !strings.Contains(err.Error(), "connect Jira first") {
		t.Fatalf("not connected: %v", err)
	}
	if _, err := store.Connect(ctx, "js1", "user_admin", "acme", "sam@acme.io", "tok"); err != nil {
		t.Fatal(err)
	}
	if _, err := src.Plan(ctx, "js1", json.RawMessage(`{"projects":[]}`)); err != ErrOptions {
		t.Fatalf("no projects: %v", err)
	}

	team := "11111111-1111-1111-1111-111111111111"
	plan, err := src.Plan(ctx, "js1", json.RawMessage(`{"projects":["SHOP"],"teams":{"board:7":"`+team+`"}}`))
	if err != nil {
		t.Fatal(err)
	}
	p := plan.Projects[0]
	// SHOP is taken here, so SHOP2; the board keeps Jira's columns that hold a status, with its team
	if p.SourceID != "10000" || p.Name != "Shop" || p.Prefix != "SHOP2" || len(p.Boards) != 1 {
		t.Fatalf("project: %+v", p)
	}
	b := p.Boards[0]
	var cols []string
	for _, c := range b.Columns {
		cols = append(cols, c.SourceID+":"+c.Name)
	}
	if b.SourceID != "board:7" || b.Name != "Shop board" || b.TeamID == nil || *b.TeamID != team || b.EstimateScale != "linear" ||
		!slices.Equal(cols, []string{"0:To Do", "2:In Progress", "3:Done"}) {
		t.Fatalf("board: %+v %v", b, cols)
	}
	byNumber := map[int]imports.Ticket{}
	for _, tk := range p.Tickets {
		byNumber[tk.Number] = tk
	}
	if len(byNumber) != 4 {
		t.Fatalf("tickets (both search pages): %+v", p.Tickets)
	}
	epic, bug, sub, story := byNumber[1], byNumber[12], byNumber[13], byNumber[14]
	if epic.Type != "epic" || epic.Board != "board:7" || epic.Column != "0" {
		t.Fatalf("epic: %+v", epic)
	}
	if bug.Type != "bug" || bug.Priority != "urgent" || !slices.Equal(bug.Labels, []string{"pay", "Checkout"}) || bug.Column != "2" ||
		bug.Parent != "101" || bug.Assignee == nil || *bug.Assignee != "user_sam" || bug.DueDate == nil || *bug.DueDate != "2025-04-01" ||
		!slices.Equal(bug.BlockedBy, []string{"113"}) || bug.Estimate != "5" || bug.Description != "Card declined" ||
		bug.URL != jira.URL+"/browse/SHOP-12" || bug.CreatedAt == nil || bug.CreatedAt.Format(time.RFC3339) != "2025-03-01T09:00:00Z" || bug.Done {
		t.Fatalf("bug: %+v", bug)
	}
	// all the comments (the issue only carried one), members by id and anyone else by name
	if len(bug.Comments) != 2 || bug.Comments[0].Author != "user_sam" || bug.Comments[0].Body != "first" || bug.Comments[1].Author != "Old Name" {
		t.Fatalf("comments: %+v", bug.Comments)
	}
	if sub.Type != "task" || sub.Parent != "112" || sub.Column != "3" || !sub.Done || sub.DoneAt == nil || sub.DoneAt.Day() != 5 || sub.Estimate != "4" {
		t.Fatalf("sub-task: %+v", sub)
	}
	// not on any board: the backlog, done, its estimate left out
	if story.Type != "feature" || story.Board != "" || !story.Done || story.Estimate != "" {
		t.Fatalf("story: %+v", story)
	}
	if !slices.ContainsFunc(plan.Skipped, func(s imports.Skip) bool { return s.Kind == "estimate" && s.SourceID == "114" }) {
		t.Fatalf("skipped: %+v", plan.Skipped)
	}

	// through the preview: LONGKEY can't be a prefix and board 8 has one column, so it asks; a given prefix is used
	h := imports.NewSystem(db, organizations.Admins{DB: db}, func() bool { return true }, map[string]imports.Source{"jira": src})
	preview := func(options string) imports.Import {
		req := httptest.NewRequest("POST", "/", strings.NewReader(`{"source":"jira","options":`+options+`}`)).WithContext(
			clerk.ContextWithSessionClaims(ctx, &clerk.SessionClaims{RegisteredClaims: clerk.RegisteredClaims{Subject: "user_admin"}, Claims: clerk.Claims{ActiveOrganizationID: "js1"}}))
		rec := httptest.NewRecorder()
		h.Preview(rec, req)
		var im imports.Import
		json.Unmarshal(rec.Body.Bytes(), &im)
		if rec.Code != 201 {
			t.Fatalf("preview %s: %d %s", options, rec.Code, rec.Body)
		}
		return im
	}
	codes := func(im imports.Import) (out []string) {
		for _, pr := range im.Problems {
			out = append(out, pr.Code+":"+pr.SourceID)
		}
		return
	}
	im := preview(`{"projects":["SHOP","LONGKEY"]}`)
	if got := codes(im); !slices.Contains(got, "bad_prefix:10001") || !slices.Contains(got, "board_columns:board:8") {
		t.Fatalf("problems: %v", got)
	}
	im = preview(`{"projects":["SHOP","LONGKEY"],"prefixes":{"LONGKEY":"SHOP2"}}`) // clashes with SHOP's own
	if got := codes(im); !slices.Contains(got, "prefix_taken:10001") {
		t.Fatalf("a given prefix that clashes: %v", got)
	}
	im = preview(`{"projects":["SHOP","LONGKEY"],"prefixes":{"LONGKEY":"LONG"}}`)
	if got := codes(im); slices.ContainsFunc(got, func(c string) bool { return strings.HasPrefix(c, "bad_prefix") || strings.HasPrefix(c, "prefix_taken") }) ||
		im.Projects[1].Prefix != "LONG" {
		t.Fatalf("a given prefix: %v %+v", got, im.Projects)
	}

	// the projects to pick from
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil).WithContext(
		clerk.ContextWithSessionClaims(ctx, &clerk.SessionClaims{RegisteredClaims: clerk.RegisteredClaims{Subject: "user_admin"}, Claims: clerk.Claims{ActiveOrganizationID: "js1"}}))
	System{Store: store, Admins: organizations.Admins{DB: db}, On: func() bool { return true }}.Projects(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"key":"SHOP"`) {
		t.Fatalf("projects: %d %s", rec.Code, rec.Body)
	}
}

func TestPrefix(t *testing.T) {
	taken := map[string]bool{"SHOP": true, "SHOP2": true, "ABCDE": true}
	for key, want := range map[string]string{"SHOP": "SHOP3", "WEB": "WEB", "ABCDE": "ABCDE2", "ABCDEF": "ABCDEF", "LONGKEY": "", "A": "", "MY_APP": "MYAPP"} {
		if got := Prefix(key, taken); got != want {
			t.Errorf("Prefix(%s) = %q, want %q", key, got, want)
		}
	}
	if got := Prefix("ABCDEF", map[string]bool{"ABCDEF": true}); got != "" {
		t.Errorf("a taken 6-character key: %q", got)
	}
}
