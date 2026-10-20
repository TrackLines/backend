package live

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/clerk/clerk-sdk-go/v2"
	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/boards"
)

// The organization's running standups and refinements in one request (TL-466): its own only, not
// archived boards', and not a session whose host has gone (that one ends).
func TestRunningSessions(t *testing.T) {
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
	clock := time.UnixMilli(5_000_000)
	s := System{VK: valkeyForTest(t), Boards: boards.Store{DB: db}, Now: func() time.Time { return clock }}
	var pid, other string
	if err := db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('org_rs', 'Running') RETURNING id`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('org_rs_other', 'Theirs') RETURNING id`).Scan(&other); err != nil {
		t.Fatal(err)
	}
	web, _ := boards.Store{DB: db}.Create(ctx, "org_rs", pid, "Web", "")
	quiet, _ := boards.Store{DB: db}.Create(ctx, "org_rs", pid, "Quiet", "")
	old, _ := boards.Store{DB: db}.Create(ctx, "org_rs", pid, "Old", "")
	theirs, _ := boards.Store{DB: db}.Create(ctx, "org_rs_other", other, "Theirs", "")
	if _, err := db.Exec(ctx, `UPDATE boards SET archived_at = now() WHERE id = $1`, old.ID); err != nil {
		t.Fatal(err)
	}
	list := func(org string) []RunningSession {
		c := &clerk.SessionClaims{RegisteredClaims: clerk.RegisteredClaims{Subject: "sam"}}
		c.ActiveOrganizationID = org
		rec := httptest.NewRecorder()
		s.RunningHandler(rec, httptest.NewRequest("GET", "/", nil).WithContext(clerk.ContextWithSessionClaims(ctx, c)))
		if rec.Code != 200 {
			t.Fatalf("running: %d %s", rec.Code, rec.Body)
		}
		var out []RunningSession
		json.Unmarshal(rec.Body.Bytes(), &out)
		return out
	}

	if got := list("org_rs"); len(got) != 0 {
		t.Fatalf("nothing running: %+v", got)
	}
	for _, x := range []struct {
		sc         scope
		kind, host string
	}{
		{projectScope(refinement, pid), refinement, "ava"},
		{boardScope(standup, web.ID), standup, "bo"},
		{boardScope(standup, old.ID), standup, "cy"},     // archived: not listed
		{boardScope(standup, theirs.ID), standup, "eve"}, // another org's
	} {
		if _, err := s.startHosted(ctx, x.sc, x.kind, x.host); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.heartbeat(ctx, boardScope(standup, web.ID), "sam"); err != nil {
		t.Fatal(err)
	}
	got := list("org_rs")
	if len(got) != 2 {
		t.Fatalf("running: %+v", got)
	}
	ref, st := got[0], got[1] // refinement before standups, within the project
	if ref.Kind != refinement || ref.ID != pid || ref.Name != "Running" || ref.Host != "ava" {
		t.Fatalf("refinement: %+v", ref)
	}
	if st.Kind != standup || st.ID != web.ID || st.Name != "Web" || st.ProjectID != pid || st.ProjectName != "Running" || st.Host != "bo" || len(st.People) != 2 {
		t.Fatalf("standup: %+v", st)
	}
	for _, x := range got {
		if x.ID == quiet.ID || x.ID == old.ID || x.ID == theirs.ID {
			t.Fatalf("listed what isn't running here: %+v", x)
		}
	}
	// an API key (the TUI's personal key) reads it too
	req := httptest.NewRequest("GET", "/", nil).WithContext(auth.WithAPIKey(ctx, "sam", "org_rs", "", "personal"))
	rec := httptest.NewRecorder()
	s.RunningHandler(rec, req)
	if rec.Code != 200 {
		t.Fatalf("api key: %d", rec.Code)
	}
	// their hosts gone over two minutes: the sessions end, so they're not listed
	clock = clock.Add(3 * time.Minute)
	if got := list("org_rs"); len(got) != 0 {
		t.Fatalf("hosts gone, still listed: %+v", got)
	}
	// live sessions off: an empty list, not an error
	rec = httptest.NewRecorder()
	System{Boards: boards.Store{DB: db}}.RunningHandler(rec, httptest.NewRequest("GET", "/", nil).WithContext(req.Context()))
	if rec.Code != 200 || rec.Body.String() != "[]\n" {
		t.Fatalf("no valkey: %d %q", rec.Code, rec.Body)
	}
}
