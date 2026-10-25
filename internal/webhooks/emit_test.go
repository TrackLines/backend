package webhooks_test

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/tracklines/backend/internal/boards"
	"github.com/tracklines/backend/internal/comments"
	"github.com/tracklines/backend/internal/roadmaps"
	"github.com/tracklines/backend/internal/sprints"
	"github.com/tracklines/backend/internal/tickets"
	"github.com/tracklines/backend/internal/webhooks"
)

// Changes queue their events for the org's subscribed, enabled endpoints, and only those.
func TestEmit(t *testing.T) {
	ctx := context.Background()
	db, box := webhooks.OpenDB(t)
	if _, err := db.Exec(ctx, `DELETE FROM webhook_endpoints WHERE org_id IN ('wh4', 'wh5'); DELETE FROM projects WHERE owner_clerk_id IN ('wh4', 'wh5')`); err != nil {
		t.Fatal(err)
	}
	subscribed := slices.DeleteFunc(slices.Clone(webhooks.Events), func(e string) bool { return e == "ticket.assigned" })
	store := webhooks.Store{DB: db, Box: box}
	ep, _, err := store.Create(ctx, "wh4", "user_a", "https://93.184.215.14/hook", subscribed)
	if err != nil {
		t.Fatal(err)
	}
	// a disabled endpoint gets nothing queued
	off, _, err := store.Create(ctx, "wh4", "user_a", "https://93.184.215.14/off", subscribed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `UPDATE webhook_endpoints SET enabled = false WHERE id = $1`, off.ID); err != nil {
		t.Fatal(err)
	}
	board := func(org string) (*boards.Board, string) {
		var pid string
		if err := db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ($1, 'p') RETURNING id`, org).Scan(&pid); err != nil {
			t.Fatal(err)
		}
		b, err := boards.Store{DB: db}.Create(ctx, org, pid, "b", "")
		if err != nil {
			t.Fatal(err)
		}
		return b, pid
	}
	b, pid := board("wh4")
	todo, doing, done := b.Columns[0].ID, b.Columns[1].ID, b.Columns[len(b.Columns)-1].ID

	// queued: what's been queued since the last look, event → data
	seen := 0
	type queued struct {
		event string
		data  map[string]any
	}
	next := func() []queued {
		t.Helper()
		rows, err := db.Query(ctx, `SELECT event, data FROM webhook_outbox WHERE endpoint_id = $1 ORDER BY created_at, id OFFSET $2`, ep.ID, seen)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []queued
		for rows.Next() {
			var q queued
			var raw []byte
			if err := rows.Scan(&q.event, &raw); err != nil || json.Unmarshal(raw, &q.data) != nil {
				t.Fatal(err)
			}
			out = append(out, q)
		}
		seen += len(out)
		return out
	}
	events := func(qs []queued) (out []string) {
		for _, q := range qs {
			out = append(out, q.event)
		}
		return out
	}

	ts := tickets.Store{DB: db}
	tk, err := ts.CreateAs(ctx, "wh4", "user_a", todo, "task", "Hook me", "")
	if err != nil {
		t.Fatal(err)
	}
	if q := next(); len(q) != 1 || q[0].event != "ticket.created" || q[0].data["id"] != tk.ID || q[0].data["project_id"] != pid || q[0].data["title"] != "Hook me" {
		t.Fatalf("created: %+v", q)
	}
	if err := ts.Move(ctx, "wh4", tk.ID, doing, 0); err != nil {
		t.Fatal(err)
	}
	if q := next(); len(q) != 1 || q[0].event != "ticket.moved" || q[0].data["from_column_id"] != todo || q[0].data["to_column_id"] != doing {
		t.Fatalf("moved: %+v", q)
	}
	if err := ts.Move(ctx, "wh4", tk.ID, doing, 0); err != nil { // a reorder in the same column isn't a move
		t.Fatal(err)
	}
	if err := ts.Move(ctx, "wh4", tk.ID, done, 0); err != nil {
		t.Fatal(err)
	}
	if q := next(); !slices.Equal(events(q), []string{"ticket.moved", "ticket.done"}) || q[1].data["done_at"] == nil {
		t.Fatalf("done: %+v", q)
	}
	if _, err := ts.Claim(ctx, "wh4", "user_a", tk.ID); err != nil { // not subscribed to ticket.assigned
		t.Fatal(err)
	}
	if q := next(); len(q) != 0 {
		t.Fatalf("unsubscribed event queued: %+v", q)
	}
	c, err := comments.Store{DB: db}.Create(ctx, "wh4", "user_a", tk.ID, "looks good", nil)
	if err != nil {
		t.Fatal(err)
	}
	q := next()
	if comment, _ := q[0].data["comment"].(map[string]any); len(q) != 1 || q[0].event != "comment.created" || q[0].data["ticket_key"] != tk.Key ||
		comment["id"] != c.ID || comment["body"] != "looks good" || comment["author"] != "user_a" {
		t.Fatalf("comment: %+v", q)
	}

	// sprints: started; closing starts the next
	sp, err := sprints.Store{DB: db}.Start(ctx, "wh4", b.ID, 14)
	if err != nil {
		t.Fatal(err)
	}
	q = next()
	if s, _ := q[0].data["sprint"].(map[string]any); len(q) != 1 || q[0].event != "sprint.started" || q[0].data["board_id"] != b.ID || s["id"] != sp.ID || s["number"] != 1.0 {
		t.Fatalf("sprint started: %+v", q)
	}
	if _, err := (sprints.Store{DB: db}).Close(ctx, "wh4", sp.ID); err != nil {
		t.Fatal(err)
	}
	if q := next(); !slices.Equal(events(q), []string{"sprint.closed", "sprint.started"}) {
		t.Fatalf("sprint closed: %+v", q)
	}

	// to the backlog (to_column_id null), resolved there, and to the bin
	other, err := ts.CreateAs(ctx, "wh4", "user_a", todo, "bug", "Other", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := ts.ToBacklog(ctx, "wh4", other.ID); err != nil {
		t.Fatal(err)
	}
	if err := ts.Resolve(ctx, "wh4", other.ID); err != nil {
		t.Fatal(err)
	}
	if err := ts.Delete(ctx, "wh4", other.ID); err != nil {
		t.Fatal(err)
	}
	q = next()
	if !slices.Equal(events(q), []string{"ticket.created", "ticket.moved", "ticket.done", "ticket.deleted"}) ||
		q[1].data["to_column_id"] != nil || q[1].data["from_column_id"] != todo || q[3].data["id"] != other.ID {
		t.Fatalf("backlog, resolve, delete: %+v", q)
	}

	// roadmaps: an update published
	r, err := roadmaps.Store{DB: db}.Create(ctx, "wh4", pid, "Plan", "", "team")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (roadmaps.Store{DB: db}).CreateUpdate(ctx, r.ID, "wh4", "at_risk", "slipping", "user_a", "Ann"); err != nil {
		t.Fatal(err)
	}
	if q := next(); len(q) != 1 || q[0].event != "roadmap.update_published" || q[0].data["roadmap_id"] != r.ID {
		t.Fatalf("roadmap update: %+v", q)
	}

	// a rolled-back change sends nothing
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := webhooks.Emit(ctx, tx, "wh4", "ticket.created", func() (any, error) { return map[string]string{"id": "x"}, nil }); err != nil {
		t.Fatal(err)
	}
	_ = tx.Rollback(ctx)
	if q := next(); len(q) != 0 {
		t.Fatalf("rolled back, still queued: %+v", q)
	}

	// an org without endpoints: nothing queued, and the data isn't even built
	b5, _ := board("wh5")
	if _, err := ts.CreateAs(ctx, "wh5", "user_b", b5.Columns[0].ID, "task", "Quiet", ""); err != nil {
		t.Fatal(err)
	}
	if err := webhooks.Emit(ctx, db, "wh5", "ticket.created", func() (any, error) { t.Fatal("built data for nobody"); return nil, nil }); err != nil {
		t.Fatal(err)
	}
	var total, disabled int
	_ = db.QueryRow(ctx, `SELECT count(*) FILTER (WHERE o.endpoint_id = $1), count(*) FROM webhook_outbox o
		JOIN webhook_endpoints e ON e.id = o.endpoint_id WHERE e.org_id IN ('wh4', 'wh5')`, off.ID).Scan(&disabled, &total)
	if disabled != 0 || total != seen {
		t.Fatalf("queued elsewhere: %d for the disabled endpoint, %d in all (want %d)", disabled, total, seen)
	}
}
