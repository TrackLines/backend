package boards

import (
	"context"
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

func TestTemplates(t *testing.T) {
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
	const org = "tpl-org"
	if _, err := db.Exec(ctx, `DELETE FROM board_templates WHERE org_id = 'tpl-org'; DELETE FROM boards WHERE owner_clerk_id = 'tpl-org';
		DELETE FROM projects WHERE owner_clerk_id = 'tpl-org'; DELETE FROM org_admins WHERE org_id = 'tpl-org';
		INSERT INTO org_admins (org_id, user_clerk_id, granted_by) VALUES ('tpl-org', 'admin-u', 'test')`); err != nil {
		t.Fatal(err)
	}
	var pid string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ($1, 'p') RETURNING id`, org).Scan(&pid)
	s := Store{DB: db}

	// a built-in sets columns, WIP limits, style and scale
	kanban, err := s.Template(ctx, org, "builtin:kanban")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateFrom(ctx, org, pid, "Flow", "", kanban)
	if err != nil {
		t.Fatal(err)
	}
	full, _ := s.Get(ctx, b.ID, org)
	if full.Style != "kanban" || len(full.Columns) != 4 || full.Columns[1].WIPLimit == nil || *full.Columns[1].WIPLimit != 3 || full.Columns[3].Name != "Done" {
		t.Fatalf("kanban board: %+v", full)
	}
	// plain Create is still Simple
	if simple, _ := s.Create(ctx, org, pid, "Plain", ""); len(simple.Columns) != 3 || simple.Style != "sprints" {
		t.Fatalf("simple board: %+v", simple)
	}

	// save a board's setup as an org template, then build from it
	from, err := s.TemplateFromBoard(ctx, org, b.ID)
	if err != nil || len(from.Columns) != 4 || from.Style != "kanban" {
		t.Fatalf("from board: %+v %v", from, err)
	}
	from.Name = "Our flow"
	saved, err := s.SaveTemplate(ctx, org, from)
	if err != nil {
		t.Fatal(err)
	}
	if all, _ := s.Templates(ctx, org); len(all) != len(Builtins)+1 || all[len(all)-1].Name != "Our flow" {
		t.Fatalf("list: %+v", all)
	}
	if _, err := s.Template(ctx, "other-org", saved.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("another org's template should be invisible:", err)
	}

	// HTTP: admins only, validation, built-ins can't be deleted
	h := NewSystem(db, organizations.Admins{DB: db})
	as := func(user string) context.Context {
		c := &clerk.SessionClaims{RegisteredClaims: clerk.RegisteredClaims{Subject: user}}
		c.ActiveOrganizationID = org
		return clerk.ContextWithSessionClaims(ctx, c)
	}
	create := func(user, body string) int {
		rec := httptest.NewRecorder()
		h.CreateTemplate(rec, httptest.NewRequest("POST", "/", strings.NewReader(body)).WithContext(as(user)))
		return rec.Code
	}
	if code := create("member-u", `{"name":"x","from_board_id":"`+b.ID+`"}`); code != 403 {
		t.Fatalf("non-admin: %d", code)
	}
	if code := create("admin-u", `{"name":"x","style":"sprints","estimate_scale":"none","columns":[]}`); code != 400 {
		t.Fatalf("no columns: %d", code)
	}
	if code := create("admin-u", `{"name":"Copied","from_board_id":"`+b.ID+`"}`); code != 201 {
		t.Fatalf("from board: %d", code)
	}
	del := func(id string) int {
		req := httptest.NewRequest("DELETE", "/", nil).WithContext(as("admin-u"))
		req.SetPathValue("id", id)
		rec := httptest.NewRecorder()
		h.DeleteTemplate(rec, req)
		return rec.Code
	}
	if code := del("builtin:simple"); code != 400 {
		t.Fatalf("delete built-in: %d", code)
	}
	if code := del(saved.ID); code != 204 {
		t.Fatalf("delete: %d", code)
	}
	// board create with a template, and an unknown one
	createBoard := func(body string) int {
		req := httptest.NewRequest("POST", "/", strings.NewReader(body)).WithContext(as("admin-u"))
		req.SetPathValue("id", pid)
		rec := httptest.NewRecorder()
		h.Create(rec, req)
		return rec.Code
	}
	if code := createBoard(`{"name":"S","template_id":"builtin:scrum"}`); code != 201 {
		t.Fatalf("board from scrum: %d", code)
	}
	if code := createBoard(`{"name":"S","template_id":"nope"}`); code != 400 {
		t.Fatalf("unknown template: %d", code)
	}
}
