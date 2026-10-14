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
	"github.com/tracklines/backend/internal/comments"
)

func TestSearch(t *testing.T) {
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
	if _, err := db.Exec(ctx, `DELETE FROM boards WHERE owner_clerk_id IN ('se1', 'se2'); DELETE FROM projects WHERE owner_clerk_id IN ('se1', 'se2')`); err != nil {
		t.Fatal(err)
	}
	var pid, otherPid, foreignPid string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('se1', 'Shop') RETURNING id`).Scan(&pid)
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('se1', 'Docs') RETURNING id`).Scan(&otherPid)
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('se2', 'Shop') RETURNING id`).Scan(&foreignPid)
	b, _ := boards.Store{DB: db}.Create(ctx, "se1", pid, "web", "")
	s := Store{DB: db}
	mk := func(project, column, title, desc, typ, prio string) *Ticket {
		var tk *Ticket
		var err error
		if column != "" {
			tk, err = s.Create(ctx, "se1", column, typ, title, desc)
		} else {
			owner := "se1"
			if project == foreignPid {
				owner = "se2"
			}
			tk, err = s.CreateBacklog(ctx, owner, project, typ, title, desc)
		}
		if err != nil {
			t.Fatal(err)
		}
		if prio != "" {
			_ = s.Update(ctx, "se1", tk.ID, "", title, desc, prio)
		}
		return tk
	}
	titleHit := mk(pid, b.Columns[0].ID, "Checkout crash on save", "", "bug", "urgent")
	descHit := mk(pid, "", "Login page", "It crashes when saving the password field", "bug", "")
	commentHit := mk(otherPid, "", "Unrelated docs task", "", "task", "")
	if _, err := (comments.Store{DB: db}).Create(ctx, "se1", "u", commentHit.ID, "the checkout crashes here too", nil); err != nil {
		t.Fatal(err)
	}
	mk(foreignPid, "", "Checkout crash", "another org", "bug", "")
	done := mk(pid, b.Columns[0].ID, "Old crash fixed", "", "bug", "")
	_ = s.Move(ctx, "se1", done.ID, b.Columns[2].ID, 0)
	_ = s.SetLabels(ctx, "se1", descHit.ID, []string{"Auth"})
	_, _ = s.Claim(ctx, "se1", "claude", titleHit.ID)

	search := func(q SearchQuery) []SearchResult {
		t.Helper()
		out, err := s.Search(ctx, "se1", q)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	ids := func(rs []SearchResult) []string {
		var out []string
		for _, r := range rs {
			out = append(out, r.Title)
		}
		return out
	}
	// prefix words, title beats description beats comments; other orgs never show
	got := search(SearchQuery{Text: "crash"})
	if len(got) != 4 || got[0].ID != titleHit.ID {
		t.Fatalf("crash: %v", ids(got))
	}
	for _, r := range got {
		if r.ProjectID == foreignPid {
			t.Fatal("another org's ticket in results")
		}
	}
	if got := search(SearchQuery{Text: "pass"}); len(got) != 1 || got[0].ID != descHit.ID || !strings.Contains(got[0].Snippet, "password") {
		t.Fatalf("description hit + snippet: %+v", got)
	}
	if got := search(SearchQuery{Text: "checkout crashes"}); len(got) != 1 || got[0].ID != commentHit.ID || !strings.Contains(got[0].Snippet, "checkout") {
		t.Fatalf("comment hit: %+v", ids(got))
	}
	if got := search(SearchQuery{Text: "chec"}); len(got) != 2 { // type-ahead: title + comment
		t.Fatalf("prefix: %v", ids(got))
	}
	// similar tickets (TL-275): any word may match, so a typo doesn't hide them; most matches first;
	// short words are ignored or "a" would match everything. Every-word search still needs them all.
	if got := search(SearchQuery{Text: "checkout crsh typo"}); len(got) != 0 {
		t.Fatalf("every word must match: %v", ids(got))
	}
	if got := search(SearchQuery{Text: "checkout crash typo", AnyWord: true}); len(got) < 2 || got[0].ID != titleHit.ID {
		t.Fatalf("any word, best first: %v", ids(got))
	}
	if got := search(SearchQuery{Text: "a an login", AnyWord: true}); len(got) != 1 || got[0].ID != descHit.ID {
		t.Fatalf("any word, short words ignored: %v", ids(got))
	}
	// punctuation and operators can't break the query
	for _, nasty := range []string{"crash & | ! :* (", "'; DROP TABLE tickets; --", "!!!"} {
		if _, err := s.Search(ctx, "se1", SearchQuery{Text: nasty}); err != nil {
			t.Fatalf("%q: %v", nasty, err)
		}
	}
	// an exact key finds just that ticket
	if got := search(SearchQuery{Text: strings.ToLower(descHit.Key)}); len(got) != 1 || got[0].ID != descHit.ID {
		t.Fatalf("key: %v", ids(got))
	}
	// filters, alone (newest first) or with text
	checks := map[string]struct {
		q    SearchQuery
		want int
	}{
		"priority":   {SearchQuery{Text: "crash", Priorities: []string{"urgent"}}, 1},
		"type":       {SearchQuery{Types: []string{"task"}}, 1},
		"done":       {SearchQuery{Text: "crash", Status: "done"}, 1},
		"open":       {SearchQuery{Text: "crash", Status: "open"}, 3},
		"label":      {SearchQuery{Labels: []string{"auth"}}, 1},
		"assignee":   {SearchQuery{Assignee: "claude"}, 1},
		"unassigned": {SearchQuery{Text: "crash", Unassigned: true}, 3},
		"project":    {SearchQuery{ProjectID: otherPid}, 1},
		"board":      {SearchQuery{BoardID: b.ID}, 2},
		"no filter":  {SearchQuery{}, 4}, // the other org's ticket never counts
	}
	for name, c := range checks {
		if got := search(c.q); len(got) != c.want {
			t.Errorf("%s: %d %v", name, len(got), ids(got))
		}
	}
	if got := search(SearchQuery{Text: "crash", Limit: 2}); len(got) != 2 {
		t.Fatalf("limit: %d", len(got))
	}
	if got := search(SearchQuery{Text: "crash"}); got[0].BoardName == nil || *got[0].BoardName != "web" || got[0].ProjectName != "Shop" {
		t.Fatalf("where it lives: %+v", got[0])
	}

	// HTTP: me = the key's agent; bad filters are 400
	h := NewSystem(db)
	call := func(query string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/api/search?"+query, nil).WithContext(auth.WithAPIKeyUser(ctx, "se1", "claude"))
		rec := httptest.NewRecorder()
		h.Search(rec, req)
		return rec
	}
	if rec := call("assignee=me"); rec.Code != 200 || !strings.Contains(rec.Body.String(), titleHit.ID) || strings.Contains(rec.Body.String(), descHit.ID) {
		t.Fatalf("me: %d %s", rec.Code, rec.Body)
	}
	if rec := call("q=crash&priority=urgent&priority=high"); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"key":"`) {
		t.Fatalf("http: %d %s", rec.Code, rec.Body)
	}
	for _, bad := range []string{"type=epic", "priority=asap", "status=maybe", "limit=500"} {
		if rec := call(bad); rec.Code != 400 {
			t.Errorf("%s: %d", bad, rec.Code)
		}
	}
}
