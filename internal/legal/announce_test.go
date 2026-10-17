package legal

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TL-424: the first time the web app lists a new version of the Terms or Privacy policy, every user
// gets one in-app notice; the versions there at launch are recorded quietly.
func TestAnnounce(t *testing.T) {
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
	if _, err := db.Exec(ctx, `DELETE FROM legal_announcements; DELETE FROM notifications WHERE kind = 'legal';
		INSERT INTO users (clerk_id, email) VALUES ('lg_ava', 'ava@x.y'), ('lg_bo', 'bo@x.y') ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	list := []Version{{Doc: "terms", Version: "2026-10-17", Summary: "First published."}, {Doc: "privacy", Version: "2026-10-17", Summary: "First published."}}
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/legal-versions.json" {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		json.NewEncoder(w).Encode(list)
	}))
	defer site.Close()
	a := Announcer{DB: db, Site: site.URL, Client: site.Client()}
	notices := func(user string) []string {
		rows, _ := db.Query(ctx, `SELECT org_id || '|' || text || '|' || COALESCE(link, '') FROM notifications WHERE kind = 'legal' AND user_id = $1 ORDER BY created_at`, user)
		defer rows.Close()
		var out []string
		for rows.Next() {
			var s string
			rows.Scan(&s)
			out = append(out, s)
		}
		return out
	}

	// launch: recorded, nobody notified
	if err := a.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if got := notices("lg_ava"); len(got) != 0 {
		t.Fatalf("launch versions announced: %v", got)
	}
	// a new Terms version: everyone, once, in no particular organization, linking to it
	mu.Lock()
	list = append(list, Version{Doc: "terms", Version: "2026-11-20", Summary: "Downtime credit now counts several short outages."}, Version{Doc: "cookies", Version: "2026-11-20"})
	mu.Unlock()
	for i := 0; i < 2; i++ { // the second check finds nothing new
		if err := a.Check(ctx); err != nil {
			t.Fatal(err)
		}
	}
	want := "|We've updated our Terms of service (from 2026-11-20): Downtime credit now counts several short outages.|/terms/2026-11-20"
	for _, u := range []string{"lg_ava", "lg_bo"} {
		if got := notices(u); len(got) != 1 || got[0] != want {
			t.Errorf("%s: %v", u, got)
		}
	}
	// the site unreachable: an error, nothing changed
	if err := (Announcer{DB: db, Site: "http://127.0.0.1:1"}).Check(ctx); err == nil {
		t.Error("an unreachable site wasn't an error")
	}
}
