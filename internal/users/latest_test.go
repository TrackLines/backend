package users

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
)

func TestLatestTicketsSetting(t *testing.T) {
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
	s := Store{DB: db}
	for _, id := range []string{"latest_a", "latest_b"} {
		if err := s.Upsert(ctx, id, id+"@x"); err != nil {
			t.Fatal(err)
		}
	}
	call := func(method, user, body string) (int, string) {
		req := httptest.NewRequest(method, "/api/me/latest-tickets", strings.NewReader(body)).WithContext(auth.WithAPIKeyUser(ctx, user, "ui"))
		rec := httptest.NewRecorder()
		if method == "GET" {
			s.GetLatestTickets(rec, req)
		} else {
			s.PutLatestTickets(rec, req)
		}
		return rec.Code, strings.TrimSpace(rec.Body.String())
	}
	if code, body := call("GET", "latest_a", ""); code != 200 || body != `{"limit":0,"per_project":3}` {
		t.Fatalf("default: %d %s", code, body)
	}
	if code, body := call("GET", "latest_nobody", ""); code != 200 || body != `{"limit":0,"per_project":3}` {
		t.Fatalf("no row: %d %s", code, body)
	}
	if code, _ := call("PUT", "latest_a", `{"limit":12,"per_project":2}`); code != 204 {
		t.Fatalf("set: %d", code)
	}
	if _, body := call("GET", "latest_a", ""); body != `{"limit":12,"per_project":2}` {
		t.Fatalf("after set: %s", body)
	}
	if _, body := call("GET", "latest_b", ""); body != `{"limit":0,"per_project":3}` { // only the caller's own
		t.Fatalf("another user changed: %s", body)
	}
	for _, bad := range []string{`{"limit":21,"per_project":3}`, `{"limit":-1,"per_project":3}`, `{"limit":5,"per_project":0}`, `{"limit":5,"per_project":21}`} {
		if code, _ := call("PUT", "latest_a", bad); code != 400 {
			t.Fatalf("%s: %d", bad, code)
		}
	}
}
