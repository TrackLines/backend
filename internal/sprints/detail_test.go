package sprints

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/boards"
	"github.com/tracklines/backend/internal/tickets"
)

func TestSprintDetail(t *testing.T) {
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
	if _, err := db.Exec(ctx, `DELETE FROM boards WHERE owner_clerk_id = 'sd1'; DELETE FROM projects WHERE owner_clerk_id = 'sd1'`); err != nil {
		t.Fatal(err)
	}
	var pid string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('sd1', 'p') RETURNING id`).Scan(&pid)
	bs, ts, s := boards.Store{DB: db}, tickets.Store{DB: db}, Store{DB: db}
	b, _ := bs.Create(ctx, "sd1", pid, "b", "")
	_ = bs.SetEstimateScale(ctx, b.ID, "sd1", "fibonacci")
	sp, _ := s.Start(ctx, "sd1", b.ID, 14)
	add := func(est string) string {
		tk, _ := ts.Create(ctx, "sd1", b.Columns[0].ID, "task", "t"+est, "")
		if est != "" {
			_ = ts.SetEstimate(ctx, "sd1", tk.ID, est)
		}
		return tk.ID
	}
	five := add("5")
	add("3")
	add("")
	_ = ts.Move(ctx, "sd1", five, b.Columns[2].ID, 0)

	// open: scope is what's in it now; nothing carried over yet
	open, err := s.Detail(ctx, "sd1", sp.ID)
	if err != nil || open.Unit != "points" || open.Burn.Total != 8 || open.Burn.Unestimated != 1 || len(open.Tickets) != 3 || open.CarriedOver != nil {
		t.Fatalf("open: %+v %v", open, err)
	}
	if _, err := s.Close(ctx, "sd1", sp.ID); err != nil {
		t.Fatal(err)
	}
	// closed: the burn starts from the recorded scope, the tickets are what it finished
	d, err := s.Detail(ctx, "sd1", sp.ID)
	if err != nil || d.ClosedAt == nil || d.Burn.Total != 8 || len(d.Burn.Done) != 1 || d.Burn.Done[0].Value != 5 ||
		len(d.Tickets) != 1 || d.Tickets[0].ID != five || d.CarriedOver == nil || *d.CarriedOver != 2 || d.Burn.EndsAt != *d.ClosedAt {
		t.Fatalf("closed: %+v %+v %v", d, d.Burn, err)
	}
	// closed before scope was recorded: falls back to what it finished
	_, _ = db.Exec(ctx, `UPDATE sprints SET scope_tickets = NULL, scope_points = NULL WHERE id = $1`, sp.ID)
	if d, _ = s.Detail(ctx, "sd1", sp.ID); d.Burn.Total != 5 || d.CarriedOver != nil {
		t.Fatalf("no recorded scope: total %v carried %v", d.Burn.Total, d.CarriedOver)
	}
	if _, err := s.Detail(ctx, "other", sp.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("other owner:", err)
	}
}
