package projects

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/clerk/clerk-sdk-go/v2"
	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/organizations"
)

func TestProjectSecurity(t *testing.T) {
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
	const org = "project-security"
	if _, err := db.Exec(ctx, `DELETE FROM projects WHERE owner_clerk_id = $1`, org); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `DELETE FROM org_admins WHERE org_id = $1`, org); err != nil {
		t.Fatal(err)
	}
	var project Project
	if err := db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name, key_prefix) VALUES ($1, 'Protected', 'PRJ') RETURNING id::text, key_prefix`, org).Scan(&project.ID, &project.KeyPrefix); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO org_admins (org_id, user_clerk_id, granted_by) VALUES ($1, 'admin-user', 'test')`, org); err != nil {
		t.Fatal(err)
	}
	h := NewSystem(db, organizations.Admins{DB: db})
	member := session(ctx, "member-user", org)
	admin := session(ctx, "admin-user", org)
	apiKey := auth.WithAPIKey(ctx, "admin-user", org, "agent", "ai")
	call := func(as context.Context, method, body string, fn func(http.ResponseWriter, *http.Request)) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/", strings.NewReader(body)).WithContext(as)
		r.SetPathValue("id", project.ID)
		w := httptest.NewRecorder()
		fn(w, r)
		return w
	}

	for name, as := range map[string]context.Context{"member": member, "API key": apiKey} {
		if got := call(as, http.MethodPatch, `{"name":"Hijacked","description":"","key_prefix":"NEW"}`, h.Update).Code; got != http.StatusForbidden {
			t.Errorf("%s can change key prefix: got %d, want 403", name, got)
		}
		if got := call(as, http.MethodDelete, "", h.Delete).Code; got != http.StatusForbidden {
			t.Errorf("%s can delete project: got %d, want 403", name, got)
		}
	}
	var name, prefix string
	if err := db.QueryRow(ctx, `SELECT name, key_prefix FROM projects WHERE id = $1`, project.ID).Scan(&name, &prefix); err != nil {
		t.Fatal(err)
	}
	if name != "Protected" || prefix != "PRJ" {
		t.Fatalf("unauthorized update changed project: name=%q prefix=%q", name, prefix)
	}
	if got := call(admin, http.MethodPatch, `{"name":"Protected","description":"","key_prefix":"NEW"}`, h.Update).Code; got != http.StatusNoContent {
		t.Fatalf("admin key-prefix change: got %d, want 204", got)
	}
	if got := call(admin, http.MethodDelete, "", h.Delete).Code; got != http.StatusNoContent {
		t.Fatalf("admin delete: got %d, want 204", got)
	}
	var exists bool
	if err := db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM projects WHERE id = $1)`, project.ID).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("admin delete did not remove project")
	}
}

func session(ctx context.Context, user, org string) context.Context {
	return clerk.ContextWithSessionClaims(ctx, &clerk.SessionClaims{
		RegisteredClaims: clerk.RegisteredClaims{Subject: user},
		Claims:           clerk.Claims{ActiveOrganizationID: org},
	})
}
