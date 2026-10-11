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
	"github.com/tracklines/backend/internal/notifications"
)

// The ticket events people are told about, through the real handlers.
func TestTicketNotifications(t *testing.T) {
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
	if _, err := db.Exec(ctx, `DELETE FROM notifications WHERE org_id = 'nf1'; DELETE FROM org_admins WHERE org_id = 'nf1';
		DELETE FROM boards WHERE owner_clerk_id = 'nf1'; DELETE FROM projects WHERE owner_clerk_id = 'nf1';
		INSERT INTO org_admins (org_id, user_clerk_id, granted_by) VALUES ('nf1', 'user_lead', 'test')`); err != nil {
		t.Fatal(err)
	}
	var pid string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name, key_prefix) VALUES ('nf1', 'Notify', 'NF') RETURNING id`).Scan(&pid)
	bs := boards.Store{DB: db}
	b, _ := bs.Create(ctx, "nf1", pid, "web", "")
	todo, done := b.Columns[0].ID, b.Columns[2].ID
	s := Store{DB: db}
	h := NewSystem(db).WithNotifier(notifications.Store{DB: db})
	h.memberships = testAssigneeMembers{"user_ava", "user_bo", "user_lead"}
	as := func(user string) context.Context {
		return clerk.ContextWithSessionClaims(ctx, &clerk.SessionClaims{RegisteredClaims: clerk.RegisteredClaims{Subject: user}, Claims: clerk.Claims{ActiveOrganizationID: "nf1"}})
	}
	agent := auth.WithAPIKeyUser(ctx, "nf1", "Claude")
	call := func(c context.Context, handler http.HandlerFunc, id, body string) int {
		req := httptest.NewRequest("POST", "/", strings.NewReader(body)).WithContext(c)
		req.SetPathValue("id", id)
		rec := httptest.NewRecorder()
		handler(rec, req)
		return rec.Code
	}
	inbox := func(user string) []string {
		rows, _ := db.Query(ctx, `SELECT kind || ' ' || actor || ' ' || text FROM notifications WHERE org_id = 'nf1' AND user_id = $1 ORDER BY created_at, id`, user)
		defer rows.Close()
		var out []string
		for rows.Next() {
			var x string
			_ = rows.Scan(&x)
			out = append(out, x)
		}
		return out
	}

	// Ava raises a ticket; she claims it herself (no news) then a lead assigns it to Bo
	tk, _ := s.Create(ctx, "nf1", todo, "task", "Fix login", "")
	_, _ = db.Exec(ctx, `UPDATE tickets SET created_by = 'user_ava' WHERE id = $1`, tk.ID)
	if code := call(as("user_lead"), h.Assign, tk.ID, `{"assignee":"user_bo"}`); code != 200 {
		t.Fatalf("assign: %d", code)
	}
	if got := inbox("user_bo"); len(got) != 1 || got[0] != "assigned user_lead assigned you "+tk.Key+": Fix login" {
		t.Fatalf("assigned: %v", got)
	}
	// Bo finishes it: Ava (who raised it) is told; Bo isn't told about his own move
	if code := call(as("user_bo"), h.Move, tk.ID, `{"column_id":"`+done+`"}`); code != 204 {
		t.Fatalf("move: %d", code)
	}
	if got := inbox("user_ava"); len(got) != 1 || got[0] != "finished user_bo finished "+tk.Key+": Fix login" {
		t.Fatalf("finished: %v", got)
	}
	// moving it within Done again isn't news twice
	_ = call(as("user_bo"), h.Move, tk.ID, `{"column_id":"`+done+`"}`)
	if got := inbox("user_ava"); len(got) != 1 {
		t.Fatalf("told twice: %v", got)
	}
	// Ava finishing her own ticket isn't news to her
	own, _ := s.Create(ctx, "nf1", todo, "task", "Own work", "")
	_, _ = db.Exec(ctx, `UPDATE tickets SET created_by = 'user_ava' WHERE id = $1`, own.ID)
	_ = call(as("user_ava"), h.Move, own.ID, `{"column_id":"`+done+`"}`)
	if got := inbox("user_ava"); len(got) != 1 {
		t.Fatalf("own action: %v", got)
	}

	// agent review: the board's managers hear an agent finished one; sending back tells the assignee
	_ = bs.SetAgentReview(ctx, b.ID, "nf1", true)
	rv, _ := s.Create(ctx, "nf1", todo, "task", "Agent work", "")
	_, _ = db.Exec(ctx, `UPDATE tickets SET assigned_to = 'user_bo' WHERE id = $1`, rv.ID)
	if code := call(agent, h.Move, rv.ID, `{"column_id":"`+done+`"}`); code != 200 {
		t.Fatalf("agent move: %d", code)
	}
	if got := inbox("user_lead"); len(got) != 1 || !strings.HasPrefix(got[0], "review Claude finished "+rv.Key) {
		t.Fatalf("review: %v", got)
	}
	if code := call(as("user_lead"), h.ReviewTicket, rv.ID, `{"approve":false,"comment":"Needs tests"}`); code != 204 {
		t.Fatalf("send back: %d", code)
	}
	if got := inbox("user_bo"); len(got) != 2 || got[1] != "sent_back user_lead sent back "+rv.Key+": Agent work: Needs tests" {
		t.Fatalf("sent back: %v", got)
	}

	// won't do tells whoever raised it, with the reason
	idea, _ := s.CreateBacklog(ctx, "nf1", pid, "feature", "Dark mode", "")
	_, _ = db.Exec(ctx, `UPDATE tickets SET created_by = 'user_cy' WHERE id = $1`, idea.ID)
	if code := call(as("user_lead"), h.Resolve, idea.ID, `{"resolution":"wont_do","reason":"Not this year"}`); code != 204 {
		t.Fatalf("won't do: %d", code)
	}
	if got := inbox("user_cy"); len(got) != 1 || got[0] != "wont_do user_lead closed "+idea.Key+": Dark mode as won't do: Not this year" {
		t.Fatalf("won't do: %v", got)
	}
}
