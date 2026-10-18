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

// Personal keys act as their owner, whatever they're called; read-only keys only read (TL-450).
func TestPersonalAndReadOnlyKeys(t *testing.T) {
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
	if _, err := db.Exec(ctx, `INSERT INTO users (clerk_id, email) VALUES ('akp1', 'p1@k.p'), ('akp2', 'p2@k.p') ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	h := NewSystem(db)
	session := func(user string) context.Context {
		c := &clerk.SessionClaims{RegisteredClaims: clerk.RegisteredClaims{Subject: user}}
		c.ActiveOrganizationID = "org_akp"
		return clerk.ContextWithSessionClaims(ctx, c)
	}
	create := func(user, body string) (int, map[string]any) {
		rec := httptest.NewRecorder()
		h.Create(rec, httptest.NewRequest("POST", "/", strings.NewReader(body)).WithContext(session(user)))
		var out map[string]any
		json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	if code, out := create("akp1", `{"kind":"personal"}`); code != http.StatusBadRequest {
		t.Fatalf("personal key without a name: %d %v", code, out)
	}
	if code, out := create("akp1", `{"kind":"robot","name":"x"}`); code != http.StatusBadRequest {
		t.Fatalf("unknown kind: %d %v", code, out)
	}
	code, mine := create("akp1", `{"kind":"personal","name":"TRMNL","read_only":true}`)
	if code != http.StatusCreated || !strings.HasPrefix(mine["key"].(string), PrefixPersonal) || mine["kind"] != KindPersonal || mine["read_only"] != true {
		t.Fatalf("personal read-only key: %d %v", code, mine)
	}
	// the name is only a label: someone else can call theirs the same
	code, theirs := create("akp2", `{"kind":"personal","name":"TRMNL"}`)
	if code != http.StatusCreated || theirs["read_only"] != false {
		t.Fatalf("second member's TRMNL key: %d %v", code, theirs)
	}
	rec := httptest.NewRecorder()
	h.List(rec, httptest.NewRequest("GET", "/", nil).WithContext(session("akp1")))
	var list []Key
	json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list) == 0 || list[len(list)-1].Kind != KindPersonal || !list[len(list)-1].ReadOnly {
		t.Fatalf("list: %s", rec.Body)
	}

	var actor, kind string
	mw := Middleware(db, nil)(auth.Required(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		actor, kind = auth.ActorID(r.Context()), auth.APIKeyKind(r.Context())
	})))
	use := func(method, key string) int {
		actor, kind = "", ""
		req := httptest.NewRequest(method, "/", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		rec := httptest.NewRecorder()
		mw.ServeHTTP(rec, req)
		return rec.Code
	}
	// a personal key is its owner, so assignee=me is them
	if code := use("GET", mine["key"].(string)); code != http.StatusOK || actor != "akp1" || kind != KindPersonal {
		t.Fatalf("personal key read: %d actor %q kind %q", code, actor, kind)
	}
	for _, method := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		if code := use(method, mine["key"].(string)); code != http.StatusForbidden || actor != "" {
			t.Fatalf("read-only key %s: %d (handler ran: %v)", method, code, actor != "")
		}
	}
	if code := use("POST", theirs["key"].(string)); code != http.StatusOK || actor != "akp2" {
		t.Fatalf("read-write personal key POST: %d actor %q", code, actor)
	}
	// a read-only service key still works as the key's name, for reads only
	_, svc := create("akp1", `{"kind":"service","name":"akp-dashboard","read_only":true}`)
	if code := use("GET", svc["key"].(string)); code != http.StatusOK || actor != "akp-dashboard" {
		t.Fatalf("read-only service key GET: %d actor %q", code, actor)
	}
	if code := use("POST", svc["key"].(string)); code != http.StatusForbidden {
		t.Fatalf("read-only service key POST: %d", code)
	}
}
