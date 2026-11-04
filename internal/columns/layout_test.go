package columns

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

// The edit page saves a board's columns in one go (TL-470): order, names, WIP limits, new and
// removed columns together, To do first and Done last, or nothing at all.
func TestLayout(t *testing.T) {
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
	const org, other = "org_lay1", "org_lay2"
	for _, q := range []string{
		`DELETE FROM boards WHERE owner_clerk_id IN ($1, $2)`, `DELETE FROM projects WHERE owner_clerk_id IN ($1, $2)`,
		`DELETE FROM org_admins WHERE org_id IN ($1, $2)`,
		`INSERT INTO org_admins (org_id, user_clerk_id, granted_by) VALUES ($1, 'admin-l', 'test'), ($2, 'admin-o', 'test')`,
	} {
		if _, err := db.Exec(ctx, q, org, other); err != nil {
			t.Fatal(err)
		}
	}
	var pid string
	if err := db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ($1, 'Layout') RETURNING id`, org).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	b, err := boards.Store{DB: db}.Create(ctx, org, pid, "Board", "")
	if err != nil {
		t.Fatal(err)
	}
	h := NewSystem(db, organizations.Admins{DB: db})
	put := func(o, user string, cols []LayoutColumn) (int, string) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"columns": cols})
		c := &clerk.SessionClaims{RegisteredClaims: clerk.RegisteredClaims{Subject: user}}
		c.ActiveOrganizationID = o
		req := httptest.NewRequest("PUT", "/", strings.NewReader(string(body))).WithContext(clerk.ContextWithSessionClaims(ctx, c))
		req.SetPathValue("boardID", b.ID)
		rec := httptest.NewRecorder()
		h.Layout(rec, req)
		return rec.Code, strings.TrimSpace(rec.Body.String())
	}
	type col struct {
		id, name string
		wip      *int
	}
	now := func() (out []col) {
		rows, _ := db.Query(ctx, `SELECT id::text, name, wip_limit FROM columns WHERE board_id = $1 ORDER BY position`, b.ID)
		defer rows.Close()
		for rows.Next() {
			var c col
			rows.Scan(&c.id, &c.name, &c.wip)
			out = append(out, c)
		}
		return
	}
	n := func(i int) *int { return &i }
	start := now()
	if len(start) < 2 {
		t.Fatalf("a new board's columns: %+v", start)
	}
	first, last := start[0], start[len(start)-1]
	var done string
	db.QueryRow(ctx, `INSERT INTO tickets (project_id, board_id, column_id, title, position) VALUES ($1, $2, $3, 'shipped', 0) RETURNING id`, pid, b.ID, last.id).Scan(&done)

	// everything at once: a new column, a renamed one with a limit, limits on the ends ignored
	want := []LayoutColumn{{ID: first.id, Name: first.name, WIPLimit: n(5)}, {Name: "QA", WIPLimit: n(2)}}
	for _, c := range start[1 : len(start)-1] {
		want = append(want, LayoutColumn{ID: c.id, Name: "Doing", WIPLimit: n(3)})
	}
	want = append(want, LayoutColumn{ID: last.id, Name: "Shipped", WIPLimit: n(9)})
	if code, body := put(org, "admin-l", want); code != 204 {
		t.Fatalf("layout: %d %s", code, body)
	}
	got := now()
	if len(got) != len(want) || got[0].id != first.id || got[1].name != "QA" || got[len(got)-1].id != last.id || got[len(got)-1].name != "Shipped" {
		t.Fatalf("after layout: %+v", got)
	}
	if got[0].wip != nil || got[len(got)-1].wip != nil {
		t.Fatalf("limits on To do or Done: %+v", got)
	}
	if got[1].wip == nil || *got[1].wip != 2 || got[2].wip == nil || *got[2].wip != 3 {
		t.Fatalf("limits: %+v", got)
	}
	var stillDone bool
	db.QueryRow(ctx, `SELECT `+boards.TicketDoneSQL+` FROM tickets t WHERE t.id = $1`, done).Scan(&stillDone)
	if !stillDone {
		t.Fatal("the ticket in Done isn't done after the layout")
	}

	// reorder the middle and drop QA (empty): fine
	middle := got[2 : len(got)-1]
	reordered := []LayoutColumn{{ID: first.id, Name: first.name}}
	for i := len(middle) - 1; i >= 0; i-- {
		reordered = append(reordered, LayoutColumn{ID: middle[i].id, Name: middle[i].name, WIPLimit: middle[i].wip})
	}
	reordered = append(reordered, LayoutColumn{ID: last.id, Name: "Shipped"})
	if code, body := put(org, "admin-l", reordered); code != 204 {
		t.Fatalf("reorder and drop QA: %d %s", code, body)
	}
	if got := now(); len(got) != len(reordered) {
		t.Fatalf("QA not removed: %+v", got)
	}

	// a column still holding this sprint's tickets can't go, and nothing else changes
	keep := now()
	if len(keep) > 2 {
		db.Exec(ctx, `INSERT INTO tickets (project_id, board_id, column_id, title, position) VALUES ($1, $2, $3, 'in flight', 0)`, pid, b.ID, keep[1].id)
		code, body := put(org, "admin-l", []LayoutColumn{{ID: first.id, Name: "Renamed"}, {ID: last.id, Name: "Shipped"}})
		if code != 409 || !strings.Contains(body, keep[1].name) {
			t.Fatalf("removing a busy column: %d %s", code, body)
		}
		if got := now(); len(got) != len(keep) || got[0].name != first.name {
			t.Fatalf("a refused layout changed something: %+v", got)
		}
	}

	// the ends are pinned; bad input is refused with a reason
	mid := LayoutColumn{Name: "Mid"}
	for _, c := range []struct {
		cols []LayoutColumn
		why  string
	}{
		{[]LayoutColumn{mid, {ID: first.id, Name: "x"}, {ID: last.id, Name: "y"}}, "first column (To do) stays first"},
		{[]LayoutColumn{{ID: first.id, Name: "x"}, {ID: last.id, Name: "y"}, mid}, "last column (Done) stays last"},
		{[]LayoutColumn{{ID: first.id, Name: "x"}}, "at least two columns"},
		{[]LayoutColumn{{ID: first.id, Name: " "}, {ID: last.id, Name: "y"}}, "needs a name"},
		{[]LayoutColumn{{ID: first.id, Name: "x"}, {Name: strings.Repeat("n", 81)}, {ID: last.id, Name: "y"}}, "up to 80"},
		{[]LayoutColumn{{ID: first.id, Name: "x"}, {Name: "m", WIPLimit: n(1000)}, {ID: last.id, Name: "y"}}, "WIP limits"},
		{[]LayoutColumn{{ID: first.id, Name: "x"}, {ID: first.id, Name: "again"}, {ID: last.id, Name: "y"}}, "don't match this board"},
		{[]LayoutColumn{{ID: first.id, Name: "x"}, {ID: "00000000-0000-0000-0000-000000000000", Name: "z"}, {ID: last.id, Name: "y"}}, "don't match this board"},
	} {
		if code, body := put(org, "admin-l", c.cols); code != 400 || !strings.Contains(body, c.why) {
			t.Fatalf("%+v: %d %q, want 400 with %q", c.cols, code, body, c.why)
		}
	}

	// board managers only; another organization doesn't see the board
	if code, _ := put(org, "member-l", reordered); code != 403 {
		t.Fatalf("a member: %d", code)
	}
	if code, _ := put(other, "admin-o", reordered); code != 404 {
		t.Fatalf("another org: %d", code)
	}
}

// A column says what arriving does to a ticket's flags.gg flag (TL-786): set on the edit page with the
// layout, kept when a save leaves it out, and automatic until set (Testing/QA promote, the board's dev
// complete column promotes to production).
func TestLayoutFlagActions(t *testing.T) {
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
	const org = "org_layf"
	for _, q := range []string{`DELETE FROM boards WHERE owner_clerk_id = $1`, `DELETE FROM projects WHERE owner_clerk_id = $1`, `DELETE FROM org_admins WHERE org_id = $1`,
		`INSERT INTO org_admins (org_id, user_clerk_id, granted_by) VALUES ($1, 'admin-f', 'test')`} {
		if _, err := db.Exec(ctx, q, org); err != nil {
			t.Fatal(err)
		}
	}
	var pid string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ($1, 'Flags') RETURNING id`, org).Scan(&pid)
	bs := boards.Store{DB: db}
	b, err := bs.Create(ctx, org, pid, "Board", "")
	if err != nil {
		t.Fatal(err)
	}
	h := NewSystem(db, organizations.Admins{DB: db})
	put := func(cols []LayoutColumn) (int, string) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"columns": cols})
		c := &clerk.SessionClaims{RegisteredClaims: clerk.RegisteredClaims{Subject: "admin-f"}}
		c.ActiveOrganizationID = org
		req := httptest.NewRequest("PUT", "/", strings.NewReader(string(body))).WithContext(clerk.ContextWithSessionClaims(ctx, c))
		req.SetPathValue("boardID", b.ID)
		rec := httptest.NewRecorder()
		h.Layout(rec, req)
		return rec.Code, rec.Body.String()
	}
	got := func() map[string][2]string { // name → set, effective
		t.Helper()
		full, err := bs.Get(ctx, b.ID, org)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string][2]string{}
		for _, c := range full.Columns {
			set := ""
			if c.FlagAction != nil {
				set = *c.FlagAction
			}
			out[c.Name] = [2]string{set, c.FlagEffective}
		}
		return out
	}
	s := func(v string) *string { return &v }
	full, _ := bs.Get(ctx, b.ID, org)
	first, last := full.Columns[0], full.Columns[len(full.Columns)-1]
	if code, body := put([]LayoutColumn{{ID: first.ID, Name: first.Name}, {Name: "Testing"}, {Name: "Review", FlagAction: s("archive")},
		{Name: "Ship it", FlagAction: s("bogus")}, {ID: last.ID, Name: last.Name}}); code != 400 || !strings.Contains(body, "flag action") {
		t.Fatalf("bad action: %d %s", code, body)
	}
	if code, body := put([]LayoutColumn{{ID: first.ID, Name: first.Name}, {Name: "Testing"}, {Name: "Review", FlagAction: s("archive")},
		{Name: "Ready", FlagAction: s("promote_to_production")}, {ID: last.ID, Name: last.Name}}); code != 204 {
		t.Fatalf("save: %d %s", code, body)
	}
	c := got()
	if c["Testing"] != [2]string{"", "promote"} || c["Review"] != [2]string{"archive", "archive"} || c["Ready"] != [2]string{"promote_to_production", "promote_to_production"} || c[first.Name][1] != "none" {
		t.Fatalf("after save: %v", c)
	}
	// left out keeps it; "" makes it automatic again; the board's dev complete column goes to production
	full, _ = bs.Get(ctx, b.ID, org)
	var keep []LayoutColumn
	for _, col := range full.Columns {
		lc := LayoutColumn{ID: col.ID, Name: col.Name}
		if col.Name == "Ready" {
			lc.FlagAction = s("")
		}
		keep = append(keep, lc)
	}
	if code, body := put(keep); code != 204 {
		t.Fatalf("resave: %d %s", code, body)
	}
	if c := got(); c["Review"][0] != "archive" || c["Ready"] != [2]string{"", "none"} {
		t.Fatalf("kept and cleared: %v", c)
	}
	_, _ = db.Exec(ctx, `UPDATE boards SET dev_complete_column_id = (SELECT id FROM columns WHERE board_id = $1 AND name = 'Ready') WHERE id = $1`, b.ID)
	if c := got(); c["Ready"][1] != "promote_to_production" {
		t.Fatalf("dev complete: %v", c)
	}
}
