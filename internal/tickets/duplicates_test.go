package tickets

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/boards"
)

// Mark as duplicate (TL-219): a never-resolved ticket (resolution NULL) used to fail with
// "cannot scan NULL into *bool"; it closes into the board's Done column, copies what was asked,
// retries are harmless, and the rules hold.
func TestMarkDuplicate(t *testing.T) {
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
	for _, q := range []string{`DELETE FROM boards WHERE owner_clerk_id IN ('dup1', 'dup2')`, `DELETE FROM projects WHERE owner_clerk_id IN ('dup1', 'dup2')`} {
		if _, err := db.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	project := func(org string) string {
		var id string
		if err := db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ($1, 'Dup') RETURNING id`, org).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	pid := project("dup1")
	b, err := boards.Store{DB: db}.Create(ctx, "dup1", pid, "b", "")
	if err != nil {
		t.Fatal(err)
	}
	s := Store{DB: db}
	copyTk, _ := s.Create(ctx, "dup1", b.Columns[0].ID, "bug", "login broken", "")
	original, _ := s.CreateBacklog(ctx, "dup1", pid, "bug", "can't sign in", "")
	other, _ := s.CreateBacklog(ctx, "dup1", pid, "bug", "sign-in fails", "")
	elsewhere, _ := s.CreateBacklog(ctx, "dup2", project("dup2"), "bug", "x", "")
	// audit: an AI key can't close a ticket as a duplicate on a board where a person reviews agents' work
	if _, err := db.Exec(ctx, `UPDATE boards SET agent_review = true WHERE id = $1`, b.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkDuplicate(ctx, "dup1", "agent", copyTk.ID, original.ID, DuplicateOptions{Agent: true}); !errors.Is(err, ErrAgentReview) {
		t.Fatalf("agent on a review board: %v", err)
	}
	if _, err := db.Exec(ctx, `UPDATE boards SET agent_review = false WHERE id = $1`, b.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO ticket_labels (ticket_id, label) VALUES ($1, 'auth')`, copyTk.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO ticket_comments (ticket_id, body, author) VALUES ($1, 'seen on mobile', 'user_sam')`, copyTk.ID); err != nil {
		t.Fatal(err)
	}

	h := NewSystem(db)
	mark := func(org, id, of string) int {
		req := httptest.NewRequest("POST", "/", strings.NewReader(`{"duplicate_of":"`+of+`","copy_labels":true,"copy_comments":true}`))
		req = req.WithContext(auth.WithAPIKey(ctx, "user_me", org, "agent", ""))
		req.SetPathValue("id", id)
		rec := httptest.NewRecorder()
		h.MarkDuplicate(rec, req)
		return rec.Code
	}

	if code := mark("dup1", copyTk.ID, original.ID); code != 204 {
		t.Fatalf("mark a never-resolved ticket as a duplicate: %d", code)
	}
	var resolution, column string
	var duplicateOf *string
	_ = db.QueryRow(ctx, `SELECT resolution, column_id, duplicate_of FROM tickets WHERE id = $1`, copyTk.ID).Scan(&resolution, &column, &duplicateOf)
	if resolution != "duplicate" || column != b.Columns[len(b.Columns)-1].ID || duplicateOf == nil || *duplicateOf != original.ID {
		t.Fatalf("after marking: resolution=%s column=%s duplicate_of=%v", resolution, column, duplicateOf)
	}
	var labels, comments int
	_ = db.QueryRow(ctx, `SELECT (SELECT count(*) FROM ticket_labels WHERE ticket_id = $1 AND label = 'auth'),
		(SELECT count(*) FROM ticket_comments WHERE ticket_id = $1 AND body LIKE '[Copied from %seen on mobile')`, original.ID).Scan(&labels, &comments)
	if labels != 1 || comments != 1 {
		t.Fatalf("copied: labels=%d comments=%d", labels, comments)
	}

	// a retry changes nothing (comments aren't copied twice)
	if code := mark("dup1", copyTk.ID, original.ID); code != 204 {
		t.Fatalf("retry: %d", code)
	}
	_ = db.QueryRow(ctx, `SELECT count(*) FROM ticket_comments WHERE ticket_id = $1`, original.ID).Scan(&comments)
	if comments != 1 {
		t.Fatalf("retry copied comments again: %d", comments)
	}
	for name, c := range map[string]struct {
		org, id, of string
		want        int
	}{
		"already a duplicate of something else": {"dup1", copyTk.ID, other.ID, 409},
		"onto a duplicate":                      {"dup1", other.ID, copyTk.ID, 400},
		"onto itself":                           {"dup1", other.ID, other.ID, 400},
		"onto another org's ticket":             {"dup1", other.ID, elsewhere.ID, 404},
		"from another org":                      {"dup2", other.ID, original.ID, 404},
	} {
		if code := mark(c.org, c.id, c.of); code != c.want {
			t.Errorf("%s: %d, want %d", name, code, c.want)
		}
	}
}
