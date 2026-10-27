package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tracklines/backend/internal/auth"
)

func promptDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	m, err := migrate.New("file://../migrations", "pgx5"+url[len("postgres"):])
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatal(err)
	}
	db, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	return db
}

// TL-674: the pre-prompt is versioned like the terms. Version 1 is exactly the text agents got before;
// staff publish new versions (never empty, never the same text); sessions and the docs get the latest;
// every version stays readable as it was
func TestPromptVersions(t *testing.T) {
	ctx := context.Background()
	db := promptDB(t)
	if _, err := db.Exec(ctx, `DELETE FROM mcp_prompts WHERE version > 1`); err != nil {
		t.Fatal(err)
	}
	s := NewPromptStore(db)
	if got := s.Instructions(ctx); got != Instructions {
		t.Fatalf("version 1 isn't the built-in text:\n%q", got)
	}

	// publishing: refused when empty or unchanged; a new version otherwise, by whoever published it
	for _, body := range []string{"", "   "} {
		if _, err := s.Publish(ctx, "user_staff", body, ""); !errors.Is(err, ErrPromptEmpty) {
			t.Fatalf("empty: %v", err)
		}
	}
	if _, err := s.Publish(ctx, "user_staff", "  "+Instructions+"\n", ""); !errors.Is(err, ErrPromptUnchanged) {
		t.Fatalf("unchanged: %v", err)
	}
	if _, err := s.Publish(ctx, "user_staff", strings.Repeat("x", MaxPrompt+1), ""); !errors.Is(err, ErrPromptTooLong) {
		t.Fatalf("too long: %v", err)
	}
	v2, err := s.Publish(ctx, "user_staff", "Work tickets like a teammate.\n\nRun the tests.", "  shorter,   clearer ")
	if err != nil || v2.Version != 2 || v2.Note != "shorter, clearer" || v2.PublishedBy != "user_staff" {
		t.Fatalf("published: %+v %v", v2, err)
	}
	if got := s.Instructions(ctx); got != "Work tickets like a teammate.\n\nRun the tests." {
		t.Fatalf("a new session gets: %q", got)
	}

	// another instance's cache catches up within its TTL
	other := NewPromptStore(db)
	other.TTL = 20 * time.Millisecond
	_ = other.Instructions(ctx)
	if _, err := s.Publish(ctx, "user_staff", "Version three.", ""); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	if got := other.Instructions(ctx); got != "Version three." {
		t.Fatalf("other instance: %q", got)
	}

	// the public reads: latest with its version; the list without text or who; one version as it was
	get := func(h func(*PromptStore) func(w *httptest.ResponseRecorder), out any) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h(s)(rec)
		if out != nil {
			_ = json.Unmarshal(rec.Body.Bytes(), out)
		}
		return rec
	}
	var latest struct {
		Instructions string `json:"instructions"`
		Version      int    `json:"version"`
		PublishedAt  string `json:"published_at"`
		Tools        []Prompt
	}
	get(func(s *PromptStore) func(*httptest.ResponseRecorder) {
		return func(w *httptest.ResponseRecorder) { s.Prompts(w, httptest.NewRequest("GET", "/", nil)) }
	}, &latest)
	if latest.Instructions != "Version three." || latest.Version != 3 || latest.PublishedAt == "" || len(latest.Tools) != len(tools) {
		t.Fatalf("latest: %+v", latest)
	}
	var list []PromptVersion
	rec := get(func(s *PromptStore) func(*httptest.ResponseRecorder) {
		return func(w *httptest.ResponseRecorder) { s.Versions(w, httptest.NewRequest("GET", "/", nil)) }
	}, &list)
	if len(list) != 3 || list[0].Version != 3 || list[2].Version != 1 || strings.Contains(rec.Body.String(), "user_staff") || strings.Contains(rec.Body.String(), "Version three") {
		t.Fatalf("versions: %s", rec.Body)
	}
	one := func(n string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/", nil)
		req.SetPathValue("version", n)
		s.Version(rec, req)
		return rec
	}
	var v1 PromptVersion
	if rec := one("1"); rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &v1) != nil || v1.Body != Instructions || v1.PublishedBy != "" {
		t.Fatalf("version 1: %d %+v", rec.Code, v1)
	}
	for _, n := range []string{"99", "0", "x"} {
		if rec := one(n); rec.Code != 404 {
			t.Fatalf("version %s: %d", n, rec.Code)
		}
	}

	// staff: current and history with who; publish through the handler, the reasons as codes
	rec = httptest.NewRecorder()
	s.Staff(rec, httptest.NewRequest("GET", "/", nil))
	if !strings.Contains(rec.Body.String(), `"current":{"version":3`) || !strings.Contains(rec.Body.String(), "user_staff") {
		t.Fatalf("staff: %s", rec.Body)
	}
	post := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/", strings.NewReader(body)).WithContext(auth.WithAPIKey(ctx, "user_admin", "org_staff", "", "personal"))
		req.Header.Set("Accept", "application/json")
		s.PublishHandler(rec, req)
		return rec
	}
	if rec := post(`{"body":"Version three."}`); rec.Code != 409 || !strings.Contains(rec.Body.String(), "mcp_prompt_unchanged") {
		t.Fatalf("unchanged: %d %s", rec.Code, rec.Body)
	}
	if rec := post(`{"body":""}`); rec.Code != 400 || !strings.Contains(rec.Body.String(), "mcp_prompt_empty") {
		t.Fatalf("empty: %d %s", rec.Code, rec.Body)
	}
	if rec := post(`{"body":"Version four.","note":"x"}`); rec.Code != 201 || !strings.Contains(rec.Body.String(), `"published_by":"user_admin"`) {
		t.Fatalf("publish: %d %s", rec.Code, rec.Body)
	}

	// back to how it was, for other tests
	if _, err := db.Exec(ctx, `DELETE FROM mcp_prompts WHERE version > 1`); err != nil {
		t.Fatal(err)
	}
}

// the database unreachable: sessions get the built-in instructions, never nothing
func TestPromptFallback(t *testing.T) {
	db, err := pgxpool.New(context.Background(), "postgres://nobody:x@127.0.0.1:1/none?connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if got := NewPromptStore(db).Instructions(context.Background()); got != Instructions {
		t.Fatalf("fallback: %q", got)
	}
}
