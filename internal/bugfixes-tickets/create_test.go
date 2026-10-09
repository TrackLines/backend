package bugfixesTickets

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgreSQL is provisioned by this package's TestMain.
func TestCreate(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Fatal("TEST_DATABASE_URL not set; package TestMain should provision PostgreSQL")
	}
	ctx := context.Background()
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
	key := "bf_test_key_for_create"
	h := sha256.Sum256([]byte(key))
	if _, err := db.Exec(ctx, `DELETE FROM users WHERE clerk_id IN ('bf1', 'bf2');
		DELETE FROM roadmaps WHERE owner_clerk_id IN ('bf1', 'bf2'); DELETE FROM boards WHERE owner_clerk_id IN ('bf1', 'bf2'); DELETE FROM projects WHERE owner_clerk_id IN ('bf1', 'bf2'); -- org-owned rows no longer cascade from users
		INSERT INTO users (clerk_id, email) VALUES ('bf1', 'a@b.c'), ('bf2', 'd@e.f')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO api_keys (owner_clerk_id, org_id, name, prefix, hash) VALUES ('bf1', 'bf1', 'bugfixes', 'bf_test', $1)`, h[:]); err != nil {
		t.Fatal(err)
	}
	board := func(owner string) (string, string) {
		var pid, bid, col string
		_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ($1, 'p') RETURNING id`, owner).Scan(&pid)
		_ = db.QueryRow(ctx, `INSERT INTO boards (project_id, owner_clerk_id, name) VALUES ($1, $2, 'b') RETURNING id`, pid, owner).Scan(&bid)
		_ = db.QueryRow(ctx, `INSERT INTO columns (board_id, name, position) VALUES ($1, 'Triage', 0) RETURNING id`, bid).Scan(&col)
		return bid, col
	}
	bid, col := board("bf1")
	otherBid, otherCol := board("bf2")
	_, otherBoardsCol := board("bf1")

	srv := Middleware(db)(http.HandlerFunc(NewSystem(db).Create))
	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/api/bugfixes/tickets", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}
	body := func(b, c, extra string) string {
		return `{"board_id":"` + b + `","column_id":"` + c + `","title":"NPE in checkout","body":"stack…"` + extra + `}`
	}

	rec := post(body(bid, col, `,"priority":"high","labels":["agent:checkout-api"," agent:CHECKOUT-api "]`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var got Ticket
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.CreatedBy != "bugfixes" || got.Priority != "high" || strings.Join(got.Labels, ",") != "agent:checkout-api" {
		t.Fatalf("response: %+v", got)
	}
	var typ, prio, project string
	var labels []string
	if err := db.QueryRow(ctx, `SELECT t.type::text, t.priority::text, t.project_id::text,
		ARRAY(SELECT label FROM ticket_labels WHERE ticket_id = t.id) FROM tickets t WHERE t.id = $1`, got.ID).Scan(&typ, &prio, &project, &labels); err != nil {
		t.Fatal(err)
	}
	if typ != "bug" || prio != "high" || project == "" || len(labels) != 1 {
		t.Fatalf("stored: %s %s %q %v", typ, prio, project, labels)
	}
	if rec := post(body(bid, col, "")); rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"priority":"medium"`) {
		t.Fatalf("defaults: %d %s", rec.Code, rec.Body)
	}

	for name, c := range map[string]struct {
		body string
		code int
	}{
		"someone else's board":      {body(otherBid, otherCol, ""), 404},
		"column from another board": {body(bid, otherBoardsCol, ""), 404},
		"not a uuid":                {body("x", "y", ""), 404},
		"bad priority":              {body(bid, col, `,"priority":"asap"`), 400},
		"label too long":            {body(bid, col, `,"labels":["`+strings.Repeat("x", 51)+`"]`), 400},
		"missing title":             {`{"board_id":"` + bid + `","column_id":"` + col + `"}`, 400},
	} {
		if rec := post(c.body); rec.Code != c.code {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	var n int
	_ = db.QueryRow(ctx, `SELECT count(*) FROM tickets t JOIN projects p ON p.id = t.project_id WHERE p.owner_clerk_id = 'bf2'`).Scan(&n)
	if n != 0 {
		t.Fatal("a bugfixes key created a ticket on another user's board")
	}
}
