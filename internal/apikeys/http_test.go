package apikeys

import (
	"context"
	"encoding/json"
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
)

// Key handlers: create (shows the secret once), list (mine, in this org, never the secret), revoke
// (only mine); a revoked key stops working.
func TestKeyHandlers(t *testing.T) {
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
	if _, err := db.Exec(ctx, `DELETE FROM users WHERE clerk_id IN ('akh1', 'akh2'); INSERT INTO users (clerk_id, email) VALUES ('akh1', 'a@k.h'), ('akh2', 'b@k.h')`); err != nil {
		t.Fatal(err)
	}
	h := NewSystem(db)
	as := func(user, org string) context.Context {
		c := &clerk.SessionClaims{RegisteredClaims: clerk.RegisteredClaims{Subject: user}}
		c.ActiveOrganizationID = org
		return clerk.ContextWithSessionClaims(ctx, c)
	}
	run := func(action string, c context.Context, id, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest("POST", "/", strings.NewReader(body)).WithContext(c)
		req.SetPathValue("id", id)
		rec := httptest.NewRecorder()
		map[string]func(){"create": func() { h.Create(rec, req) }, "list": func() { h.List(rec, req) }, "revoke": func() { h.Revoke(rec, req) }}[action]()
		return rec
	}
	expect := func(want int, action string, c context.Context, id, body string) *httptest.ResponseRecorder {
		t.Helper()
		rec := run(action, c, id, body)
		if rec.Code != want {
			t.Fatalf("%s %s %s: %d %s, want %d", action, id, body, rec.Code, rec.Body, want)
		}
		return rec
	}
	list := func(c context.Context) []map[string]any {
		var out []map[string]any
		_ = json.Unmarshal(expect(200, "list", c, "", "").Body.Bytes(), &out)
		return out
	}

	// create: a name and a kind (ai | service); the secret comes back once
	expect(400, "create", as("akh1", "org_akh"), "", `{"name":"  ","kind":"ai"}`)
	expect(400, "create", as("akh1", "org_akh"), "", `{"name":"Claude","kind":"admin"}`)
	var created struct {
		Key  string `json:"key"`
		ID   string `json:"id"`
		Name string `json:"name"`
		Kind string `json:"kind"`
	}
	_ = json.Unmarshal(expect(201, "create", as("akh1", "org_akh"), "", `{"name":" Claude ","kind":"ai"}`).Body.Bytes(), &created)
	if !strings.HasPrefix(created.Key, Prefix) || created.Name != "Claude" || created.Kind != "ai" {
		t.Fatalf("created: %+v", created)
	}

	// list: my keys in this org, never the secret; not another person's, not another org's
	mine := list(as("akh1", "org_akh"))
	if len(mine) != 1 || mine[0]["id"] != created.ID || strings.Contains(expect(200, "list", as("akh1", "org_akh"), "", "").Body.String(), created.Key) {
		t.Fatalf("my keys: %+v", mine)
	}
	if got := list(as("akh2", "org_akh")); len(got) != 0 {
		t.Fatalf("someone else's view: %+v", got)
	}
	if got := list(as("akh1", "org_other")); len(got) != 0 {
		t.Fatalf("another org's view: %+v", got)
	}

	// the key works until revoked; only its owner can revoke it
	works := func() int {
		called := false
		mw := Middleware(db)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = auth.ViaAPIKey(r.Context()) }))
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("Authorization", "Bearer "+created.Key)
		rec := httptest.NewRecorder()
		mw.ServeHTTP(rec, req)
		if called {
			return 200
		}
		return rec.Code
	}
	if got := works(); got != 200 {
		t.Fatalf("new key: %d", got)
	}
	expect(404, "revoke", as("akh2", "org_akh"), created.ID, "")
	expect(404, "revoke", as("akh1", "org_akh"), "not-a-uuid", "")
	expect(204, "revoke", as("akh1", "org_akh"), created.ID, "")
	if got := works(); got != 401 {
		t.Fatalf("revoked key: %d", got)
	}
	if got := list(as("akh1", "org_akh")); len(got) != 0 {
		t.Fatalf("revoked key still listed: %+v", got)
	}
	expect(404, "revoke", as("akh1", "org_akh"), created.ID, "")
}
