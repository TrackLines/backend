package attachments

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestValidUploadThingURL(t *testing.T) {
	for u, want := range map[string]bool{
		"https://abc123.ufs.sh/f/key":     true,
		"https://utfs.io/f/key":           true,
		"http://abc123.ufs.sh/f/key":      false, // not https
		"https://ufs.sh.evil.com/f/key":   false, // look-alike
		"https://evil.com/abc.ufs.sh/key": false,
		"javascript:alert(1)":             false,
	} {
		if got := ValidUploadThingURL(u); got != want {
			t.Errorf("%s: got %v want %v", u, got, want)
		}
	}
}

func TestDeleteFiles(t *testing.T) {
	var gotKey string
	var gotBody map[string][]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v6/deleteFiles" {
			t.Errorf("path %s", r.URL.Path)
		}
		gotKey = r.Header.Get("x-uploadthing-api-key")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	defer srv.Close()
	token := base64.StdEncoding.EncodeToString([]byte(`{"apiKey":"sk_live_x","appId":"app","regions":["sea1"]}`))
	if err := (UploadThing{Token: token, BaseURL: srv.URL}).DeleteFiles(context.Background(), "k1"); err != nil {
		t.Fatal(err)
	}
	if gotKey != "sk_live_x" || len(gotBody["fileKeys"]) != 1 || gotBody["fileKeys"][0] != "k1" {
		t.Fatalf("request: key=%q body=%v", gotKey, gotBody)
	}
	if err := (UploadThing{Token: "not-base64!"}).DeleteFiles(context.Background(), "k"); err == nil {
		t.Fatal("bad token should error")
	}
}

// Needs a throwaway db: TEST_DATABASE_URL=postgres://... go test ./internal/attachments/
func TestStore(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
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
	if _, err := db.Exec(ctx, `DELETE FROM users WHERE clerk_id IN ('a1', 'a2');
		DELETE FROM roadmaps WHERE owner_clerk_id IN ('a1', 'a2'); DELETE FROM boards WHERE owner_clerk_id IN ('a1', 'a2'); DELETE FROM projects WHERE owner_clerk_id IN ('a1', 'a2'); -- org-owned rows no longer cascade from users
		INSERT INTO users (clerk_id, email) VALUES ('a1', 'a@b.c'), ('a2', 'd@e.f')`); err != nil {
		t.Fatal(err)
	}
	var ticket string
	if err := db.QueryRow(ctx, `WITH p AS (INSERT INTO projects (owner_clerk_id, name) VALUES ('a1', 'p') RETURNING id)
		INSERT INTO tickets (project_id, title, position) SELECT id, 'backlog t', 0 FROM p RETURNING id`).Scan(&ticket); err != nil {
		t.Fatal(err)
	}
	s := Store{DB: db}
	good := Attachment{Key: "key-1", URL: "https://app.ufs.sh/f/key-1", Name: "log.txt", Size: 42, ContentType: "text/plain"}

	if _, err := s.Create(ctx, "a1", "claude", ticket, Attachment{Key: "x", URL: "https://evil.com/x", Name: "x"}); !errors.Is(err, ErrBadFile) {
		t.Fatalf("bad url: %v", err)
	}
	if _, err := s.Create(ctx, "a2", "x", ticket, good); !errors.Is(err, ErrNotFound) {
		t.Fatalf("non-owner create: %v", err)
	}
	a, err := s.Create(ctx, "a1", "claude", ticket, good)
	if err != nil || a.Name != "log.txt" || a.CreatedBy != "claude" || a.Size != 42 {
		t.Fatalf("create: %+v %v", a, err)
	}
	if _, err := s.Create(ctx, "a1", "claude", ticket, good); !errors.Is(err, ErrNotFound) {
		t.Fatalf("duplicate key should not create twice: %v", err)
	}
	if list, _ := s.List(ctx, "a1", ticket); len(list) != 1 {
		t.Fatalf("list: %+v", list)
	}
	if _, err := s.List(ctx, "a2", ticket); !errors.Is(err, ErrNotFound) {
		t.Fatalf("non-owner list: %v", err)
	}
	if _, err := s.Delete(ctx, "a2", a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("non-owner delete: %v", err)
	}
	if key, err := s.Delete(ctx, "a1", a.ID); err != nil || key != "key-1" {
		t.Fatalf("delete: %q %v", key, err)
	}
	if list, _ := s.List(ctx, "a1", ticket); len(list) != 0 {
		t.Fatalf("after delete: %+v", list)
	}
}

// Needs a throwaway db: TEST_DATABASE_URL=postgres://... go test ./internal/attachments/
func TestDoneLock(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
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
	if _, err := db.Exec(ctx, `DELETE FROM users WHERE clerk_id = 'd1'; DELETE FROM projects WHERE owner_clerk_id = 'd1'; DELETE FROM boards WHERE owner_clerk_id = 'd1'; INSERT INTO users (clerk_id, email) VALUES ('d1', 'a@b.c')`); err != nil {
		t.Fatal(err)
	}
	var pid, bid, todo, done, ticket string
	if err := db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('d1', 'p') RETURNING id`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `INSERT INTO boards (project_id, owner_clerk_id, name) VALUES ($1, 'd1', 'b') RETURNING id`, pid).Scan(&bid); err != nil {
		t.Fatal(err)
	}
	_ = db.QueryRow(ctx, `INSERT INTO columns (board_id, name, position) VALUES ($1, 'To do', 0) RETURNING id`, bid).Scan(&todo)
	_ = db.QueryRow(ctx, `INSERT INTO columns (board_id, name, position) VALUES ($1, 'Done', 1) RETURNING id`, bid).Scan(&done)
	if err := db.QueryRow(ctx, `INSERT INTO tickets (project_id, board_id, column_id, title, position) VALUES ($1, $2, $3, 't', 0) RETURNING id`, pid, bid, todo).Scan(&ticket); err != nil {
		t.Fatal(err)
	}
	s := Store{DB: db}
	file := func(k string) Attachment { return Attachment{Key: k, URL: "https://x.ufs.sh/f/" + k, Name: k} }
	move := func(col string) {
		if _, err := db.Exec(ctx, `UPDATE tickets SET column_id = $2 WHERE id = $1`, ticket, col); err != nil {
			t.Fatal(err)
		}
	}

	a, err := s.Create(ctx, "d1", "x", ticket, file("dl-1"))
	if err != nil {
		t.Fatalf("add while open: %v", err)
	}
	move(done)
	if _, err := s.Create(ctx, "d1", "x", ticket, file("dl-2")); !errors.Is(err, ErrDone) {
		t.Fatalf("add on done ticket: %v", err)
	}
	if _, err := s.Delete(ctx, "d1", a.ID); !errors.Is(err, ErrDone) {
		t.Fatalf("remove on done ticket: %v", err)
	}
	move(todo)
	if _, err := s.Create(ctx, "d1", "x", ticket, file("dl-2")); err != nil {
		t.Fatalf("add after moving out of Done: %v", err)
	}
	if _, err := s.Delete(ctx, "d1", a.ID); err != nil {
		t.Fatalf("remove after moving out of Done: %v", err)
	}
}
