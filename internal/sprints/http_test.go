package sprints

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
	"github.com/tracklines/backend/internal/boards"
	"github.com/tracklines/backend/internal/organizations"
)

// The sprint handlers end to end: start (managers; pulls in the board's tickets), list, detail,
// length/goal, close (carries unfinished work over), velocity; never across orgs.
func TestSprintHandlers(t *testing.T) {
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
	const org, other = "org_sh1", "org_sh2"
	for _, q := range []string{
		`DELETE FROM boards WHERE owner_clerk_id IN ($1, $2)`, `DELETE FROM projects WHERE owner_clerk_id IN ($1, $2)`,
		`DELETE FROM org_admins WHERE org_id IN ($1, $2)`,
		`INSERT INTO org_admins (org_id, user_clerk_id, granted_by) VALUES ($1, 'admin-u', 'test'), ($2, 'admin-x', 'test')`,
	} {
		if _, err := db.Exec(ctx, q, org, other); err != nil {
			t.Fatal(err)
		}
	}
	var pid string
	if err := db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ($1, 'Sprints') RETURNING id`, org).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	bs := boards.Store{DB: db}
	b, _ := bs.Create(ctx, org, pid, "Team", "")
	flow, _ := bs.Create(ctx, org, pid, "Flow", "")
	if err := bs.SetStyle(ctx, flow.ID, org, "kanban"); err != nil {
		t.Fatal(err)
	}
	var todo, finished string
	if err := db.QueryRow(ctx, `INSERT INTO tickets (project_id, board_id, column_id, title, position) VALUES ($1, $2, $3, 'carry me', 0) RETURNING id`,
		pid, b.ID, b.Columns[0].ID).Scan(&todo); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `INSERT INTO tickets (project_id, board_id, column_id, title, position) VALUES ($1, $2, $3, 'finished', 0) RETURNING id`,
		pid, b.ID, b.Columns[len(b.Columns)-1].ID).Scan(&finished); err != nil {
		t.Fatal(err)
	}

	h := NewSystem(db, organizations.Admins{DB: db})
	run := func(action, o, user, id, body string) *httptest.ResponseRecorder {
		t.Helper()
		c := &clerk.SessionClaims{RegisteredClaims: clerk.RegisteredClaims{Subject: user}}
		c.ActiveOrganizationID = o
		req := httptest.NewRequest("POST", "/", strings.NewReader(body)).WithContext(clerk.ContextWithSessionClaims(ctx, c))
		req.SetPathValue("id", id)
		rec := httptest.NewRecorder()
		map[string]func(){"start": func() { h.Start(rec, req) }, "list": func() { h.List(rec, req) }, "get": func() { h.Get(rec, req) },
			"update": func() { h.Update(rec, req) }, "close": func() { h.Close(rec, req) }, "velocity": func() { h.Velocity(rec, req) }}[action]()
		return rec
	}
	expect := func(want int, action, o, user, id, body string, out any) {
		t.Helper()
		rec := run(action, o, user, id, body)
		if rec.Code != want {
			t.Fatalf("%s as %s/%s %s: %d %s, want %d", action, o, user, body, rec.Code, rec.Body, want)
		}
		if out != nil && json.Unmarshal(rec.Body.Bytes(), out) != nil {
			t.Fatalf("%s: bad JSON %s", action, rec.Body)
		}
	}

	// start: managers only, valid length and goal, not twice, not on kanban; pulls in the board's tickets
	expect(403, "start", org, "member-u", b.ID, `{"length_days":14}`, nil)
	expect(404, "start", other, "admin-x", b.ID, `{"length_days":14}`, nil)
	expect(400, "start", org, "admin-u", b.ID, `{"length_days":0}`, nil)
	expect(400, "start", org, "admin-u", b.ID, `{"length_days":14,"goal":"`+strings.Repeat("g", 201)+`"}`, nil)
	expect(409, "start", org, "admin-u", flow.ID, `{"length_days":14}`, nil)
	var first Sprint
	expect(201, "start", org, "admin-u", b.ID, `{"length_days":14,"goal":"Ship search"}`, &first)
	if first.Number != 1 || first.LengthDays != 14 || first.Goal != "Ship search" {
		t.Fatalf("started: %+v", first)
	}
	expect(409, "start", org, "admin-u", b.ID, `{"length_days":14}`, nil)
	var inSprint int
	_ = db.QueryRow(ctx, `SELECT count(*) FROM tickets WHERE board_id = $1 AND sprint_id = $2`, b.ID, first.ID).Scan(&inSprint)
	if inSprint != 2 {
		t.Fatalf("tickets pulled in: %d", inSprint)
	}

	// list and detail: any member; another org can't see them
	var list []Sprint
	expect(200, "list", org, "member-u", b.ID, "", &list)
	if len(list) != 1 || list[0].ID != first.ID {
		t.Fatalf("list: %+v", list)
	}
	expect(404, "list", other, "admin-x", b.ID, "", nil)
	var detail SprintDetail
	expect(200, "get", org, "member-u", first.ID, "", &detail)
	if detail.ID != first.ID || len(detail.Tickets) != 2 {
		t.Fatalf("detail: %+v", detail)
	}
	expect(404, "get", other, "admin-x", first.ID, "", nil)

	// length and goal: managers only; something to change is required
	var updated Sprint
	expect(200, "update", org, "admin-u", first.ID, `{"length_days":7,"goal":"Ship search v2"}`, &updated)
	if updated.LengthDays != 7 || updated.Goal != "Ship search v2" || updated.StartsAt != first.StartsAt {
		t.Fatalf("updated: %+v (was %+v)", updated, first)
	}
	expect(400, "update", org, "admin-u", first.ID, `{}`, nil)
	expect(400, "update", org, "admin-u", first.ID, `{"length_days":366}`, nil)
	expect(403, "update", org, "member-u", first.ID, `{"goal":"x"}`, nil)
	expect(404, "update", other, "admin-x", first.ID, `{"goal":"x"}`, nil)

	// close: managers only; opens the next sprint and carries unfinished work over, leaving done work behind
	expect(403, "close", org, "member-u", first.ID, "", nil)
	expect(404, "close", other, "admin-x", first.ID, "", nil)
	expect(400, "close", org, "admin-u", first.ID, `{"next_length_days":400}`, nil)
	var next Sprint
	expect(200, "close", org, "admin-u", first.ID, "", &next)
	if next.Number != 2 || next.LengthDays != 7 {
		t.Fatalf("next sprint: %+v", next)
	}
	var carried, stayed string
	_ = db.QueryRow(ctx, `SELECT sprint_id::text FROM tickets WHERE id = $1`, todo).Scan(&carried)
	_ = db.QueryRow(ctx, `SELECT sprint_id::text FROM tickets WHERE id = $1`, finished).Scan(&stayed)
	if carried != next.ID || stayed != first.ID {
		t.Fatalf("carry over: unfinished in %s (want %s), done in %s (want %s)", carried, next.ID, stayed, first.ID)
	}
	expect(404, "close", org, "admin-u", first.ID, "", nil) // already closed

	// velocity: the closed sprint's finished work, and the open one's burn
	var v Velocity
	expect(200, "velocity", org, "member-u", b.ID, "", &v)
	if len(v.Sprints) != 1 || v.Sprints[0].Number != 1 || v.Current == nil {
		t.Fatalf("velocity: %+v", v)
	}
	expect(404, "velocity", other, "admin-x", b.ID, "", nil)
}
