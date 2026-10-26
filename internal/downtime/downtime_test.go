package downtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tracklines/backend/internal/billing"
	"github.com/tracklines/backend/internal/notifications"
)

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestThePolicy(t *testing.T) {
	if Qualifies(at("2026-10-17T10:00:00Z"), at("2026-10-17T11:00:00Z")) {
		t.Error("exactly an hour qualifies")
	}
	if !Qualifies(at("2026-10-17T10:00:00Z"), at("2026-10-17T11:00:01Z")) {
		t.Error("over an hour doesn't")
	}
	if d := Days(at("2026-10-17T23:30:00Z"), at("2026-10-18T00:45:00Z")); !slices.Equal(d, []string{"2026-10-17", "2026-10-18"}) {
		t.Errorf("crossing midnight: %v", d)
	}
	if d := Days(at("2026-10-17T22:00:00Z"), at("2026-10-18T00:00:00Z")); !slices.Equal(d, []string{"2026-10-17"}) {
		t.Errorf("ending at midnight: %v", d)
	}
	if d := Days(at("2026-10-17T23:30:00+02:00"), at("2026-10-18T01:30:00+02:00")); !slices.Equal(d, []string{"2026-10-17"}) {
		t.Errorf("UTC days, not local: %v", d)
	}
	c := Clusters([]Outage{
		{StartsAt: at("2026-10-17T01:00:00Z"), EndsAt: at("2026-10-17T01:10:00Z")},
		{StartsAt: at("2026-10-17T05:00:00Z"), EndsAt: at("2026-10-17T05:10:00Z")},
		{StartsAt: at("2026-10-17T09:00:00Z"), EndsAt: at("2026-10-17T09:10:00Z")}, // three in a day
		{StartsAt: at("2026-10-18T01:00:00Z"), EndsAt: at("2026-10-18T01:40:00Z")},
		{StartsAt: at("2026-10-18T05:00:00Z"), EndsAt: at("2026-10-18T05:40:00Z")}, // two, 80 minutes
		{StartsAt: at("2026-10-19T01:00:00Z"), EndsAt: at("2026-10-19T01:20:00Z")},
		{StartsAt: at("2026-10-19T05:00:00Z"), EndsAt: at("2026-10-19T05:20:00Z")}, // two, 40 minutes
	})
	if !c["2026-10-17"] || !c["2026-10-18"] || c["2026-10-19"] {
		t.Errorf("clusters: %v", c)
	}
	// £10/month: October has 31 days, so 32p; February 2027 has 28: 36p; £120/year is £10/month
	if DayPrice(billing.Price{Amount: 1000, Interval: "month"}, "2026-10-17") != 32 || DayPrice(billing.Price{Amount: 1000, Interval: "month"}, "2027-02-03") != 36 ||
		DayPrice(billing.Price{Amount: 12000, Interval: "year"}, "2026-10-17") != 32 {
		t.Error("a day's price")
	}
	for in, want := range map[string]string{"2026-10-17": "17 Oct", "2026-10-17 2026-10-18": "17 and 18 Oct", "2026-10-02 2026-09-30 2026-10-01": "30 Sep, 1 and 2 Oct"} {
		if got := DayList(strings.Fields(in)); got != want {
			t.Errorf("%s: %q", in, got)
		}
	}
}

type fakeStripe struct {
	mu      sync.Mutex
	credits []string // customer day amount currency
	fail    string   // a customer whose credit fails
}

func (f *fakeStripe) Subscription(_ context.Context, id string) (billing.Subscription, error) {
	if id == "sub_broken" {
		return billing.Subscription{}, errors.New("no such subscription")
	}
	return billing.Subscription{ID: id, Price: billing.Price{Amount: 3100, Currency: "gbp", Interval: "month"}}, nil
}

func (f *fakeStripe) Credit(_ context.Context, customer string, amount int64, currency, description, key string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if customer == f.fail {
		return "", errors.New("card_declined")
	}
	f.credits = append(f.credits, fmt.Sprintf("%s %s %d %s", customer, key[len(key)-10:], amount, currency))
	return fmt.Sprintf("cbtxn_%d", len(f.credits)), nil
}

type inbox struct{ got []string }

func (n *inbox) Notify(_ context.Context, org string, to []string, e notifications.Event) error {
	n.got = append(n.got, org+" "+strings.Join(to, ",")+": "+e.Text)
	return nil
}

func TestRecordPreviewApply(t *testing.T) {
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
	if _, err := db.Exec(ctx, `
		INSERT INTO org_billing (org_id, billing_email, stripe_customer_id, stripe_subscription_id, stripe_status, plan) VALUES
			('org_pro', 'a@x', 'cus_pro', 'sub_pro', 'active', 'pro'),
			('org_ent', 'b@x', 'cus_ent', 'sub_ent', 'active', NULL),
			('org_lapsed', 'c@x', 'cus_lapsed', 'sub_lapsed', 'canceled', 'pro'),
			('org_broken', 'd@x', 'cus_broken', 'sub_broken', 'active', 'pro'),
			('org_declined', 'e@x', 'cus_declined', 'sub_declined', 'active', 'pro');
		INSERT INTO org_billing (org_id, billing_email) VALUES ('org_free', 'f@x');
		INSERT INTO org_admins (org_id, user_clerk_id, granted_by) VALUES ('org_pro', 'admin1', 'x'), ('org_pro', 'admin2', 'x'), ('org_ent', 'admin3', 'x');`); err != nil {
		t.Fatal(err)
	}
	stripe, box := &fakeStripe{fail: "cus_declined"}, &inbox{}
	s := System{DB: db, Stripe: stripe, Notify: box, Now: func() time.Time { return at("2026-10-26T12:00:00Z") }}
	do := func(h http.HandlerFunc, method, path, id, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.SetPathValue("id", id)
		rec := httptest.NewRecorder()
		h(rec, req)
		return rec
	}
	record := func(start, end string) Outage {
		rec := do(s.Create, "POST", "/api/staff/outages", "", fmt.Sprintf(`{"starts_at":%q,"ends_at":%q,"note":"db"}`, start, end))
		var o Outage
		if rec.Code != 201 || json.Unmarshal(rec.Body.Bytes(), &o) != nil {
			t.Fatalf("record: %d %s", rec.Code, rec.Body)
		}
		return o
	}
	for _, body := range []string{`{"starts_at":"2026-10-17T11:00:00Z","ends_at":"2026-10-17T10:00:00Z"}`, `{"starts_at":"2026-10-27T10:00:00Z","ends_at":"2026-10-27T12:00:00Z"}`} {
		if rec := do(s.Create, "POST", "/api/staff/outages", "", body); rec.Code != 400 {
			t.Errorf("%s: %d", body, rec.Code)
		}
	}

	// an outage across midnight: two days for every paying org, nothing for Free or lapsed ones
	big := record("2026-10-17T23:00:00Z", "2026-10-18T00:30:00Z")
	if !big.Qualifies || big.Minutes != 90 || len(big.Days) != 2 {
		t.Fatalf("%+v", big)
	}
	rec := do(s.Preview, "GET", "/preview", big.ID, "")
	var preview struct {
		Credits  []Line
		Problems []Problem
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &preview)
	var orgs []string
	for _, l := range preview.Credits {
		orgs = append(orgs, l.Org+" "+l.Day)
		if l.Amount != 100 || l.Currency != "gbp" { // £31 in a 31-day month
			t.Errorf("amount: %+v", l)
		}
	}
	if !slices.Equal(orgs, []string{"org_declined 2026-10-17", "org_declined 2026-10-18", "org_ent 2026-10-17", "org_ent 2026-10-18", "org_pro 2026-10-17", "org_pro 2026-10-18"}) ||
		len(preview.Problems) != 1 || preview.Problems[0].Org != "org_broken" {
		t.Fatalf("preview: %v %+v", orgs, preview.Problems)
	}
	if len(stripe.credits) != 0 {
		t.Fatal("preview credited")
	}

	rec = do(s.Apply, "POST", "/apply", big.ID, `{}`)
	var applied struct {
		Credited []Line
		Problems []Problem
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &applied)
	slices.Sort(stripe.credits)
	if rec.Code != 200 || len(applied.Credited) != 4 || len(applied.Problems) != 3 || !slices.Equal(stripe.credits, []string{
		"cus_ent 2026-10-17 100 gbp", "cus_ent 2026-10-18 100 gbp", "cus_pro 2026-10-17 100 gbp", "cus_pro 2026-10-18 100 gbp"}) {
		t.Fatalf("apply: %d %+v %v", rec.Code, applied, stripe.credits)
	}
	slices.Sort(box.got)
	if !slices.Equal(box.got, []string{"org_ent admin3: credited 17 and 18 Oct for downtime: those days are free on your next bill",
		"org_pro admin1,admin2: credited 17 and 18 Oct for downtime: those days are free on your next bill"}) {
		t.Errorf("told: %v", box.got)
	}
	var stored, pending int
	_ = db.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE stripe_id = '') FROM downtime_credits`).Scan(&stored, &pending)
	if stored != 4 || pending != 0 {
		t.Errorf("stored %d, pending %d (the declined one is released)", stored, pending)
	}

	// applying again only retries what failed; a second outage the same day credits nothing new
	stripe.fail = ""
	again := do(s.Apply, "POST", "/apply", big.ID, `{}`)
	_ = json.Unmarshal(again.Body.Bytes(), &applied)
	if len(applied.Credited) != 2 || applied.Credited[0].Org != "org_declined" || len(stripe.credits) != 6 {
		t.Errorf("retry: %+v %v", applied, stripe.credits)
	}
	second := record("2026-10-18T10:00:00Z", "2026-10-18T12:00:00Z")
	_ = json.Unmarshal(do(s.Apply, "POST", "/apply", second.ID, `{}`).Body.Bytes(), &applied)
	if len(applied.Credited) != 0 || len(stripe.credits) != 6 {
		t.Errorf("a day credited twice: %+v", applied)
	}

	// a short outage isn't credited unless staff decide its cluster is; one with credits stays
	short := record("2026-10-20T10:00:00Z", "2026-10-20T10:20:00Z")
	if rec := do(s.Apply, "POST", "/apply", short.ID, `{}`); rec.Code != 400 {
		t.Errorf("short: %d", rec.Code)
	}
	for _, o := range []string{"2026-10-20T12:00:00Z", "2026-10-20T15:00:00Z"} {
		record(o, at(o).Add(10*time.Minute).Format(time.RFC3339))
	}
	var list []Outage
	_ = json.Unmarshal(do(s.List, "GET", "/api/staff/outages", "", "").Body.Bytes(), &list)
	for _, o := range list {
		if o.ID == short.ID && !slices.Equal(o.Clusters, []string{"2026-10-20"}) {
			t.Errorf("cluster not flagged: %+v", o)
		}
	}
	_ = json.Unmarshal(do(s.Apply, "POST", "/apply", short.ID, `{"include_short":true}`).Body.Bytes(), &applied)
	if len(applied.Credited) != 3 {
		t.Errorf("cluster credit: %+v", applied)
	}
	if rec := do(s.Delete, "DELETE", "/x", big.ID, ""); rec.Code != 409 {
		t.Errorf("delete credited: %d", rec.Code)
	}
	unused := record("2026-10-25T10:00:00Z", "2026-10-25T10:05:00Z")
	if rec := do(s.Delete, "DELETE", "/x", unused.ID, ""); rec.Code != 204 {
		t.Errorf("delete: %d", rec.Code)
	}
}
