package notifications

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tracklines/backend/internal/auth"
)

// fakeDevices records what was sent; tokens containing "gone" are reported as no longer valid.
type fakeDevices struct {
	mu   sync.Mutex
	sent map[string]Message
}

func (f *fakeDevices) SendDevice(_ context.Context, token string, m Message) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent[token] = m
	return strings.Contains(token, "gone"), nil
}

func TestDevices(t *testing.T) {
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
	if _, err := db.Exec(ctx, `DELETE FROM device_tokens WHERE user_id LIKE 'user_dv%'; DELETE FROM projects WHERE owner_clerk_id = 'dv1'`); err != nil {
		t.Fatal(err)
	}
	var pid, tid string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name, key_prefix) VALUES ('dv1', 'Devices', 'DV') RETURNING id`).Scan(&pid)
	_ = db.QueryRow(ctx, `INSERT INTO tickets (project_id, created_by, title, position) VALUES ($1, 'user_dva', 'Fix login', 0) RETURNING id`, pid).Scan(&tid)

	// no VAPID keys: only the phones
	push := NewPush(db, PushConfig{})
	devices := &fakeDevices{sent: map[string]Message{}}
	push.Devices = devices
	h := NewSystem(Store{DB: db}).WithPush(push)
	call := func(c context.Context, handler http.HandlerFunc, method, target, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, target, strings.NewReader(body)).WithContext(c)
		if i := strings.LastIndex(target, "/"); method == "DELETE" {
			req.SetPathValue("token", target[i+1:])
		}
		rec := httptest.NewRecorder()
		handler(rec, req)
		return rec
	}
	ava, bo := person("user_dva", "dv1"), person("user_dvb", "dv1")
	count := func(q string, args ...any) int {
		var n int
		if err := db.QueryRow(ctx, q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// Ava registers her phone and an old install; bad input and API keys are refused
	for _, tok := range []string{"ava-phone", "ava-gone"} {
		if rec := call(ava, h.RegisterDevice, "POST", "/", `{"token":"`+tok+`","platform":"android","app_version":"1.2.0"}`); rec.Code != 204 {
			t.Fatalf("register %s: %d %s", tok, rec.Code, rec.Body)
		}
	}
	for _, bad := range []string{`{"token":"","platform":"android"}`, `{"token":"x","platform":"windows"}`, `{"token":"x"}`} {
		if rec := call(ava, h.RegisterDevice, "POST", "/", bad); rec.Code != 400 {
			t.Fatalf("%s: %d", bad, rec.Code)
		}
	}
	if rec := call(auth.WithAPIKeyUser(ctx, "dv1", "Claude"), h.RegisterDevice, "POST", "/", `{"token":"agent","platform":"ios"}`); rec.Code != 403 {
		t.Fatalf("api key: %d", rec.Code)
	}
	// registering again refreshes it rather than adding a row
	_, _ = db.Exec(ctx, `UPDATE device_tokens SET last_seen = now() - interval '30 days' WHERE token = 'ava-phone'`)
	call(ava, h.RegisterDevice, "POST", "/", `{"token":"ava-phone","platform":"android","app_version":"1.3.0"}`)
	if n := count(`SELECT count(*) FROM device_tokens WHERE token = 'ava-phone' AND app_version = '1.3.0' AND last_seen > now() - interval '1 minute'`); n != 1 {
		t.Fatalf("refresh: %d", n)
	}

	// Bo assigns Ava a ticket: both her installs are sent to, and the gone one is dropped
	if err := push.Notify(ctx, "dv1", []string{"user_dva", "user_dvb"}, Event{Kind: "assigned", Text: "assigned you DV-1: Fix login", Actor: "user_dvb", TicketID: tid}); err != nil {
		t.Fatal(err)
	}
	push.Wait()
	got := devices.sent["ava-phone"]
	if got.Body != "Someone assigned you DV-1: Fix login" || got.URL != "/tickets/"+tid || got.Kind != "assigned" || got.Tag != "assigned:DV-1" {
		t.Fatalf("message: %+v", got)
	}
	if len(devices.sent) != 2 || count(`SELECT count(*) FROM device_tokens WHERE user_id = 'user_dva'`) != 1 {
		t.Fatalf("sent %d; gone token should be dropped", len(devices.sent))
	}

	// only your own device can be removed
	call(bo, h.UnregisterDevice, "DELETE", "/api/push/devices/ava-phone", "")
	if count(`SELECT count(*) FROM device_tokens WHERE token = 'ava-phone'`) != 1 {
		t.Fatal("Bo removed Ava's device")
	}
	// a phone passed on: whoever signs in next takes the token, so it stops getting Ava's pushes
	call(bo, h.RegisterDevice, "POST", "/", `{"token":"ava-phone","platform":"android"}`)
	if count(`SELECT count(*) FROM device_tokens WHERE token = 'ava-phone' AND user_id = 'user_dvb'`) != 1 {
		t.Fatal("token didn't move to the new person")
	}
	if rec := call(bo, h.UnregisterDevice, "DELETE", "/api/push/devices/ava-phone", ""); rec.Code != 204 || count(`SELECT count(*) FROM device_tokens WHERE token = 'ava-phone'`) != 0 {
		t.Fatalf("unregister: %d", rec.Code)
	}
}
