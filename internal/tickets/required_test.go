package tickets

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/boards"
)

func TestRequiredEstimates(t *testing.T) {
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
	if _, err := db.Exec(ctx, `DELETE FROM boards WHERE owner_clerk_id = 're1'; DELETE FROM projects WHERE owner_clerk_id = 're1'`); err != nil {
		t.Fatal(err)
	}
	var pid string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('re1', 'p') RETURNING id`).Scan(&pid)
	bs := boards.Store{DB: db}
	fib, _ := bs.Create(ctx, "re1", pid, "fib", "")
	fib2, _ := bs.Create(ctx, "re1", pid, "fib2", "")
	plain, _ := bs.Create(ctx, "re1", pid, "plain", "")
	for _, b := range []*boards.Board{fib, fib2} {
		if _, err := db.Exec(ctx, `UPDATE boards SET estimate_scale = 'fibonacci' WHERE id = $1`, b.ID); err != nil {
			t.Fatal(err)
		}
	}
	s := Store{DB: db}
	h := NewSystem(db)
	call := func(hf http.HandlerFunc, method, id, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/", strings.NewReader(body)).WithContext(auth.WithAPIKeyUser(ctx, "re1", "claude"))
		req.SetPathValue("id", id)
		rec := httptest.NewRecorder()
		hf(rec, req)
		return rec
	}
	tickets := func() int {
		var n int
		_ = db.QueryRow(ctx, `SELECT count(*) FROM tickets WHERE project_id = $1`, pid).Scan(&n)
		return n
	}
	estimate := func(id string) string {
		var e *string
		_ = db.QueryRow(ctx, `SELECT estimate FROM tickets WHERE id = $1`, id).Scan(&e)
		if e == nil {
			return ""
		}
		return *e
	}

	// create on a scaled board: required, on the scale, and a refusal leaves no ticket behind
	before := tickets()
	if rec := call(h.Create, "POST", fib.Columns[0].ID, `{"title":"x"}`); rec.Code != 400 || !strings.Contains(rec.Body.String(), "estimate required") {
		t.Fatalf("create without estimate: %d %s", rec.Code, rec.Body)
	}
	if rec := call(h.Create, "POST", fib.Columns[0].ID, `{"title":"x","estimate":"4"}`); rec.Code != 400 {
		t.Fatalf("create off the scale: %d", rec.Code)
	}
	if tickets() != before {
		t.Fatal("refused creates left tickets behind")
	}
	rec := call(h.Create, "POST", fib.Columns[0].ID, `{"title":"sized","estimate":"5"}`)
	var sized Ticket
	if rec.Code != 201 || json.Unmarshal(rec.Body.Bytes(), &sized) != nil || sized.Estimate == nil || *sized.Estimate != "5" {
		t.Fatalf("create with estimate: %d %s", rec.Code, rec.Body)
	}
	if rec := call(h.Create, "POST", plain.Columns[0].ID, `{"title":"free"}`); rec.Code != 201 {
		t.Fatalf("create on a board without a scale: %d", rec.Code)
	}

	// moving in from the backlog: refused without an estimate, sized on the way in with one
	bl, _ := s.CreateBacklog(ctx, "re1", pid, "task", "from backlog", "")
	if rec := call(h.Move, "POST", bl.ID, `{"column_id":"`+fib.Columns[0].ID+`"}`); rec.Code != 400 || !strings.Contains(rec.Body.String(), "fibonacci") {
		t.Fatalf("move in unestimated: %d %s", rec.Code, rec.Body)
	}
	if rec := call(h.Move, "POST", bl.ID, `{"column_id":"`+fib.Columns[0].ID+`","estimate":"4"}`); rec.Code != 400 {
		t.Fatalf("move in off the scale: %d", rec.Code)
	}
	if got, _ := s.Get(ctx, "re1", bl.ID); got.ColumnID != nil {
		t.Fatal("refused move still placed the ticket")
	}
	if rec := call(h.Move, "POST", bl.ID, `{"column_id":"`+fib.Columns[0].ID+`","estimate":"3"}`); rec.Code != 204 || estimate(bl.ID) != "3" {
		t.Fatalf("move in with estimate: %d %q", rec.Code, estimate(bl.ID))
	}
	// between boards on the same scale the estimate carries over; from a board without one it's needed
	if rec := call(h.Move, "POST", bl.ID, `{"column_id":"`+fib2.Columns[0].ID+`"}`); rec.Code != 204 || estimate(bl.ID) != "3" {
		t.Fatalf("same scale: %d %q", rec.Code, estimate(bl.ID))
	}
	free, _ := s.Create(ctx, "re1", plain.Columns[0].ID, "task", "unsized", "")
	if rec := call(h.Move, "POST", free.ID, `{"column_id":"`+fib.Columns[0].ID+`"}`); rec.Code != 400 {
		t.Fatalf("from a board without a scale: %d", rec.Code)
	}
	if rec := call(h.Move, "POST", free.ID, `{"column_id":"`+plain.Columns[1].ID+`","estimate":"3"}`); rec.Code != 400 {
		t.Fatalf("estimate on a board without a scale: %d", rec.Code)
	}

	// a ticket that was already unestimated on the board (from before) still moves within it
	var legacy string
	_ = db.QueryRow(ctx, `INSERT INTO tickets (project_id, board_id, column_id, type, title, position, created_by)
		VALUES ($1, $2, $3, 'task', 'legacy', 99, 're1') RETURNING id`, pid, fib.ID, fib.Columns[0].ID).Scan(&legacy)
	if rec := call(h.Move, "POST", legacy, `{"column_id":"`+fib.Columns[1].ID+`"}`); rec.Code != 204 {
		t.Fatalf("within the board: %d %s", rec.Code, rec.Body)
	}

	// an estimate can't be cleared on a scaled board, but can be changed
	if rec := call(h.Update, "PATCH", sized.ID, `{"title":"sized","estimate":""}`); rec.Code != 400 || estimate(sized.ID) != "5" {
		t.Fatalf("clear: %d %q", rec.Code, estimate(sized.ID))
	}
	if rec := call(h.Update, "PATCH", sized.ID, `{"title":"sized","estimate":"8"}`); rec.Code != 204 || estimate(sized.ID) != "8" {
		t.Fatalf("change: %d %q", rec.Code, estimate(sized.ID))
	}

	// automated (bugfixes) creates aren't asked for an estimate
	if bug, err := s.CreateAs(ctx, "re1", "bugfixes", fib.Columns[0].ID, "bug", "crash", ""); err != nil || bug.ColumnID == nil {
		t.Fatalf("bugfixes create: %+v %v", bug, err)
	}
}
