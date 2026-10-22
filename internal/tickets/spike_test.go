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

func TestValidTypeSpike(t *testing.T) {
	if !ValidType("spike") {
		t.Fatal("spike should be a valid ticket type")
	}
}

// Spikes (TL-502): a workable type that lives on a board and carries a days+hours timebox, set via
// PATCH. The timebox round-trips, clears on 0/0, validates its ranges, and is left alone when omitted.
func TestSpikeTimebox(t *testing.T) {
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
	const org = "org_spike"
	if _, err := db.Exec(ctx, `DELETE FROM boards WHERE owner_clerk_id = 'org_spike'; DELETE FROM projects WHERE owner_clerk_id = 'org_spike'`); err != nil {
		t.Fatal(err)
	}
	var pid string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ($1, 'Spikes') RETURNING id`, org).Scan(&pid)
	b, _ := boards.Store{DB: db}.Create(ctx, org, pid, "Board", "")
	s := Store{DB: db}

	sp, err := s.Create(ctx, org, b.Columns[0].ID, "spike", "look into 1Password", "")
	if err != nil {
		t.Fatal("create spike on a board:", err)
	}
	if sp.Type != "spike" {
		t.Fatalf("type = %q, want spike", sp.Type)
	}
	if sp.TimeboxDays != nil || sp.TimeboxHours != nil {
		t.Fatal("a fresh spike has no timebox")
	}

	// set, then read it back everywhere (Get reads via the tickets view + shared columns)
	if err := s.SetTimebox(ctx, org, sp.ID, 1, 4); err != nil {
		t.Fatal("set timebox:", err)
	}
	d, _ := s.Get(ctx, org, sp.ID)
	if d.TimeboxDays == nil || *d.TimeboxDays != 1 || d.TimeboxHours == nil || *d.TimeboxHours != 4 {
		t.Fatalf("timebox = %v/%v, want 1/4", d.TimeboxDays, d.TimeboxHours)
	}

	// 0/0 clears it back to null
	if err := s.SetTimebox(ctx, org, sp.ID, 0, 0); err != nil {
		t.Fatal("clear timebox:", err)
	}
	d, _ = s.Get(ctx, org, sp.ID)
	if d.TimeboxDays != nil || d.TimeboxHours != nil {
		t.Fatal("0/0 should clear the timebox")
	}

	// org scoping: another org can't touch it
	if err := s.SetTimebox(ctx, "org_other", sp.ID, 1, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-org set: %v, want ErrNotFound", err)
	}

	// over HTTP: bad hours → 400, good → 204 and stored, omitted → unchanged
	h := NewSystem(db)
	c := auth.WithAPIKey(ctx, "user_s", org, "ui", "")
	update := func(body string) int {
		req := httptest.NewRequest("PATCH", "/", strings.NewReader(body)).WithContext(c)
		req.SetPathValue("id", sp.ID)
		rec := httptest.NewRecorder()
		h.Update(rec, req)
		return rec.Code
	}
	if code := update(`{"title":"look into 1Password","timebox_hours":99}`); code != 400 {
		t.Fatalf("hours 99 → %d, want 400", code)
	}
	if code := update(`{"title":"look into 1Password","timebox_days":2,"timebox_hours":3}`); code != 204 {
		t.Fatalf("valid timebox → %d, want 204", code)
	}
	d, _ = s.Get(ctx, org, sp.ID)
	if d.TimeboxDays == nil || *d.TimeboxDays != 2 || d.TimeboxHours == nil || *d.TimeboxHours != 3 {
		t.Fatalf("after update timebox = %v/%v, want 2/3", d.TimeboxDays, d.TimeboxHours)
	}
	if code := update(`{"title":"renamed"}`); code != 204 {
		t.Fatalf("rename → %d, want 204", code)
	}
	if d, _ = s.Get(ctx, org, sp.ID); d.TimeboxDays == nil || *d.TimeboxDays != 2 {
		t.Fatal("omitting timebox must leave it unchanged")
	}
}
