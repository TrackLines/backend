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

func signedIn(r *http.Request, id string) *http.Request { return inOrg(r, id, "") }

// inOrg signs id in with org active; the plan is the org's (TL-243).
func inOrg(r *http.Request, id, org string) *http.Request {
	c := &clerk.SessionClaims{RegisteredClaims: clerk.RegisteredClaims{Subject: id}}
	c.ActiveOrganizationID = org
	return r.WithContext(clerk.ContextWithSessionClaims(r.Context(), c))
}

// adminsExcept: everyone is an org admin except the named members.
func adminsExcept(members ...string) func(context.Context, string, string) (bool, error) {
	return func(_ context.Context, _, user string) (bool, error) {
		for _, m := range members {
			if m == user {
				return false, nil
			}
		}
		return true, nil
	}
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
	if _, err := db.Exec(ctx, `INSERT INTO users (clerk_id, email) VALUES ('k1', 'a@b.c'), ('k2', 'p@b.c') ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO org_billing (org_id, stripe_status) VALUES ('org_k2', 'active') ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	stripeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		switch r.URL.Path {
		case "/v1/customers":
			_, _ = w.Write([]byte(`{"id":"cus_k1"}`))
		case "/v1/checkout/sessions":
			if r.Form.Get("customer") != "cus_k1" || r.Form.Get("line_items[0][price]") != "price_1" || r.Form.Get("client_reference_id") != "org_k1" ||
				r.Form.Get("success_url") != "http://app/settings?billing=success" {
				t.Errorf("bad session form: %v", r.Form)
			}
			_, _ = w.Write([]byte(`{"url":"https://checkout.stripe.com/c/1"}`))
		}
	}))
	defer stripeSrv.Close()
	s := Service{DB: db, Stripe: Stripe{SecretKey: "sk", BaseURL: stripeSrv.URL}, ProPriceID: "price_1", WebhookSecret: "whsec", ReturnURL: "http://app/settings/",
		IsAdmin: adminsExcept("member")}

	// only an org admin buys the org's plan (TL-243)
	rec := httptest.NewRecorder()
	s.Checkout(rec, inOrg(httptest.NewRequest("POST", "/", nil), "member", "org_k1"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a member checked out: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	s.Checkout(rec, signedIn(httptest.NewRequest("POST", "/", nil), "k1")) // no active org
	if rec.Code != http.StatusForbidden {
		t.Fatalf("no org: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	s.Checkout(rec, inOrg(httptest.NewRequest("POST", "/", nil), "k1", "org_k1"))
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != http.StatusOK || body["url"] != "https://checkout.stripe.com/c/1" {
		t.Fatalf("checkout: %d %s", rec.Code, rec.Body)
	}
	var email string
	if err := db.QueryRow(ctx, `SELECT billing_email FROM org_billing WHERE org_id = 'org_k1'`).Scan(&email); err != nil || email != "a@b.c" {
		t.Fatalf("the org is billed to the admin who paid: %q %v", email, err)
	}
	rec = httptest.NewRecorder()
	s.Checkout(rec, inOrg(httptest.NewRequest("POST", "/", nil), "k2", "org_k2"))
	if rec.Code != http.StatusConflict {
		t.Fatalf("paid org: got %d", rec.Code)
	}
}
