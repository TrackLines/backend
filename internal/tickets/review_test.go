package tickets

import (
	"context"
	"errors"
	"net/http"
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

// TL-117: on a board with agent review, an agent's move into Done waits for a person.
func TestAgentReview(t *testing.T) {
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
	if _, err := db.Exec(ctx, `DELETE FROM boards WHERE owner_clerk_id = 'rv1'; DELETE FROM projects WHERE owner_clerk_id = 'rv1'`); err != nil {
		t.Fatal(err)
	}
	var pid string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('rv1', 'Review') RETURNING id`).Scan(&pid)
	bs := boards.Store{DB: db}
	b, _ := bs.Create(ctx, "rv1", pid, "web", "") // To do, In progress, Done
	todo, doing, done := b.Columns[0].ID, b.Columns[1].ID, b.Columns[2].ID
	s := Store{DB: db}
	h := NewSystem(db)
	agent := auth.WithAPIKeyUser(ctx, "rv1", "claude")
	person := clerk.ContextWithSessionClaims(ctx, &clerk.SessionClaims{RegisteredClaims: clerk.RegisteredClaims{Subject: "user_ava"}, Claims: clerk.Claims{ActiveOrganizationID: "rv1"}})
	call := func(as context.Context, handler http.HandlerFunc, id, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest("POST", "/", strings.NewReader(body)).WithContext(as)
		req.SetPathValue("id", id)
		rec := httptest.NewRecorder()
		handler(rec, req)
		return rec
	}
	move := func(as context.Context, id, col string) *httptest.ResponseRecorder {
		return call(as, h.Move, id, `{"column_id":"`+col+`"}`)
	}
	review := func(as context.Context, id, body string) *httptest.ResponseRecorder {
		return call(as, h.ReviewTicket, id, body)
	}
	backlog := func(as context.Context, id string) *httptest.ResponseRecorder {
		return call(as, h.ToBacklog, id, "")
	}
	resolve := func(as context.Context, id string) *httptest.ResponseRecorder {
		return call(as, h.Resolve, id, "")
	}
	state := func(id string) (column string, done, waiting bool) {
		tk, err := s.Get(ctx, "rv1", id)
		if err != nil {
			t.Fatal(err)
		}
		return *tk.ColumnID, tk.Done, tk.ReviewRequestedAt != nil
	}

	// off: agents finish straight to Done
	a, _ := s.Create(ctx, "rv1", todo, "task", "agent work", "")
	if rec := move(agent, a.ID, done); rec.Code != 204 {
		t.Fatalf("review off: %d %s", rec.Code, rec.Body)
	}
	if col, d, w := state(a.ID); col != done || !d || w {
		t.Fatalf("review off state: %s %v %v", col, d, w)
	}

	if err := bs.SetAgentReview(ctx, b.ID, "rv1", true); err != nil {
		t.Fatal(err)
	}
	// on: the agent's move into Done stops in the column before it, waiting, and isn't done
	c, _ := s.Create(ctx, "rv1", todo, "task", "needs a look", "")
	rec := move(agent, c.ID, done)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"needs_review":true`) {
		t.Fatalf("agent finish: %d %s", rec.Code, rec.Body)
	}
	if col, d, w := state(c.ID); col != doing || d || !w {
		t.Fatalf("waiting state: %s done=%v waiting=%v", col, d, w)
	}
	// Neither backlog transfer nor later resolution can let an agent bypass the review.
	bypass, _ := s.Create(ctx, "rv1", todo, "task", "review bypass", "")
	if rec := move(agent, bypass.ID, done); rec.Code != 200 {
		t.Fatalf("request review: %d %s", rec.Code, rec.Body)
	}
	if rec := backlog(agent, bypass.ID); rec.Code != http.StatusConflict {
		t.Fatalf("agent backlog bypass: %d %s", rec.Code, rec.Body)
	}
	if rec := backlog(person, bypass.ID); rec.Code != http.StatusNoContent {
		t.Fatalf("person backlog: %d %s", rec.Code, rec.Body)
	}
	if rec := resolve(agent, bypass.ID); rec.Code != http.StatusConflict {
		t.Fatalf("agent resolve bypass: %d %s", rec.Code, rec.Body)
	}
	if rec := resolve(person, bypass.ID); rec.Code != http.StatusNoContent {
		t.Fatalf("person resolve: %d %s", rec.Code, rec.Body)
	}
	// nor by taking work off a review board before asking for review: backlog, then resolve (TL-249)
	sneak, _ := s.Create(ctx, "rv1", doing, "task", "half done", "")
	if rec := backlog(agent, sneak.ID); rec.Code != http.StatusConflict {
		t.Fatalf("agent took work off a review board: %d %s", rec.Code, rec.Body)
	}
	if rec := backlog(person, sneak.ID); rec.Code != http.StatusNoContent { // people still can
		t.Fatalf("person backlog: %d %s", rec.Code, rec.Body)
	}
	// agents can't review their own work; nor review what isn't waiting
	if rec := review(agent, c.ID, `{"approve":true}`); rec.Code != 403 {
		t.Fatalf("agent review: %d", rec.Code)
	}
	if rec := review(person, a.ID, `{"approve":true}`); rec.Code != 409 {
		t.Fatalf("not waiting: %d", rec.Code)
	}
	if rec := review(person, c.ID, `{}`); rec.Code != 400 {
		t.Fatalf("approve required: %d", rec.Code)
	}
	// sent back: the first column, with the reviewer's comment, no longer waiting
	if rec := review(person, c.ID, `{"approve":false,"comment":"Missing tests"}`); rec.Code != 204 {
		t.Fatalf("send back: %d %s", rec.Code, rec.Body)
	}
	if col, d, w := state(c.ID); col != todo || d || w {
		t.Fatalf("sent back state: %s %v %v", col, d, w)
	}
	var author string
	if err := db.QueryRow(ctx, `SELECT author FROM ticket_comments WHERE ticket_id = $1 AND body = 'Missing tests'`, c.ID).Scan(&author); err != nil || author != "user_ava" {
		t.Fatalf("review comment: %q %v", author, err)
	}
	// finished again, then approved: Done
	_ = move(agent, c.ID, done)
	if rec := review(person, c.ID, `{"approve":true}`); rec.Code != 204 {
		t.Fatalf("approve: %d %s", rec.Code, rec.Body)
	}
	if col, d, w := state(c.ID); col != done || !d || w {
		t.Fatalf("approved state: %s %v %v", col, d, w)
	}
	// people still move straight to Done; an agent's other moves never wait
	p, _ := s.Create(ctx, "rv1", todo, "task", "person's work", "")
	if rec := move(person, p.ID, done); rec.Code != 204 {
		t.Fatalf("person move: %d", rec.Code)
	}
	if _, d, w := state(p.ID); !d || w {
		t.Fatal("a person's finish waited for review")
	}
	q, _ := s.Create(ctx, "rv1", todo, "task", "in flight", "")
	if rec := move(agent, q.ID, doing); rec.Code != 204 {
		t.Fatalf("agent mid move: %d", rec.Code)
	}
	if _, _, w := state(q.ID); w {
		t.Fatal("a move short of Done waited for review")
	}
	// the board says it's on
	if got, _ := bs.Get(ctx, b.ID, "rv1"); !got.AgentReview {
		t.Fatal("board agent_review not returned")
	}
}
