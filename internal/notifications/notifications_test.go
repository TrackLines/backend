package notifications

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"testing"

	"github.com/clerk/clerk-sdk-go/v2"
	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMentioned(t *testing.T) {
	people := []Person{{ID: "user_ava", Names: []string{"Ava Smith", "Ava"}}, {ID: "user_bo", Names: []string{"bo@example.com", "bo"}}, {ID: "user_al", Names: []string{"Al"}}}
	got := Mentioned("Thanks @ava smith, and @Bo, can you check? cc @alan", people)
	if !reflect.DeepEqual(got, []string{"user_ava", "user_bo"}) {
		t.Fatalf("mentioned: %v", got) // @alan is not @Al
	}
	if got := Mentioned("email me at x@ava.com", people); len(got) != 0 {
		t.Fatalf("not a mention: %v", got)
	}
}

func person(user, org string) context.Context {
	return clerk.ContextWithSessionClaims(context.Background(), &clerk.SessionClaims{RegisteredClaims: clerk.RegisteredClaims{Subject: user}, Claims: clerk.Claims{ActiveOrganizationID: org}})
}

func TestInbox(t *testing.T) {
	ctx := context.Background()
	url := os.Getenv("TEST_DATABASE_URL")
	m, err := migrate.New("file://../migrations", "pgx5"+url[len("postgres"):])
	if err != nil {
		t.Fatal(err)
	}
	_ = m.Up()
	db, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(ctx, `DELETE FROM notifications WHERE org_id IN ('nt1', 'nt2'); DELETE FROM org_admins WHERE org_id = 'nt1'`); err != nil {
		t.Fatal(err)
	}
	s := Store{DB: db}
	// people only, once each, never the actor
	if err := s.Notify(ctx, "nt1", []string{"user_ava", "user_ava", "Claude", "user_bo"}, Event{Kind: "assigned", Text: "assigned you TL-1: A", Actor: "user_bo"}); err != nil {
		t.Fatal(err)
	}
	_ = s.Notify(ctx, "nt1", []string{"user_ava"}, Event{Kind: "mention", Text: "mentioned you on TL-2: B", Actor: "user_bo"})
	_ = s.Notify(ctx, "nt2", []string{"user_ava"}, Event{Kind: "assigned", Text: "other org", Actor: "user_cy"})
	var n int
	_ = db.QueryRow(ctx, `SELECT count(*) FROM notifications WHERE org_id = 'nt1'`).Scan(&n)
	if n != 2 {
		t.Fatalf("rows: %d", n)
	}

	h := NewSystem(s)
	call := func(as context.Context, handler http.HandlerFunc, method, path, id string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil).WithContext(as)
		req.SetPathValue("id", id)
		rec := httptest.NewRecorder()
		handler(rec, req)
		return rec
	}
	ava := person("user_ava", "nt1")
	count := func() int {
		var out map[string]int
		_ = json.Unmarshal(call(ava, h.UnreadCount, "GET", "/", "").Body.Bytes(), &out)
		return out["count"]
	}
	if count() != 2 {
		t.Fatalf("unread: %d", count())
	}
	var list []Notification
	_ = json.Unmarshal(call(ava, h.List, "GET", "/", "").Body.Bytes(), &list)
	if len(list) != 2 || list[0].Kind != "mention" || list[0].ReadAt != nil {
		t.Fatalf("inbox (newest first, this org only): %+v", list)
	}
	// someone else can't read Ava's
	if rec := call(person("user_bo", "nt1"), h.Read, "POST", "/", list[0].ID); rec.Code != 404 {
		t.Fatalf("other's read: %d", rec.Code)
	}
	if rec := call(ava, h.Read, "POST", "/", list[0].ID); rec.Code != 204 || count() != 1 {
		t.Fatalf("read one: %d, unread %d", rec.Code, count())
	}
	// unread first, then newest
	_ = json.Unmarshal(call(ava, h.List, "GET", "/", "").Body.Bytes(), &list)
	if list[0].Kind != "assigned" || list[1].ReadAt == nil {
		t.Fatalf("unread first: %+v", list)
	}
	if rec := call(ava, h.ReadAll, "POST", "/", ""); rec.Code != 204 || count() != 0 {
		t.Fatalf("read all: %d, unread %d", rec.Code, count())
	}
	// the other org's notification is still unread there
	var other map[string]int
	_ = json.Unmarshal(call(person("user_ava", "nt2"), h.UnreadCount, "GET", "/", "").Body.Bytes(), &other)
	if other["count"] != 1 {
		t.Fatalf("other org: %v", other)
	}

	// board managers: admins and the board team's leaders
	if _, err := db.Exec(ctx, `INSERT INTO org_admins (org_id, user_clerk_id, granted_by) VALUES ('nt1', 'user_admin', 'test')`); err != nil {
		t.Fatal(err)
	}
	if got, err := s.BoardManagers(ctx, "nt1", "00000000-0000-0000-0000-000000000000"); err != nil || !reflect.DeepEqual(got, []string{"user_admin"}) {
		t.Fatalf("managers: %v %v", got, err)
	}
}
