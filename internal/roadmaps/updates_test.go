package roadmaps

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
)

func TestValidUpdate(t *testing.T) {
	for _, tc := range []struct {
		health, note string
		valid        bool
	}{
		{HealthOnTrack, "Progress is steady", true},
		{HealthAtRisk, "Blocked on review", true},
		{HealthOffTrack, "Schedule has slipped", true},
		{"green", "Progress", false},
		{HealthOnTrack, " \n ", false},
		{HealthOnTrack, strings.Repeat("a", MaxUpdateNoteRunes+1), false},
	} {
		if got := ValidUpdate(tc.health, tc.note); got != tc.valid {
			t.Errorf("ValidUpdate(%q, %d chars) = %v, want %v", tc.health, utf8.RuneCountInString(tc.note), got, tc.valid)
		}
	}
}

func TestRoadmapUpdatesPermissionsHistoryAndPrivacy(t *testing.T) {
	ctx := context.Background()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Fatal("TEST_DATABASE_URL not set; package TestMain should provision PostgreSQL")
	}
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
	if _, err := db.Exec(ctx, `DELETE FROM roadmaps WHERE owner_clerk_id IN ('ru1', 'ru2');
		DELETE FROM boards WHERE owner_clerk_id IN ('ru1', 'ru2');
		DELETE FROM projects WHERE owner_clerk_id IN ('ru1', 'ru2');
		INSERT INTO users (clerk_id, email) VALUES ('ru1', 'ru1@example.test'), ('ru2', 'ru2@example.test') ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	var projectID string
	if err := db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('ru1', 'Roadmap updates test') RETURNING id`).Scan(&projectID); err != nil {
		t.Fatal(err)
	}
	store := Store{DB: db}
	public, err := store.Create(ctx, "ru1", projectID, "Public", "private roadmap description", Public)
	if err != nil {
		t.Fatal(err)
	}
	loginOnly, err := store.Create(ctx, "ru1", projectID, "Login only", "", LoginOnly)
	if err != nil {
		t.Fatal(err)
	}
	team, err := store.Create(ctx, "ru1", projectID, "Team", "", Team)
	if err != nil {
		t.Fatal(err)
	}
	var boardID, columnID, itemID, ticketID string
	if err := db.QueryRow(ctx, `INSERT INTO boards (project_id, owner_clerk_id, name) VALUES ($1, 'ru1', 'Updates test') RETURNING id`, projectID).Scan(&boardID); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `INSERT INTO columns (board_id, name, position) VALUES ($1, 'To do', 0) RETURNING id`, boardID).Scan(&columnID); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `INSERT INTO tickets (project_id, board_id, column_id, title, position) VALUES ($1, $2, $3, 'PRIVATE LINKED TICKET TITLE', 0) RETURNING id`, projectID, boardID, columnID).Scan(&ticketID); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `INSERT INTO roadmap_items (roadmap_id, title, position) VALUES ($1, 'Feature', 0) RETURNING id`, public.ID).Scan(&itemID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO roadmap_item_tickets (item_id, ticket_id) VALUES ($1, $2)`, itemID, ticketID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateUpdate(ctx, public.ID, "ru2", HealthOnTrack, "not yours"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("non-owner create: %v", err)
	}
	first, err := store.CreateUpdate(ctx, public.ID, "ru1", HealthAtRisk, "Waiting on a design review")
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.CreateUpdate(ctx, public.ID, "ru1", HealthOnTrack, "Review is complete")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `UPDATE roadmap_updates SET published_at = $2 WHERE id = $1`, first.ID, time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `UPDATE roadmap_updates SET published_at = $2 WHERE id = $1`, second.ID, time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	ordered, err := store.ListUpdates(ctx, public.ID)
	if err != nil || len(ordered) != 2 || ordered[0].ID != first.ID || ordered[1].ID != second.ID {
		t.Fatalf("chronological update history: %+v, %v", ordered, err)
	}

	h := NewSystem(db)
	request := func(method, roadmapID, body, org string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/api/roadmaps/"+roadmapID+"/updates", strings.NewReader(body))
		if org != "" {
			req = req.WithContext(auth.WithAPIKeyUser(ctx, org, "roadmap-test-agent"))
		}
		req.SetPathValue("id", roadmapID)
		rec := httptest.NewRecorder()
		if method == http.MethodPost {
			h.PublishUpdate(rec, req)
		} else {
			h.ListUpdates(rec, req)
		}
		return rec
	}

	if rec := request(http.MethodPost, public.ID, `{"health":"off_track","note":"On-call review is taking longer"}`, "ru1"); rec.Code != http.StatusCreated {
		t.Fatalf("owner publish: %d %s", rec.Code, rec.Body)
	}
	if rec := request(http.MethodPost, public.ID, `{"health":"on_track","note":"intrusion"}`, "ru2"); rec.Code != http.StatusNotFound {
		t.Fatalf("non-owner publish: %d %s", rec.Code, rec.Body)
	}
	if rec := request(http.MethodPost, public.ID, `{"health":"green","note":"bad state"}`, "ru1"); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid publish: %d %s", rec.Code, rec.Body)
	}

	if rec := request(http.MethodGet, public.ID, "", ""); rec.Code != http.StatusOK {
		t.Fatalf("public anonymous read: %d %s", rec.Code, rec.Body)
	} else {
		var got []Update
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || len(got) != 3 {
			t.Fatalf("public history: %s %v", rec.Body, err)
		}
		for _, forbidden := range []string{"project_id", "owner_clerk_id", "private roadmap description", "PRIVATE LINKED TICKET TITLE", ticketID} {
			if strings.Contains(rec.Body.String(), forbidden) {
				t.Errorf("public update response leaked %q: %s", forbidden, rec.Body)
			}
		}
	}
	for _, tc := range []struct {
		roadmapID, org string
		want           int
	}{
		{loginOnly.ID, "", http.StatusUnauthorized},
		{loginOnly.ID, "ru2", http.StatusOK},
		{team.ID, "", http.StatusUnauthorized},
		{team.ID, "ru2", http.StatusForbidden},
		{team.ID, "ru1", http.StatusOK},
	} {
		if rec := request(http.MethodGet, tc.roadmapID, "", tc.org); rec.Code != tc.want {
			t.Errorf("read %s as %q: got %d, want %d (%s)", tc.roadmapID, tc.org, rec.Code, tc.want, rec.Body)
		}
	}
}
