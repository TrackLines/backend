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
	projectFor := func(_ context.Context, path string) (string, error) {
		if strings.HasPrefix(path, "/api/roadmaps/") {
			return "project1", nil
		}
		return "", nil
	}
	allowed := func(_ context.Context, project, origin string) (bool, error) {
		return project == "project1" && origin == "https://fan.example", nil
	}
	var recorded []string
	recordFn := func(project, origin, path, key string) { recorded = append(recorded, origin+" "+path+" "+key) }
	build := func(on bool) http.Handler {
		return CORS([]string{"https://tracklin.es"}, func() bool { return on }, projectFor, allowed, recordFn, "GET, POST, OPTIONS", "Content-Type")(strict, pass)
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

// project makes a project in org, starting clean.
func project(t *testing.T, db *pgxpool.Pool, org, name string) string {
	t.Helper()
	var id string
	if err := db.QueryRow(context.Background(), `INSERT INTO projects (owner_clerk_id, name) VALUES ($1, $2) RETURNING id`, org, name).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func clean(t *testing.T, db *pgxpool.Pool, orgs ...string) {
	t.Helper()
	for _, q := range []string{`DELETE FROM projects WHERE owner_clerk_id = ANY($1)`, `DELETE FROM org_admins WHERE org_id = ANY($1)`} {
		if _, err := db.Exec(context.Background(), q, orgs); err != nil {
			t.Fatal(err)
		}
	}
}

// TL-683: each project has its own list; nothing crosses projects
func TestStore(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	clean(t, db, "em1", "em2")
	s := Store{DB: db}
	shop, docs := project(t, db, "em1", "Shop"), project(t, db, "em1", "Docs")

	a, err := s.Add(ctx, "em1", shop, "user_ava", "https://Shop.Example/pricing")
	if err != nil || a.Origin != "https://shop.example" {
		t.Fatalf("Add normalised: %+v %v", a, err)
	}
	// idempotent re-add returns the same row, doesn't duplicate
	again, err := s.Add(ctx, "em1", shop, "user_ava", "https://shop.example")
	if err != nil || again.ID != a.ID {
		t.Fatalf("re-add should be idempotent: %+v vs %+v (%v)", again, a, err)
	}
	if _, err := s.Add(ctx, "em1", shop, "user_ava", "not-an-origin"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid origin: want ErrInvalid, got %v", err)
	}
	// another project of the same org is separate
	if _, err := s.Add(ctx, "em1", docs, "user_ava", "https://docs.example"); err != nil {
		t.Fatal(err)
	}
	list, err := s.List(ctx, shop)
	if err != nil || len(list) != 1 || list[0].Origin != "https://shop.example" {
		t.Fatalf("List shop: %+v %v", list, err)
	}
	if ok, _ := s.Allowed(ctx, shop, "https://shop.example"); !ok {
		t.Fatal("shop should allow its origin")
	}
	if ok, _ := s.Allowed(ctx, shop, "https://docs.example"); ok {
		t.Fatal("shop allowed docs' origin")
	}
	// remove is project-scoped
	if err := s.Remove(ctx, docs, a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-project remove: want ErrNotFound, got %v", err)
	}
	if err := s.Remove(ctx, shop, a.ID); err != nil {
		t.Fatalf("remove own: %v", err)
	}
	if ok, _ := s.Allowed(ctx, shop, "https://shop.example"); ok {
		t.Fatal("origin should be gone after remove")
	}
	// a project of the org, or not
	if s.InOrg(ctx, "em1", docs) != nil || !errors.Is(s.InOrg(ctx, "em2", docs), ErrNotFound) || !errors.Is(s.InOrg(ctx, "em1", "not-a-uuid"), ErrNotFound) {
		t.Fatal("InOrg")
	}
	// deleting the project takes its list
	if _, err := db.Exec(ctx, `DELETE FROM projects WHERE id = $1`, docs); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = db.QueryRow(ctx, `SELECT count(*) FROM embed_origins WHERE project_id = $1`, docs).Scan(&n)
	if n != 0 {
		t.Fatalf("%d origins outlived their project", n)
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
		"/api/roadmaps/" + rid:              pid,
		"/api/roadmaps/" + rid + "/updates": pid,
		"/api/intake/emtok":                 pid,
		"/api/roadmaps/" + rid + "/items":   "", // not an embed route
		"/api/intake/nope":                  "", // unknown token
		"/api/search":                       "",
	} {
		got, err := r.ProjectFor(ctx, path)
		if err != nil || got != want {
			t.Errorf("ProjectFor(%q) = %q, %v; want %q", path, got, err, want)
		}
	}
	// a non-UUID roadmap id must not error, just resolve to nothing
	if got, err := r.ProjectFor(ctx, "/api/roadmaps/not-a-uuid"); err != nil || got != "" {
		t.Errorf("ProjectFor(non-uuid) = %q, %v; want \"\"", got, err)
	}
	// end to end: a site allowed on another project of the org can't embed this one's roadmap
	other := project(t, db, "emorg", "Other")
	s := Store{DB: db}
	if _, err := s.Add(ctx, "emorg", other, "u", "https://other.example"); err != nil {
		t.Fatal(err)
	}
	owner, _ := r.ProjectFor(ctx, "/api/roadmaps/"+rid)
	if ok, _ := s.Allowed(ctx, owner, "https://other.example"); ok {
		t.Fatal("another project's site may embed this roadmap")
	}
}

func TestKeys(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	clean(t, db, "emk1")
	s := Store{DB: db}
	mine, theirs := project(t, db, "emk1", "Mine"), project(t, db, "emk1", "Theirs")

	k, err := s.CreateKey(ctx, "emk1", mine, "user_ava", "Marketing site")
	if err != nil || !strings.HasPrefix(k.Key, "tle_") || k.Label != "Marketing site" || k.Hits != 0 {
		t.Fatalf("CreateKey: %+v %v", k, err)
	}

	// record hits: two on one origin, one on another, plus calls that must NOT count
	s.RecordUsage(ctx, mine, "https://a.example", "/api/roadmaps/x", k.Key)
	s.RecordUsage(ctx, mine, "https://a.example", "/api/roadmaps/x/updates", k.Key)
	s.RecordUsage(ctx, mine, "https://b.example", "/api/intake/tok", k.Key)
	s.RecordUsage(ctx, theirs, "https://a.example", "/api/roadmaps/x", k.Key) // another project's resource
	s.RecordUsage(ctx, mine, "https://a.example", "/api/search", k.Key)       // not an embed path
	s.RecordUsage(ctx, mine, "https://a.example", "/api/roadmaps/x", "")      // no key

	keys, err := s.ListKeys(ctx, mine)
	if err != nil || len(keys) != 1 {
		t.Fatalf("ListKeys: %+v %v", keys, err)
	}
	if keys[0].Hits != 3 || keys[0].Origins != 2 || keys[0].LastUsed == nil {
		t.Fatalf("usage aggregation: %+v", keys[0])
	}
	if keys, _ := s.ListKeys(ctx, theirs); len(keys) != 0 {
		t.Fatalf("another project lists the key: %+v", keys)
	}
	usage, err := s.KeyUsage(ctx, mine, k.ID)
	if err != nil {
		t.Fatal(err)
	}
	// roadmap on a.example (2 hits) ahead of intake on b.example (1)
	if len(usage) != 2 || usage[0].Origin != "https://a.example" || usage[0].Resource != "roadmap" || usage[0].Hits != 2 {
		t.Fatalf("KeyUsage: %+v", usage)
	}
	// project scoping on reads and deletes
	if u, err := s.KeyUsage(ctx, theirs, k.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-project usage: want ErrNotFound, got %+v %v", u, err)
	}
	if err := s.DeleteKey(ctx, theirs, k.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-project delete: want ErrNotFound, got %v", err)
	}
	if err := s.DeleteKey(ctx, mine, k.ID); err != nil {
		t.Fatal(err)
	}
	if keys, _ := s.ListKeys(ctx, mine); len(keys) != 0 {
		t.Fatalf("key should be gone: %+v", keys)
	}
}

// the routes: a project's managers manage, members read, API keys and other orgs can't
func TestAPI(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	clean(t, db, "emo1", "emo2")
	for _, q := range []string{
		`INSERT INTO org_admins (org_id, user_clerk_id, granted_by) VALUES ('emo1', 'user_ava', 'test'), ('emo2', 'user_eve', 'test')`,
	} {
		if _, err := db.Exec(ctx, q); err != nil {
			t.Fatal(q, err)
		}
	}
	p1, p2 := project(t, db, "emo1", "One"), project(t, db, "emo1", "Two")
	h := NewSystem(db).WithManagers(organizations.Admins{DB: db})
	signedIn := func(org, user string) context.Context {
		c := &clerk.SessionClaims{RegisteredClaims: clerk.RegisteredClaims{Subject: user}}
		c.ActiveOrganizationID = org
		return clerk.ContextWithSessionClaims(ctx, c)
	}
	admin := signedIn("emo1", "user_ava")
	viaKey := auth.WithAPIKeyUser(ctx, "emo1", "user_ava") // an admin's API key — must not manage
	call := func(c context.Context, handler http.HandlerFunc, method, body string, path map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/", strings.NewReader(body)).WithContext(c)
		req.Header.Set("Accept", "application/json")
		for k, v := range path {
			req.SetPathValue(k, v)
		}
		rec := httptest.NewRecorder()
		handler(rec, req)
		return rec
	}
	on := func(project string, more ...string) map[string]string {
		m := map[string]string{"id": project}
		for i := 0; i+1 < len(more); i += 2 {
			m[more[i]] = more[i+1]
		}
		return m
	}

	// a manager adds an origin to project one
	rec := call(admin, h.Add, http.MethodPost, `{"origin":"https://shop.example"}`, on(p1))
	if rec.Code != http.StatusCreated {
		t.Fatalf("manager add: %d %s", rec.Code, rec.Body)
	}
	var added Origin
	_ = json.Unmarshal(rec.Body.Bytes(), &added)
	// an API key, even an admin's, may not change the list; a member may not either
	if rec := call(viaKey, h.Add, http.MethodPost, `{"origin":"https://evil.example"}`, on(p1)); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "embed_managers") {
		t.Fatalf("api key add: %d %s", rec.Code, rec.Body)
	}
	if rec := call(signedIn("emo1", "user_bob"), h.Add, http.MethodPost, `{"origin":"https://evil.example"}`, on(p1)); rec.Code != http.StatusForbidden {
		t.Fatalf("member add: %d", rec.Code)
	}
	// lists: the manager can manage; a member reads only; project two has none of it
	var out listOut
	_ = json.Unmarshal(call(admin, h.List, http.MethodGet, "", on(p1)).Body.Bytes(), &out)
	if len(out.Origins) != 1 || !out.CanManage {
		t.Fatalf("manager list: %+v", out)
	}
	_ = json.Unmarshal(call(signedIn("emo1", "user_bob"), h.List, http.MethodGet, "", on(p1)).Body.Bytes(), &out)
	if len(out.Origins) != 1 || out.CanManage {
		t.Fatalf("member list: %+v", out)
	}
	_ = json.Unmarshal(call(admin, h.List, http.MethodGet, "", on(p2)).Body.Bytes(), &out)
	if len(out.Origins) != 0 {
		t.Fatalf("project two: %+v", out)
	}
	// another org: its admin can't see or touch project one at all
	if rec := call(signedIn("emo2", "user_eve"), h.List, http.MethodGet, "", on(p1)); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-org list: %d", rec.Code)
	}
	if rec := call(signedIn("emo2", "user_eve"), h.Remove, http.MethodDelete, "", on(p1, "origin", added.ID)); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-org delete: %d", rec.Code)
	}
	// the wrong project of the right org: not found
	if rec := call(admin, h.Remove, http.MethodDelete, "", on(p2, "origin", added.ID)); rec.Code != http.StatusNotFound {
		t.Fatalf("wrong project delete: %d", rec.Code)
	}
	if rec := call(admin, h.Remove, http.MethodDelete, "", on(p1, "origin", added.ID)); rec.Code != http.StatusNoContent {
		t.Fatalf("manager delete: %d", rec.Code)
	}

	// keys: the same rules
	if rec := call(viaKey, h.CreateKey, http.MethodPost, `{"label":"x"}`, on(p1)); rec.Code != http.StatusForbidden {
		t.Fatalf("api key create: %d", rec.Code)
	}
	rec = call(admin, h.CreateKey, http.MethodPost, `{"label":"Docs"}`, on(p1))
	if rec.Code != http.StatusCreated {
		t.Fatalf("manager create: %d %s", rec.Code, rec.Body)
	}
	var k Key
	_ = json.Unmarshal(rec.Body.Bytes(), &k)
	var keys keysOut
	_ = json.Unmarshal(call(signedIn("emo1", "user_bob"), h.ListKeys, http.MethodGet, "", on(p1)).Body.Bytes(), &keys)
	if len(keys.Keys) != 1 || keys.CanManage {
		t.Fatalf("member keys: %+v", keys)
	}
	if rec := call(admin, h.KeyUsage, http.MethodGet, "", on(p1, "key", k.ID)); rec.Code != http.StatusOK {
		t.Fatalf("usage: %d", rec.Code)
	}
	if rec := call(admin, h.DeleteKey, http.MethodDelete, "", on(p2, "key", k.ID)); rec.Code != http.StatusNotFound {
		t.Fatalf("wrong project key delete: %d", rec.Code)
	}
	if rec := call(admin, h.DeleteKey, http.MethodDelete, "", on(p1, "key", k.ID)); rec.Code != http.StatusNoContent {
		t.Fatalf("manager key delete: %d", rec.Code)
	}
}

// TL-683's migration: what an org allowed, each of its projects allows; a key goes to the org's oldest
// project, token and usage intact; an org with no projects keeps nothing. Run on today's data shape, in a
// transaction that's rolled back.
func TestMigrationMovesToProjects(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	clean(t, db, "emm1", "emm2")
	read := func(name string) string {
		b, err := os.ReadFile("../migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	must := func(sql string, args ...any) {
		t.Helper()
		if _, err := tx.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql[:min(len(sql), 60)], err)
		}
	}
	must(read("000099_embed_per_project.down.sql"))
	var first, second string
	_ = tx.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name, created_at) VALUES ('emm1', 'First', now() - interval '2 days') RETURNING id`).Scan(&first)
	_ = tx.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('emm1', 'Second') RETURNING id`).Scan(&second)
	must(`INSERT INTO embed_origins (org, origin) VALUES ('emm1', 'https://a.example'), ('emm1', 'https://b.example'), ('emm2', 'https://lonely.example')`)
	var key string
	_ = tx.QueryRow(ctx, `INSERT INTO embed_keys (org, key) VALUES ('emm1', 'tle_migrate') RETURNING id`).Scan(&key)
	must(`INSERT INTO embed_keys (org, key) VALUES ('emm2', 'tle_orphan')`)
	must(`INSERT INTO embed_usage (embed_key_id, origin, resource, day, hits) VALUES ($1, 'https://a.example', 'roadmap', current_date, 5)`, key)

	must(read("000099_embed_per_project.up.sql"))
	count := func(sql string, args ...any) int {
		var n int
		if err := tx.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	for _, p := range []string{first, second} {
		if n := count(`SELECT count(*) FROM embed_origins WHERE project_id = $1`, p); n != 2 {
			t.Fatalf("project %s has %d origins, want 2", p, n)
		}
	}
	if n := count(`SELECT count(*) FROM embed_origins WHERE org = 'emm2'`); n != 0 {
		t.Fatalf("an org with no projects kept %d origins", n)
	}
	var owner string
	_ = tx.QueryRow(ctx, `SELECT project_id::text FROM embed_keys WHERE key = 'tle_migrate'`).Scan(&owner)
	if owner != first || count(`SELECT hits FROM embed_usage WHERE embed_key_id = $1`, key) != 5 {
		t.Fatalf("key went to %s (want the oldest, %s), or lost its usage", owner, first)
	}
	if n := count(`SELECT count(*) FROM embed_keys WHERE key = 'tle_orphan'`); n != 0 {
		t.Fatal("an org with no projects kept its key")
	}
}
