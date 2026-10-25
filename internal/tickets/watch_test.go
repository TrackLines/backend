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
	"github.com/tracklines/backend/internal/boards"
	"github.com/tracklines/backend/internal/notifications"
)

// TL-587: people @mentioned on a ticket watch it: they hear its moves, assignment and finishing,
// once, never about their own changes, and not once they've muted it.
func TestWatchersHearTicketEvents(t *testing.T) {
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
	if _, err := db.Exec(ctx, `DELETE FROM notifications WHERE org_id = 'wt1'; DELETE FROM ticket_mentions WHERE org_id = 'wt1';
		DELETE FROM ticket_watch_mutes WHERE org_id = 'wt1'; DELETE FROM org_admins WHERE org_id = 'wt1';
		DELETE FROM boards WHERE owner_clerk_id = 'wt1'; DELETE FROM projects WHERE owner_clerk_id = 'wt1';
		INSERT INTO org_admins (org_id, user_clerk_id, granted_by) VALUES ('wt1', 'user_lead', 'test')`); err != nil {
		t.Fatal(err)
	}
	var pid string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name, key_prefix) VALUES ('wt1', 'Watch', 'WT') RETURNING id`).Scan(&pid)
	b, _ := boards.Store{DB: db}.Create(ctx, "wt1", pid, "web", "")
	todo, doing, done := b.Columns[0].ID, b.Columns[1].ID, b.Columns[2].ID
	h := NewSystem(db).WithNotifier(notifications.Store{DB: db})
	h.memberships = testAssigneeMembers{"user_ava", "user_bo", "user_cy", "user_lead"}
	as := func(user string) context.Context {
		return clerk.ContextWithSessionClaims(ctx, &clerk.SessionClaims{RegisteredClaims: clerk.RegisteredClaims{Subject: user}, Claims: clerk.Claims{ActiveOrganizationID: "wt1"}})
	}
	call := func(c context.Context, handler http.HandlerFunc, id, body string) int {
		req := httptest.NewRequest("POST", "/", strings.NewReader(body)).WithContext(c)
		req.SetPathValue("id", id)
		rec := httptest.NewRecorder()
		handler(rec, req)
		return rec.Code
	}
	inbox := func(user string) []string {
		rows, _ := db.Query(ctx, `SELECT kind || ' ' || actor || ' ' || text FROM notifications WHERE org_id = 'wt1' AND user_id = $1 ORDER BY created_at, id`, user)
		defer rows.Close()
		var out []string
		for rows.Next() {
			var x string
			_ = rows.Scan(&x)
			out = append(out, x)
		}
		return out
	}

	tk, _ := Store{DB: db}.Create(ctx, "wt1", todo, "task", "Fix login", "")
	_, _ = db.Exec(ctx, `UPDATE tickets SET created_by = 'user_ava' WHERE id = $1`, tk.ID)
	// Bo and Cy are mentioned on it (Cy twice: still told once)
	if _, err := db.Exec(ctx, `INSERT INTO ticket_mentions (org_id, user_id, ticket_id) VALUES ('wt1', 'user_bo', $1), ('wt1', 'user_cy', $1), ('wt1', 'user_cy', $1)`, tk.ID); err != nil {
		t.Fatal(err)
	}
	name := tk.Key + ": Fix login"

	// assigned to Bo: Bo gets "assigned you" only; Cy hears it as a watcher
	if code := call(as("user_lead"), h.Assign, tk.ID, `{"assignee":"user_bo"}`); code != 200 {
		t.Fatalf("assign: %d", code)
	}
	if got := inbox("user_bo"); len(got) != 1 || got[0] != "assigned user_lead assigned you "+name {
		t.Fatalf("bo, assigned: %v", got)
	}
	if got := inbox("user_cy"); len(got) != 1 || got[0] != "watching user_lead assigned "+name {
		t.Fatalf("cy, assigned: %v", got)
	}
	// Bo moves it along: Cy hears where it went; Bo isn't told about his own move
	if code := call(as("user_bo"), h.Move, tk.ID, `{"column_id":"`+doing+`"}`); code != 204 {
		t.Fatalf("move: %d", code)
	}
	if got := inbox("user_cy"); len(got) != 2 || got[1] != "watching user_bo moved "+name+" to "+b.Columns[1].Name {
		t.Fatalf("cy, moved: %v", got)
	}
	if got := inbox("user_bo"); len(got) != 1 {
		t.Fatalf("bo's own move: %v", got)
	}
	// Cy mutes it: finishing it tells Ava (who raised it), not Cy
	if _, err := db.Exec(ctx, `INSERT INTO ticket_watch_mutes (org_id, user_id, ticket_id) VALUES ('wt1', 'user_cy', $1)`, tk.ID); err != nil {
		t.Fatal(err)
	}
	if code := call(as("user_bo"), h.Move, tk.ID, `{"column_id":"`+done+`"}`); code != 204 {
		t.Fatalf("finish: %d", code)
	}
	if got := inbox("user_cy"); len(got) != 2 {
		t.Fatalf("cy muted: %v", got)
	}
	if got := inbox("user_ava"); len(got) != 1 || got[0] != "finished user_bo finished "+name {
		t.Fatalf("ava, finished: %v", got)
	}
}
