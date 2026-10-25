package webhooks

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tracklines/backend/internal/notifications"
)

// receiver is the org's endpoint: it records what arrives and answers with status.
type receiver struct {
	mu     sync.Mutex
	status int
	got    []*http.Request
	bodies [][]byte
}

func (rc *receiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	rc.mu.Lock()
	rc.got, rc.bodies = append(rc.got, r), append(rc.bodies, b)
	status := rc.status
	rc.mu.Unlock()
	w.WriteHeader(status)
	_, _ = w.Write([]byte("thanks " + strings.Repeat("x", 3000)))
}

func (rc *receiver) count() int { rc.mu.Lock(); defer rc.mu.Unlock(); return len(rc.got) }

func TestDelivery(t *testing.T) {
	ctx := context.Background()
	h := setupOrgs(t)
	db := h.store.DB
	rc := &receiver{status: 200}
	srv := httptest.NewTLSServer(rc)
	defer srv.Close()
	// the receiver is on 127.0.0.1: the test trusts it; production's client refuses it (safehttp)
	w := Worker{DB: db, Box: h.store.Box, Client: srv.Client(), Notify: notifications.Store{DB: db}}
	if _, err := db.Exec(ctx, `DELETE FROM notifications WHERE org_id = 'wh1'`); err != nil {
		t.Fatal(err)
	}
	e, secret, err := h.store.Create(ctx, "wh1", "user_admin", srv.URL+"/hook", []string{"ticket.created"})
	if err != nil {
		t.Fatal(err)
	}
	delivery := func() Delivery {
		t.Helper()
		ds, _, err := h.store.Deliveries(ctx, "wh1", e.ID, 1)
		if err != nil || len(ds) == 0 {
			t.Fatalf("deliveries: %v", err)
		}
		return ds[0]
	}
	tick := func() {
		t.Helper()
		if _, err := w.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	due := func() { // the next retry, now
		t.Helper()
		if _, err := db.Exec(ctx, `UPDATE webhook_outbox SET next_attempt_at = now() WHERE state = 'pending' AND endpoint_id = $1`, e.ID); err != nil {
			t.Fatal(err)
		}
	}

	// a ping arrives signed: the receiver can check it with the secret
	if err := h.store.Ping(ctx, "wh1", e.ID); err != nil {
		t.Fatal(err)
	}
	tick()
	if rc.count() != 1 {
		t.Fatalf("sent %d", rc.count())
	}
	req, body := rc.got[0], rc.bodies[0]
	d := delivery()
	if req.Header.Get("Content-Type") != "application/json" || req.Header.Get("User-Agent") != "TrackLin.es-Webhooks" ||
		req.Header.Get("TrackLines-Event") != "ping" || req.Header.Get("TrackLines-Delivery") != d.ID || req.URL.Path != "/hook" {
		t.Fatalf("headers: %v %s", req.Header, req.URL)
	}
	ts, v1, _ := strings.Cut(strings.TrimPrefix(req.Header.Get("TrackLines-Signature"), "t="), ",v1=")
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "." + string(body)))
	if v1 != hex.EncodeToString(mac.Sum(nil)) {
		t.Fatalf("signature %s doesn't verify", req.Header.Get("TrackLines-Signature"))
	}
	var env struct{ Data map[string]string }
	if err := json.Unmarshal(body, &env); err != nil || env.Data["message"] != "ping" || string(body) != d.RequestBody {
		t.Fatalf("body %s; shown as %s", body, d.RequestBody)
	}
	if d.State != "ok" || d.Attempts != 1 || d.StatusCode == nil || *d.StatusCode != 200 || d.NextAttemptAt != nil || d.DurationMS == nil ||
		d.ResponseBody == nil || len(*d.ResponseBody) != maxResponse || d.Error != nil {
		t.Fatalf("delivered: %+v", d)
	}

	// failing: retried after 1m, 5m, 30m, 2h, 12h, then failed (failures reset here so it stays on)
	rc.status = 500
	if err := h.store.Ping(ctx, "wh1", e.ID); err != nil {
		t.Fatal(err)
	}
	for i, wait := range Backoff {
		tick()
		d = delivery()
		var secs float64
		_ = db.QueryRow(ctx, `SELECT EXTRACT(EPOCH FROM next_attempt_at - now())::float8 FROM webhook_outbox WHERE id = $1`, d.ID).Scan(&secs)
		left := time.Duration(secs * float64(time.Second))
		if d.State != "pending" || d.Attempts != i+1 || *d.StatusCode != 500 || left < wait-5*time.Second || left > wait {
			t.Fatalf("attempt %d: %+v, next in %s", i+1, d, left)
		}
		if _, err := db.Exec(ctx, `UPDATE webhook_endpoints SET consecutive_failures = 0 WHERE id = $1`, e.ID); err != nil {
			t.Fatal(err)
		}
		due()
	}
	tick()
	if d = delivery(); d.State != "failed" || d.Attempts != 6 || d.NextAttemptAt != nil {
		t.Fatalf("after the last attempt: %+v", d)
	}
	sent := rc.count()
	tick()
	if rc.count() != sent {
		t.Fatal("a failed delivery was sent again")
	}

	// redeliver: the same payload, pending from now with its retries afresh; then a success resets the failures
	rc.status = 204
	if _, err := db.Exec(ctx, `UPDATE webhook_endpoints SET consecutive_failures = 3 WHERE id = $1`, e.ID); err != nil {
		t.Fatal(err)
	}
	if rec := call(as("user_admin", "wh1"), h.Redeliver, "POST", "", e.ID, d.ID); rec.Code != 202 {
		t.Fatalf("redeliver: %d", rec.Code)
	}
	if got := delivery(); got.State != "pending" || got.Attempts != 0 {
		t.Fatalf("redelivering: %+v", got)
	}
	tick()
	if got := delivery(); got.State != "ok" || got.Attempts != 1 || string(rc.bodies[len(rc.bodies)-1]) != d.RequestBody {
		t.Fatalf("redelivered: %+v", got)
	}
	if list, _ := h.store.List(ctx, "wh1"); list[0].ConsecutiveFailures != 0 {
		t.Fatalf("success didn't reset failures: %+v", list[0])
	}

	// 5 failed attempts in a row turn the endpoint off and tell the org's admins
	rc.status = 503
	for i := 0; i < DisableAfter; i++ {
		if err := h.store.Ping(ctx, "wh1", e.ID); err != nil {
			t.Fatal(err)
		}
		tick()
	}
	list, _ := h.store.List(ctx, "wh1")
	if list[0].Enabled || list[0].DisabledReason == nil || *list[0].DisabledReason != "failures" || list[0].ConsecutiveFailures != DisableAfter {
		t.Fatalf("not disabled: %+v", list[0])
	}
	var kind, text string
	if err := db.QueryRow(ctx, `SELECT kind, text FROM notifications WHERE org_id = 'wh1' AND user_id = 'user_admin'`).Scan(&kind, &text); err != nil ||
		kind != "webhook_disabled" || !strings.Contains(text, "127.0.0.1") {
		t.Fatalf("admin notification: %q %q %v", kind, text, err)
	}
	// while off, nothing is sent; its due deliveries wait for it to be turned on
	sent = rc.count()
	due()
	tick()
	if rc.count() != sent {
		t.Fatal("sent to a disabled endpoint")
	}

	// a month on, deliveries are cleared out
	if _, err := db.Exec(ctx, `UPDATE webhook_outbox SET created_at = now() - interval '31 days' WHERE id = $1`, d.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := w.Cleanup(ctx, time.Now()); err != nil || n != 1 {
		t.Fatalf("cleanup: %d %v", n, err)
	}

	// production's client won't reach a private address, whatever the URL says
	w.Client = nil
	if _, err := db.Exec(ctx, `UPDATE webhook_endpoints SET enabled = true, disabled_reason = NULL, consecutive_failures = 0 WHERE id = $1`, e.ID); err != nil {
		t.Fatal(err)
	}
	sent = rc.count()
	if err := h.store.Ping(ctx, "wh1", e.ID); err != nil {
		t.Fatal(err)
	}
	tick()
	if d = delivery(); rc.count() != sent || d.Error == nil || !strings.Contains(*d.Error, "private") {
		t.Fatalf("safehttp: %+v", d)
	}
}

// Two workers at once never send the same delivery.
func TestClaimSkipsLocked(t *testing.T) {
	ctx := context.Background()
	h := setupOrgs(t)
	rc := &receiver{status: 200}
	srv := httptest.NewTLSServer(rc)
	defer srv.Close()
	e, _, err := h.store.Create(ctx, "wh2", "user_other", srv.URL, []string{"ticket.created"})
	if err != nil {
		t.Fatal(err)
	}
	const n = 60
	for range n {
		if err := h.store.Ping(ctx, "wh2", e.ID); err != nil {
			t.Fatal(err)
		}
	}
	w := Worker{DB: h.store.DB, Box: h.store.Box, Client: srv.Client()}
	var wg sync.WaitGroup
	for range 3 {
		wg.Go(func() {
			if _, err := w.Tick(ctx); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	seen := map[string]int{}
	for _, r := range rc.got {
		seen[r.Header.Get("TrackLines-Delivery")]++
	}
	if len(rc.got) != n || len(seen) != n {
		t.Fatalf("sent %d requests for %d deliveries", len(rc.got), len(seen))
	}
}
