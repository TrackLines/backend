package jira

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/clerk/clerk-sdk-go/v2"
	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/organizations"
	"github.com/tracklines/backend/internal/secrets"
)

// TL-537: an admin connects Jira (checked with Jira first), the token is kept sealed for 30 days, can
// be extended or deleted, and is gone once expired; only the org's signed-in admins, behind the flag.
func TestConnection(t *testing.T) {
	ctx := context.Background()
	url := os.Getenv("TEST_DATABASE_URL")
	m, err := migrate.New("file://../../migrations", "pgx5"+url[len("postgres"):])
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
	if _, err := db.Exec(ctx, `DELETE FROM jira_connections WHERE org_id IN ('jc1', 'jc2'); DELETE FROM org_admins WHERE org_id IN ('jc1', 'jc2');
		INSERT INTO org_admins (org_id, user_clerk_id, granted_by) VALUES ('jc1', 'user_admin', 'test'), ('jc2', 'user_other', 'test')`); err != nil {
		t.Fatal(err)
	}

	jira := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, _ := r.BasicAuth(); u != "sam@acme.io" || p != "secret-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, `{"displayName": "Sam Doe"}`)
	}))
	defer jira.Close()
	box, _ := secrets.New(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)))
	now := time.Now()
	var dialled string
	store := Store{DB: db, Box: box, Now: func() time.Time { return now }, Dial: func(site, email, token string) Client {
		dialled = site
		return Client{Site: jira.URL, Email: email, Token: token, HTTP: jira.Client()}
	}}
	on := true
	h := System{Store: store, Admins: organizations.Admins{DB: db}, On: func() bool { return on }}

	as := func(user, org string) context.Context {
		return clerk.ContextWithSessionClaims(ctx, &clerk.SessionClaims{RegisteredClaims: clerk.RegisteredClaims{Subject: user}, Claims: clerk.Claims{ActiveOrganizationID: org}})
	}
	admin := as("user_admin", "jc1")
	call := func(c context.Context, f http.HandlerFunc, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/", strings.NewReader(body)).WithContext(c)
		req.Header.Set("Accept", "application/json")
		rec := httptest.NewRecorder()
		f(rec, req)
		return rec
	}
	code := func(rec *httptest.ResponseRecorder) string {
		var e struct{ Code string }
		json.Unmarshal(rec.Body.Bytes(), &e)
		return fmt.Sprint(rec.Code, " ", e.Code)
	}
	connect := func(c context.Context, site, token string) *httptest.ResponseRecorder {
		return call(c, h.Connect, fmt.Sprintf(`{"site": %q, "email": "sam@acme.io", "token": %q}`, site, token))
	}

	// nothing yet; a bad site or token is refused, and nothing is kept
	if rec := call(admin, h.Get, ""); rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != "null" {
		t.Fatalf("none: %d %s", rec.Code, rec.Body)
	}
	if got := code(connect(admin, "https://evil.com", "secret-token")); got != "400 jira_site" {
		t.Fatalf("bad site: %s", got)
	}
	if got := code(connect(admin, "acme", "wrong")); got != "400 jira_unauthorized" {
		t.Fatalf("bad token: %s", got)
	}
	if got := code(call(admin, h.Connect, `{"site": "acme", "email": "nobody", "token": "x"}`)); got != "400 jira_login" {
		t.Fatalf("bad email: %s", got)
	}

	// connected: checked with Jira at the normalised site, kept 30 days, the token sealed
	rec := connect(admin, "acme", "secret-token")
	var c Connection
	json.Unmarshal(rec.Body.Bytes(), &c)
	if rec.Code != 200 || c.Site != "https://acme.atlassian.net" || c.Account != "Sam Doe" || c.CreatedBy != "user_admin" || dialled != c.Site {
		t.Fatalf("connect: %d %+v (dialled %s)", rec.Code, c, dialled)
	}
	if strings.Contains(rec.Body.String(), "secret-token") {
		t.Fatal("the token came back")
	}
	if exp, _ := time.Parse(time.RFC3339, c.ExpiresAt); exp.Sub(now) < Lifetime-time.Minute || exp.Sub(now) > Lifetime+time.Minute {
		t.Fatalf("expires %s, not 30 days on", c.ExpiresAt)
	}
	var raw []byte
	db.QueryRow(ctx, `SELECT token FROM jira_connections WHERE org_id = 'jc1'`).Scan(&raw)
	if bytes.Contains(raw, []byte("secret-token")) {
		t.Fatal("the token is stored in the clear")
	}
	if cl, err := store.Client(ctx, "jc1"); err != nil || cl.Token != "secret-token" || cl.Email != "sam@acme.io" {
		t.Fatalf("client: %+v %v", cl, err)
	}

	// another org sees nothing and can't use it; non-admins, API keys and the flag off are refused
	if rec := call(as("user_other", "jc2"), h.Get, ""); strings.TrimSpace(rec.Body.String()) != "null" {
		t.Fatalf("another org: %s", rec.Body)
	}
	if _, err := store.Client(ctx, "jc2"); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("another org's client: %v", err)
	}
	if rec := call(as("user_member", "jc1"), h.Get, ""); rec.Code != 403 {
		t.Fatalf("member: %d", rec.Code)
	}
	if rec := call(auth.WithAPIKeyUser(ctx, "jc1", "user_admin"), h.Get, ""); rec.Code != 403 {
		t.Fatalf("api key: %d", rec.Code)
	}
	on = false
	if rec := call(admin, h.Get, ""); rec.Code != 404 {
		t.Fatalf("flag off: %d", rec.Code)
	}
	on = true

	// extended: 30 days from now; expired: gone when read (and not extendable)
	now = now.Add(20 * 24 * time.Hour)
	json.Unmarshal(call(admin, h.Extend, "").Body.Bytes(), &c)
	if exp, _ := time.Parse(time.RFC3339, c.ExpiresAt); exp.Sub(now) < Lifetime-time.Minute {
		t.Fatalf("extend: %s", c.ExpiresAt)
	}
	now = now.Add(Lifetime + time.Hour)
	if _, err := store.Client(ctx, "jc1"); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("expired client: %v", err)
	}
	var n int
	db.QueryRow(ctx, `SELECT count(*) FROM jira_connections WHERE org_id = 'jc1'`).Scan(&n)
	if n != 0 {
		t.Fatal("an expired token wasn't deleted on reading")
	}
	if got := code(call(admin, h.Extend, "")); got != "409 jira_not_connected" {
		t.Fatalf("extend expired: %s", got)
	}

	// deleted
	connect(admin, "acme", "secret-token")
	if rec := call(admin, h.Delete, ""); rec.Code != 204 {
		t.Fatalf("delete: %d", rec.Code)
	}
	if rec := call(admin, h.Get, ""); strings.TrimSpace(rec.Body.String()) != "null" {
		t.Fatalf("after delete: %s", rec.Body)
	}

	// without the server's key nothing is kept
	h.Store.Box = secrets.Box{}
	if got := code(connect(admin, "acme", "secret-token")); got != "503 integration_not_configured" {
		t.Fatalf("no key: %s", got)
	}
}
