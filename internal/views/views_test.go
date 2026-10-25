package views

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
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
)

// leaders: who manages which project (org admins and team leaders, as the real model decides).
type leaders map[string]bool // user/project

func (l leaders) IsProjectManager(_ context.Context, _, user, project string) (bool, error) {
	return l[user+"/"+project], nil
}

// Saved views (TL-633): personal ones are their owner's alone; shared ones everyone in the project
// sees, and only its managers make, change or delete; another org's are never found.
func TestSavedViews(t *testing.T) {
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
	var pid string
	if err := db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('org_v', 'Views') RETURNING id`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	h := System{DB: db, Managers: leaders{"lead/" + pid: true}}
	req := func(who, org, method, id, body string) *http.Request {
		r := httptest.NewRequest(method, "/", strings.NewReader(body)).WithContext(auth.WithAPIKey(ctx, who, org, "", "personal"))
		r.Header.Set("Accept", "application/json")
		r.SetPathValue("id", id)
		return r
	}
	run := func(f http.HandlerFunc, r *http.Request) (int, string) {
		rec := httptest.NewRecorder()
		f(rec, r)
		return rec.Code, rec.Body.String()
	}
	list := func(who string) []View {
		_, body := run(h.List, req(who, "org_v", "GET", pid, ""))
		var out []View
		_ = json.Unmarshal([]byte(body), &out)
		return out
	}
	create := func(who, body string) (int, View) {
		code, b := run(h.Create, req(who, "org_v", "POST", pid, body))
		var v View
		_ = json.Unmarshal([]byte(b), &v)
		return code, v
	}

	// a member's personal view: theirs alone
	code, mine := create("amy", `{"name":"My bugs","query":{"type":["bug"]}}`)
	if code != 201 || !mine.Mine || !mine.CanEdit || mine.Shared {
		t.Fatalf("personal: %d %+v", code, mine)
	}
	if len(list("bo")) != 0 {
		t.Fatal("bo sees amy's personal view")
	}
	// sharing: managers only
	if code, _ := create("amy", `{"name":"Team","shared":true}`); code != 403 {
		t.Fatalf("member sharing → %d", code)
	}
	code, shared := create("lead", `{"name":"Team triage","shared":true,"query":{"priority":["urgent"]}}`)
	if code != 201 || !shared.Shared {
		t.Fatalf("leader sharing: %d %+v", code, shared)
	}
	got := list("bo")
	if len(got) != 1 || got[0].ID != shared.ID || got[0].CanEdit {
		t.Fatalf("bo should see the shared view, read-only: %+v", got)
	}
	if got = list("amy"); len(got) != 2 || got[0].ID != shared.ID {
		t.Fatalf("amy: shared first, then hers: %+v", got)
	}
	// changing: bo can't touch the shared one; amy can rename hers but not share it; the leader can
	if code, _ := run(h.Update, req("bo", "org_v", "PATCH", shared.ID, `{"name":"mine now"}`)); code != 403 {
		t.Fatalf("member editing shared → %d", code)
	}
	if code, b := run(h.Update, req("amy", "org_v", "PATCH", mine.ID, `{"name":"Bugs"}`)); code != 200 || !strings.Contains(b, `"name":"Bugs"`) {
		t.Fatalf("rename own: %d %s", code, b)
	}
	if code, _ := run(h.Update, req("amy", "org_v", "PATCH", mine.ID, `{"shared":true}`)); code != 403 {
		t.Fatalf("member sharing own → %d", code)
	}
	if code, _ := run(h.Update, req("lead", "org_v", "PATCH", shared.ID, `{"query":{"priority":["high"]}}`)); code != 200 {
		t.Fatalf("leader edits shared → %d", code)
	}
	// another org: not found, for everything
	if code, _ := run(h.List, req("amy", "org_x", "GET", pid, "")); code != 404 {
		t.Fatalf("another org's project → %d", code)
	}
	if code, _ := run(h.Delete, req("amy", "org_x", "DELETE", mine.ID, "")); code != 404 {
		t.Fatalf("another org's view → %d", code)
	}
	// validation
	for _, bad := range []string{`{"name":""}`, `{"name":"` + strings.Repeat("x", 81) + `"}`, `{"name":"x","query":[1]}`, `{"name":"x","query":{"a":"` + strings.Repeat("y", 5000) + `"}}`} {
		if code, _ := create("amy", bad); code != 400 {
			t.Fatalf("%.40s → %d", bad, code)
		}
	}
	// deleting: own, yes; someone else's personal one isn't even found
	if code, _ := run(h.Delete, req("bo", "org_v", "DELETE", mine.ID, "")); code != 404 {
		t.Fatalf("bo deleting amy's → %d", code)
	}
	if code, _ := run(h.Delete, req("amy", "org_v", "DELETE", mine.ID, "")); code != 204 {
		t.Fatalf("own delete → %d", code)
	}
}
