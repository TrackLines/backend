package tickets

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/clerk/clerk-sdk-go/v2"
	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/boards"
	"github.com/tracklines/backend/internal/notifications"
)

type announced struct{ got []notifications.Event }

func (a *announced) Notify(_ context.Context, _ string, to []string, e notifications.Event) error {
	if len(to) == 0 { // an announcement: for spaces, not people
		a.got = append(a.got, e)
	}
	return nil
}

// A new ticket is announced to the project's spaces (Google Chat, TL-288), on a board or in the
// backlog; it reaches nobody's inbox.
func TestTicketCreatedIsAnnounced(t *testing.T) {
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
	if _, err := db.Exec(ctx, `DELETE FROM notifications WHERE org_id = 'an1'; DELETE FROM boards WHERE owner_clerk_id = 'an1'; DELETE FROM projects WHERE owner_clerk_id = 'an1'`); err != nil {
		t.Fatal(err)
	}
	var pid string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name, key_prefix) VALUES ('an1', 'Shop', 'SHP') RETURNING id`).Scan(&pid)
	b, _ := boards.Store{DB: db}.Create(ctx, "an1", pid, "Backend", "")
	spaces := &announced{}
	h := NewSystem(db).WithNotifier(notifications.Multi{notifications.Store{DB: db}, spaces})
	ava := clerk.ContextWithSessionClaims(ctx, &clerk.SessionClaims{RegisteredClaims: clerk.RegisteredClaims{Subject: "user_ava"}, Claims: clerk.Claims{ActiveOrganizationID: "an1"}})
	create := func(handler func(*httptest.ResponseRecorder, string), id string) int {
		rec := httptest.NewRecorder()
		handler(rec, id)
		return rec.Code
	}
	onBoard := func(rec *httptest.ResponseRecorder, id string) {
		req := httptest.NewRequest("POST", "/", strings.NewReader(`{"title":"Fix login"}`)).WithContext(ava)
		req.SetPathValue("id", id)
		h.Create(rec, req)
	}
	inBacklog := func(rec *httptest.ResponseRecorder, id string) {
		req := httptest.NewRequest("POST", "/", strings.NewReader(`{"title":"Dark mode","type":"feature"}`)).WithContext(ava)
		req.SetPathValue("id", id)
		h.CreateBacklog(rec, req)
	}
	if create(onBoard, b.Columns[0].ID) != 201 || create(inBacklog, pid) != 201 {
		t.Fatal("create failed")
	}
	if len(spaces.got) != 2 || spaces.got[0].Kind != "created" || spaces.got[0].Text != "created SHP-1: Fix login on Backend" || spaces.got[0].BoardID != b.ID ||
		spaces.got[1].Text != "added SHP-2: Dark mode to the backlog" || spaces.got[1].Actor != "user_ava" || spaces.got[1].TicketID == "" {
		t.Fatalf("announced: %+v", spaces.got)
	}
	var inbox int
	_ = db.QueryRow(ctx, `SELECT count(*) FROM notifications WHERE org_id = 'an1'`).Scan(&inbox)
	if inbox != 0 {
		t.Fatalf("a new ticket reached %d inboxes", inbox)
	}
}
