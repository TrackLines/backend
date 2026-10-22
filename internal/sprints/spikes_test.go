package sprints

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/boards"
	"github.com/tracklines/backend/internal/httpx"
	"github.com/tracklines/backend/internal/tickets"
)

func TestBusinessMinutes(t *testing.T) {
	at := func(s string) time.Time { v, _ := time.Parse(time.RFC3339, s); return v }
	for _, c := range []struct {
		from, to string
		want     float64
	}{
		{"2026-10-05T09:00:00Z", "2026-10-05T15:00:00Z", 6 * 60},  // Monday, within the day
		{"2026-10-05T07:00:00Z", "2026-10-05T20:00:00Z", 8 * 60},  // clamped to 09–17
		{"2026-10-02T16:00:00Z", "2026-10-05T12:00:00Z", 4 * 60},  // Friday 1h + Monday 3h; the weekend skipped
		{"2026-10-03T10:00:00Z", "2026-10-04T18:00:00Z", 0},       // all weekend
		{"2026-10-05T12:00:00Z", "2026-10-05T11:00:00Z", 0},       // backwards
		{"2026-10-05T09:00:00Z", "2026-10-07T17:00:00Z", 24 * 60}, // three whole days
	} {
		if got := businessMinutes(at(c.from), at(c.to)); got != c.want {
			t.Errorf("%s → %s: %v minutes, want %v", c.from, c.to, got, c.want)
		}
	}
	// the working day is the caller's: 13:00–21:00 UTC is 09:00–17:00 in New York (EDT)
	ny, _ := time.LoadLocation("America/New_York")
	if got := businessMinutes(at("2026-10-05T13:00:00Z").In(ny), at("2026-10-05T21:00:00Z").In(ny)); got != 8*60 {
		t.Errorf("New York day: %v minutes", got)
	}
}

// TL-508: the board report counts timeboxed spikes finished in the period, within or over their timebox.
func TestReportSpikeOutcomes(t *testing.T) {
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
	db.Exec(ctx, `DELETE FROM boards WHERE owner_clerk_id = 'sk1'; DELETE FROM projects WHERE owner_clerk_id = 'sk1'`)
	var pid string
	if err := db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('sk1', 'p') RETURNING id`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	bs, ts, s := boards.Store{DB: db}, tickets.Store{DB: db}, Store{DB: db}
	b, _ := bs.Create(ctx, "sk1", pid, "Research", "")
	todo, done := b.Columns[0].ID, b.Columns[len(b.Columns)-1].ID
	spike := func(kind string, started, finished string, days, hours *int) {
		t.Helper()
		tk, err := ts.Create(ctx, "sk1", todo, kind, "s", "")
		if err != nil {
			t.Fatal(err)
		}
		if err := ts.Move(ctx, "sk1", tk.ID, done, 0); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(ctx, `UPDATE tickets SET started_at = $2, done_at = $3, timebox_days = $4, timebox_hours = $5 WHERE id = $1`,
			tk.ID, started, finished, days, hours); err != nil {
			t.Fatal(err)
		}
	}
	one, two, zero := 1, 2, 0
	spike("spike", "2026-10-05T09:00:00Z", "2026-10-05T15:00:00Z", &one, &zero) // 6h of a day's 8h: within
	spike("spike", "2026-10-02T16:00:00Z", "2026-10-05T12:00:00Z", &zero, &two) // 4h of 2h: over by 2h
	spike("spike", "2026-10-05T09:00:00Z", "2026-10-05T10:00:00Z", nil, nil)    // no timebox: left out
	spike("spike", "2026-09-01T09:00:00Z", "2026-09-01T10:00:00Z", &one, &zero) // finished before the period
	spike("task", "2026-10-05T09:00:00Z", "2026-10-05T10:00:00Z", &one, &zero)  // not a spike

	from, to := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)
	rep, err := s.BoardReport(httpx.WithZoneContext(ctx, time.UTC), "sk1", b.ID, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if want := (Spikes{Done: 2, Within: 1, Over: 1, OverHours: 2}); rep.Spikes != want {
		t.Fatalf("spikes %+v, want %+v", rep.Spikes, want)
	}
	// another organization sees nothing of this board
	if _, err := s.BoardReport(ctx, "sk2", b.ID, from, to); err == nil {
		t.Fatal("another org read the report")
	}
}
