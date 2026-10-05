package billing

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5/pgxpool"
)

func sign(payload, secret string, t time.Time) string {
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "%d.%s", t.Unix(), payload)
	return fmt.Sprintf("t=%d,v1=%s", t.Unix(), hex.EncodeToString(mac.Sum(nil)))
}

func TestVerifyWebhook(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	body := `{"type":"x"}`
	if err := VerifyWebhook([]byte(body), sign(body, "whsec", now), "whsec", now); err != nil {
		t.Fatalf("valid: %v", err)
	}
	for name, hdr := range map[string]string{
		"wrong secret": sign(body, "other", now),
		"stale":        sign(body, "whsec", now.Add(-10*time.Minute)),
		"malformed":    "garbage",
	} {
		if VerifyWebhook([]byte(body), hdr, "whsec", now) == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if VerifyWebhook([]byte(body+" "), sign(body, "whsec", now), "whsec", now) == nil {
		t.Error("tampered body accepted")
	}
}

// Needs a throwaway db: TEST_DATABASE_URL=postgres://... go test ./internal/billing/
func TestWebhook(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
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
	if _, err := db.Exec(ctx, `INSERT INTO users (clerk_id, email, stripe_customer_id) VALUES ('w1', 'w@b.c', 'cus_w1') ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	subs := map[string]string{"sub_new": `{"id":"sub_new","customer":"cus_w1","status":"active"}`,
		"sub_old": `{"id":"sub_old","customer":"cus_w1","status":"canceled"}`,
		"sub_x":   `{"id":"sub_x","customer":"cus_unknown","status":"active"}`}
	stripeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(subs[strings.TrimPrefix(r.URL.Path, "/v1/subscriptions/")]))
	}))
	defer stripeSrv.Close()
	now := time.Now()
	s := Service{DB: db, Stripe: Stripe{SecretKey: "sk", BaseURL: stripeSrv.URL}, PriceID: "p", WebhookSecret: "whsec", ReturnURL: "r"}
	h := s.Webhook(func() time.Time { return now })
	send := func(body, secret string) int {
		req := httptest.NewRequest("POST", "/", strings.NewReader(body))
		req.Header.Set("Stripe-Signature", sign(body, secret, now))
		rec := httptest.NewRecorder()
		h(rec, req)
		return rec.Code
	}
	status := func() string {
		var st string
		_ = db.QueryRow(ctx, `SELECT stripe_status FROM users WHERE clerk_id = 'w1'`).Scan(&st)
		return st
	}

	if code := send(`{"type":"checkout.session.completed","data":{"object":{"subscription":"sub_new"}}}`, "forged"); code != http.StatusBadRequest {
		t.Fatalf("forged: %d", code)
	}
	// event payload claims "canceled" but Stripe says active: we trust Stripe
	if code := send(`{"type":"checkout.session.completed","data":{"object":{"subscription":"sub_new","status":"canceled"}}}`, "whsec"); code != 200 || status() != "active" {
		t.Fatalf("checkout completed: %d %q", code, status())
	}
	if paid, _ := IsPaid(ctx, db, "w1"); !paid {
		t.Fatal("not paid after checkout")
	}
	// late event for an old canceled subscription must not downgrade the live one
	if code := send(`{"type":"customer.subscription.deleted","data":{"object":{"id":"sub_old"}}}`, "whsec"); code != 200 || status() != "active" {
		t.Fatalf("stale event: %d %q", code, status())
	}
	if code := send(`{"type":"customer.subscription.updated","data":{"object":{"id":"sub_x"}}}`, "whsec"); code != 200 {
		t.Fatalf("unknown customer: %d", code)
	}
	if code := send(`{"type":"invoice.paid","data":{"object":{"id":"in_1"}}}`, "whsec"); code != 200 {
		t.Fatalf("ignored type: %d", code)
	}
	subs["sub_new"] = `{"id":"sub_new","customer":"cus_w1","status":"canceled"}`
	if code := send(`{"type":"customer.subscription.deleted","data":{"object":{"id":"sub_new"}}}`, "whsec"); code != 200 || status() != "canceled" {
		t.Fatalf("cancel: %d %q", code, status())
	}
	if paid, _ := IsPaid(ctx, db, "w1"); paid {
		t.Fatal("still paid after cancel")
	}
}
