package tickets

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/clerk/clerk-sdk-go/v2"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/boards"
)

// TL-76: GET /api/tickets/{id}/history gives a ticket's lifecycle, oldest first, with skipped stages,
// to anyone who can read the ticket (resolved ones included), and to nobody else.
func TestHistoryHTTP(t *testing.T) {
	ctx, db := historyDB(t)
	if _, err := db.Exec(ctx, `DELETE FROM boards WHERE owner_clerk_id = 'hh1'; DELETE FROM projects WHERE owner_clerk_id = 'hh1'`); err != nil {
		t.Fatal(err)
	}
	var pid string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('hh1', 'p') RETURNING id`).Scan(&pid)
	b, _ := boards.Store{DB: db}.Create(ctx, "hh1", pid, "Backend", "")
	s := Store{DB: db}
	tk, _ := s.CreateAs(ctx, "hh1", "user_ava", b.Columns[0].ID, "task", "t", "")
	inOrg := func(org string) *http.Request {
		c := &clerk.SessionClaims{RegisteredClaims: clerk.RegisteredClaims{Subject: "user_ava"}}
		c.ActiveOrganizationID = org
		r := httptest.NewRequest(http.MethodGet, "/api/tickets/"+tk.ID+"/history", nil).WithContext(clerk.ContextWithSessionClaims(ctx, c))
		r.SetPathValue("id", tk.ID)
		return r
	}
	_ = s.Move(auth.WithAPIKeyUser(ctx, "user_ava", "claude"), "hh1", tk.ID, b.Columns[len(b.Columns)-1].ID, 0)
	_ = s.ToBacklog(ctx, "hh1", tk.ID)
	_ = s.ResolveAs(ctx, "hh1", "user_ava", tk.ID, ResolutionDone, "")

	h := NewSystem(db)
	rec := httptest.NewRecorder()
	h.History(rec, inOrg("hh1"))
	var got []HistoryEntry
	if err := json.Unmarshal(rec.Body.Bytes(), &got); rec.Code != 200 || err != nil {
		t.Fatalf("history: %d %s", rec.Code, rec.Body)
	}
	var stages []string
	for _, e := range got {
		stages = append(stages, e.Stage+":"+e.Outcome)
	}
	want := "backlog:skipped column:entered column:skipped column:entered backlog:entered resolved:entered"
	if strings.Join(stages, " ") != want {
		t.Fatalf("order: %v", stages)
	}
	if got[3].Actor == nil || *got[3].Actor != "claude" || !got[3].Done || got[0].At == "" {
		t.Fatalf("entry: %+v", got[3])
	}
	// another organization, or a ticket that isn't there: not found
	for _, r := range []*http.Request{inOrg("someone-else"), func() *http.Request {
		r := inOrg("hh1")
		r.SetPathValue("id", "00000000-0000-0000-0000-000000000000")
		return r
	}()} {
		rec := httptest.NewRecorder()
		h.History(rec, r)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("not ours: %d", rec.Code)
		}
	}
}
