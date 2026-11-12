package billing

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
)

func launchDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Fatal("TEST_DATABASE_URL not set; package TestMain should provision PostgreSQL")
	}
	m, err := migrate.New("file://../migrations", "pgx5"+url[len("postgres"):])
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatal(err)
	}
	db, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	return db
}

// TL-774: the free year of Pro, behind launch-offer, only for orgs it was sent to, claimed once.
func TestLaunchOffer(t *testing.T) {
	ctx := context.Background()
	db := launchDB(t)
	if _, err := db.Exec(ctx, `DELETE FROM launch_offers WHERE org_id LIKE 'org_lo%';
		DELETE FROM org_billing WHERE org_id LIKE 'org_lo%';
		INSERT INTO users (clerk_id, email) VALUES ('lo_admin', 'lo@b.c') ON CONFLICT DO NOTHING;
		INSERT INTO launch_offers (org_id, offered_by) VALUES ('org_lo1', 'staff'), ('org_lo_paid', 'staff'), ('org_lo_fail', 'staff');
		INSERT INTO org_billing (org_id, stripe_status, plan) VALUES ('org_lo_paid', 'active', 'pro')`); err != nil {
		t.Fatal(err)
	}
	var fail atomic.Bool
	var subs atomic.Int32
	stripeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		switch r.URL.Path {
		case "/v1/customers":
			_, _ = w.Write([]byte(`{"id":"cus_` + r.Form.Get("metadata[org_id]") + `"}`))
		case "/v1/subscriptions":
			if fail.Load() {
				w.WriteHeader(http.StatusBadGateway)
				_, _ = w.Write([]byte(`{"error":{"type":"api_error"}}`))
				return
			}
			subs.Add(1)
			if r.Form.Get("items[0][price]") != "price_pro" || r.Form.Get("trial_period_days") != "365" ||
				r.Form.Get("trial_settings[end_behavior][missing_payment_method]") != "cancel" || r.Form.Get("metadata[launch_offer]") != "1" ||
				r.Form.Get("customer") != "cus_"+r.Form.Get("metadata[org_id]") || r.Header.Get("Idempotency-Key") == "" {
				t.Errorf("bad subscription form: %v", r.Form)
			}
			end := time.Now().Add(365 * 24 * time.Hour).Unix()
			_, _ = w.Write([]byte(`{"id":"sub_` + r.Form.Get("metadata[org_id]") + `","trial_end":` + jsonInt(end) + `}`))
		default:
			t.Errorf("unexpected stripe call %s", r.URL.Path)
		}
	}))
	defer stripeSrv.Close()
	on := true
	s := Service{DB: db, Stripe: Stripe{SecretKey: "sk", BaseURL: stripeSrv.URL}, ProPriceID: "price_pro", WebhookSecret: "whsec", ReturnURL: "http://app/settings",
		IsAdmin: adminsExcept("lo_member"), LaunchOfferOn: func() bool { return on }}
	call := func(method, user, org string) (int, LaunchOffer) {
		rec := httptest.NewRecorder()
		req := inOrg(httptest.NewRequest(method, "/", nil), user, org)
		if method == http.MethodGet {
			s.LaunchOfferStatus(rec, req)
		} else {
			s.ClaimLaunchOffer(rec, req)
		}
		var o LaunchOffer
		_ = json.Unmarshal(rec.Body.Bytes(), &o)
		return rec.Code, o
	}

	// flag off: there's no offer to see or claim
	on = false
	if code, _ := call(http.MethodGet, "lo_admin", "org_lo1"); code != http.StatusNotFound {
		t.Fatalf("flag off GET: %d", code)
	}
	if code, _ := call(http.MethodPost, "lo_admin", "org_lo1"); code != http.StatusNotFound {
		t.Fatalf("flag off POST: %d", code)
	}
	on = true

	// not sent to this org: nothing to claim
	if code, o := call(http.MethodGet, "lo_admin", "org_lo_unsent"); code != http.StatusOK || o.Eligible || o.Claimed {
		t.Fatalf("unsent org: %d %+v", code, o)
	}
	if code, _ := call(http.MethodPost, "lo_admin", "org_lo_unsent"); code != http.StatusForbidden {
		t.Fatalf("unsent org claimed: %d", code)
	}
	// a member sees it, but asks an admin
	if code, o := call(http.MethodGet, "lo_member", "org_lo1"); code != http.StatusOK || !o.Eligible || o.Admin {
		t.Fatalf("member view: %d %+v", code, o)
	}
	if code, _ := call(http.MethodPost, "lo_member", "org_lo1"); code != http.StatusForbidden {
		t.Fatalf("member claimed: %d", code)
	}
	// already paying: no free year on top
	if code, o := call(http.MethodGet, "lo_admin", "org_lo_paid"); code != http.StatusOK || o.Eligible {
		t.Fatalf("paid org offered: %d %+v", code, o)
	}
	if code, _ := call(http.MethodPost, "lo_admin", "org_lo_paid"); code != http.StatusConflict {
		t.Fatalf("paid org claimed: %d", code)
	}

	// the admin claims: Pro at once, trialing, a year out
	code, o := call(http.MethodPost, "lo_admin", "org_lo1")
	if code != http.StatusOK || !o.Claimed || o.TrialEndsAt == nil || o.TrialEndsAt.Before(time.Now().Add(364*24*time.Hour)) {
		t.Fatalf("claim: %d %+v", code, o)
	}
	if plan, err := PlanOf(ctx, db, "org_lo1"); err != nil || plan != Pro {
		t.Fatalf("plan after claim: %q %v", plan, err)
	}
	var sub, by string
	if err := db.QueryRow(ctx, `SELECT subscription_id, claimed_by FROM launch_offers WHERE org_id = 'org_lo1'`).Scan(&sub, &by); err != nil || sub != "sub_org_lo1" || by != "lo_admin" {
		t.Fatalf("claim recorded: %q %q %v", sub, by, err)
	}
	// once only, even if they ask again
	if code, _ := call(http.MethodPost, "lo_admin", "org_lo1"); code != http.StatusConflict {
		t.Fatalf("second claim: %d", code)
	}
	if code, o := call(http.MethodGet, "lo_admin", "org_lo1"); code != http.StatusOK || !o.Claimed || o.Eligible {
		t.Fatalf("after claim: %d %+v", code, o)
	}
	if subs.Load() != 1 {
		t.Fatalf("subscriptions made: %d", subs.Load())
	}

	// Stripe fails: nothing given, and the claim can be made again
	fail.Store(true)
	if code, _ := call(http.MethodPost, "lo_admin", "org_lo_fail"); code != http.StatusBadGateway {
		t.Fatalf("stripe down: %d", code)
	}
	if code, o := call(http.MethodGet, "lo_admin", "org_lo_fail"); code != http.StatusOK || !o.Eligible || o.Claimed {
		t.Fatalf("after a failed claim: %d %+v", code, o)
	}
	fail.Store(false)
	if code, _ := call(http.MethodPost, "lo_admin", "org_lo_fail"); code != http.StatusOK {
		t.Fatalf("retry: %d", code)
	}

	// an API key can't claim the org's plan
	rec := httptest.NewRecorder()
	s.ClaimLaunchOffer(rec, httptest.NewRequest(http.MethodPost, "/", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("no session: %d", rec.Code)
	}
}

func jsonInt(n int64) string { b, _ := json.Marshal(n); return string(b) }

// TL-774: the price-increase promise, 30 and 7 days before the free year ends, to the org's admins.
func TestTrialReminders(t *testing.T) {
	ctx := context.Background()
	db := launchDB(t)
	if _, err := db.Exec(ctx, `DELETE FROM launch_offers WHERE org_id LIKE 'org_tr%';
		DELETE FROM org_billing WHERE org_id LIKE 'org_tr%';
		DELETE FROM notifications WHERE org_id LIKE 'org_tr%';
		DELETE FROM org_admins WHERE org_id LIKE 'org_tr%';
		INSERT INTO org_admins (org_id, user_clerk_id, granted_by) VALUES ('org_tr30', 'tr_a', 'clerk:x'), ('org_tr30', 'tr_b', 'clerk:x'),
			('org_tr7', 'tr_c', 'clerk:x'), ('org_trgone', 'tr_d', 'clerk:x'), ('org_trlate', 'tr_e', 'clerk:x'), ('org_trfar', 'tr_f', 'clerk:x');
		INSERT INTO org_billing (org_id, stripe_subscription_id, stripe_status, plan) VALUES
			('org_tr30', 'sub_30', 'trialing', 'pro'), ('org_tr7', 'sub_7', 'trialing', 'pro'),
			('org_trgone', 'sub_gone', 'canceled', 'pro'), ('org_trlate', 'sub_late', 'trialing', 'pro'), ('org_trfar', 'sub_far', 'trialing', 'pro');
		INSERT INTO launch_offers (org_id, offered_by, claimed_at, claimed_by, subscription_id, trial_ends_at, reminded_30_at) VALUES
			('org_tr30', 's', now(), 'tr_a', 'sub_30', now() + interval '20 days', NULL),
			('org_tr7', 's', now(), 'tr_c', 'sub_7', now() + interval '5 days', now() - interval '20 days'),
			('org_trgone', 's', now(), 'tr_d', 'sub_gone', now() + interval '20 days', NULL),
			('org_trlate', 's', now(), 'tr_e', 'sub_late', now() + interval '3 days', NULL),
			('org_trfar', 's', now(), 'tr_f', 'sub_far', now() + interval '200 days', NULL)`); err != nil {
		t.Fatal(err)
	}
	count := func(org, like string) int {
		var n int
		if err := db.QueryRow(ctx, `SELECT count(*) FROM notifications WHERE org_id = $1 AND kind = 'billing' AND text LIKE $2 AND link = $3 AND NOT urgent`, org, like, LaunchOfferLink).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	r := TrialReminders{DB: db}
	for range 2 { // a second run sends nothing new
		if err := r.Once(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if n := count("org_tr30", "%30 days%"); n != 2 {
		t.Fatalf("both admins told at 30 days: %d", n)
	}
	if n := count("org_tr7", "%7 days%"); n != 1 {
		t.Fatalf("7 days: %d", n)
	}
	if n := count("org_trgone", "%"); n != 0 {
		t.Fatalf("a cancelled trial was reminded: %d", n)
	}
	if n, n30 := count("org_trlate", "%7 days%"), count("org_trlate", "%30 days%"); n != 1 || n30 != 0 {
		t.Fatalf("missed 30-day one: 7 days %d, 30 days %d", n, n30)
	}
	if n := count("org_trfar", "%"); n != 0 {
		t.Fatalf("too early: %d", n)
	}
}

// TL-825: a caller that leaves before its status is read isn't a billing failure: nothing is answered or logged.
func TestLaunchOfferStatusCallerGone(t *testing.T) {
	db := launchDB(t)
	s := Service{DB: db, LaunchOfferOn: func() bool { return true }, IsAdmin: adminsExcept("")}
	req := inOrg(httptest.NewRequest(http.MethodGet, "/", nil), "lo_admin", "org_lo_gone")
	gone, cancel := context.WithCancel(req.Context())
	cancel()
	rec := httptest.NewRecorder()
	s.LaunchOfferStatus(rec, req.WithContext(gone))
	if rec.Body.Len() != 0 || rec.Code != http.StatusOK { // nothing written: the recorder's default
		t.Fatalf("answered a caller that left: %d %s", rec.Code, rec.Body)
	}
}
