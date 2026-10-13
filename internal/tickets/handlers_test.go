package tickets

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/clerk/clerk-sdk-go/v2"
	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/boards"
)

// Handlers not covered elsewhere: backlog list, blocked-by, external links (integrations only) and
// delete; all scoped to the caller's org.
func TestTicketHandlers(t *testing.T) {
	ctx := context.Background()
	url := os.Getenv("TEST_DATABASE_URL")
	m, err := migrate.New("file://../migrations", "pgx5"+url[len("postgres"):])
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
	defer db.Close()
	const org, other = "org_tk1", "org_tk2"
	if _, err := db.Exec(ctx, `DELETE FROM boards WHERE owner_clerk_id IN ('org_tk1', 'org_tk2'); DELETE FROM projects WHERE owner_clerk_id IN ('org_tk1', 'org_tk2')`); err != nil {
		t.Fatal(err)
	}
	s := Store{DB: db}
	var pid, otherPid string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ($1, 'Mine') RETURNING id`, org).Scan(&pid)
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ($1, 'Theirs') RETURNING id`, other).Scan(&otherPid)
	b, _ := boards.Store{DB: db}.Create(ctx, org, pid, "Board", "")
	bug, _ := s.CreateBacklog(ctx, org, pid, "bug", "a bug", "")
	idea, _ := s.CreateBacklog(ctx, org, pid, "feature", "an idea", "")
	onBoard, _ := s.Create(ctx, org, b.Columns[0].ID, "task", "on the board", "")
	theirs, _ := s.CreateBacklog(ctx, other, otherPid, "task", "theirs", "")

	h := NewSystem(db)
	person := func(o string) context.Context { return auth.WithAPIKey(ctx, "user_tk", o, "ui", "") } // OrgID + UserID; ViaAPIKey
	run := func(action string, c context.Context, id, query, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest("POST", "/?"+query, strings.NewReader(body)).WithContext(c)
		req.SetPathValue("id", id)
		rec := httptest.NewRecorder()
		map[string]func(){"backlog": func() { h.Backlog(rec, req) }, "blockedBy": func() { h.SetBlockedBy(rec, req) },
			"external": func() { h.SetExternal(rec, req) }, "delete": func() { h.Delete(rec, req) }}[action]()
		return rec
	}
	expect := func(want int, action string, c context.Context, id, query, body string) *httptest.ResponseRecorder {
		t.Helper()
		rec := run(action, c, id, query, body)
		if rec.Code != want {
			t.Fatalf("%s %s %s %s: %d %s, want %d", action, id, query, body, rec.Code, rec.Body, want)
		}
		return rec
	}
	titles := func(rec *httptest.ResponseRecorder) string {
		var out []Ticket
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		var ts []string
		for _, t := range out {
			ts = append(ts, t.Title)
		}
		return strings.Join(ts, ",")
	}

	// backlog: only the project's off-board tickets, filterable by type; another org gets 404
	if got := titles(expect(200, "backlog", person(org), pid, "", "")); !strings.Contains(got, "a bug") || !strings.Contains(got, "an idea") || strings.Contains(got, "on the board") {
		t.Fatalf("backlog: %s", got)
	}
	if got := titles(expect(200, "backlog", person(org), pid, "type=bug", "")); got != "a bug" {
		t.Fatalf("bugs only: %s", got)
	}
	expect(400, "backlog", person(org), pid, "type=epic", "")
	expect(404, "backlog", person(other), pid, "", "")

	// blocked by: same project only, no self or cycles; [] clears; another org can't
	expect(204, "blockedBy", person(org), onBoard.ID, "", `{"ticket_ids":["`+bug.ID+`"]}`)
	if got, _ := s.Get(ctx, org, onBoard.ID); !got.Blocked {
		t.Fatalf("not blocked: %+v", got)
	}
	expect(400, "blockedBy", person(org), onBoard.ID, "", `{"ticket_ids":["`+onBoard.ID+`"]}`)
	expect(400, "blockedBy", person(org), bug.ID, "", `{"ticket_ids":["`+onBoard.ID+`"]}`) // would be a cycle
	expect(400, "blockedBy", person(org), onBoard.ID, "", `{"ticket_ids":["`+theirs.ID+`"]}`)
	expect(404, "blockedBy", person(other), onBoard.ID, "", `{"ticket_ids":[]}`)
	expect(204, "blockedBy", person(org), onBoard.ID, "", `{"ticket_ids":[]}`)
	if got, _ := s.Get(ctx, org, onBoard.ID); got.Blocked {
		t.Fatalf("still blocked: %+v", got)
	}

	// external: integrations (API keys) only, validated; null clears
	session := &clerk.SessionClaims{RegisteredClaims: clerk.RegisteredClaims{Subject: "user_tk"}}
	session.ActiveOrganizationID = org
	link := `{"source":"bugfixes","project":"42","id":"7","url":"https://bugfix.es/r/7"}`
	expect(403, "external", clerk.ContextWithSessionClaims(ctx, session), bug.ID, "", link) // a person can't
	expect(204, "external", auth.WithAPIKey(ctx, "user_tk", org, "Bugfixes", "service"), bug.ID, "", link)
	var source, ext *string
	_ = db.QueryRow(ctx, `SELECT external_source, external_id FROM tickets WHERE id = $1`, bug.ID).Scan(&source, &ext)
	if source == nil || *source != "bugfixes" || ext == nil || *ext != "7" {
		t.Fatalf("linked: %v %v", source, ext)
	}
	expect(400, "external", auth.WithAPIKey(ctx, "user_tk", org, "Bugfixes", "service"), bug.ID, "", `{"source":"jira","project":"1","id":"2"}`)
	expect(400, "external", auth.WithAPIKey(ctx, "user_tk", org, "Bugfixes", "service"), bug.ID, "", `{"source":"bugfixes","project":"1","id":"2","url":"javascript:alert(1)"}`)
	expect(404, "external", auth.WithAPIKey(ctx, "user_tk", other, "Bugfixes", "service"), bug.ID, "", link)
	expect(204, "external", auth.WithAPIKey(ctx, "user_tk", org, "Bugfixes", "service"), bug.ID, "", `null`)
	_ = db.QueryRow(ctx, `SELECT external_source FROM tickets WHERE id = $1`, bug.ID).Scan(&source)
	if source != nil {
		t.Fatalf("not cleared: %v", *source)
	}

	// delete: own org only; really gone
	expect(404, "delete", person(other), idea.ID, "", "")
	expect(204, "delete", person(org), idea.ID, "", "")
	if _, err := s.Get(ctx, org, idea.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted ticket still there: %v", err)
	}
	expect(404, "delete", person(org), idea.ID, "", "")
}
