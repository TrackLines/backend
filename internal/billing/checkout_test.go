package billing

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/clerk/clerk-sdk-go/v2"
	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5/pgxpool"
)

func signedIn(r *http.Request, id string) *http.Request {
	return r.WithContext(clerk.ContextWithSessionClaims(r.Context(), &clerk.SessionClaims{RegisteredClaims: clerk.RegisteredClaims{Subject: id}}))
}

func TestCheckoutUnconfigured(t *testing.T) {
	rec := httptest.NewRecorder()
	Service{}.Checkout(rec, signedIn(httptest.NewRequest("POST", "/", nil), "u"))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d", rec.Code)
	}
}

// PostgreSQL is provisioned by this package's TestMain.
func TestCheckout(t *testing.T) {
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
	if _, err := db.Exec(ctx, `INSERT INTO users (clerk_id, email) VALUES ('k1', 'a@b.c'), ('k2', 'p@b.c') ON CONFLICT DO NOTHING;
		UPDATE users SET stripe_status = 'active' WHERE clerk_id = 'k2'`); err != nil {
		t.Fatal(err)
	}
	stripeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		switch r.URL.Path {
		case "/v1/customers":
			_, _ = w.Write([]byte(`{"id":"cus_k1"}`))
		case "/v1/checkout/sessions":
			if r.Form.Get("customer") != "cus_k1" || r.Form.Get("line_items[0][price]") != "price_1" ||
				r.Form.Get("success_url") != "http://app/settings?billing=success" {
				t.Errorf("bad session form: %v", r.Form)
			}
			_, _ = w.Write([]byte(`{"url":"https://checkout.stripe.com/c/1"}`))
		}
	}))
	defer stripeSrv.Close()
	s := Service{DB: db, Stripe: Stripe{SecretKey: "sk", BaseURL: stripeSrv.URL}, PriceID: "price_1", WebhookSecret: "whsec", ReturnURL: "http://app/settings/"}

	rec := httptest.NewRecorder()
	s.Checkout(rec, signedIn(httptest.NewRequest("POST", "/", nil), "k1"))
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != http.StatusOK || body["url"] != "https://checkout.stripe.com/c/1" {
		t.Fatalf("checkout: %d %s", rec.Code, rec.Body)
	}
	rec = httptest.NewRecorder()
	s.Checkout(rec, signedIn(httptest.NewRequest("POST", "/", nil), "k2"))
	if rec.Code != http.StatusConflict {
		t.Fatalf("paid user: got %d", rec.Code)
	}
}
