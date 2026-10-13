package columns

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/clerk/clerk-sdk-go/v2"
	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/boards"
	"github.com/tracklines/backend/internal/organizations"
)

// Column handlers: add, rename / WIP limit, delete (only when empty), reorder (an exact permutation);
// board managers only, never across orgs.
func TestColumnHandlers(t *testing.T) {
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
	const org, other = "org_ch1", "org_ch2"
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
	if err := db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ($1, 'Columns') RETURNING id`, org).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	b, err := boards.Store{DB: db}.Create(ctx, org, pid, "Board", "")
	if err != nil {
		t.Fatal(err)
	}
	h := NewSystem(db, organizations.Admins{DB: db})
	run := func(action, o, user, id, body string) int {
		t.Helper()
		c := &clerk.SessionClaims{RegisteredClaims: clerk.RegisteredClaims{Subject: user}}
		c.ActiveOrganizationID = o
		req := httptest.NewRequest("POST", "/", strings.NewReader(body)).WithContext(clerk.ContextWithSessionClaims(ctx, c))
		req.SetPathValue("boardID", id)
		req.SetPathValue("id", id)
		rec := httptest.NewRecorder()
		map[string]func(){"create": func() { h.Create(rec, req) }, "update": func() { h.Update(rec, req) },
			"delete": func() { h.Delete(rec, req) }, "reorder": func() { h.Reorder(rec, req) }}[action]()
		return rec.Code
	}
	expect := func(want int, action, o, user, id, body string) {
		t.Helper()
		if got := run(action, o, user, id, body); got != want {
			t.Fatalf("%s as %s/%s on %s %s: %d, want %d", action, o, user, id, body, got, want)
		}
	}
	columns := func() (ids, names []string, wip map[string]*int) {
		rows, err := db.Query(ctx, `SELECT id::text, name, wip_limit FROM columns WHERE board_id = $1 ORDER BY position`, b.ID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		wip = map[string]*int{}
		for rows.Next() {
			var id, name string
			var limit *int
			_ = rows.Scan(&id, &name, &limit)
			ids, names, wip[name] = append(ids, id), append(names, name), limit
		}
		return
	}
	start, _, _ := columns()

	// add: managers only, a name is required, another org can't
	expect(403, "create", org, "member-u", b.ID, `{"name":"QA"}`)
	expect(404, "create", other, "admin-x", b.ID, `{"name":"QA"}`)
	expect(400, "create", org, "admin-u", b.ID, `{"name":"  "}`)
	expect(201, "create", org, "admin-u", b.ID, `{"name":"QA"}`)
	ids, names, _ := columns()
	if len(ids) != len(start)+1 {
		t.Fatalf("added: %v", names)
	}
	qa := ids[len(ids)-1]

	// rename and WIP limit (0 removes it); bounds; another org can't
	expect(204, "update", org, "admin-u", qa, `{"name":"Testing","wip_limit":3}`)
	if _, names, wip := columns(); names[len(names)-1] != "Testing" || wip["Testing"] == nil || *wip["Testing"] != 3 {
		t.Fatalf("renamed with limit: %v %v", names, wip)
	}
	expect(204, "update", org, "admin-u", qa, `{"wip_limit":0}`)
	if _, _, wip := columns(); wip["Testing"] != nil {
		t.Fatalf("limit removed: %v", *wip["Testing"])
	}
	expect(400, "update", org, "admin-u", qa, `{}`)
	expect(400, "update", org, "admin-u", qa, `{"name":""}`)
	expect(400, "update", org, "admin-u", qa, `{"wip_limit":1000}`)
	expect(403, "update", org, "member-u", qa, `{"name":"x"}`)
	expect(404, "update", other, "admin-x", qa, `{"name":"x"}`)

	// reorder: an exact permutation only
	ids, _, _ = columns()
	reversed := make([]string, len(ids))
	for i, id := range ids {
		reversed[len(ids)-1-i] = id
	}
	list := func(xs []string) string { return `{"column_ids":["` + strings.Join(xs, `","`) + `"]}` }
	expect(400, "reorder", org, "admin-u", b.ID, list(ids[1:]))
	expect(400, "reorder", org, "admin-u", b.ID, list(append(ids[1:], ids[1])))
	expect(403, "reorder", org, "member-u", b.ID, list(reversed))
	expect(404, "reorder", other, "admin-x", b.ID, list(reversed))
	expect(204, "reorder", org, "admin-u", b.ID, list(reversed))
	if now, _, _ := columns(); strings.Join(now, ",") != strings.Join(reversed, ",") {
		t.Fatalf("reordered: %v want %v", now, reversed)
	}

	// delete: refused while it holds the sprint's tickets; managers only; another org can't
	if _, err := db.Exec(ctx, `INSERT INTO tickets (project_id, board_id, column_id, sprint_id, title, position)
		VALUES ($1, $2, $3, (SELECT id FROM sprints WHERE board_id = $2 AND closed_at IS NULL), 'in QA', 0)`, pid, b.ID, qa); err != nil {
		t.Fatal(err)
	}
	expect(409, "delete", org, "admin-u", qa, "")
	if _, err := db.Exec(ctx, `DELETE FROM tickets WHERE column_id = $1`, qa); err != nil {
		t.Fatal(err)
	}
	expect(403, "delete", org, "member-u", qa, "")
	expect(404, "delete", other, "admin-x", qa, "")
	expect(204, "delete", org, "admin-u", qa, "")
	if ids, _, _ := columns(); len(ids) != len(start) {
		t.Fatalf("deleted: %v", ids)
	}

	// with an open sprint, only the sprint's tickets hold a column (TL-200: no sprint used to 500)
	expect(201, "create", org, "admin-u", b.ID, `{"name":"Review"}`)
	ids, _, _ = columns()
	review := ids[len(ids)-1]
	var sprint string
	if err := db.QueryRow(ctx, `INSERT INTO sprints (board_id, number, length_days, ends_at) VALUES ($1, 1, 14, now() + interval '14 days') RETURNING id`, b.ID).Scan(&sprint); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO tickets (project_id, board_id, column_id, sprint_id, title, position) VALUES ($1, $2, $3, $4, 'in review', 0)`, pid, b.ID, review, sprint); err != nil {
		t.Fatal(err)
	}
	expect(409, "delete", org, "admin-u", review, "")
	if _, err := db.Exec(ctx, `DELETE FROM tickets WHERE column_id = $1`, review); err != nil {
		t.Fatal(err)
	}
	expect(204, "delete", org, "admin-u", review, "")
}
