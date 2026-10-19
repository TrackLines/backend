package apikeys

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/clerk/clerk-sdk-go/v2"
	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Service keys are the organization's: every member sees them and when they expire, only admins
// make and revoke them (TL-462). Personal and AI keys stay their owner's.
func TestServiceKeysAreAdmins(t *testing.T) {
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
	if _, err := db.Exec(ctx, `INSERT INTO users (clerk_id, email) VALUES ('aks_admin', 'a@k.s'), ('aks_member', 'm@k.s') ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	h := NewSystem(db)
	h.IsAdmin = func(_ context.Context, org, user string) (bool, error) {
		return org == "org_aks" && user == "aks_admin", nil
	}
	call := func(handler func(w *httptest.ResponseRecorder, c context.Context), user string) *httptest.ResponseRecorder {
		c := &clerk.SessionClaims{RegisteredClaims: clerk.RegisteredClaims{Subject: user}}
		c.ActiveOrganizationID = "org_aks"
		rec := httptest.NewRecorder()
		handler(rec, clerk.ContextWithSessionClaims(ctx, c))
		return rec
	}
	create := func(user, body string) (int, Key) {
		rec := call(func(w *httptest.ResponseRecorder, c context.Context) {
			h.Create(w, httptest.NewRequest("POST", "/", strings.NewReader(body)).WithContext(c))
		}, user)
		var k Key
		json.Unmarshal(rec.Body.Bytes(), &k)
		return rec.Code, k
	}
	revoke := func(user, id string) int {
		return call(func(w *httptest.ResponseRecorder, c context.Context) {
			r := httptest.NewRequest("DELETE", "/", nil).WithContext(c)
			r.SetPathValue("id", id)
			h.Revoke(w, r)
		}, user).Code
	}
	list := func(user string) []Key {
		var out []Key
		json.Unmarshal(call(func(w *httptest.ResponseRecorder, c context.Context) {
			h.List(w, httptest.NewRequest("GET", "/", nil).WithContext(c))
		}, user).Body.Bytes(), &out)
		return out
	}

	if code, _ := create("aks_member", `{"kind":"service","name":"aks-deploys"}`); code != 403 {
		t.Fatalf("member made a service key: %d", code)
	}
	code, svc := create("aks_admin", `{"kind":"service","name":"aks-deploys"}`)
	if code != 201 {
		t.Fatalf("admin's service key: %d", code)
	}
	_, mine := create("aks_member", `{"kind":"personal","name":"TRMNL"}`)
	_, adminsOwn := create("aks_admin", `{"kind":"personal","name":"TRMNL"}`)

	// the member sees the org's service key and when it expires, and their own keys; not the admin's personal key
	seen := map[string]Key{}
	for _, k := range list("aks_member") {
		seen[k.ID] = k
	}
	if k, ok := seen[svc.ID]; !ok || k.ExpiresAt == "" {
		t.Fatalf("member's list lacks the service key or its expiry: %+v", seen)
	}
	if _, ok := seen[mine.ID]; !ok {
		t.Fatalf("member's own key missing: %+v", seen)
	}
	if _, ok := seen[adminsOwn.ID]; ok {
		t.Fatal("member sees the admin's personal key")
	}

	if code := revoke("aks_member", svc.ID); code != 404 {
		t.Fatalf("member revoked the service key: %d", code)
	}
	if code := revoke("aks_admin", mine.ID); code != 404 {
		t.Fatalf("admin revoked a member's personal key: %d", code)
	}
	// an admin revokes a service key someone else made (one from before TL-462)
	if _, err := db.Exec(ctx, `UPDATE api_keys SET owner_clerk_id = 'aks_member' WHERE id = $1`, svc.ID); err != nil {
		t.Fatal(err)
	}
	if code := revoke("aks_admin", svc.ID); code != 204 {
		t.Fatalf("admin revoking the org's service key: %d", code)
	}
}
