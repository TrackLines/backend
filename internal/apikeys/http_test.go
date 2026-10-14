package apikeys

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
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

	// create: agent keys say their client and get a generated name (TL-269); the secret comes back once
	expect(400, "create", as("akh1", "org_akh"), "", `{"kind":"ai"}`)                     // no client
	expect(400, "create", as("akh1", "org_akh"), "", `{"kind":"ai","client":"emacs"}`)    // not one we know
	expect(400, "create", as("akh1", "org_akh"), "", `{"name":"Claude","kind":"admin"}`)
	type made struct {
		Key     string  `json:"key"`
		ID      string  `json:"id"`
		Name    string  `json:"name"`
		Kind    string  `json:"kind"`
		Client  *string `json:"client"`
		Display string  `json:"display"`
	}
	agent := func(user, org, body string) made {
		t.Helper()
		var m made
		_ = json.Unmarshal(expect(201, "create", as(user, org), "", body).Body.Bytes(), &m)
		return m
	}
	generated := regexp.MustCompile(`^claude-\d{6}$`)
	created := agent("akh1", "org_akh", `{"name":"ignored","kind":"ai","client":"claude-code"}`)
	if !strings.HasPrefix(created.Key, Prefix) || !generated.MatchString(created.Name) || created.Kind != "ai" ||
		created.Client == nil || *created.Client != "claude-code" || created.Display != "Claude" {
		t.Fatalf("created: %+v", created)
	}
	if again := agent("akh1", "org_akh", `{"kind":"ai","client":"claude-code"}`); again.Name == created.Name || !generated.MatchString(again.Name) {
		t.Fatalf("a second Claude key needs its own name: %s and %s", created.Name, again.Name)
	} else {
		expect(204, "revoke", as("akh1", "org_akh"), again.ID, "")
	}
	// "other" agents: named by what they're called, or "AI agent"; the label can't pose as anyone
	if aider := agent("akh2", "org_other", `{"kind":"ai","client":"other","label":" Aider "}`); aider.Display != "Aider" || !strings.HasPrefix(aider.Name, "agent-") {
		t.Fatalf("other agent: %+v", aider)
	}
	if unnamed := agent("akh2", "org_other", `{"kind":"ai","client":"other"}`); unnamed.Display != "AI agent" {
		t.Fatalf("unnamed other agent: %+v", unnamed)
	}
	expect(400, "create", as("akh2", "org_other"), "", `{"kind":"ai","client":"other","label":"user_admin"}`)
	expect(400, "create", as("akh2", "org_other"), "", `{"kind":"ai","client":"other","label":"`+strings.Repeat("x", MaxLabel+1)+`"}`)

	// script (service) keys are still named by hand, and can't pose as a person, the system, an
	// integration, or another member's key (TL-247)
	expect(400, "create", as("akh1", "org_akh"), "", `{"name":"  ","kind":"service"}`)
	for _, name := range []string{"user_admin123", "User_X", "legacy", "GitHub", "bitbucket"} {
		expect(400, "create", as("akh2", "org_akh"), "", `{"name":"`+name+`","kind":"service"}`)
	}
	var deploy struct {
		ID      string `json:"id"`
		Display string `json:"display"`
	}
	_ = json.Unmarshal(expect(201, "create", as("akh1", "org_akh"), "", `{"name":"Deploy","kind":"service"}`).Body.Bytes(), &deploy)
	if deploy.Display != "Deploy" {
		t.Fatalf("service key display: %+v", deploy)
	}
	expect(409, "create", as("akh2", "org_akh"), "", `{"name":"deploy","kind":"service"}`)   // akh1's Deploy
	expect(201, "create", as("akh2", "org_other"), "", `{"name":"Deploy","kind":"service"}`) // another org's names are its own
	expect(204, "revoke", as("akh1", "org_akh"), deploy.ID, "")

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
		mw := Middleware(db, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = auth.ViaAPIKey(r.Context()) }))
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

// A key whose owner has left its org stops working on its next use, even if the Clerk webhook never
// arrived: it's refused and revoked. If Clerk can't answer, the key is refused (503), never let in.
func TestKeyOwnerMustStillBeAMember(t *testing.T) {
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
	if _, err := db.Exec(ctx, `DELETE FROM users WHERE clerk_id = 'akm1'; INSERT INTO users (clerk_id, email) VALUES ('akm1', 'm@k.h')`); err != nil {
		t.Fatal(err)
	}
	s := Store{DB: db}
	plain, _, err := s.Create(ctx, "akm1", "org_akm", "Claude", KindAI, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var member bool
	var clerkDown error
	var asked []string
	mw := Middleware(db, func(_ context.Context, org, user string) (bool, error) {
		asked = append(asked, org+"/"+user)
		return member, clerkDown
	})
	use := func() int {
		let := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) }))
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("Authorization", "Bearer "+plain)
		rec := httptest.NewRecorder()
		let.ServeHTTP(rec, req)
		return rec.Code
	}

	member = true
	if got := use(); got != http.StatusTeapot || asked[0] != "org_akm/akm1" {
		t.Fatalf("a member's key: %d (asked %v)", got, asked)
	}
	clerkDown = errors.New("clerk unavailable")
	if got := use(); got != http.StatusServiceUnavailable {
		t.Fatalf("Clerk down: %d", got)
	}
	clerkDown, member = nil, false
	if got := use(); got != http.StatusUnauthorized {
		t.Fatalf("a leaver's key: %d", got)
	}
	member = true // even if they rejoin, the key stays revoked
	if got := use(); got != http.StatusUnauthorized {
		t.Fatalf("revoked key used again: %d", got)
	}
}
