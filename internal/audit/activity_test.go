package audit

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/boards"
	"github.com/tracklines/backend/internal/comments"
	"github.com/tracklines/backend/internal/tickets"
)

// hooked is a pool that names each request's caller, as the server's does (boards.ActorHook).
func hooked(t *testing.T) *pgxpool.Pool {
	t.Helper()
	db(t) // migrated
	cfg, err := pgxpool.ParseConfig(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	boards.ActorHook(cfg)
	p, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

// activity is the ticket's queued events other than stages, as "action actor/kind data".
func activity(t *testing.T, ticket string) []string {
	t.Helper()
	rows, err := db(t).Query(context.Background(), `SELECT action, COALESCE(actor, '-'), actor_kind, data::text FROM audit_outbox
		WHERE target_id = $1 AND action <> 'ticket.stage' ORDER BY queued`, ticket)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a, who, kind, data string
		_ = rows.Scan(&a, &who, &kind, &data)
		out = append(out, fmt.Sprintf("%s %s/%s %s", a, who, kind, data))
	}
	return out
}

// TL-677: every change to a ticket is queued, by whoever made it, through the real code paths, and text is
// recorded as changed, never what it says
func TestTicketActivityQueued(t *testing.T) {
	ctx := context.Background()
	p := hooked(t)
	org := "org_act"
	project, b, cols := board(t, org)
	var ticket, blocker string
	if err := p.QueryRow(ctx, `INSERT INTO tickets (project_id, board_id, column_id, title, description, position, created_by) VALUES ($1, $2, $3, 'Secret plan', 'do not share', 0, 'user_1') RETURNING id`,
		project, b, cols[0]).Scan(&ticket); err != nil {
		t.Fatal(err)
	}
	if err := p.QueryRow(ctx, `INSERT INTO tickets (project_id, board_id, column_id, title, position, created_by) VALUES ($1, $2, $3, 'b', 1, 'user_1') RETURNING id`,
		project, b, cols[0]).Scan(&blocker); err != nil {
		t.Fatal(err)
	}
	ava := auth.WithAPIKey(ctx, "user_ava", org, "", "personal")
	claude := auth.WithAPIKey(ctx, "user_ava", org, "Claude", "ai")
	s := tickets.Store{DB: p}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	// fields: one event per change, values for the short ones, only "changed" for text
	must(s.Update(ava, org, ticket, "bug", "Secret plan v2", "still secret", "urgent"))
	must(s.Update(ava, org, ticket, "", "Secret plan v2", "still secret", "")) // nothing changed: nothing queued
	who := "Claude"
	_, err := s.Assign(claude, org, "user_ava", ticket, &who, true)
	must(err)
	_, err = s.Assign(ava, org, "user_ava", ticket, nil, true)
	must(err)
	must(s.SetLabels(ava, org, ticket, []string{"api", "bug"}))
	must(s.SetLabels(ava, org, ticket, []string{"bug", "ui"})) // only the difference
	must(s.SetLabels(ava, org, ticket, []string{"bug", "ui"})) // the same again: nothing
	must(s.SetBlockedBy(ava, org, ticket, []string{blocker}))
	must(s.SetBlockedBy(ava, org, ticket, nil))
	c, err := comments.Store{DB: p}.Create(ava, org, "user_ava", ticket, "my private thoughts", nil)
	must(err)
	_, err = p.Exec(ava, `UPDATE ticket_comments SET body = 'edited thoughts' WHERE id = $1`, c.ID)
	must(err)
	_, err = p.Exec(ava, `DELETE FROM ticket_comments WHERE id = $1`, c.ID)
	must(err)
	_, err = p.Exec(ava, `INSERT INTO ticket_attachments (ticket_id, file_key, url, name, size_bytes, content_type, created_by)
		VALUES ($1, 'k-act', 'https://x.ufs.sh/f/k-act', 'spec.pdf', 2048, 'application/pdf', 'user_ava')`, ticket)
	must(err)
	_, err = p.Exec(ava, `INSERT INTO ticket_prs (ticket_id, provider, repo, number, url, title, state) VALUES ($1, 'github', 'tl/web', 42, 'https://github.com/tl/web/pull/42', 'pr', 'open')`, ticket)
	must(err)
	_, err = p.Exec(ctx, `UPDATE ticket_prs SET state = 'merged' WHERE ticket_id = $1`, ticket) // a webhook: nobody
	must(err)
	must(s.Delete(ava, org, ticket))
	_, err = p.Exec(ava, `UPDATE tickets_all SET deleted_at = NULL WHERE id = $1`, ticket)
	must(err)

	got := activity(t, ticket)
	want := []string{
		`ticket.updated user_ava/person {"fields": {"type": {"to": "bug", "from": "task"}, "title": true, "priority": {"to": "urgent", "from": "medium"}, "description": true}}`,
		`ticket.assigned Claude/agent {"to": "Claude", "from": null}`,
		`ticket.assigned user_ava/person {"to": null, "from": "Claude"}`,
		`ticket.labels_changed user_ava/person {"added": ["api", "bug"]}`,
		`ticket.labels_changed user_ava/person {"removed": ["api"]}`,
		`ticket.labels_changed user_ava/person {"added": ["ui"]}`,
		`ticket.blocked_by_changed user_ava/person {"added": ["` + blocker + `"]}`,
		`ticket.blocked_by_changed user_ava/person {"removed": ["` + blocker + `"]}`,
		`comment.added user_ava/person {"comment_id": "` + c.ID + `"}`,
		`comment.edited user_ava/person {"comment_id": "` + c.ID + `"}`,
		`comment.deleted user_ava/person {"comment_id": "` + c.ID + `"}`,
	}
	if len(got) < len(want) {
		t.Fatalf("queued:\n%s", strings.Join(got, "\n"))
	}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("event %d:\n got %s\nwant %s\nall:\n%s", i, got[i], w, strings.Join(got, "\n"))
		}
	}
	rest := strings.Join(got[len(want):], "\n")
	for _, w := range []string{
		`attachment.added user_ava/person {"name": "spec.pdf", "size": 2048,`,
		`pr.linked user_ava/person {"repo": "tl/web", "state": "open", "number": 42, "provider": "github"}`,
		`pr.state_changed -/system {"to": "merged", "from": "open", "repo": "tl/web", "number": 42, "provider": "github"}`,
		`ticket.deleted user_ava/person {}`,
		`ticket.restored user_ava/person {}`,
	} {
		if !strings.Contains(rest, w) {
			t.Fatalf("missing %s in:\n%s", w, rest)
		}
	}
	// never the text
	all := strings.Join(got, "\n")
	for _, secret := range []string{"Secret plan", "secret", "thoughts"} {
		if strings.Contains(all, secret) {
			t.Fatalf("text leaked into the audit: %q in\n%s", secret, all)
		}
	}
}

// a ticket purged takes its cascades with it: its labels, comments and files queue nothing more
func TestPurgeQueuesNoCascadeNoise(t *testing.T) {
	ctx := context.Background()
	p := hooked(t)
	project, _, _ := board(t, "org_act2")
	var ticket string
	if err := p.QueryRow(ctx, `INSERT INTO tickets (project_id, title, position, created_by) VALUES ($1, 't', 0, 'user_1') RETURNING id`, project).Scan(&ticket); err != nil {
		t.Fatal(err)
	}
	_, _ = p.Exec(ctx, `INSERT INTO ticket_labels (ticket_id, label) VALUES ($1, 'x')`, ticket)
	_, _ = p.Exec(ctx, `INSERT INTO ticket_comments (ticket_id, body, author) VALUES ($1, 'c', 'user_1')`, ticket)
	before := len(activity(t, ticket))
	if _, err := p.Exec(ctx, `DELETE FROM tickets_all WHERE id = $1`, ticket); err != nil {
		t.Fatal(err)
	}
	got := activity(t, ticket)
	if len(got) != before+1 || !strings.HasPrefix(got[len(got)-1], "ticket.purged") {
		t.Fatalf("after the purge:\n%s", strings.Join(got, "\n"))
	}
}

// 200 tickets changed in one statement: one event each, quickly (the bulk edit's worst case)
func TestManyTicketsChangedAtOnce(t *testing.T) {
	ctx := context.Background()
	p := hooked(t)
	project, _, _ := board(t, "org_act3")
	if _, err := p.Exec(ctx, `INSERT INTO tickets (project_id, title, position, created_by) SELECT $1, 't' || n, n, 'user_1' FROM generate_series(1, 200) n`, project); err != nil {
		t.Fatal(err)
	}
	ava := auth.WithAPIKey(ctx, "user_ava", "org_act3", "", "personal")
	start := time.Now()
	tag, err := p.Exec(ava, `UPDATE tickets SET priority = 'high', assigned_to = 'user_ava' WHERE project_id = $1`, project)
	took := time.Since(start)
	if err != nil || tag.RowsAffected() != 200 {
		t.Fatalf("%v %d", err, tag.RowsAffected())
	}
	var n int
	_ = p.QueryRow(ctx, `SELECT count(*) FROM audit_outbox WHERE org_id = 'org_act3' AND action IN ('ticket.updated', 'ticket.assigned') AND actor = 'user_ava'`).Scan(&n)
	t.Logf("200 tickets, 2 changes each: %s", took)
	if n != 400 || took > 3*time.Second {
		t.Fatalf("%d events in %s", n, took)
	}
}
