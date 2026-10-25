package clerkhooks

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
)

// A leaver's open tickets are unassigned with a comment saying why; done ones keep their assignee
// (TL-608). Leaving one org frees only that org's tickets; a deleted account frees the rest.
func TestLeaversTicketsAreUnassigned(t *testing.T) {
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
	if _, err := db.Exec(ctx, `INSERT INTO users (clerk_id, email) VALUES ('erin', 'erin@x.y'), ('finn', 'finn@x.y')`); err != nil {
		t.Fatal(err)
	}
	ticket := func(org, assignee string, done bool) string {
		t.Helper()
		var pid, id string
		if err := db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ($1, 'p') RETURNING id`, org).Scan(&pid); err != nil {
			t.Fatal(err)
		}
		var resolved *time.Time
		if done {
			now := time.Now()
			resolved = &now
		}
		if err := db.QueryRow(ctx, `INSERT INTO tickets (project_id, created_by, title, position, assigned_to, resolved_at)
			VALUES ($1, 'x', 't', 0, $2, $3) RETURNING id`, pid, assignee, resolved).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	openX, doneX, openY, finnX := ticket("org_x", "erin", false), ticket("org_x", "erin", true), ticket("org_y", "erin", false), ticket("org_x", "finn", false)
	check := func(id, assignee string, comments int) {
		t.Helper()
		var got *string
		var n int
		if err := db.QueryRow(ctx, `SELECT assigned_to, (SELECT count(*) FROM ticket_comments c WHERE c.ticket_id = t.id AND c.author = $2)
			FROM tickets t WHERE id = $1`, id, Author).Scan(&got, &n); err != nil {
			t.Fatal(err)
		}
		if (got == nil) != (assignee == "") || (got != nil && *got != assignee) || n != comments {
			t.Fatalf("ticket %s: assignee %v (want %q), %d comments (want %d)", id, got, assignee, n, comments)
		}
	}

	now := time.Now()
	h := NewSystem(db, secret).Webhook(func() time.Time { return now })
	n := 0
	send := func(body string) {
		t.Helper()
		n++
		id := fmt.Sprintf("msg_unassign_%d", n)
		req := httptest.NewRequest("POST", "/api/webhooks/clerk", strings.NewReader(body))
		req.Header.Set("svix-id", id)
		req.Header.Set("svix-timestamp", fmt.Sprint(now.Unix()))
		req.Header.Set("svix-signature", sign(key, id, now, body))
		rec := httptest.NewRecorder()
		h(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("%s: %d", body, rec.Code)
		}
	}
	left := `{"type":"organizationMembership.deleted","data":{"organization":{"id":"org_x"},"public_user_data":{"user_id":"erin"}}}`
	send(left)
	send(left) // Clerk resends: no second comment
	check(openX, "", 1)
	check(doneX, "erin", 0)
	check(openY, "erin", 0) // another org's
	check(finnX, "finn", 0) // someone else's
	var body string
	if err := db.QueryRow(ctx, `SELECT body FROM ticket_comments WHERE ticket_id = $1`, openX).Scan(&body); err != nil || !strings.Contains(body, "left the organization") {
		t.Fatalf("comment: %q %v", body, err)
	}

	send(`{"type":"user.deleted","data":{"id":"erin"}}`)
	check(openY, "", 1)
	check(doneX, "erin", 0)
	if err := db.QueryRow(ctx, `SELECT body FROM ticket_comments WHERE ticket_id = $1`, openY).Scan(&body); err != nil || !strings.Contains(body, "account was deleted") {
		t.Fatalf("comment: %q %v", body, err)
	}
}
