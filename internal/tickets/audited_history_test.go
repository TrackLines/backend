package tickets

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tracklines/backend/internal/auth"
)

// TL-667: read from the auditor, the history is what the auditor holds plus what's still queued for it, in
// the order it happened, in the same shape as before; the auditor unreachable is a 503 the frontend names.
func TestAuditedHistory(t *testing.T) {
	ctx, db := historyDB(t)
	if _, err := db.Exec(ctx, `DELETE FROM projects WHERE owner_clerk_id = 'ah1'`); err != nil {
		t.Fatal(err)
	}
	var pid, b, todo, doing, tid string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('ah1', 'p') RETURNING id`).Scan(&pid)
	_ = db.QueryRow(ctx, `INSERT INTO boards (project_id, owner_clerk_id, name) VALUES ($1, 'ah1', 'Web') RETURNING id`, pid).Scan(&b)
	_ = db.QueryRow(ctx, `INSERT INTO columns (board_id, name, position) VALUES ($1, 'To do', 0) RETURNING id`, b).Scan(&todo)
	_ = db.QueryRow(ctx, `INSERT INTO columns (board_id, name, position) VALUES ($1, 'Doing', 1) RETURNING id`, b).Scan(&doing)
	_ = db.QueryRow(ctx, `INSERT INTO tickets (project_id, board_id, column_id, title, position, created_by) VALUES ($1, $2, $3, 't', 0, 'user_1') RETURNING id`,
		pid, b, todo).Scan(&tid)
	if _, err := db.Exec(ctx, `UPDATE tickets SET column_id = $2 WHERE id = $1`, tid, doing); err != nil {
		t.Fatal(err)
	}
	s := Store{DB: db}
	fromTable, err := s.History(ctx, "ah1", tid)
	if err != nil || len(fromTable) != 3 {
		t.Fatalf("table: %d %v", len(fromTable), err)
	}

	// everything still queued: the auditor has nothing yet, and the history is complete from the outbox
	empty := func(context.Context, string, string) ([]HistoryEntry, error) { return nil, nil }
	got, err := s.AuditedHistory(ctx, "ah1", tid, empty)
	if err != nil || fmt.Sprintf("%+v", entries(got)) != fmt.Sprintf("%+v", entries(fromTable)) {
		t.Fatalf("queued only:\n got %+v\nwant %+v (%v)", entries(got), entries(fromTable), err)
	}
	// the first two delivered (gone from the outbox), the last still queued: merged in order
	if _, err := db.Exec(ctx, `DELETE FROM audit_outbox WHERE target_id = $1 AND (data->>'stage_order')::int < 2`, tid); err != nil {
		t.Fatal(err)
	}
	delivered := func(_ context.Context, org, ticket string) ([]HistoryEntry, error) {
		if org != "ah1" || ticket != tid {
			t.Errorf("asked for %s %s", org, ticket)
		}
		return fromTable[:2], nil
	}
	got, err = s.AuditedHistory(ctx, "ah1", tid, delivered)
	if err != nil || fmt.Sprintf("%+v", entries(got)) != fmt.Sprintf("%+v", entries(fromTable)) {
		t.Fatalf("merged:\n got %+v\nwant %+v (%v)", entries(got), entries(fromTable), err)
	}
	// not this org's ticket: not found, and the auditor isn't asked
	asked := false
	if _, err := s.AuditedHistory(ctx, "someone_else", tid, func(context.Context, string, string) ([]HistoryEntry, error) { asked = true; return nil, nil }); !errors.Is(err, ErrNotFound) || asked {
		t.Fatalf("other org: %v, asked %v", err, asked)
	}
	down := func(context.Context, string, string) ([]HistoryEntry, error) {
		return nil, errors.New("connection refused")
	}
	if _, err := s.AuditedHistory(ctx, "ah1", tid, down); !errors.Is(err, ErrHistoryUnavailable) {
		t.Fatalf("down: %v", err)
	}

	// the route: the table while the flag is off, the auditor while it's on, a coded 503 when it's down
	call := func(h System) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(auth.WithAPIKey(ctx, "user_1", "ah1", "", "personal"))
		req.Header.Set("Accept", "application/json")
		req.SetPathValue("id", tid)
		rec := httptest.NewRecorder()
		h.History(rec, req)
		return rec
	}
	off := NewSystem(db).HistoryFromAuditor(down, func() bool { return false })
	if rec := call(off); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"column_name":"Doing"`) {
		t.Fatalf("flag off: %d %s", rec.Code, rec.Body)
	}
	on := NewSystem(db).HistoryFromAuditor(down, func() bool { return true })
	if rec := call(on); rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), `"code":"history_unavailable"`) {
		t.Fatalf("down: %d %s", rec.Code, rec.Body)
	}
	if rec := call(NewSystem(db).HistoryFromAuditor(delivered, func() bool { return true })); rec.Code != http.StatusOK || strings.Count(rec.Body.String(), `"stage"`) != 3 {
		t.Fatalf("on: %d %s", rec.Code, rec.Body)
	}
}

func entries(h []HistoryEntry) []string {
	var out []string
	for _, e := range h {
		s := func(p *string) string {
			if p == nil {
				return "-"
			}
			return *p
		}
		out = append(out, fmt.Sprintf("%s %s/%s #%d %s %s %s %s %v %s %s", e.At, e.Stage, e.Outcome, e.StageOrder, s(e.BoardID), s(e.BoardName), s(e.ColumnID), s(e.ColumnName), e.Done, s(e.Resolution), s(e.Actor)))
	}
	return out
}
