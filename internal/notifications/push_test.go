package notifications

import (
	"context"
	"encoding/json"
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

func TestValidEndpoint(t *testing.T) {
	for _, ok := range []string{"https://fcm.googleapis.com/fcm/send/abc", "https://updates.push.services.mozilla.com/wpush/v2/x", "https://web.push.apple.com/Q", "https://wns2-par02p.notify.windows.com/w/?token=x"} {
		if !ValidEndpoint(ok) {
			t.Errorf("should accept %s", ok)
		}
	}
	// the server POSTs to it, so nothing else: no internal addresses, http, ports or look-alikes
	for _, bad := range []string{"http://fcm.googleapis.com/x", "https://169.254.169.254/latest", "https://localhost/x", "https://evil.com/fcm.googleapis.com",
		"https://fcm.googleapis.com.evil.com/x", "https://fcm.googleapis.com:8443/x", "https://user@fcm.googleapis.com/x", "notaurl"} {
		if ValidEndpoint(bad) {
			t.Errorf("should refuse %s", bad)
		}
	}
}

func TestPush(t *testing.T) {
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
	if _, err := db.Exec(ctx, `DELETE FROM push_subscriptions WHERE user_id IN ('user_ava', 'user_bo'); DELETE FROM notifications WHERE org_id = 'pu1';
		DELETE FROM projects WHERE owner_clerk_id = 'pu1'`); err != nil {
		t.Fatal(err)
	}
	var pid, tid string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name, key_prefix) VALUES ('pu1', 'Push', 'PU') RETURNING id`).Scan(&pid)
	_ = db.QueryRow(ctx, `INSERT INTO tickets (project_id, created_by, title, position) VALUES ($1, 'user_ava', 'Fix login', 0) RETURNING id`, pid).Scan(&tid)

	push := NewPush(db, PushConfig{PublicKey: "BPub", PrivateKey: "priv", Subject: "mailto:t@example.com"})
	push.People = func(context.Context, string) ([]Person, error) {
		return []Person{{ID: "user_bo", Names: []string{"Bo Smith", "Bo"}}}, nil
	}
	var mu sync.Mutex
	sent := map[string]Message{}
	push.send = func(_ context.Context, s subscription, payload []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		var msg Message
		_ = json.Unmarshal(payload, &msg)
		sent[s.endpoint] = msg
		if strings.Contains(s.endpoint, "gone") {
			return http.StatusGone, nil
		}
		return http.StatusCreated, nil
	}
	h := NewSystem(Store{DB: db}).WithPush(push)
	call := func(c context.Context, handler http.HandlerFunc, method, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/", strings.NewReader(body)).WithContext(c)
		rec := httptest.NewRecorder()
		handler(rec, req)
		return rec
	}
	ava := person("user_ava", "pu1")

	// the key browsers subscribe with
	if rec := call(ava, h.PushKey, "GET", ""); !strings.Contains(rec.Body.String(), `"public_key":"BPub"`) || !strings.Contains(rec.Body.String(), `"enabled":true`) {
		t.Fatalf("key: %s", rec.Body)
	}
	if rec := call(ava, NewSystem(Store{DB: db}).PushKey, "GET", ""); strings.TrimSpace(rec.Body.String()) != `{"enabled":false}` {
		t.Fatalf("not configured: %s", rec.Body)
	}
	// Ava turns it on for two devices (one has since expired); agents and bad endpoints can't subscribe
	sub := func(endpoint string) string {
		return `{"endpoint":"` + endpoint + `","keys":{"p256dh":"BNcR","auth":"tBHI"}}`
	}
	for _, e := range []string{"https://fcm.googleapis.com/fcm/send/ava-phone", "https://updates.push.services.mozilla.com/wpush/ava-gone"} {
		if rec := call(ava, h.Subscribe, "POST", sub(e)); rec.Code != 204 {
			t.Fatalf("subscribe %s: %d %s", e, rec.Code, rec.Body)
		}
	}
	if rec := call(ava, h.Subscribe, "POST", sub("https://169.254.169.254/x")); rec.Code != 400 {
		t.Fatalf("internal endpoint: %d", rec.Code)
	}
	if rec := call(auth.WithAPIKeyUser(ctx, "pu1", "Claude"), h.Subscribe, "POST", sub("https://fcm.googleapis.com/fcm/send/agent")); rec.Code != 403 {
		t.Fatalf("api key: %d", rec.Code)
	}
	if rec := call(ava, NewSystem(Store{DB: db}).Subscribe, "POST", sub("https://fcm.googleapis.com/fcm/send/x")); rec.Code != 503 {
		t.Fatalf("not configured: %d", rec.Code)
	}

	// Bo assigns Ava a ticket: the inbox and both her devices; the expired one is dropped
	notify := Multi{Store{DB: db}, push}
	if err := notify.Notify(ctx, "pu1", []string{"user_ava", "user_bo"}, Event{Kind: "assigned", Text: "assigned you PU-1: Fix login", Actor: "user_bo", TicketID: tid}); err != nil {
		t.Fatal(err)
	}
	push.Wait()
	phone := sent["https://fcm.googleapis.com/fcm/send/ava-phone"]
	if phone.Body != "Bo Smith assigned you PU-1: Fix login" || phone.URL != "/tickets/"+tid /* the id, not the key (TL-276) */ || phone.Tag != "assigned:PU-1" {
		t.Fatalf("message: %+v", phone)
	}
	var devices, inbox int
	_ = db.QueryRow(ctx, `SELECT count(*) FROM push_subscriptions WHERE user_id = 'user_ava'`).Scan(&devices)
	_ = db.QueryRow(ctx, `SELECT count(*) FROM notifications WHERE org_id = 'pu1' AND user_id = 'user_ava'`).Scan(&inbox)
	if devices != 1 || inbox != 1 || len(sent) != 2 {
		t.Fatalf("devices %d (gone dropped), inbox %d, sent %d", devices, inbox, len(sent))
	}
	// Bo did it, so nothing went to Bo; agents are named as themselves
	sent = map[string]Message{}
	_ = push.Notify(ctx, "pu1", []string{"user_ava"}, Event{Kind: "review", Text: "finished PU-1: Fix login, and it needs your review", Actor: "Claude", TicketID: tid})
	push.Wait()
	if got := sent["https://fcm.googleapis.com/fcm/send/ava-phone"].Body; got != "Claude finished PU-1: Fix login, and it needs your review" {
		t.Fatalf("agent actor: %q", got)
	}
	// turning it off on a device (only your own)
	if rec := call(person("user_bo", "pu1"), h.Unsubscribe, "DELETE", sub("https://fcm.googleapis.com/fcm/send/ava-phone")); rec.Code != 204 {
		t.Fatalf("other's unsubscribe: %d", rec.Code)
	}
	_ = db.QueryRow(ctx, `SELECT count(*) FROM push_subscriptions WHERE user_id = 'user_ava'`).Scan(&devices)
	if devices != 1 {
		t.Fatal("someone else removed Ava's device")
	}
	_ = call(ava, h.Unsubscribe, "DELETE", sub("https://fcm.googleapis.com/fcm/send/ava-phone"))
	_ = db.QueryRow(ctx, `SELECT count(*) FROM push_subscriptions WHERE user_id = 'user_ava'`).Scan(&devices)
	if devices != 0 {
		t.Fatal("unsubscribe")
	}
}
