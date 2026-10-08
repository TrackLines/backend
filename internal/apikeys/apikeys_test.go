package apikeys

import (
	"context"
	"errors"
	"net/http"
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

// Needs a throwaway db: TEST_DATABASE_URL=postgres://... go test ./internal/apikeys/
func TestKeys(t *testing.T) {
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
	if _, err := db.Exec(ctx, `DELETE FROM users WHERE clerk_id IN ('k1', 'k2');
		DELETE FROM roadmaps WHERE owner_clerk_id IN ('k1', 'k2'); DELETE FROM boards WHERE owner_clerk_id IN ('k1', 'k2'); DELETE FROM projects WHERE owner_clerk_id IN ('k1', 'k2'); -- org-owned rows no longer cascade from users
		INSERT INTO users (clerk_id, email) VALUES ('k1', 'a@b.c'), ('k2', 'd@e.f')`); err != nil {
		t.Fatal(err)
	}
	s := Store{DB: db}

	plain, k, err := s.Create(ctx, "k1", "o1", "codex", KindAI)
	if err != nil || !strings.HasPrefix(plain, Prefix) || len(plain) < 40 || k.Prefix != plain[:10] || k.Kind != KindAI {
		t.Fatalf("create: %q %+v %v", plain, k, err)
	}
	var stored int
	_ = db.QueryRow(ctx, `SELECT count(*) FROM api_keys WHERE prefix = $1 OR name = $1 OR encode(hash, 'escape') = $1`, plain).Scan(&stored)
	if stored != 0 {
		t.Fatal("plaintext key found in the database")
	}

	// a handler behind the same chain the service uses
	var seen string
	var viaKey bool
	h := Middleware(db)(auth.Required(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, _ = auth.UserID(r.Context())
		viaKey = auth.ViaAPIKey(r.Context())
	})))
	call := func(handler http.Handler, authz string) int {
		req := httptest.NewRequest("GET", "/", nil)
		if authz != "" {
			req.Header.Set("Authorization", authz)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code
	}

	if code := call(h, "Bearer "+plain); code != 200 || seen != "k1" || !viaKey {
		t.Fatalf("valid key: %d user=%q viaKey=%v", code, seen, viaKey)
	}
	var lastUsed *string
	_ = db.QueryRow(ctx, `SELECT last_used_at::text FROM api_keys WHERE id = $1`, k.ID).Scan(&lastUsed)
	if lastUsed == nil {
		t.Fatal("last_used_at not stamped")
	}
	if code := call(h, "Bearer tl_not-a-real-key"); code != http.StatusUnauthorized {
		t.Fatalf("bad key: %d", code)
	}
	if code := call(h, "Bearer some.clerk.jwt"); code != http.StatusUnauthorized {
		t.Fatalf("non-key bearer should fall through to Clerk (invalid → 401): %d", code)
	}
	if code := call(Middleware(db)(auth.SessionRequired(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))), "Bearer "+plain); code != http.StatusForbidden {
		t.Fatalf("key managing keys: %d, want 403", code)
	}

	if list, _ := s.List(ctx, "k1", "o1"); len(list) != 1 || list[0].Name != "codex" || list[0].Kind != KindAI {
		t.Fatalf("list: %+v", list)
	}
	if err := s.Revoke(ctx, k.ID, "k2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("non-owner revoke: %v", err)
	}
	if err := s.Revoke(ctx, k.ID, "k1"); err != nil {
		t.Fatal(err)
	}
	if code := call(h, "Bearer "+plain); code != http.StatusUnauthorized {
		t.Fatalf("revoked key: %d", code)
	}
	if list, _ := s.List(ctx, "k1", "o1"); len(list) != 0 {
		t.Fatalf("revoked key still listed: %+v", list)
	}
}
