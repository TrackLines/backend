package tickets

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/boards"
)

// TL-591: claiming says where "in progress" is: the column after To do, or null on a board with
// only To do and Done, where the column after To do is Done and moving there would finish it unstarted.
func TestClaimSaysWhereInProgressIs(t *testing.T) {
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
	if _, err := db.Exec(ctx, `DELETE FROM boards WHERE owner_clerk_id = 'cc1'; DELETE FROM projects WHERE owner_clerk_id = 'cc1'`); err != nil {
		t.Fatal(err)
	}
	var pid string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name, key_prefix) VALUES ('cc1', 'Claim', 'CC') RETURNING id`).Scan(&pid)
	bs := boards.Store{DB: db}
	three, _ := bs.Create(ctx, "cc1", pid, "three", "")
	two, _ := bs.Create(ctx, "cc1", pid, "two", "")
	if _, err := db.Exec(ctx, `DELETE FROM columns WHERE id = $1`, two.Columns[1].ID); err != nil { // just To do and Done
		t.Fatal(err)
	}
	s := Store{DB: db}
	h := NewSystem(db)
	claim := func(id string) *string {
		req := httptest.NewRequest("POST", "/", nil).WithContext(auth.WithAPIKeyUser(ctx, "cc1", "Claude"))
		req.SetPathValue("id", id)
		rec := httptest.NewRecorder()
		h.Claim(rec, req)
		var out struct {
			ID                 string  `json:"id"`
			InProgressColumnID *string `json:"in_progress_column_id"`
		}
		if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &out) != nil || out.ID != id {
			t.Fatalf("claim %s: %d %s", id, rec.Code, rec.Body)
		}
		return out.InProgressColumnID
	}

	a, _ := s.Create(ctx, "cc1", three.Columns[0].ID, "task", "Three columns", "")
	if got := claim(a.ID); got == nil || *got != three.Columns[1].ID {
		t.Fatalf("three columns: %v, want %s", got, three.Columns[1].ID)
	}
	b, _ := s.Create(ctx, "cc1", two.Columns[0].ID, "task", "Two columns", "")
	if got := claim(b.ID); got != nil {
		t.Fatalf("To do and Done only: %v, want null (not Done)", *got)
	}
	var backlog string
	_ = db.QueryRow(ctx, `INSERT INTO tickets (project_id, created_by, title, position) VALUES ($1, 'x', 'Backlog', 0) RETURNING id`, pid).Scan(&backlog)
	if got := claim(backlog); got != nil {
		t.Fatalf("backlog: %v, want null", *got)
	}
}
