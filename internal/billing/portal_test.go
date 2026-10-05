package billing

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Needs a throwaway db: TEST_DATABASE_URL=postgres://... go test ./internal/billing/
func TestPortalAndStatus(t *testing.T) {
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
	if _, err := db.Exec(ctx, `INSERT INTO users (clerk_id, email, stripe_customer_id) VALUES ('p0', 'n@b.c', NULL), ('p1', 'c@b.c', 'cus_portal') ON CONFLICT DO NOTHING`); err != nil {
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
	if rec.Body.String() != `{"board_limit":1,"paid":false}`+"\n" {
		t.Fatalf("status: %s", rec.Body)
	}
}
