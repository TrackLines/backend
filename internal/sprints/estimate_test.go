package sprints

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
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/boards"
	"github.com/tracklines/backend/internal/organizations"
	"github.com/tracklines/backend/internal/tickets"
)

// TL-851: a backlog ticket's estimate can be set without planning it (retro-board's poker), with a service
// key, on the scale of the board it's for; planning or moving it onto that scale keeps it.
func TestSetTicketEstimate(t *testing.T) {
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
	if _, err := db.Exec(ctx, `DELETE FROM org_admins WHERE org_id IN ('ee1', 'ee2');
		DELETE FROM boards WHERE owner_clerk_id IN ('ee1', 'ee2'); DELETE FROM projects WHERE owner_clerk_id IN ('ee1', 'ee2');
		INSERT INTO org_admins (org_id, user_clerk_id, granted_by) VALUES ('ee1', 'user_admin', 'test'), ('ee2', 'user_other', 'test')`); err != nil {
		t.Fatal(err)
	}
	bs := boards.Store{DB: db}
	var pid string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('ee1', 'p') RETURNING id`).Scan(&pid)
	fib, _ := bs.Create(ctx, "ee1", pid, "Web", "")
	shirt, _ := bs.Create(ctx, "ee1", pid, "Apps", "")
	_, _ = db.Exec(ctx, `UPDATE boards SET estimate_scale = 'fibonacci' WHERE id = $1; `, fib.ID)
	_, _ = db.Exec(ctx, `UPDATE boards SET estimate_scale = 'tshirt' WHERE id = $1`, shirt.ID)
	s := Store{DB: db}
	ts := tickets.Store{DB: db}
	h := NewSystem(db, organizations.Admins{DB: db})

	key := func(owner, org, kind string) context.Context {
		return auth.WithAPIKey(ctx, owner, org, "retro-board", kind)
	}
	session := func(user string) context.Context {
		c := &clerk.SessionClaims{RegisteredClaims: clerk.RegisteredClaims{Subject: user}}
		c.ActiveOrganizationID = "ee1"
		return clerk.ContextWithSessionClaims(ctx, c)
	}
	put := func(who context.Context, ticket, body string) (int, string) {
		req := httptest.NewRequest("PUT", "/", strings.NewReader(body)).WithContext(who)
		req.SetPathValue("id", ticket)
		req.Header.Set("Accept", "application/json") // coded errors
		rec := httptest.NewRecorder()
		h.SetEstimate(rec, req)
		return rec.Code, rec.Body.String()
	}
	stored := func(id string) (estimate, board, planned string) {
		var e, b, p *string
		_ = db.QueryRow(ctx, `SELECT estimate, estimate_board_id::text, planned_sprint_id::text FROM tickets WHERE id = $1`, id).Scan(&e, &b, &p)
		for _, x := range []struct {
			in  *string
			out *string
		}{{e, &estimate}, {b, &board}, {p, &planned}} {
			if x.in != nil {
				*x.out = *x.in
			}
		}
		return
	}
	svc := key("user_admin", "ee1", "service")
	tk, _ := ts.CreateBacklog(ctx, "ee1", pid, "task", "from poker", "")

	// two scales in the project: say which board
	if code, body := put(svc, tk.ID, `{"estimate":"5"}`); code != 400 || !strings.Contains(body, "estimate_board_needed") {
		t.Fatalf("no board_id with two scales: %d %s", code, body)
	}
	if code, body := put(svc, tk.ID, `{"estimate":"4","board_id":"`+fib.ID+`"}`); code != 400 || !strings.Contains(body, "bad_estimate") {
		t.Fatalf("off scale: %d %s", code, body)
	}
	if code, _ := put(svc, tk.ID, `{"board_id":"`+fib.ID+`"}`); code != 400 {
		t.Fatalf("no estimate: %d", code)
	}
	// who: refinement is admins, team leaders and service keys; not agents, personal keys or members
	for name, who := range map[string]context.Context{
		"agent key": key("user_admin", "ee1", "ai"), "personal key": key("user_admin", "ee1", "personal"),
		"a member": session("user_member"), "a service key whose maker isn't an admin": key("user_member", "ee1", "service"),
	} {
		if code, _ := put(who, tk.ID, `{"estimate":"5","board_id":"`+fib.ID+`"}`); code != 403 {
			t.Fatalf("%s set an estimate: %d", name, code)
		}
	}
	if code, _ := put(key("user_other", "ee2", "service"), tk.ID, `{"estimate":"5","board_id":"`+fib.ID+`"}`); code != 404 {
		t.Fatalf("another org: %d", code)
	}

	// set, without planning it
	if code, body := put(svc, tk.ID, `{"estimate":"5","board_id":"`+fib.ID+`"}`); code != 200 || !strings.Contains(body, `"estimate_scale":"fibonacci"`) {
		t.Fatalf("set: %d %s", code, body)
	}
	if e, b, p := stored(tk.ID); e != "5" || b != fib.ID || p != "" {
		t.Fatalf("stored: %q %q %q", e, b, p)
	}
	if code, _ := put(session("user_admin"), tk.ID, `{"estimate":"8","board_id":"`+fib.ID+`"}`); code != 200 {
		t.Fatalf("an admin in the app: %d", code)
	}

	// planning onto the same scale keeps it; a different scale clears it, as before
	if _, err := s.Start(ctx, "ee1", fib.ID, 14); err != nil {
		t.Fatal(err)
	}
	fibPlans, _ := s.Plan(ctx, "ee1", fib.ID, 14, "")
	if err := s.PlanTicket(ctx, "ee1", tk.ID, &fibPlans[0].ID, nil); err != nil {
		t.Fatal(err)
	}
	if e, _, p := stored(tk.ID); e != "8" || p != fibPlans[0].ID {
		t.Fatalf("planned onto the same scale: %q %q", e, p)
	}
	other, _ := ts.CreateBacklog(ctx, "ee1", pid, "task", "other", "")
	_, _ = put(svc, other.ID, `{"estimate":"3","board_id":"`+fib.ID+`"}`)
	if _, err := s.Start(ctx, "ee1", shirt.ID, 14); err != nil {
		t.Fatal(err)
	}
	shirtPlans, _ := s.Plan(ctx, "ee1", shirt.ID, 14, "")
	if err := s.PlanTicket(ctx, "ee1", other.ID, &shirtPlans[0].ID, nil); err != nil {
		t.Fatal(err)
	}
	if e, _, _ := stored(other.ID); e != "" {
		t.Fatalf("planned onto another scale kept %q", e)
	}
	// a planned ticket's estimate goes on its planned board's scale, whatever board_id says
	if code, body := put(svc, other.ID, `{"estimate":"M","board_id":"`+fib.ID+`"}`); code != 200 || !strings.Contains(body, `"estimate_scale":"tshirt"`) {
		t.Fatalf("planned ticket: %d %s", code, body)
	}

	// moving onto a board on the same scale keeps it
	moved, _ := ts.CreateBacklog(ctx, "ee1", pid, "task", "moved", "")
	_, _ = put(svc, moved.ID, `{"estimate":"13","board_id":"`+fib.ID+`"}`)
	if err := ts.Move(ctx, "ee1", moved.ID, fib.Columns[0].ID, 0); err != nil {
		t.Fatal(err)
	}
	if e, _, _ := stored(moved.ID); e != "13" {
		t.Fatalf("moved onto the same scale: %q", e)
	}

	// "" clears; epics take none
	if code, _ := put(svc, tk.ID, `{"estimate":""}`); code != 200 {
		t.Fatalf("clear: %d", code)
	}
	if e, _, _ := stored(tk.ID); e != "" {
		t.Fatalf("cleared: %q", e)
	}
	epic, _ := ts.CreateBacklog(ctx, "ee1", pid, "epic", "big", "")
	if code, body := put(svc, epic.ID, `{"estimate":"5","board_id":"`+fib.ID+`"}`); code != 409 || !strings.Contains(body, "epic_estimate") {
		t.Fatalf("epic: %d %s", code, body)
	}

	// one scale in the project: no board_id needed; no scale at all: said so
	_, _ = db.Exec(ctx, `UPDATE boards SET estimate_scale = 'none' WHERE id = $1`, shirt.ID)
	single, _ := ts.CreateBacklog(ctx, "ee1", pid, "task", "single", "")
	if code, body := put(svc, single.ID, `{"estimate":"2"}`); code != 200 || !strings.Contains(body, fib.ID) {
		t.Fatalf("one scale: %d %s", code, body)
	}
	_, _ = db.Exec(ctx, `UPDATE boards SET estimate_scale = 'none' WHERE id = $1`, fib.ID)
	none, _ := ts.CreateBacklog(ctx, "ee1", pid, "task", "none", "")
	if code, body := put(svc, none.ID, `{"estimate":"2"}`); code != 400 || !strings.Contains(body, "no_estimate_scale") {
		t.Fatalf("no scale: %d %s", code, body)
	}
}
