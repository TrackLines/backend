package tickets

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
)

func TestAwayStatus(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	day := func(d int) time.Time { return time.Date(2026, 10, d, 0, 0, 0, 0, time.UTC) }
	// away now; back-to-back leave joins up, a gap doesn't
	until, next := awayStatus([]awaySpan{{day(19), day(22)}, {day(22), day(24)}, {day(27), day(28)}}, now)
	if until == nil || !until.Equal(day(24)) || next != nil {
		t.Errorf("away now: %v %v", until, next)
	}
	// not yet: the next leave, joined up
	until, next = awayStatus([]awaySpan{{day(23), day(24)}, {day(24), day(26)}}, now)
	if until != nil || next == nil || !next[0].Equal(day(23)) || !next[1].Equal(day(26)) {
		t.Errorf("coming up: %v %v", until, next)
	}
	// back already: nothing
	if until, next = awayStatus([]awaySpan{{day(18), now}}, now); until != nil || next != nil {
		t.Errorf("back at now: %v %v", until, next)
	}
}

// TL-407: the assignee list says who's on holiday (from Google Calendar's time away); agents never.
func TestAssigneesSayWhosAway(t *testing.T) {
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
	if _, err := db.Exec(ctx, `DELETE FROM calendar_connections WHERE org_id IN ('aw1', 'aw2');
		INSERT INTO calendar_connections (org_id, domain, admin_email, connected_by) VALUES ('aw1', 'x.test', 'a@x.test', 't'), ('aw2', 'y.test', 'a@y.test', 't');
		INSERT INTO time_away (org_id, user_id, starts_at, ends_at, source_id) VALUES
			('aw1', 'user_ava', now() - interval '1 day', now() + interval '2 days', 'a'),
			('aw1', 'user_ben', now() + interval '3 days', now() + interval '4 days', 'b'),
			('aw2', 'user_cat', now() - interval '1 hour', now() + interval '1 hour', 'c')`); err != nil {
		t.Fatal(err)
	}
	h := System{store: Store{DB: db}, memberships: testAssigneeMembers{"user_ava", "user_ben", "user_cat"}}
	list := func(org string) map[string]Assignee {
		req := httptest.NewRequest("GET", "/api/assignees", nil)
		req = req.WithContext(auth.WithAPIKey(req.Context(), "user_ben", org, "codex", "ai"))
		rec := httptest.NewRecorder()
		h.Assignees(rec, req)
		var got []Assignee
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
		out := map[string]Assignee{}
		for _, a := range got {
			out[a.ID] = a
		}
		return out
	}

	got := list("aw1")
	if a := got["user_ava"]; a.Away == nil || a.Away.Company || a.AwayNext != nil {
		t.Errorf("Ava is away: %+v %+v", a.Away, a.AwayNext)
	} else if until, _ := time.Parse(time.RFC3339, a.Away.Until); until.Before(time.Now().Add(47 * time.Hour)) {
		t.Errorf("Ava back: %s", a.Away.Until)
	}
	if b := got["user_ben"]; b.Away != nil || b.AwayNext == nil {
		t.Errorf("Ben goes in 3 days: %+v %+v", b.Away, b.AwayNext)
	}
	if c := got["user_cat"]; c.Away != nil || c.AwayNext != nil {
		t.Errorf("Cat's leave is another org's: %+v", c.Away)
	}

	// a company day off: everyone (people, not agents) is away, as a company day
	db.Exec(ctx, `INSERT INTO time_away (org_id, user_id, starts_at, ends_at, all_day, source_id) VALUES ('aw1', NULL, now() - interval '1 hour', now() + interval '5 hours', true, 'xmas')`)
	got = list("aw1")
	if b := got["user_ben"]; b.Away == nil || !b.Away.Company {
		t.Errorf("Ben on a company day off: %+v", b.Away)
	}
	if a := got["user_ava"]; a.Away == nil || a.Away.Company {
		t.Errorf("Ava's own leave isn't a company day: %+v", a.Away)
	}
	if c := got["codex"]; c.Away != nil {
		t.Error("an agent marked away")
	}

}
