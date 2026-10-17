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
	// org_p1 pays through Stripe; org_p2 has Pro granted by hand (no subscription); org_p0 is free
	if _, err := db.Exec(ctx, `INSERT INTO org_billing (org_id, stripe_customer_id, stripe_subscription_id, stripe_status)
		VALUES ('org_p1', 'cus_portal', 'sub_1', 'active'), ('org_p2', 'cus_stale', NULL, 'active') ON CONFLICT DO NOTHING`); err != nil {
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
	s := Service{DB: db, Stripe: Stripe{SecretKey: "sk", BaseURL: stripeSrv.URL}, ProPriceID: "p", WebhookSecret: "w", ReturnURL: "http://app/settings",
		IsAdmin: adminsExcept("member")}

	rec := httptest.NewRecorder()
	s.Portal(rec, inOrg(httptest.NewRequest("POST", "/", nil), "admin", "org_p1"))
	if rec.Code != 200 || !json.Valid(rec.Body.Bytes()) {
		t.Fatalf("portal: %d %s", rec.Code, rec.Body)
	}
	// a member can't manage the org's billing (TL-243)
	rec = httptest.NewRecorder()
	s.Portal(rec, inOrg(httptest.NewRequest("POST", "/", nil), "member", "org_p1"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a member opened the portal: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	s.Portal(rec, inOrg(httptest.NewRequest("POST", "/", nil), "admin", "org_p0"))
	if rec.Code != http.StatusConflict {
		t.Fatalf("no customer: %d", rec.Code)
	}
	// granted by hand: paid, nothing billed, and the portal says so instead of asking Stripe
	rec = httptest.NewRecorder()
	s.Portal(rec, inOrg(httptest.NewRequest("POST", "/", nil), "admin", "org_p2"))
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "isn't billed through Stripe") {
		t.Fatalf("granted plan portal: %d %s", rec.Code, rec.Body)
	}
	// everyone in the org sees its plan; only admins may manage it
	for _, c := range []struct{ user, org, want string }{
		{"admin", "org_p0", `{"billed":false,"can_manage":true,"member_limit":20,"paid":false,"plan":"free","project_limit":1}`},
		{"admin", "org_p1", `{"billed":true,"can_manage":true,"member_limit":-1,"paid":true,"plan":"enterprise","project_limit":-1}`},
		{"member", "org_p1", `{"billed":true,"can_manage":false,"member_limit":-1,"paid":true,"plan":"enterprise","project_limit":-1}`},
		{"admin", "org_p2", `{"billed":false,"can_manage":true,"member_limit":-1,"paid":true,"plan":"enterprise","project_limit":-1}`},
	} {
		rec = httptest.NewRecorder()
		s.Status(rec, inOrg(httptest.NewRequest("GET", "/", nil), c.user, c.org))
		if rec.Body.String() != c.want+"\n" {
			t.Fatalf("status %s in %s: %s", c.user, c.org, rec.Body)
		}
	}
}
