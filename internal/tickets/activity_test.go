package tickets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/tracklines/backend/internal/auth"
)

// TL-679: a ticket's activity: the stages from the table while the auditor isn't in use; with it, the
// auditor's events plus what's still queued for it, each once, in order; the auditor down is a coded 503
func TestActivity(t *testing.T) {
	ctx, db := historyDB(t)
	if _, err := db.Exec(ctx, `DELETE FROM projects WHERE owner_clerk_id = 'act1'`); err != nil {
		t.Fatal(err)
	}
	var pid, b, todo, tid string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('act1', 'p') RETURNING id`).Scan(&pid)
	_ = db.QueryRow(ctx, `INSERT INTO boards (project_id, owner_clerk_id, name) VALUES ($1, 'act1', 'Web') RETURNING id`, pid).Scan(&b)
	_ = db.QueryRow(ctx, `INSERT INTO columns (board_id, name, position) VALUES ($1, 'To do', 0) RETURNING id`, b).Scan(&todo)
	_ = db.QueryRow(ctx, `INSERT INTO tickets (project_id, board_id, column_id, title, position, created_by) VALUES ($1, $2, $3, 't', 0, 'user_1') RETURNING id`,
		pid, b, todo).Scan(&tid)
	// assigned by user_1, named as the app names them (boards.ActorHook / SetActor)
	if err := pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('tracklines.actor', 'user_1', true), set_config('tracklines.actor_kind', 'person', true)`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE tickets SET assigned_to = 'user_1' WHERE id = $1`, tid)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	s := Store{DB: db}
	actions := func(es []ActivityEntry) string {
		var out []string
		for _, e := range es {
			out = append(out, e.Action+"/"+e.ActorKind)
		}
		return fmt.Sprint(out)
	}

	// the flag off: the stages, as events, under the ids the queue gives them
	local, err := s.Activity(ctx, "act1", tid, nil)
	if err != nil || actions(local) != "[ticket.stage/person ticket.stage/person]" {
		t.Fatalf("table: %s %v", actions(local), err)
	}
	var queuedID string
	_ = db.QueryRow(ctx, `SELECT id::text FROM audit_outbox WHERE target_id = $1 AND action = 'ticket.stage' ORDER BY queued LIMIT 1`, tid).Scan(&queuedID)
	if local[0].ID != queuedID || !strings.Contains(string(local[0].Data), `"stage": "backlog"`) {
		t.Fatalf("stage event: %+v (queued as %s)", local[0], queuedID)
	}

	// the flag on, nothing delivered yet: everything from the queue, the assignment included
	none := func(context.Context, string, string) ([]ActivityEntry, error) { return nil, nil }
	all, err := s.Activity(ctx, "act1", tid, none)
	if err != nil || actions(all) != "[ticket.stage/person ticket.stage/person ticket.assigned/person]" {
		t.Fatalf("queued only: %s %v", actions(all), err)
	}
	// partly delivered, and one delivered but not yet cleared from the queue: each once, in order
	delivered := func(_ context.Context, org, ticket string) ([]ActivityEntry, error) {
		if org != "act1" || ticket != tid {
			t.Errorf("asked for %s %s", org, ticket)
		}
		return all[:2], nil
	}
	got, err := s.Activity(ctx, "act1", tid, delivered)
	if err != nil || actions(got) != actions(all) {
		t.Fatalf("merged: %s %v", actions(got), err)
	}
	// another org's ticket: not found, and the auditor isn't asked
	asked := false
	if _, err := s.Activity(ctx, "someone_else", tid, func(context.Context, string, string) ([]ActivityEntry, error) { asked = true; return nil, nil }); !errors.Is(err, ErrNotFound) || asked {
		t.Fatalf("other org: %v, asked %v", err, asked)
	}
	down := func(context.Context, string, string) ([]ActivityEntry, error) {
		return nil, errors.New("connection refused")
	}
	if _, err := s.Activity(ctx, "act1", tid, down); !errors.Is(err, ErrHistoryUnavailable) {
		t.Fatalf("down: %v", err)
	}

	// the route
	call := func(h System) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(auth.WithAPIKey(ctx, "user_1", "act1", "", "personal"))
		req.Header.Set("Accept", "application/json")
		req.SetPathValue("id", tid)
		rec := httptest.NewRecorder()
		h.Activity(rec, req)
		return rec
	}
	off := NewSystem(db).ActivityFromAuditor(down, func() bool { return false })
	if rec := call(off); rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "ticket.assigned") {
		t.Fatalf("flag off: %d %s", rec.Code, rec.Body)
	}
	if rec := call(NewSystem(db).ActivityFromAuditor(down, func() bool { return true })); rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), `"code":"history_unavailable"`) {
		t.Fatalf("down: %d %s", rec.Code, rec.Body)
	}
	rec := call(NewSystem(db).ActivityFromAuditor(delivered, func() bool { return true }))
	var body []ActivityEntry
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != http.StatusOK || actions(body) != actions(all) || body[2].Actor == nil || *body[2].Actor != "user_1" {
		t.Fatalf("on: %d %s", rec.Code, rec.Body)
	}
}
