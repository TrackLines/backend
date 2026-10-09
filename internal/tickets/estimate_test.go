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

// PostgreSQL is provisioned by this package's TestMain.
func TestEstimates(t *testing.T) {
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
	if _, err := db.Exec(ctx, `DELETE FROM users WHERE clerk_id = 'es1';
		DELETE FROM boards WHERE owner_clerk_id = 'es1'; DELETE FROM projects WHERE owner_clerk_id = 'es1';
		INSERT INTO users (clerk_id, email) VALUES ('es1', 'a@b.c')`); err != nil {
		t.Fatal(err)
	}
	var pid string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('es1', 'p') RETURNING id`).Scan(&pid)
	bs := boards.Store{DB: db}
	fib, _ := bs.Create(ctx, "es1", pid, "fib", "")
	tee, _ := bs.Create(ctx, "es1", pid, "tee", "")
	fib2, _ := bs.Create(ctx, "es1", pid, "fib2", "")
	for _, b := range []*boards.Board{fib, fib2} {
		if err := bs.SetEstimateScale(ctx, b.ID, "es1", "fibonacci"); err != nil {
			t.Fatal(err)
		}
	}
	_ = bs.SetEstimateScale(ctx, tee.ID, "es1", "tshirt")
	s := Store{DB: db}
	est := func(id string) string {
		var e *string
		_ = db.QueryRow(ctx, `SELECT estimate FROM tickets WHERE id = $1`, id).Scan(&e)
		if e == nil {
			return ""
		}
		return *e
	}

	a, _ := s.Create(ctx, "es1", fib.Columns[0].ID, "task", "a", "")
	if err := s.SetEstimate(ctx, "es1", a.ID, "M"); !errors.Is(err, boards.ErrBadEstimate) {
		t.Fatal("t-shirt size on a fibonacci board:", err)
	}
	if err := s.SetEstimate(ctx, "es1", a.ID, "8"); err != nil || est(a.ID) != "8" {
		t.Fatal("set:", err, est(a.ID))
	}
	if d, _ := s.Get(ctx, "es1", a.ID); d.EstimateScale != "fibonacci" || d.Estimate == nil || *d.Estimate != "8" {
		t.Fatalf("detail: %+v", d)
	}
	// same scale on another board: kept; different scale: cleared
	if err := s.Move(ctx, "es1", a.ID, fib2.Columns[0].ID, 0); err != nil || est(a.ID) != "8" {
		t.Fatal("same-scale move:", err, est(a.ID))
	}
	if err := s.Move(ctx, "es1", a.ID, tee.Columns[0].ID, 0); err != nil || est(a.ID) != "" {
		t.Fatal("cross-scale move:", err, est(a.ID))
	}
	_ = s.SetEstimate(ctx, "es1", a.ID, "XL")
	if err := s.ToBacklog(ctx, "es1", a.ID); err != nil || est(a.ID) != "" {
		t.Fatal("to backlog:", err, est(a.ID))
	}
	if err := s.SetEstimate(ctx, "es1", a.ID, "3"); !errors.Is(err, boards.ErrBadEstimate) {
		t.Fatal("backlog estimate:", err)
	}
	if err := s.SetEstimate(ctx, "es1", a.ID, ""); err != nil {
		t.Fatal("clearing a backlog ticket:", err)
	}
	if err := s.SetEstimate(ctx, "other", a.ID, ""); !errors.Is(err, ErrNotFound) {
		t.Fatal("other owner:", err)
	}

	// changing scale clears open estimates, but not a closed sprint's (velocity history)
	var closed string
	_ = db.QueryRow(ctx, `INSERT INTO sprints (board_id, number, length_days, ends_at, closed_at)
		VALUES ($1, 1, 7, now(), now()) RETURNING id`, fib.ID).Scan(&closed)
	old, _ := s.Create(ctx, "es1", fib.Columns[2].ID, "task", "old", "")
	open, _ := s.Create(ctx, "es1", fib.Columns[0].ID, "task", "open", "")
	_ = s.SetEstimate(ctx, "es1", old.ID, "5")
	_ = s.SetEstimate(ctx, "es1", open.ID, "13")
	_, _ = db.Exec(ctx, `UPDATE tickets SET sprint_id = $2 WHERE id = $1`, old.ID, closed)
	if err := bs.SetEstimateScale(ctx, fib.ID, "es1", "linear"); err != nil {
		t.Fatal(err)
	}
	if est(old.ID) != "5" || est(open.ID) != "" {
		t.Fatalf("scale change: closed %q open %q", est(old.ID), est(open.ID))
	}

	// HTTP: estimate on create, bad value, backlog create
	h := NewSystem(db)
	as := auth.WithAPIKeyUser(ctx, "es1", "claude")
	for _, c := range []struct {
		body string
		want int
	}{{`{"title":"x","estimate":"4"}`, 201}, {`{"title":"x","estimate":"7"}`, 400}} {
		req := httptest.NewRequest("POST", "/", strings.NewReader(c.body)).WithContext(as)
		req.SetPathValue("id", fib.Columns[0].ID) // fib is linear now
		rec := httptest.NewRecorder()
		h.Create(rec, req)
		if rec.Code != c.want {
			t.Fatalf("create %s: %d %s", c.body, rec.Code, rec.Body)
		}
	}
	req := httptest.NewRequest("POST", "/", strings.NewReader(`{"title":"x","estimate":"3"}`)).WithContext(as)
	req.SetPathValue("id", pid)
	rec := httptest.NewRecorder()
	h.CreateBacklog(rec, req)
	if rec.Code != 400 {
		t.Fatalf("backlog create with estimate: %d", rec.Code)
	}
}
