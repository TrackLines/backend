package tickets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/boards"
)

// Bulk edits (TL-633): each action through the single-change handler, per-ticket results, a failure
// not stopping the rest, the 200 cap, another org's tickets refused, keys as well as ids.
func TestBulk(t *testing.T) {
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
	const org = "org_bulk"
	var pid string
	if err := db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name, key_prefix) VALUES ($1, 'Bulk', 'BLK') RETURNING id`, org).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	b, err := boards.Store{DB: db}.Create(ctx, org, pid, "Board", "")
	if err != nil {
		t.Fatal(err)
	}
	s := Store{DB: db}
	var ids []string
	for _, title := range []string{"one", "two", "three"} {
		tk, err := s.CreateBacklog(ctx, org, pid, "task", title, "keep me")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, tk.ID)
	}
	var other string
	if err := db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('org_bulk_other', 'X') RETURNING id`).Scan(&other); err != nil {
		t.Fatal(err)
	}
	foreign, _ := s.CreateBacklog(ctx, "org_bulk_other", other, "task", "not yours", "")

	h := NewSystem(db)
	c := auth.WithAPIKey(ctx, "user_bulk", org, "", "personal")
	bulk := func(body string) (int, []BulkResult) {
		t.Helper()
		rec := httptest.NewRecorder()
		h.Bulk(rec, httptest.NewRequest("POST", "/api/tickets/bulk", strings.NewReader(body)).WithContext(c))
		var out struct{ Results []BulkResult }
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out.Results
	}
	idsJSON := func(v ...string) string { b, _ := json.Marshal(v); return string(b) }

	// priority keeps title and description; labels add/remove; another org's ticket fails alone
	code, res := bulk(`{"ticket_ids":` + idsJSON(ids[0], ids[1], foreign.ID) + `,"action":"priority","priority":"urgent"}`)
	if code != 200 || len(res) != 3 || !res[0].OK || !res[1].OK || res[2].OK || res[2].Status != 404 {
		t.Fatalf("priority: %d %+v", code, res)
	}
	if d, _ := s.Get(ctx, org, ids[0]); d.Priority != "urgent" || d.Title != "one" || d.Description != "keep me" {
		t.Fatalf("priority changed more: %+v", d.Ticket)
	}
	if f, _ := s.Get(ctx, "org_bulk_other", foreign.ID); f.Priority == "urgent" {
		t.Fatal("changed another org's ticket")
	}
	// TL-669: on a paid plan the org log has the bulk edit (each ticket's change is in its own activity);
	// on Free, nothing
	bulkEvents := func() []string {
		rows, _ := db.Query(ctx, `SELECT data::text FROM audit_outbox WHERE org_id = $1 AND action = 'tickets.bulk_edited' ORDER BY queued`, org)
		defer rows.Close()
		var out []string
		for rows.Next() {
			var d string
			_ = rows.Scan(&d)
			out = append(out, d)
		}
		return out
	}
	if got := bulkEvents(); len(got) != 0 {
		t.Fatalf("a Free org's bulk edit was logged: %v", got)
	}
	if _, err := db.Exec(ctx, `INSERT INTO org_billing (org_id, stripe_status, plan) VALUES ($1, 'active', 'pro')`, org); err != nil {
		t.Fatal(err)
	}
	bulk(`{"ticket_ids":` + idsJSON(ids[0], foreign.ID) + `,"action":"priority","priority":"high"}`)
	if got := bulkEvents(); len(got) != 1 || got[0] != `{"count": 2, "action": "priority", "changed": 1}` {
		t.Fatalf("bulk edit in the org log: %v", got)
	}
	if _, err := db.Exec(ctx, `DELETE FROM org_billing WHERE org_id = $1`, org); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO ticket_labels (ticket_id, label) VALUES ($1, 'Old')`, ids[0]); err != nil {
		t.Fatal(err)
	}
	if _, res = bulk(`{"ticket_ids":` + idsJSON(ids[0], ids[1]) + `,"action":"labels","add":["ux"],"remove":["old"]}`); !res[0].OK {
		t.Fatalf("labels: %+v", res)
	}
	if d, _ := s.Get(ctx, org, ids[0]); !slices.Equal(d.Labels, []string{"ux"}) {
		t.Fatalf("labels = %v", d.Labels)
	}

	// a bad value fails each ticket with the single change's own code; nothing else breaks
	if _, res = bulk(`{"ticket_ids":` + idsJSON(ids[2]) + `,"action":"priority","priority":"panic"}`); res[0].OK || res[0].Code != "bad_priority" {
		t.Fatalf("bad priority: %+v", res)
	}

	// keys work; move onto the board lands them in the order sent
	key := func(id string) string { d, _ := s.Get(ctx, org, id); return d.Key }
	if _, res = bulk(`{"ticket_ids":` + idsJSON(key(ids[1]), key(ids[0])) + `,"action":"move","column_id":"` + b.Columns[0].ID + `"}`); !res[0].OK || !res[1].OK {
		t.Fatalf("move: %+v", res)
	}
	var order []string
	rows, _ := db.Query(ctx, `SELECT title FROM tickets WHERE column_id = $1 ORDER BY position`, b.Columns[0].ID)
	for rows.Next() {
		var title string
		_ = rows.Scan(&title)
		order = append(order, title)
	}
	if !slices.Equal(order, []string{"two", "one"}) {
		t.Fatalf("moved order %v, want two, one", order)
	}

	// resolve (backlog), assign/unassign, delete
	if _, res = bulk(`{"ticket_ids":` + idsJSON(ids[2]) + `,"action":"resolve","resolution":"wont_do","reason":"dup of idea"}`); !res[0].OK {
		t.Fatalf("resolve: %+v", res)
	}
	if d, _ := s.Get(ctx, org, ids[2]); d.Resolution == nil || *d.Resolution != "wont_do" {
		t.Fatalf("resolution = %v", d.Resolution)
	}
	if _, res = bulk(`{"ticket_ids":` + idsJSON(ids[0]) + `,"action":"assign","assignee":"user_bulk"}`); !res[0].OK {
		t.Fatalf("assign: %+v", res)
	}
	if _, res = bulk(`{"ticket_ids":` + idsJSON(ids[0]) + `,"action":"assign","assignee":null}`); !res[0].OK {
		t.Fatalf("unassign: %+v", res)
	}
	if d, _ := s.Get(ctx, org, ids[0]); d.AssignedTo != nil {
		t.Fatal("null should unassign")
	}
	if _, res = bulk(`{"ticket_ids":` + idsJSON(ids[1]) + `,"action":"delete"}`); !res[0].OK {
		t.Fatalf("delete: %+v", res)
	}
	if _, err := s.Get(ctx, org, ids[1]); !errors.Is(err, ErrNotFound) {
		t.Fatal("deleted ticket still readable")
	}

	// the cap and the action
	many := make([]string, MaxBulk+1)
	for i := range many {
		many[i] = fmt.Sprint("no-such-ticket-", i)
	}
	if code, _ = bulk(`{"ticket_ids":` + idsJSON(many...) + `,"action":"delete"}`); code != 400 {
		t.Fatalf("201 ids → %d", code)
	}
	if _, res = bulk(`{"ticket_ids":` + idsJSON(ids[0], ids[2], ids[0]) + `,"action":"priority","priority":"low"}`); len(res) != 2 {
		t.Fatalf("a repeated id should be changed once: %+v", res)
	}
	if code, _ = bulk(`{"ticket_ids":[],"action":"delete"}`); code != 400 {
		t.Fatalf("no ids → %d", code)
	}
	if code, _ = bulk(`{"ticket_ids":` + idsJSON(ids[0]) + `,"action":"explode"}`); code != 400 {
		t.Fatalf("bad action → %d", code)
	}
}
