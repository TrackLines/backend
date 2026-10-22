package embed

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
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/organizations"
)

func TestNormalizeOrigin(t *testing.T) {
	ok := map[string]string{
		"https://example.com":         "https://example.com",
		"https://example.com/roadmap": "https://example.com", // path stripped
		"https://Example.COM":         "https://example.com", // host lowercased
		"https://example.com:3000":    "https://example.com:3000",
		"  https://example.com/  ":    "https://example.com", // trimmed
		"http://localhost:5173":       "http://localhost:5173",
	}
	for in, want := range ok {
		got, err := NormalizeOrigin(in)
		if err != nil || got != want {
			t.Errorf("NormalizeOrigin(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "example.com", "*", "javascript:alert(1)", "null", "ftp://example.com", "https://"} {
		if got, err := NormalizeOrigin(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("NormalizeOrigin(%q) = %q, %v; want ErrInvalid", bad, got, err)
		}
	}
}

func TestMatch(t *testing.T) {
	cases := []struct{ path, kind, key string }{
		{"/api/roadmaps/abc", "roadmap", "abc"},
		{"/api/roadmaps/abc/updates", "roadmap", "abc"},
		{"/api/intake/tok_123", "intake", "tok_123"},
		{"/api/roadmaps/", "", ""},
		{"/api/roadmaps/abc/items", "", ""}, // not a public embed route
		{"/api/intake/", "", ""},
		{"/api/search", "", ""},
		{"/api/projects/p/roadmaps", "", ""},
	}
	for _, c := range cases {
		if k, key := match(c.path); k != c.kind || key != c.key {
			t.Errorf("match(%q) = %q,%q; want %q,%q", c.path, k, key, c.kind, c.key)
		}
	}
}

// TestCORS drives the middleware with fakes: only a listed third-party origin, with the flag on, gets
// ACAO; preflight is answered; everything else falls through to the strict handler untouched.
func TestCORS(t *testing.T) {
	strict := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Handler", "strict")
		w.WriteHeader(http.StatusOK)
	})
	pass := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Handler", "pass")
		w.WriteHeader(http.StatusOK)
	})
	orgFor := func(_ context.Context, path string) (string, error) {
		if strings.HasPrefix(path, "/api/roadmaps/") {
			return "org1", nil
		}
		return "", nil
	}
	allowed := func(_ context.Context, org, origin string) (bool, error) {
		return org == "org1" && origin == "https://fan.example", nil
	}
	var recorded []string
	recordFn := func(org, origin, path, key string) { recorded = append(recorded, origin+" "+path+" "+key) }
	build := func(on bool) http.Handler {
		return CORS([]string{"https://tracklin.es"}, func() bool { return on }, orgFor, allowed, recordFn, "GET, POST, OPTIONS", "Content-Type")(strict, pass)
	}
	do := func(h http.Handler, method, path, origin string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	// allowed third-party GET: ACAO reflected, served by pass (bypassing strict's 403)
	rec := do(build(true), http.MethodGet, "/api/roadmaps/r1", "https://fan.example")
	if rec.Header().Get("Access-Control-Allow-Origin") != "https://fan.example" || rec.Header().Get("X-Handler") != "pass" {
		t.Fatalf("allowed GET: acao=%q handler=%q", rec.Header().Get("Access-Control-Allow-Origin"), rec.Header().Get("X-Handler"))
	}
	// preflight: answered here, no inner handler runs
	rec = do(build(true), http.MethodOptions, "/api/roadmaps/r1", "https://fan.example")
	if rec.Code != http.StatusNoContent || rec.Header().Get("Access-Control-Allow-Methods") == "" || rec.Header().Get("X-Handler") != "" {
		t.Fatalf("preflight: code=%d methods=%q handler=%q", rec.Code, rec.Header().Get("Access-Control-Allow-Methods"), rec.Header().Get("X-Handler"))
	}
	// unknown third-party origin → strict (which would 403 it in production)
	if rec := do(build(true), http.MethodGet, "/api/roadmaps/r1", "https://evil.example"); rec.Header().Get("X-Handler") != "strict" || rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("unknown origin not sent to strict: %q %q", rec.Header().Get("X-Handler"), rec.Header().Get("Access-Control-Allow-Origin"))
	}
	// feature off → strict even for a listed origin
	if rec := do(build(false), http.MethodGet, "/api/roadmaps/r1", "https://fan.example"); rec.Header().Get("X-Handler") != "strict" {
		t.Fatalf("flag off should use strict, got %q", rec.Header().Get("X-Handler"))
	}
	// app origin → strict (no DB lookup needed)
	if rec := do(build(true), http.MethodGet, "/api/roadmaps/r1", "https://tracklin.es"); rec.Header().Get("X-Handler") != "strict" {
		t.Fatalf("app origin should use strict, got %q", rec.Header().Get("X-Handler"))
	}
	// embeddable origin but non-embed path → strict
	if rec := do(build(true), http.MethodGet, "/api/search", "https://fan.example"); rec.Header().Get("X-Handler") != "strict" {
		t.Fatalf("non-embed path should use strict, got %q", rec.Header().Get("X-Handler"))
	}
	// no Origin header → strict, untouched
	if rec := do(build(true), http.MethodGet, "/api/roadmaps/r1", ""); rec.Header().Get("X-Handler") != "strict" {
		t.Fatalf("no-origin should use strict, got %q", rec.Header().Get("X-Handler"))
	}

	// a data-key (?embed=) on an allowed request is recorded for usage stats; without one, nothing is
	recorded = nil
	do(build(true), http.MethodGet, "/api/roadmaps/r1?embed=tle_x", "https://fan.example")
	if len(recorded) != 1 || recorded[0] != "https://fan.example /api/roadmaps/r1 tle_x" {
		t.Fatalf("usage not recorded: %v", recorded)
	}
	recorded = nil
	do(build(true), http.MethodGet, "/api/roadmaps/r1", "https://fan.example")                 // no key
	do(build(true), http.MethodOptions, "/api/roadmaps/r1?embed=tle_x", "https://fan.example") // preflight
	if len(recorded) != 0 {
		t.Fatalf("should not record without a key or on preflight: %v", recorded)
	}
}

func testDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
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
	t.Cleanup(db.Close)
	return db
}

func TestStore(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	if _, err := db.Exec(ctx, `DELETE FROM embed_origins WHERE org IN ('em1', 'em2')`); err != nil {
		t.Fatal(err)
	}
	s := Store{DB: db}

	a, err := s.Add(ctx, "em1", "user_ava", "https://Shop.Example/pricing")
	if err != nil || a.Origin != "https://shop.example" {
		t.Fatalf("Add normalised: %+v %v", a, err)
	}
	// idempotent re-add returns the same row, doesn't duplicate
	again, err := s.Add(ctx, "em1", "user_ava", "https://shop.example")
	if err != nil || again.ID != a.ID {
		t.Fatalf("re-add should be idempotent: %+v vs %+v (%v)", again, a, err)
	}
	if _, err := s.Add(ctx, "em1", "user_ava", "not-an-origin"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid origin: want ErrInvalid, got %v", err)
	}
	// another org's identical origin is separate, and scoping holds
	if _, err := s.Add(ctx, "em2", "user_eve", "https://shop.example"); err != nil {
		t.Fatal(err)
	}
	list, err := s.List(ctx, "em1")
	if err != nil || len(list) != 1 {
		t.Fatalf("List em1: %+v %v", list, err)
	}
	if ok, _ := s.Allowed(ctx, "em1", "https://shop.example"); !ok {
		t.Fatal("em1 should allow its origin")
	}
	if ok, _ := s.Allowed(ctx, "em1", "https://other.example"); ok {
		t.Fatal("em1 should not allow an unlisted origin")
	}
	// remove is org-scoped: em2 can't delete em1's row
	if err := s.Remove(ctx, "em2", a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-org remove: want ErrNotFound, got %v", err)
	}
	if err := s.Remove(ctx, "em1", a.ID); err != nil {
		t.Fatalf("remove own: %v", err)
	}
	if ok, _ := s.Allowed(ctx, "em1", "https://shop.example"); ok {
		t.Fatal("origin should be gone after remove")
	}
}

func TestResolver(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	for _, q := range []string{
		`DELETE FROM roadmaps WHERE owner_clerk_id = 'emu1'`,
		`DELETE FROM projects WHERE owner_clerk_id = 'emorg'`,
		`INSERT INTO users (clerk_id, email) VALUES ('emu1', 'a@b.c') ON CONFLICT DO NOTHING`,
	} {
		if _, err := db.Exec(ctx, q); err != nil {
			t.Fatal(q, err)
		}
	}
	var pid string
	if err := db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name, intake_token) VALUES ('emorg', 'P', 'emtok') RETURNING id`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	var rid string
	if err := db.QueryRow(ctx, `INSERT INTO roadmaps (owner_clerk_id, project_id, title) VALUES ('emu1', $1, 'R') RETURNING id`, pid).Scan(&rid); err != nil {
		t.Fatal(err)
	}
	r := Resolver{DB: db}
	for path, want := range map[string]string{
		"/api/roadmaps/" + rid:              "emorg",
		"/api/roadmaps/" + rid + "/updates": "emorg",
		"/api/intake/emtok":                 "emorg",
		"/api/roadmaps/" + rid + "/items":   "", // not an embed route
		"/api/intake/nope":                  "", // unknown token
		"/api/search":                       "",
	} {
		got, err := r.OrgFor(ctx, path)
		if err != nil || got != want {
			t.Errorf("OrgFor(%q) = %q, %v; want %q", path, got, err, want)
		}
	}
	// a non-UUID roadmap id must not error, just resolve to nothing
	if got, err := r.OrgFor(ctx, "/api/roadmaps/not-a-uuid"); err != nil || got != "" {
		t.Errorf("OrgFor(non-uuid) = %q, %v; want \"\"", got, err)
	}
}

func TestKeys(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	if _, err := db.Exec(ctx, `DELETE FROM embed_keys WHERE org IN ('emk1', 'emk2')`); err != nil {
		t.Fatal(err)
	}
	s := Store{DB: db}

	k, err := s.CreateKey(ctx, "emk1", "user_ava", "Marketing site")
	if err != nil || !strings.HasPrefix(k.Key, "tle_") || k.Label != "Marketing site" || k.Hits != 0 {
		t.Fatalf("CreateKey: %+v %v", k, err)
	}

	// record hits: two on one origin, one on another, plus calls that must NOT count
	s.RecordUsage(ctx, "emk1", "https://a.example", "/api/roadmaps/x", k.Key)
	s.RecordUsage(ctx, "emk1", "https://a.example", "/api/roadmaps/x/updates", k.Key)
	s.RecordUsage(ctx, "emk1", "https://b.example", "/api/intake/tok", k.Key)
	s.RecordUsage(ctx, "emk2", "https://a.example", "/api/roadmaps/x", k.Key) // wrong org for the key
	s.RecordUsage(ctx, "emk1", "https://a.example", "/api/search", k.Key)     // not an embed path
	s.RecordUsage(ctx, "emk1", "https://a.example", "/api/roadmaps/x", "")    // no key

	keys, err := s.ListKeys(ctx, "emk1")
	if err != nil || len(keys) != 1 {
		t.Fatalf("ListKeys: %+v %v", keys, err)
	}
	if keys[0].Hits != 3 || keys[0].Origins != 2 || keys[0].LastUsed == nil {
		t.Fatalf("usage aggregation: %+v", keys[0])
	}
	usage, err := s.KeyUsage(ctx, "emk1", k.ID)
	if err != nil {
		t.Fatal(err)
	}
	// roadmap on a.example (2 hits) ahead of intake on b.example (1)
	if len(usage) != 2 || usage[0].Origin != "https://a.example" || usage[0].Resource != "roadmap" || usage[0].Hits != 2 {
		t.Fatalf("KeyUsage: %+v", usage)
	}
	// org scoping on reads
	if u, err := s.KeyUsage(ctx, "emk2", k.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-org usage: want ErrNotFound, got %+v %v", u, err)
	}
	// delete takes the usage with it
	if err := s.DeleteKey(ctx, "emk2", k.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-org delete: want ErrNotFound, got %v", err)
	}
	if err := s.DeleteKey(ctx, "emk1", k.ID); err != nil {
		t.Fatal(err)
	}
	if keys, _ := s.ListKeys(ctx, "emk1"); len(keys) != 0 {
		t.Fatalf("key should be gone: %+v", keys)
	}
}

func TestKeysAPI(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	for _, q := range []string{
		`DELETE FROM embed_keys WHERE org IN ('emka', 'emkb')`,
		`DELETE FROM org_admins WHERE org_id IN ('emka', 'emkb')`,
		`INSERT INTO org_admins (org_id, user_clerk_id, granted_by) VALUES ('emka', 'user_ava', 'test'), ('emkb', 'user_eve', 'test')`,
	} {
		if _, err := db.Exec(ctx, q); err != nil {
			t.Fatal(q, err)
		}
	}
	h := NewSystem(db).WithAdmins(organizations.Admins{DB: db})
	signedIn := func(org, user string) context.Context {
		c := &clerk.SessionClaims{RegisteredClaims: clerk.RegisteredClaims{Subject: user}}
		c.ActiveOrganizationID = org
		return clerk.ContextWithSessionClaims(ctx, c)
	}
	admin := signedIn("emka", "user_ava")
	viaKey := auth.WithAPIKeyUser(ctx, "emka", "user_ava")
	call := func(c context.Context, handler http.HandlerFunc, method, body string, path map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/", strings.NewReader(body)).WithContext(c)
		for k, v := range path {
			req.SetPathValue(k, v)
		}
		rec := httptest.NewRecorder()
		handler(rec, req)
		return rec
	}

	// an API key, even an admin's, can't mint keys
	if rec := call(viaKey, h.CreateKey, http.MethodPost, `{"label":"x"}`, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("api key create: want 403, got %d", rec.Code)
	}
	rec := call(admin, h.CreateKey, http.MethodPost, `{"label":"Docs"}`, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("admin create: %d %s", rec.Code, rec.Body)
	}
	var k Key
	_ = json.Unmarshal(rec.Body.Bytes(), &k)

	// a member sees the key list, without can_manage
	rec = call(signedIn("emka", "user_bob"), h.ListKeys, http.MethodGet, "", nil)
	var out keysOut
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if len(out.Keys) != 1 || out.CanManage {
		t.Fatalf("member list: %+v", out)
	}
	// another org's admin can't delete it
	if rec := call(signedIn("emkb", "user_eve"), h.DeleteKey, http.MethodDelete, "", map[string]string{"id": k.ID}); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-org delete: want 404, got %d", rec.Code)
	}
	if rec := call(admin, h.DeleteKey, http.MethodDelete, "", map[string]string{"id": k.ID}); rec.Code != http.StatusNoContent {
		t.Fatalf("owner delete: want 204, got %d", rec.Code)
	}
}

func TestOriginsAPI(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	for _, q := range []string{
		`DELETE FROM embed_origins WHERE org IN ('emo1', 'emo2')`,
		`DELETE FROM org_admins WHERE org_id IN ('emo1', 'emo2')`,
		`INSERT INTO org_admins (org_id, user_clerk_id, granted_by) VALUES ('emo1', 'user_ava', 'test'), ('emo2', 'user_eve', 'test')`,
	} {
		if _, err := db.Exec(ctx, q); err != nil {
			t.Fatal(q, err)
		}
	}
	h := NewSystem(db).WithAdmins(organizations.Admins{DB: db})
	signedIn := func(org, user string) context.Context {
		c := &clerk.SessionClaims{RegisteredClaims: clerk.RegisteredClaims{Subject: user}}
		c.ActiveOrganizationID = org
		return clerk.ContextWithSessionClaims(ctx, c)
	}
	admin := signedIn("emo1", "user_ava")
	viaKey := auth.WithAPIKeyUser(ctx, "emo1", "user_ava") // an admin's API key — must not manage
	call := func(c context.Context, handler http.HandlerFunc, method, body string, path map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/", strings.NewReader(body)).WithContext(c)
		for k, v := range path {
			req.SetPathValue(k, v)
		}
		rec := httptest.NewRecorder()
		handler(rec, req)
		return rec
	}

	// admin adds an origin
	rec := call(admin, h.Add, http.MethodPost, `{"origin":"https://shop.example"}`, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("admin add: %d %s", rec.Code, rec.Body)
	}
	var added Origin
	_ = json.Unmarshal(rec.Body.Bytes(), &added)

	// an API key, even an admin's, may not change the list
	if rec := call(viaKey, h.Add, http.MethodPost, `{"origin":"https://evil.example"}`, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("api key add: want 403, got %d", rec.Code)
	}
	// list shows the one origin and can_manage for the admin
	rec = call(admin, h.List, http.MethodGet, "", nil)
	var out listOut
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if len(out.Origins) != 1 || !out.CanManage {
		t.Fatalf("admin list: %+v", out)
	}
	// a member who isn't an admin sees the list but can't manage
	rec = call(signedIn("emo1", "user_bob"), h.List, http.MethodGet, "", nil)
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if len(out.Origins) != 1 || out.CanManage {
		t.Fatalf("member list: %+v", out)
	}
	// another org's admin can't delete it
	if rec := call(signedIn("emo2", "user_eve"), h.Remove, http.MethodDelete, "", map[string]string{"id": added.ID}); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-org delete: want 404, got %d", rec.Code)
	}
	// the owning admin can
	if rec := call(admin, h.Remove, http.MethodDelete, "", map[string]string{"id": added.ID}); rec.Code != http.StatusNoContent {
		t.Fatalf("owner delete: want 204, got %d", rec.Code)
	}
}
