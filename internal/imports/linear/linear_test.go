package linear

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/clerk/clerk-sdk-go/v2"
	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/imports"
	"github.com/tracklines/backend/internal/organizations"
	"github.com/tracklines/backend/internal/secrets"
)

// fakeLinear is workspace Acme: Engineering (ENG, three issues over two pages) and Design (DES, two,
// t-shirt estimates), and the project Checkout. The first teams query is rate limited.
func fakeLinear(t *testing.T) (*httptest.Server, *int) {
	limited := 1
	sam := map[string]any{"name": "Sam Doe", "email": "Sam@Acme.io"}
	st := func(id, name, typ string, pos float64) map[string]any {
		return map[string]any{"id": id, "name": name, "type": typ, "position": pos}
	}
	conn := func(nodes []any, more bool, cursor string) map[string]any {
		return map[string]any{"nodes": nodes, "pageInfo": map[string]any{"hasNextPage": more, "endCursor": cursor}}
	}
	issue := func(id string, n int, ident, state, typ string, extra map[string]any) map[string]any {
		is := map[string]any{"id": id, "number": n, "identifier": ident, "title": ident + " title", "description": "", "priority": 0,
			"url": "https://linear.app/acme/issue/" + ident, "createdAt": "2026-09-01T10:00:00.000Z", "updatedAt": "2026-10-20T10:00:00.000Z",
			"state": map[string]any{"id": state, "type": typ}, "labels": conn(nil, false, ""), "relations": conn(nil, false, ""),
			"comments": conn(nil, false, "")}
		for k, v := range extra {
			is[k] = v
		}
		return is
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/upload/shot.png" {
			if r.Header.Get("Authorization") != "lin_api_ok" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Write([]byte("png!"))
			return
		}
		if r.Header.Get("Authorization") != "lin_api_ok" {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"errors":[{"message":"Authentication required","extensions":{"code":"AUTHENTICATION_ERROR"}}]}`)
			return
		}
		var in struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		var data any
		switch q := in.Query; {
		case strings.Contains(q, "viewer"):
			data = map[string]any{"viewer": map[string]any{"name": "Sam Doe", "organization": map[string]any{"name": "Acme"}}}
		case strings.Contains(q, "organization {"):
			data = map[string]any{"organization": map[string]any{"id": "org1", "name": "Acme"}}
		case strings.Contains(q, "teams("):
			if limited > 0 {
				limited--
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, `{"errors":[{"message":"Rate limit exceeded","extensions":{"code":"RATELIMITED"}}]}`)
				return
			}
			des := map[string]any{"id": "des", "key": "DES", "name": "Design", "issueEstimationType": "tShirt",
				"states": conn([]any{st("ds2", "Done", "completed", 1), st("ds1", "Todo", "unstarted", 0)}, false, "")}
			eng := map[string]any{"id": "eng", "key": "ENG", "name": "Engineering", "issueEstimationType": "fibonacci",
				"states": conn([]any{st("s0", "Backlog", "backlog", 0), st("s4", "Done", "completed", 0), st("s3", "In Review", "started", 2),
					st("s2", "In Progress", "started", 1), st("s1", "Todo", "unstarted", 0), st("s5", "Canceled", "canceled", 0), st("s6", "Triage", "triage", 0)}, false, "")}
			data = map[string]any{"teams": conn([]any{des, eng}, false, "")} // Design first: the bigger team still wins
		case strings.Contains(q, "team(id"):
			var page map[string]any
			switch {
			case in.Variables["team"] == "des":
				page = conn([]any{
					issue("d1", 1, "DES-1", "ds2", "completed", map[string]any{"estimate": 5, "completedAt": "2026-10-01T09:00:00.000Z"}),
					issue("d4", 4, "DES-4", "ds1", "unstarted", map[string]any{"estimate": 4}),
				}, false, "")
			case in.Variables["after"] == nil:
				page = conn([]any{
					issue("e1", 1, "ENG-1", "s2", "started", map[string]any{"priority": 1, "estimate": 3, "dueDate": "2026-11-01",
						"description": "Broken: ![shot](https://uploads.linear.app/ab/cd/shot.png)", "assignee": sam,
						"project": map[string]any{"id": "p1"}, "labels": conn([]any{map[string]any{"name": "Bug"}, map[string]any{"name": "web"}}, false, ""),
						"relations": conn([]any{map[string]any{"type": "blocks", "relatedIssue": map[string]any{"id": "e7"}}}, false, ""),
						"comments":  conn([]any{map[string]any{"id": "c1", "body": "first"}}, true, "c")}),
					issue("e2", 2, "ENG-2", "s0", "backlog", nil),
				}, true, "page2")
			default:
				page = conn([]any{issue("e7", 7, "ENG-7", "s5", "canceled", map[string]any{"parent": map[string]any{"id": "e1"}})}, false, "")
			}
			data = map[string]any{"team": map[string]any{"issues": page}}
		case strings.Contains(q, "issue(id"):
			data = map[string]any{"issue": map[string]any{"comments": conn([]any{
				map[string]any{"id": "c1", "body": "first", "createdAt": "2026-09-02T10:00:00.000Z", "user": sam},
				map[string]any{"id": "c2", "body": "second", "user": map[string]any{"name": "Old Name", "email": "gone@else.where"}},
			}, false, "")}}
		case strings.Contains(q, "projects("):
			data = map[string]any{"projects": conn([]any{map[string]any{"id": "p1", "name": "Checkout", "description": "Pay", "url": "https://linear.app/acme/project/checkout",
				"createdAt": "2026-08-01T10:00:00.000Z", "updatedAt": "2026-10-01T10:00:00.000Z"}}, false, "")}
		default:
			t.Errorf("unexpected query %s", in.Query)
		}
		json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(srv.Close)
	return srv, &limited
}

func testDB(t *testing.T) *pgxpool.Pool {
	ctx := context.Background()
	url := os.Getenv("TEST_DATABASE_URL")
	m, err := migrate.New("file://../../migrations", "pgx5"+url[len("postgres"):])
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatal(err)
	}
	db, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	return db
}

func as(ctx context.Context, user, org string) context.Context {
	return clerk.ContextWithSessionClaims(ctx, &clerk.SessionClaims{RegisteredClaims: clerk.RegisteredClaims{Subject: user}, Claims: clerk.Claims{ActiveOrganizationID: org}})
}

// TL-654: the workspace maps into one project as decided, and the preview says how keys change.
func TestSource(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	if _, err := db.Exec(ctx, `DELETE FROM linear_connections WHERE org_id = 'ls1'; DELETE FROM imports WHERE org_id = 'ls1';
		DELETE FROM projects WHERE owner_clerk_id = 'ls1'; DELETE FROM org_admins WHERE org_id = 'ls1';
		INSERT INTO org_admins (org_id, user_clerk_id, granted_by) VALUES ('ls1', 'user_admin', 'test')`); err != nil {
		t.Fatal(err)
	}
	srv, limited := fakeLinear(t)
	box, _ := secrets.New(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{5}, 32)))
	var waits int
	store := Store{DB: db, Box: box, Now: time.Now, Dial: func(key string) Client {
		return Client{Key: key, HTTP: srv.Client(), URL: srv.URL, Uploads: srv.URL + "/upload/", sleep: func(time.Duration) { waits++ }}
	}}
	src := Source{Store: store, Members: func(context.Context, string) (map[string]string, error) {
		return map[string]string{"sam@acme.io": "user_sam"}, nil
	}}
	if _, err := src.Plan(ctx, "ls1", nil); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("not connected: %v", err)
	}
	if _, err := store.Connect(ctx, "ls1", "user_admin", "lin_api_ok"); err != nil {
		t.Fatal(err)
	}
	plan, err := src.Plan(ctx, "ls1", json.RawMessage(`{"teams":{"team:eng":"11111111-1111-1111-1111-111111111111"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if *limited != 0 || waits != 1 {
		t.Fatalf("rate limit: %d left, %d waits", *limited, waits)
	}
	if len(plan.Projects) != 1 {
		t.Fatalf("one project: %+v", plan.Projects)
	}
	p := plan.Projects[0]
	if p.SourceID != "org1" || p.Name != "Acme" || p.Prefix != "ENG" || len(p.Boards) != 2 {
		t.Fatalf("project: %+v", p)
	}
	// Engineering (most issues) first: Linear's started/unstarted/completed states in order, Done last
	eng, des := p.Boards[0], p.Boards[1]
	var cols []string
	for _, c := range eng.Columns {
		cols = append(cols, c.Name)
	}
	if eng.SourceID != "team:eng" || eng.EstimateScale != "fibonacci" || eng.TeamID == nil || !slices.Equal(cols, []string{"Todo", "In Progress", "In Review", "Done"}) {
		t.Fatalf("eng board: %+v %v", eng, cols)
	}
	if des.EstimateScale != "tshirt" || len(des.Columns) != 2 || des.Columns[1].Name != "Done" {
		t.Fatalf("des board: %+v", des)
	}
	by := map[string]imports.Ticket{}
	for _, tk := range p.Tickets {
		by[tk.SourceID] = tk
	}
	e1, e2, e7, d1, d4, p1 := by["e1"], by["e2"], by["e7"], by["d1"], by["d4"], by["p1"]
	if e1.Number != 1 || e1.Type != "bug" || e1.Priority != "urgent" || e1.Estimate != "3" || e1.Board != "team:eng" || e1.Column != "s2" ||
		e1.Parent != "p1" || e1.Assignee == nil || *e1.Assignee != "user_sam" || e1.DueDate == nil || *e1.DueDate != "2026-11-01" ||
		!slices.Equal(e1.Labels, []string{"Bug", "web"}) || e1.URL != "https://linear.app/acme/issue/ENG-1" {
		t.Fatalf("ENG-1: %+v", e1)
	}
	if len(e1.Comments) != 2 || e1.Comments[0].Author != "user_sam" || e1.Comments[1].Author != "Old Name" {
		t.Fatalf("comments (all pages, members by id): %+v", e1.Comments)
	}
	if len(e1.Attachments) != 1 || e1.Attachments[0].Name != "shot.png" || e1.Attachments[0].URL != "https://uploads.linear.app/ab/cd/shot.png" {
		t.Fatalf("uploads: %+v", e1.Attachments)
	}
	if e2.Number != 2 || e2.Board != "" || e2.Priority != "medium" || e2.Type != "task" {
		t.Fatalf("backlog issue: %+v", e2)
	}
	if e7.Number != 7 || !e7.WontDo || e7.Board != "" || e7.Parent != "e1" || !slices.Equal(e7.BlockedBy, []string{"e1"}) {
		t.Fatalf("cancelled sub-issue: %+v", e7)
	}
	// Design's issues follow Engineering's highest number
	if d1.Number != 8 || !d1.Done || d1.Estimate != "L" || d1.Column != "ds2" || d1.DoneAt == nil || d4.Number != 9 || d4.Estimate != "" {
		t.Fatalf("design: %+v %+v", d1, d4)
	}
	if p1.Type != "epic" || p1.Number != 10 || p1.Title != "Checkout" || p1.Done {
		t.Fatalf("project as epic: %+v", p1)
	}
	if len(plan.Notes) != 1 || plan.Notes[0].Message != "Design's 2 issues become ENG-8 to ENG-9 (each keeps its link to Linear)" {
		t.Fatalf("notes: %+v", plan.Notes)
	}
	if !slices.ContainsFunc(plan.Skipped, func(s imports.Skip) bool { return s.Kind == "estimate" && s.SourceID == "d4" }) {
		t.Fatalf("skipped: %+v", plan.Skipped)
	}
	if len(plan.People) != 1 || plan.People[0] != (imports.Person{Name: "Sam Doe", Matched: true, Tickets: 1}) {
		t.Fatalf("people: %+v", plan.People)
	}

	// ENG is taken here: ENG2; or the prefix given
	if _, err := db.Exec(ctx, `INSERT INTO projects (owner_clerk_id, name, key_prefix) VALUES ('ls1', 'Ours', 'ENG')`); err != nil {
		t.Fatal(err)
	}
	if plan, _ := src.Plan(ctx, "ls1", nil); plan.Projects[0].Prefix != "ENG2" || !strings.Contains(plan.Notes[0].Message, "ENG2-8") {
		t.Fatalf("taken: %+v", plan)
	}
	if plan, _ := src.Plan(ctx, "ls1", json.RawMessage(`{"prefix":"acme"}`)); plan.Projects[0].Prefix != "ACME" {
		t.Fatalf("given: %+v", plan.Projects[0].Prefix)
	}

	// an uploaded file is fetched with the key, only from Linear's uploads
	c, _ := store.Client(ctx, "ls1")
	if data, err := c.Download(ctx, srv.URL+"/upload/shot.png", 100); err != nil || string(data) != "png!" {
		t.Fatalf("download: %q %v", data, err)
	}
	if _, err := c.Download(ctx, "https://evil.example/upload/shot.png", 100); err == nil {
		t.Fatal("the key went elsewhere")
	}

	// through the framework's preview: the notes come with it
	h := imports.NewSystem(db, organizations.Admins{DB: db}, func() bool { return true }, map[string]imports.Source{"linear": src})
	req := httptest.NewRequest("POST", "/", strings.NewReader(`{"source":"linear","options":{}}`)).WithContext(as(ctx, "user_admin", "ls1"))
	rec := httptest.NewRecorder()
	h.Preview(rec, req)
	var im imports.Import
	json.Unmarshal(rec.Body.Bytes(), &im)
	if rec.Code != 201 || len(im.Notes) != 1 || im.Planned == nil || im.Planned.Tickets != 6 {
		t.Fatalf("preview: %d %s", rec.Code, rec.Body)
	}
}

// TL-654: the key is checked with Linear, sealed, kept 30 days, extended or deleted; admins only, behind the flag.
func TestConnection(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	if _, err := db.Exec(ctx, `DELETE FROM linear_connections WHERE org_id IN ('lc1', 'lc2'); DELETE FROM org_admins WHERE org_id IN ('lc1', 'lc2');
		INSERT INTO org_admins (org_id, user_clerk_id, granted_by) VALUES ('lc1', 'user_admin', 'test'), ('lc2', 'user_other', 'test')`); err != nil {
		t.Fatal(err)
	}
	srv, _ := fakeLinear(t)
	box, _ := secrets.New(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{6}, 32)))
	now := time.Now()
	store := Store{DB: db, Box: box, Now: func() time.Time { return now }, Dial: func(key string) Client {
		return Client{Key: key, HTTP: srv.Client(), URL: srv.URL}
	}}
	on := true
	h := System{Store: store, Admins: organizations.Admins{DB: db}, On: func() bool { return on }}
	call := func(c context.Context, f http.HandlerFunc, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/", strings.NewReader(body)).WithContext(c)
		req.Header.Set("Accept", "application/json")
		rec := httptest.NewRecorder()
		f(rec, req)
		return rec
	}
	admin := as(ctx, "user_admin", "lc1")
	if rec := call(admin, h.Connect, `{"key":"nope"}`); rec.Code != 400 || !strings.Contains(rec.Body.String(), "linear_unauthorized") {
		t.Fatalf("bad key: %d %s", rec.Code, rec.Body)
	}
	if rec := call(admin, h.Connect, `{"key":" "}`); rec.Code != 400 || !strings.Contains(rec.Body.String(), "linear_key") {
		t.Fatalf("no key: %d %s", rec.Code, rec.Body)
	}
	rec := call(admin, h.Connect, `{"key":"lin_api_ok"}`)
	var c Connection
	json.Unmarshal(rec.Body.Bytes(), &c)
	if rec.Code != 200 || c.Workspace != "Acme" || c.Account != "Sam Doe" || strings.Contains(rec.Body.String(), "lin_api_ok") {
		t.Fatalf("connect: %d %s", rec.Code, rec.Body)
	}
	var raw []byte
	db.QueryRow(ctx, `SELECT token FROM linear_connections WHERE org_id = 'lc1'`).Scan(&raw)
	if bytes.Contains(raw, []byte("lin_api_ok")) {
		t.Fatal("the key is stored in the clear")
	}
	if rec := call(as(ctx, "user_other", "lc2"), h.Get, ""); strings.TrimSpace(rec.Body.String()) != "null" {
		t.Fatalf("another org: %s", rec.Body)
	}
	if rec := call(as(ctx, "user_member", "lc1"), h.Get, ""); rec.Code != 403 {
		t.Fatalf("member: %d", rec.Code)
	}
	if rec := call(auth.WithAPIKeyUser(ctx, "lc1", "user_admin"), h.Get, ""); rec.Code != 403 {
		t.Fatalf("api key: %d", rec.Code)
	}
	on = false
	if rec := call(admin, h.Get, ""); rec.Code != 404 {
		t.Fatalf("flag off: %d", rec.Code)
	}
	on = true
	now = now.Add(20 * 24 * time.Hour)
	json.Unmarshal(call(admin, h.Extend, "").Body.Bytes(), &c)
	if exp, _ := time.Parse(time.RFC3339, c.ExpiresAt); exp.Sub(now) < Lifetime-time.Minute {
		t.Fatalf("extend: %s", c.ExpiresAt)
	}
	now = now.Add(Lifetime + time.Hour)
	if _, err := store.Client(ctx, "lc1"); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("expired: %v", err)
	}
	call(admin, h.Connect, `{"key":"lin_api_ok"}`)
	if rec := call(admin, h.Delete, ""); rec.Code != 204 {
		t.Fatalf("delete: %d", rec.Code)
	}
	if rec := call(admin, h.Get, ""); strings.TrimSpace(rec.Body.String()) != "null" {
		t.Fatalf("after delete: %s", rec.Body)
	}
}
