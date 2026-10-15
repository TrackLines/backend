package billing

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Plans by tier (TL-290): Free 1 project, Pro 5, Enterprise unlimited; the subscription's price says
// which; Pro changes to Enterprise on the same subscription; the homepage gets prices from Stripe.
func TestPlanTiers(t *testing.T) {
	ctx := context.Background()
	url := os.Getenv("TEST_DATABASE_URL")
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
	if _, err := db.Exec(ctx, `DELETE FROM org_billing WHERE org_id LIKE 'org_t%';
		INSERT INTO org_billing (org_id, stripe_customer_id, stripe_subscription_id, stripe_status, plan) VALUES
			('org_tpro', 'cus_pro', 'sub_pro', 'active', 'pro'), ('org_tent', 'cus_ent', 'sub_ent', 'active', 'enterprise'),
			('org_told', 'cus_old', 'sub_old', 'active', NULL), ('org_tgone', 'cus_gone', 'sub_gone', 'canceled', 'enterprise'),
			('org_tgrant', NULL, NULL, 'active', 'enterprise')`); err != nil {
		t.Fatal(err)
	}

	// limits by plan; a live plan from before tiers was unlimited and stays so; a lapsed one is free
	for org, want := range map[string]int{"org_tpro": 5, "org_tent": -1, "org_told": -1, "org_tgone": 1, "org_tnone": 1, "org_tgrant": -1} {
		if got, err := ProjectLimit(ctx, db, org); err != nil || got != want {
			t.Errorf("%s: limit %d (%v), want %d", org, got, err, want)
		}
	}

	var mu sync.Mutex
	var changed, checkedOut []string
	stripeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/v1/subscriptions/sub_pro" && r.Method == "GET":
			_, _ = w.Write([]byte(`{"id":"sub_pro","customer":"cus_pro","status":"active","items":{"data":[{"id":"si_1","price":{"id":"price_pro"}}]}}`))
		case r.URL.Path == "/v1/subscriptions/sub_pro" && r.Method == "POST":
			changed = append(changed, r.Form.Get("items[0][id]")+"→"+r.Form.Get("items[0][price]")+" "+r.Form.Get("proration_behavior"))
			_, _ = w.Write([]byte(`{}`))
		case r.URL.Path == "/v1/subscriptions/sub_ent":
			_, _ = w.Write([]byte(`{"id":"sub_ent","customer":"cus_ent","status":"active","items":{"data":[{"id":"si_2","price":{"id":"price_ent"}}]}}`))
		case r.URL.Path == "/v1/customers":
			_, _ = w.Write([]byte(`{"id":"cus_` + r.Form.Get("metadata[org_id]") + `"}`)) // one per org
		case r.URL.Path == "/v1/checkout/sessions":
			checkedOut = append(checkedOut, r.Form.Get("line_items[0][price]"))
			_, _ = w.Write([]byte(`{"url":"https://checkout.stripe.com/c/1"}`))
		case r.URL.Path == "/v1/prices/price_pro":
			_, _ = w.Write([]byte(`{"unit_amount":1000,"currency":"gbp","recurring":{"interval":"month"}}`))
		case r.URL.Path == "/v1/prices/price_ent":
			_, _ = w.Write([]byte(`{"unit_amount":2500,"currency":"gbp","recurring":{"interval":"month"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer stripeSrv.Close()
	s := Service{DB: db, Stripe: Stripe{SecretKey: "sk", BaseURL: stripeSrv.URL}, ProPriceID: "price_pro", EnterprisePriceID: "price_ent",
		WebhookSecret: "whsec", ReturnURL: "http://app/settings", IsAdmin: adminsExcept("member")}

	// the webhook records the plan from the subscription's price
	if _, err := applySubscription(ctx, s, Subscription{ID: "sub_old", CustomerID: "cus_old", Status: "active", PriceID: "price_pro"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := PlanOf(ctx, db, "org_told"); got != Pro {
		t.Fatalf("webhook plan: %s", got)
	}

	// checkout: the chosen plan's price; nonsense refused
	post := func(h http.HandlerFunc, user, org, body string) int {
		rec := httptest.NewRecorder()
		h(rec, inOrg(httptest.NewRequest("POST", "/", strings.NewReader(body)), user, org))
		return rec.Code
	}
	if code := post(s.Checkout, "admin", "org_tnew", `{"plan":"enterprise"}`); code != 200 {
		t.Fatalf("enterprise checkout: %d", code)
	}
	if code := post(s.Checkout, "admin", "org_tnew2", ``); code != 200 { // Pro when not said
		t.Fatalf("default checkout: %d", code)
	}
	if code := post(s.Checkout, "admin", "org_tnew3", `{"plan":"platinum"}`); code != 400 {
		t.Fatalf("unknown plan: %d", code)
	}
	if strings.Join(checkedOut, ",") != "price_ent,price_pro" {
		t.Fatalf("checked out: %v", checkedOut)
	}

	// Pro → Enterprise changes the same subscription, prorated, and the new limit applies straight away
	if code := post(s.ChangePlan, "member", "org_tpro", `{"plan":"enterprise"}`); code != 403 {
		t.Fatalf("member changed plan: %d", code)
	}
	if code := post(s.ChangePlan, "admin", "org_tpro", `{"plan":"enterprise"}`); code != 204 {
		t.Fatalf("upgrade: %d", code)
	}
	if strings.Join(changed, ",") != "si_1→price_ent create_prorations" {
		t.Fatalf("stripe change: %v", changed)
	}
	if got, _ := ProjectLimit(ctx, db, "org_tpro"); got != -1 {
		t.Fatalf("limit after upgrade: %d", got)
	}
	if code := post(s.ChangePlan, "admin", "org_tent", `{"plan":"enterprise"}`); code != 204 || len(changed) != 1 { // already on it: Stripe untouched
		t.Fatalf("same plan: %d %v", code, changed)
	}
	if code := post(s.ChangePlan, "admin", "org_tgrant", `{"plan":"pro"}`); code != 409 { // granted by hand: nothing to change
		t.Fatalf("granted plan: %d", code)
	}
	if code := post(s.ChangePlan, "admin", "org_tnone", `{"plan":"enterprise"}`); code != 409 { // no subscription: check out instead
		t.Fatalf("no subscription: %d", code)
	}

	// the homepage's plans: limits, and prices from Stripe
	plansCache.list = nil
	rec := httptest.NewRecorder()
	s.Plans(rec, httptest.NewRequest("GET", "/", nil))
	var plans []PlanInfo
	_ = json.Unmarshal(rec.Body.Bytes(), &plans)
	if len(plans) != 3 || *plans[0].Projects != 1 || *plans[1].Projects != 5 || plans[2].Projects != nil ||
		plans[1].Price == nil || plans[1].Price.Amount != 1000 || plans[2].Price.Amount != 2500 || plans[2].Price.Interval != "month" {
		t.Fatalf("plans: %s", rec.Body)
	}
}
