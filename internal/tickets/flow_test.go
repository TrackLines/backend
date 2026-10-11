package tickets

import (
	"context"
	"errors"
	"math"
	"os"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/boards"
)

func TestBoardFlow(t *testing.T) {
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
	owner := "flow-test"
	if _, err := db.Exec(ctx, `INSERT INTO users (clerk_id, email) VALUES ($1, 'flow-test@example.test') ON CONFLICT (clerk_id) DO NOTHING`, owner); err != nil {
		t.Fatal(err)
	}
	var projectID string
	if err := db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ($1, 'flow test') RETURNING id`, owner).Scan(&projectID); err != nil {
		t.Fatal(err)
	}
	defer db.Exec(ctx, `DELETE FROM projects WHERE id = $1`, projectID)
	boardStore := boards.Store{DB: db}
	b, err := boardStore.Create(ctx, owner, projectID, "flow", "")
	if err != nil {
		t.Fatal(err)
	}
	todo, doing, done := b.Columns[0].ID, b.Columns[1].ID, b.Columns[2].ID
	ticketStore := Store{DB: db}
	flowStore := boards.Store{DB: db}

	// Reordering in the first column doesn't start cycle time. The first move out does,
	// and subsequent moves, including a return to the first column, keep that timestamp.
	active, err := ticketStore.Create(ctx, owner, todo, "task", "active", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := ticketStore.Move(ctx, owner, active.ID, todo, 0); err != nil {
		t.Fatal(err)
	}
	var started *string
	if err := db.QueryRow(ctx, `SELECT started_at::text FROM tickets WHERE id = $1`, active.ID).Scan(&started); err != nil || started != nil {
		t.Fatalf("reordering first column should not start cycle time: %v, %v", started, err)
	}
	if err := ticketStore.Move(ctx, owner, active.ID, doing, 0); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `SELECT to_char(started_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"') FROM tickets WHERE id = $1`, active.ID).Scan(&started); err != nil || started == nil {
		t.Fatalf("leaving first column should set started_at: %v, %v", started, err)
	}
	firstStart := *started
	if err := ticketStore.Move(ctx, owner, active.ID, todo, 0); err != nil {
		t.Fatal(err)
	}
	if err := ticketStore.Move(ctx, owner, active.ID, doing, 0); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `SELECT to_char(started_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"') FROM tickets WHERE id = $1`, active.ID).Scan(&started); err != nil || started == nil || *started != firstStart {
		t.Fatalf("later moves should retain started_at: %v, want %v (err %v)", started, firstStart, err)
	}

	// Use stable durations and a recent completion timestamp to cover percentiles and throughput.
	for _, days := range []int{2, 3, 5} {
		ticket, err := ticketStore.Create(ctx, owner, todo, "task", "finished", "")
		if err != nil {
			t.Fatal(err)
		}
		if err := ticketStore.Move(ctx, owner, ticket.ID, doing, 0); err != nil {
			t.Fatal(err)
		}
		if err := ticketStore.Move(ctx, owner, ticket.ID, done, 0); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(ctx, `UPDATE tickets SET started_at = now() - make_interval(days => $2) - interval '12 hours', done_at = now() - interval '12 hours' WHERE id = $1`, ticket.ID, days); err != nil {
			t.Fatal(err)
		}
	}
	legacy, err := ticketStore.Create(ctx, owner, todo, "task", "legacy", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `UPDATE tickets SET done_at = now() - interval '6 hours' WHERE id = $1`, legacy.ID); err != nil {
		t.Fatal(err)
	}

	flow, err := flowStore.Flow(ctx, owner, b.ID, 12)
	if err != nil {
		t.Fatal(err)
	}
	if len(flow.Tickets) != 4 || flow.MedianCycleDays == nil || flow.P85CycleDays == nil {
		t.Fatalf("flow ticket/percentile data: %+v", flow)
	}
	if math.Abs(*flow.MedianCycleDays-3) > 0.01 || math.Abs(*flow.P85CycleDays-4.4) > 0.01 {
		t.Fatalf("cycle-time percentiles: median %v p85 %v", *flow.MedianCycleDays, *flow.P85CycleDays)
	}
	var legacyFound bool
	for _, ticket := range flow.Tickets {
		if ticket.ID == legacy.ID {
			legacyFound = true
			if ticket.StartedAt != nil || ticket.CycleDays != nil {
				t.Fatalf("legacy ticket should have unknown cycle time: %+v", ticket)
			}
		}
	}
	if !legacyFound || len(flow.Throughput) != 12 || flow.Throughput[11].Tickets != 4 {
		t.Fatalf("legacy ticket/weekly throughput missing: %+v", flow)
	}
	if _, err := flowStore.Flow(ctx, "not-owner", b.ID, 12); !errors.Is(err, boards.ErrNotFound) {
		t.Fatalf("another org's board should be hidden: %v", err)
	}
}
