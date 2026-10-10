package billing

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
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgreSQL is provisioned by this package's TestMain.
func TestPortalAndStatus(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Fatal("TEST_DATABASE_URL not set; package TestMain should provision PostgreSQL")
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
	if _, err := db.Exec(ctx, `INSERT INTO users (clerk_id, email, stripe_customer_id) VALUES ('p0', 'n@b.c', NULL), ('p1', 'c@b.c', 'cus_portal'), ('p2', 'o@b.c', 'cus_stale') ON CONFLICT DO NOTHING;
		UPDATE users SET stripe_subscription_id = 'sub_1', stripe_status = 'active' WHERE clerk_id = 'p1';
		UPDATE users SET stripe_status = 'active' WHERE clerk_id = 'p2'`); err != nil { // p2: Pro granted by hand, no subscription
		t.Fatal(err)
	}
	stripeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.URL.Path != "/v1/billing_portal/sessions" || r.Form.Get("customer") != "cus_portal" || r.Form.Get("return_url") != "http://app/settings" {
			t.Errorf("bad portal request: %s %v", r.URL.Path, r.Form)
		}
		_, _ = w.Write([]byte(`{"url":"https://billing.stripe.com/p/1"}`))
	}))
	defer stripeSrv.Close()
	s := Service{DB: db, Stripe: Stripe{SecretKey: "sk", BaseURL: stripeSrv.URL}, PriceID: "p", WebhookSecret: "w", ReturnURL: "http://app/settings"}

	rec := httptest.NewRecorder()
	s.Portal(rec, signedIn(httptest.NewRequest("POST", "/", nil), "p1"))
	if rec.Code != 200 || !json.Valid(rec.Body.Bytes()) {
		t.Fatalf("portal: %d %s", rec.Code, rec.Body)
	}
	rec = httptest.NewRecorder()
	s.Portal(rec, signedIn(httptest.NewRequest("POST", "/", nil), "p0"))
	if rec.Code != http.StatusConflict {
		t.Fatalf("no customer: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	s.Status(rec, signedIn(httptest.NewRequest("GET", "/", nil), "p0"))
	if rec.Body.String() != `{"billed":false,"paid":false,"project_limit":1}`+"\n" {
		t.Fatalf("status: %s", rec.Body)
	}
	// granted by hand: paid, nothing billed, and the portal says so instead of asking Stripe
	rec = httptest.NewRecorder()
	s.Portal(rec, signedIn(httptest.NewRequest("POST", "/", nil), "p2"))
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "isn't billed through Stripe") {
		t.Fatalf("granted plan portal: %d %s", rec.Code, rec.Body)
	}
	for user, want := range map[string]string{"p1": `{"billed":true,"paid":true,"project_limit":-1}`, "p2": `{"billed":false,"paid":true,"project_limit":-1}`} {
		rec = httptest.NewRecorder()
		s.Status(rec, signedIn(httptest.NewRequest("GET", "/", nil), user))
		if rec.Body.String() != want+"\n" {
			t.Fatalf("status %s: %s", user, rec.Body)
		}
	}
}
