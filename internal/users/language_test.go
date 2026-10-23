package users

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
)

func TestLanguage(t *testing.T) {
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
	for _, id := range []string{"lang_a", "lang_b"} {
		if err := s.Upsert(ctx, id, id+"@x"); err != nil {
			t.Fatal(err)
		}
	}
	call := func(method, user, body string) (int, string) {
		req := httptest.NewRequest(method, "/api/me/language", strings.NewReader(body)).WithContext(auth.WithAPIKeyUser(ctx, user, "ui"))
		rec := httptest.NewRecorder()
		if method == "GET" {
			s.GetLanguage(rec, req)
		} else {
			s.PutLanguage(rec, req)
		}
		return rec.Code, strings.TrimSpace(rec.Body.String())
	}
	if code, body := call("GET", "lang_a", ""); code != 200 || body != `{"language":null}` {
		t.Fatalf("unset: %d %s", code, body)
	}
	if code, _ := call("PUT", "lang_a", `{"language":"nl"}`); code != 204 {
		t.Fatalf("set nl: %d", code)
	}
	if _, body := call("GET", "lang_a", ""); body != `{"language":"nl"}` {
		t.Fatalf("after set: %s", body)
	}
	if _, body := call("GET", "lang_b", ""); body != `{"language":null}` { // only the caller's own
		t.Fatalf("another user changed: %s", body)
	}
	for _, bad := range []string{`{"language":"fr"}`, `{"language":"NL"}`, `{"language":""}`} {
		if code, _ := call("PUT", "lang_a", bad); code != 400 {
			t.Fatalf("%s: %d", bad, code)
		}
	}
	if code, _ := call("PUT", "lang_a", `{"language":null}`); code != 204 {
		t.Fatal("clear")
	}
	if _, body := call("GET", "lang_a", ""); body != `{"language":null}` {
		t.Fatalf("after clear: %s", body)
	}
}
