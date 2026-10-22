package boards

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
	"github.com/tracklines/backend/internal/organizations"
)

// The board handlers (settings, flow, archive/restore, delete): who may, in which org, and the
// conflicts that keep history.
func TestBoardHandlers(t *testing.T) {
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
	const org, other = "org_bh1", "org_bh2"
	for _, q := range []string{
		`DELETE FROM boards WHERE owner_clerk_id IN ($1, $2)`, `DELETE FROM projects WHERE owner_clerk_id IN ($1, $2)`,
		`DELETE FROM teams WHERE org_id IN ($1, $2)`, `DELETE FROM org_admins WHERE org_id IN ($1, $2)`,
		`INSERT INTO org_admins (org_id, user_clerk_id, granted_by) VALUES ($1, 'admin-u', 'test'), ($2, 'admin-x', 'test')`,
	} {
		if _, err := db.Exec(ctx, q, org, other); err != nil {
			t.Fatal(err)
		}
	}
	s := Store{DB: db}
	var pid string
	if err := db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ($1, 'Boards') RETURNING id`, org).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	led, _ := s.Create(ctx, org, pid, "Led", "")      // lead-u leads its team
	used, _ := s.Create(ctx, org, pid, "Used", "")    // has a ticket
	unused, _ := s.Create(ctx, org, pid, "Empty", "") // never used
	var team string
	if err := db.QueryRow(ctx, `INSERT INTO teams (org_id, name) VALUES ($1, 'Squad') RETURNING id`, org).Scan(&team); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO team_members (team_id, user_clerk_id, leader) VALUES ($1, 'lead-u', true), ($1, 'member-u', false)`, team); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `UPDATE boards SET team_id = $1 WHERE id = $2`, team, led.ID); err != nil {
		t.Fatal(err)
	}
	var ticket string
	if err := db.QueryRow(ctx, `INSERT INTO tickets (project_id, board_id, column_id, title, position) VALUES ($1, $2, $3, 'open work', 0) RETURNING id`,
		pid, used.ID, used.Columns[0].ID).Scan(&ticket); err != nil {
		t.Fatal(err)
	}

	h := NewSystem(db, organizations.Admins{DB: db})
	as := func(o, user string) context.Context {
		c := &clerk.SessionClaims{RegisteredClaims: clerk.RegisteredClaims{Subject: user}}
		c.ActiveOrganizationID = o
		return clerk.ContextWithSessionClaims(ctx, c)
	}
	run := func(name string, o, user, id, body string) *httptest.ResponseRecorder {
		t.Helper()
		action, query, _ := strings.Cut(name, "?")
		req := httptest.NewRequest("POST", "/?"+query, strings.NewReader(body)).WithContext(as(o, user))
		req.SetPathValue("id", id)
		rec := httptest.NewRecorder()
		switch action {
		case "update":
			h.Update(rec, req)
		case "flow":
			h.Flow(rec, req)
		case "archive":
			h.Archive(rec, req)
		case "restore":
			h.Restore(rec, req)
		case "archived":
			h.Archived(rec, req)
		case "delete":
			h.Delete(rec, req)
		}
		return rec
	}
	expect := func(want int, name, o, user, id, body string) *httptest.ResponseRecorder {
		t.Helper()
		rec := run(name, o, user, id, body)
		if rec.Code != want {
			t.Fatalf("%s as %s/%s: %d %s, want %d", name, o, user, rec.Code, rec.Body, want)
		}
		return rec
	}

	// settings: admins and the board's team leaders; a plain member can't, another org can't see it
	expect(204, "update", org, "admin-u", used.ID, `{"name":"Used board","estimate_scale":"fibonacci","style":"kanban","agent_review":true}`)
	got, _ := s.Get(ctx, used.ID, org)
	if got.Name != "Used board" || got.EstimateScale != "fibonacci" || got.Style != "kanban" || !got.AgentReview {
		t.Fatalf("settings saved: %+v", got)
	}
	expect(204, "update", org, "lead-u", led.ID, `{"name":"Led by lead"}`)
	expect(403, "update", org, "lead-u", used.ID, `{"name":"not my board"}`)
	expect(403, "update", org, "member-u", led.ID, `{"name":"member"}`)
	expect(404, "update", other, "admin-x", used.ID, `{"name":"hijack"}`)
	expect(400, "update", org, "admin-u", used.ID, `{}`)
	expect(400, "update", org, "admin-u", used.ID, `{"name":"  "}`)
	expect(400, "update", org, "admin-u", used.ID, `{"estimate_scale":"bananas"}`)
	expect(400, "update", org, "admin-u", used.ID, `{"style":"waterfall"}`)
	// Dev complete (TL-531): one of the board's own columns, not Done; "" turns it off
	expect(204, "update", org, "admin-u", used.ID, `{"dev_complete_column_id":"`+used.Columns[1].ID+`"}`)
	if got, _ := s.Get(ctx, used.ID, org); got.DevComplete == nil || *got.DevComplete != used.Columns[1].ID {
		t.Fatalf("dev complete saved: %+v", got.DevComplete)
	}
	expect(400, "update", org, "admin-u", used.ID, `{"dev_complete_column_id":"`+used.Columns[len(used.Columns)-1].ID+`"}`)
	expect(400, "update", org, "admin-u", used.ID, `{"dev_complete_column_id":"`+led.Columns[1].ID+`"}`)
	expect(400, "update", org, "admin-u", used.ID, `{"dev_complete_column_id":"nope"}`)
	expect(404, "update", other, "admin-x", used.ID, `{"dev_complete_column_id":""}`)
	if got, _ := s.Get(ctx, used.ID, org); got.DevComplete == nil {
		t.Fatal("a refused change cleared Dev complete")
	}
	expect(204, "update", org, "admin-u", used.ID, `{"dev_complete_column_id":""}`)
	if got, _ := s.Get(ctx, used.ID, org); got.DevComplete != nil {
		t.Fatalf("dev complete cleared: %+v", got.DevComplete)
	}

	// flow: any member of the org; weeks bounded; another org can't see it
	if rec := expect(200, "flow?weeks=4", org, "member-u", used.ID, ""); !json.Valid(rec.Body.Bytes()) {
		t.Fatalf("flow: %s", rec.Body)
	}
	expect(400, "flow?weeks=0", org, "member-u", used.ID, "")
	expect(400, "flow?weeks=53", org, "member-u", used.ID, "")
	expect(404, "flow", other, "admin-x", used.ID, "")

	// delete: admins only, and only a board that was never used (archive keeps history)
	expect(403, "delete", org, "lead-u", unused.ID, "")
	expect(409, "delete", org, "admin-u", used.ID, "")
	expect(404, "delete", other, "admin-x", unused.ID, "")
	expect(204, "delete", org, "admin-u", unused.ID, "")
	if _, err := s.Get(ctx, unused.ID, org); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted board still there: %v", err)
	}

	// archive: refused while work is unfinished; then listed as archived, and restorable
	expect(409, "archive", org, "admin-u", used.ID, "")
	if _, err := db.Exec(ctx, `UPDATE tickets SET resolved_at = now() WHERE id = $1`, ticket); err != nil {
		t.Fatal(err)
	}
	expect(403, "archive", org, "member-u", used.ID, "")
	expect(404, "archive", other, "admin-x", used.ID, "")
	expect(204, "archive", org, "admin-u", used.ID, "")
	var archived []Board
	_ = json.Unmarshal(expect(200, "archived", org, "member-u", pid, "").Body.Bytes(), &archived)
	if len(archived) != 1 || archived[0].ID != used.ID {
		t.Fatalf("archived list: %+v", archived)
	}
	if rec := expect(200, "archived", other, "admin-x", pid, ""); strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Fatalf("another org's archived list: %s", rec.Body)
	}
	expect(404, "restore", other, "admin-x", used.ID, "")
	expect(403, "restore", org, "member-u", used.ID, "")
	expect(204, "restore", org, "admin-u", used.ID, "")
	if rec := expect(200, "archived", org, "member-u", pid, ""); strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Fatalf("restored board still archived: %s", rec.Body)
	}
}
