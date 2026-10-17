package notifications

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TL-424: a notice for everyone (org_id ”, e.g. new Terms) shows in the person's inbox whichever
// organization they're in, links where it says, and can be read and dismissed there.
func TestNoticesForEveryone(t *testing.T) {
	ctx := context.Background()
	url := os.Getenv("TEST_DATABASE_URL")
	m, _ := migrate.New("file://../migrations", "pgx5"+url[len("postgres"):])
	_ = m.Up()
	db, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(ctx, `DELETE FROM notifications WHERE user_id IN ('ev_ava', 'ev_bo');
		INSERT INTO notifications (org_id, user_id, kind, text, link) VALUES ('', 'ev_ava', 'legal', 'We''ve updated our Terms of service', '/terms/2026-11-20'), ('', 'ev_bo', 'legal', 'for bo', '/terms/2026-11-20')`); err != nil {
		t.Fatal(err)
	}
	h := NewSystem(Store{DB: db})
	call := func(user, org string, handler http.HandlerFunc, method, id string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/", nil).WithContext(person(user, org))
		req.SetPathValue("id", id)
		rec := httptest.NewRecorder()
		handler(rec, req)
		return rec
	}
	list := func(user, org string) []Notification {
		var out []Notification
		json.Unmarshal(call(user, org, h.List, "GET", "").Body.Bytes(), &out)
		return out
	}
	for _, org := range []string{"ev_org1", "ev_org2"} {
		got := list("ev_ava", org)
		if len(got) != 1 || got[0].Kind != "legal" || got[0].Link == nil || *got[0].Link != "/terms/2026-11-20" {
			t.Fatalf("in %s: %+v", org, got)
		}
	}
	var unread map[string]int
	json.Unmarshal(call("ev_ava", "ev_org1", h.UnreadCount, "GET", "").Body.Bytes(), &unread)
	if unread["count"] != 1 {
		t.Errorf("unread: %v", unread)
	}
	id := list("ev_ava", "ev_org1")[0].ID
	if rec := call("ev_ava", "ev_org2", h.Read, "POST", id); rec.Code != 204 {
		t.Errorf("read from another org: %d", rec.Code)
	}
	if rec := call("ev_bo", "ev_org1", h.Dismiss, "DELETE", id); rec.Code != 404 {
		t.Errorf("someone else dismissing it: %d", rec.Code)
	}
	if rec := call("ev_ava", "ev_org1", h.Dismiss, "DELETE", id); rec.Code != 204 || len(list("ev_ava", "ev_org1")) != 0 {
		t.Errorf("dismiss: %d", rec.Code)
	}
}
