package tickets

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
)

// Assign on move (TL-266): a person moving an unassigned ticket to another column takes it, except
// into To do, by an API key, over someone else, while blocked, or as the board's manager during its standup.
func TestAssignOnMove(t *testing.T) {
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
	const org = "aom1"
	for _, q := range []string{`DELETE FROM boards WHERE owner_clerk_id = 'aom1'`, `DELETE FROM projects WHERE owner_clerk_id = 'aom1'`} {
		if _, err := db.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	var pid string
	if err := db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ($1, 'Move') RETURNING id`, org).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	b, err := boards.Store{DB: db}.Create(ctx, org, pid, "b", "")
	if err != nil || len(b.Columns) < 3 {
		t.Fatalf("board: %v %+v", err, b.Columns)
	}
	todo, doing, done := b.Columns[0].ID, b.Columns[1].ID, b.Columns[len(b.Columns)-1].ID
	s := Store{DB: db}
	ticket := func() string {
		tk, err := s.Create(ctx, org, todo, "task", "work", "")
		if err != nil {
			t.Fatal(err)
		}
		return tk.ID
	}
	assignee := func(id string) string {
		var a *string
		_ = db.QueryRow(ctx, `SELECT assigned_to FROM tickets WHERE id = $1`, id).Scan(&a)
		if a == nil {
			return ""
		}
		return *a
	}

	standup := false
	h := NewSystem(db).AssigningOnMove(
		func(_ context.Context, _, user, _ string) (bool, error) { return user == "user_lead", nil },
		func(context.Context, string) bool { return standup },
	)
	person := func(user string) context.Context {
		c := &clerk.SessionClaims{RegisteredClaims: clerk.RegisteredClaims{Subject: user}}
		c.ActiveOrganizationID = org
		return clerk.ContextWithSessionClaims(ctx, c)
	}
	move := func(sys System, c context.Context, id, column string) {
		t.Helper()
		req := httptest.NewRequest("POST", "/", strings.NewReader(`{"column_id":"`+column+`","position":0}`)).WithContext(c)
		req.SetPathValue("id", id)
		rec := httptest.NewRecorder()
		sys.Move(rec, req)
		if rec.Code >= 300 {
			t.Fatalf("move: %d %s", rec.Code, rec.Body)
		}
	}

	// a member moving an unassigned ticket on takes it; into Done too
	a := ticket()
	move(h, person("user_sam"), a, doing)
	if got := assignee(a); got != "user_sam" {
		t.Fatalf("member's move: assigned to %q", got)
	}
	d := ticket()
	move(h, person("user_sam"), d, done)
	if got := assignee(d); got != "user_sam" {
		t.Fatalf("straight into Done: %q", got)
	}
	// someone else's ticket stays theirs
	move(h, person("user_bo"), a, done)
	if got := assignee(a); got != "user_sam" {
		t.Fatalf("taken over: %q", got)
	}
	// not: reordering in place, back into To do, by an API key, or with assign-on-move off
	r := ticket()
	move(h, person("user_sam"), r, todo)
	back := ticket()
	move(NewSystem(db), person("user_sam"), back, doing) // off: stays unassigned
	move(h, person("user_sam"), back, todo)
	k := ticket()
	move(h, auth.WithAPIKey(ctx, "user_sam", org, "agent", ""), k, doing)
	for name, id := range map[string]string{"reordered": r, "sent back / assign-on-move off": back, "moved by an API key": k} {
		if got := assignee(id); got != "" {
			t.Errorf("%s: assigned to %q", name, got)
		}
	}
	// a blocked ticket moves but isn't taken
	blocker, blocked := ticket(), ticket()
	if _, err := db.Exec(ctx, `INSERT INTO ticket_dependencies (ticket_id, blocked_by_id) VALUES ($1, $2)`, blocked, blocker); err != nil {
		t.Fatal(err)
	}
	move(h, person("user_sam"), blocked, doing)
	if got := assignee(blocked); got != "" {
		t.Fatalf("blocked ticket taken: %q", got)
	}

	// the board's manager: takes it outside a standup, not while one runs; members still do during it
	l1 := ticket()
	move(h, person("user_lead"), l1, doing)
	if got := assignee(l1); got != "user_lead" {
		t.Fatalf("manager outside standup: %q", got)
	}
	standup = true
	l2, m2 := ticket(), ticket()
	move(h, person("user_lead"), l2, doing)
	move(h, person("user_sam"), m2, doing)
	if got := assignee(l2); got != "" {
		t.Fatalf("manager during standup took it: %q", got)
	}
	if got := assignee(m2); got != "user_sam" {
		t.Fatalf("member during standup: %q", got)
	}
}
