package projects

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
)

// Projects belong to the caller's active org: everyone in it sees them, nobody outside does.
// PostgreSQL is provisioned by this package's TestMain.
func TestOrgSharing(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Fatal("TEST_DATABASE_URL not set; package TestMain should provision PostgreSQL")
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
	if _, err := db.Exec(ctx, `DELETE FROM projects WHERE owner_clerk_id IN ('org_x', 'org_y')`); err != nil {
		t.Fatal(err)
	}
	h := NewSystem(db)
	as := func(user, org string) context.Context { return auth.WithAPIKey(ctx, user, org, user, "ai") }
	call := func(c context.Context, method, id, body string, fn func(http.ResponseWriter, *http.Request)) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/", strings.NewReader(body)).WithContext(c)
		req.SetPathValue("id", id)
		rec := httptest.NewRecorder()
		fn(rec, req)
		return rec
	}
	alice, bob, carol := as("alice", "org_x"), as("bob", "org_x"), as("carol", "org_y")

	rec := call(alice, "POST", "", `{"name":"shared"}`, h.Create)
	var p Project
	if rec.Code != 201 || json.Unmarshal(rec.Body.Bytes(), &p) != nil {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	if rec := call(bob, "GET", p.ID, "", h.Get); rec.Code != 200 {
		t.Fatalf("same org should see it: %d", rec.Code)
	}
	if rec := call(bob, "GET", "", "", h.List); !strings.Contains(rec.Body.String(), p.ID) {
		t.Fatalf("same org list: %s", rec.Body)
	}
	if rec := call(carol, "GET", p.ID, "", h.Get); rec.Code != 404 {
		t.Fatalf("other org should get 404: %d", rec.Code)
	}
	if rec := call(carol, "GET", "", "", h.List); strings.Contains(rec.Body.String(), p.ID) {
		t.Fatalf("other org list leaks: %s", rec.Body)
	}
}
