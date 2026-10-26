// Package downtime is the downtime resolver's first, manual phase (TL-576): the Terms promise that an
// outage over an hour makes every UTC day it touched free for every paying organization. Staff record
// outages, preview the credits and apply them in Stripe, once per (org, day) ever. The uptime webhook
// and a daily automatic run come later (TL-423).
package downtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/billing"
	"github.com/tracklines/backend/internal/httpx"
	"github.com/tracklines/backend/internal/notifications"
)

// Stripe is what crediting needs from billing.Stripe.
type Stripe interface {
	Subscription(ctx context.Context, id string) (billing.Subscription, error)
	Credit(ctx context.Context, customer string, amount int64, currency, description, key string) (string, error)
}

type System struct {
	DB     *pgxpool.Pool
	Stripe Stripe
	Notify notifications.Notifier
	Now    func() time.Time
}

// Outage is one recorded outage, with what the policy makes of it.
type Outage struct {
	ID        string    `json:"id"`
	StartsAt  time.Time `json:"starts_at"`
	EndsAt    time.Time `json:"ends_at"`
	Note      string    `json:"note"`
	Minutes   int       `json:"minutes"`
	Qualifies bool      `json:"qualifies"` // over an hour: credited
	Days      []string  `json:"days"`      // the UTC days it touched
	Clusters  []string  `json:"clusters"`  // its days with several short outages: staff decide
	Credited  int       `json:"credited"`  // (org, day) credits made for it
}

// Line is one organization's credit for one day.
type Line struct {
	Org      string `json:"org_id"`
	Plan     string `json:"plan"`
	Day      string `json:"day"`
	Amount   int64  `json:"amount"` // minor units
	Currency string `json:"currency"`
	Credited bool   `json:"credited"` // already, by this or another outage: skipped
}

// Problem is an organization whose credit couldn't be worked out or made.
type Problem struct {
	Org   string `json:"org_id"`
	Error string `json:"error"`
}

var (
	ErrNotFound    = httpx.Code("outage_not_found", "outage not found")
	ErrTimes       = httpx.Code("outage_times", "an outage ends after it starts, and not in the future")
	ErrHasCredits  = httpx.Code("outage_has_credits", "this outage has credits, so it stays on record")
	ErrShort       = httpx.Code("outage_short", "this outage was an hour or less: only a cluster staff decide on is credited")
	ErrUnavailable = httpx.Code("downtime_unavailable", "Stripe isn't configured")
	ErrFailed      = httpx.Code("downtime_failed", "could not load outages")
)

const day = "2006-01-02"

// Qualifies: more than an hour in one go. Exactly an hour doesn't.
func Qualifies(start, end time.Time) bool { return end.Sub(start) > time.Hour }

// Days are the UTC days [start, end) touched: 23:59 to 00:01 is two.
func Days(start, end time.Time) []string {
	var out []string
	last := end.UTC().Add(-time.Nanosecond)
	for d := start.UTC().Truncate(24 * time.Hour); !d.After(last); d = d.AddDate(0, 0, 1) {
		out = append(out, d.Format(day))
	}
	return out
}

// Clusters are the UTC days with several short outages that add up: three or more, or more than one
// totalling over an hour that day. They aren't credited automatically; staff decide.
func Clusters(outages []Outage) map[string]bool {
	type tally struct {
		n     int
		total time.Duration
	}
	days := map[string]*tally{}
	for _, o := range outages {
		for _, d := range Days(o.StartsAt, o.EndsAt) {
			from, _ := time.Parse(day, d)
			to := from.Add(24 * time.Hour)
			t := days[d]
			if t == nil {
				t = &tally{}
				days[d] = t
			}
			t.n++
			t.total += earliest(o.EndsAt, to).Sub(latest(o.StartsAt, from))
		}
	}
	out := map[string]bool{}
	for d, t := range days {
		if t.n >= 3 || (t.n >= 2 && t.total > time.Hour) {
			out[d] = true
		}
	}
	return out
}

func earliest(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func latest(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// DayPrice is a day of the plan: the monthly price ÷ the days in that day's month (a yearly price is
// twelve months), rounded to the nearest minor unit.
func DayPrice(p billing.Price, d string) int64 {
	t, _ := time.Parse(day, d)
	days := int64(time.Date(t.Year(), t.Month()+1, 0, 0, 0, 0, 0, time.UTC).Day())
	monthly := p.Amount
	if p.Interval == "year" {
		monthly = (p.Amount + 6) / 12
	}
	return (monthly + days/2) / days
}

func (s System) failed(w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() != nil {
		return
	}
	logs.Errorf("downtime: %v", err)
	httpx.Error(w, r, http.StatusInternalServerError, ErrFailed)
}

func (s System) outages(ctx context.Context) ([]Outage, error) {
	rows, err := s.DB.Query(ctx, `SELECT o.id::text, o.starts_at, o.ends_at, o.note, (SELECT count(*) FROM downtime_credits c WHERE c.outage_id = o.id)
		FROM outages o ORDER BY o.starts_at DESC`)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Outage, error) {
		var o Outage
		return o, row.Scan(&o.ID, &o.StartsAt, &o.EndsAt, &o.Note, &o.Credited)
	})
	if err != nil {
		return nil, err
	}
	clusters := Clusters(out)
	for i := range out {
		o := &out[i]
		o.StartsAt, o.EndsAt = o.StartsAt.UTC(), o.EndsAt.UTC()
		o.Minutes = int(o.EndsAt.Sub(o.StartsAt).Minutes())
		o.Qualifies = Qualifies(o.StartsAt, o.EndsAt)
		o.Days = Days(o.StartsAt, o.EndsAt)
		o.Clusters = []string{}
		for _, d := range o.Days {
			if clusters[d] {
				o.Clusters = append(o.Clusters, d)
			}
		}
	}
	return out, nil
}

func (s System) outage(w http.ResponseWriter, r *http.Request) (Outage, bool) {
	all, err := s.outages(r.Context())
	if err != nil {
		s.failed(w, r, err)
		return Outage{}, false
	}
	for _, o := range all {
		if o.ID == r.PathValue("id") {
			return o, true
		}
	}
	httpx.Error(w, r, http.StatusNotFound, ErrNotFound)
	return Outage{}, false
}

// List answers GET /api/staff/outages: every outage, newest first.
func (s System) List(w http.ResponseWriter, r *http.Request) {
	all, err := s.outages(r.Context())
	if err != nil {
		s.failed(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, all)
}

// Create answers POST /api/staff/outages {starts_at, ends_at, note}, times in RFC 3339.
func (s System) Create(w http.ResponseWriter, r *http.Request) {
	var body struct {
		StartsAt time.Time `json:"starts_at"`
		EndsAt   time.Time `json:"ends_at"`
		Note     string    `json:"note"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil {
		httpx.Error(w, r, http.StatusBadRequest, errors.New("invalid json"))
		return
	}
	if body.StartsAt.IsZero() || !body.EndsAt.After(body.StartsAt) || body.EndsAt.After(s.Now()) {
		httpx.Error(w, r, http.StatusBadRequest, ErrTimes)
		return
	}
	var id string
	if err := s.DB.QueryRow(r.Context(), `INSERT INTO outages (starts_at, ends_at, note, created_by) VALUES ($1, $2, $3, $4) RETURNING id::text`,
		body.StartsAt, body.EndsAt, strings.TrimSpace(body.Note), auth.ActorID(r.Context())).Scan(&id); err != nil {
		s.failed(w, r, err)
		return
	}
	r.SetPathValue("id", id)
	if o, ok := s.outage(w, r); ok {
		httpx.JSON(w, http.StatusCreated, o)
	}
}

// Delete answers DELETE /api/staff/outages/{id}: a mistake can go, an outage with credits can't.
func (s System) Delete(w http.ResponseWriter, r *http.Request) {
	o, ok := s.outage(w, r)
	if !ok {
		return
	}
	if o.Credited > 0 {
		httpx.Error(w, r, http.StatusConflict, ErrHasCredits)
		return
	}
	if _, err := s.DB.Exec(r.Context(), `DELETE FROM outages WHERE id::text = $1 AND NOT EXISTS (SELECT 1 FROM downtime_credits WHERE outage_id = outages.id)`, o.ID); err != nil {
		s.failed(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type payer struct{ org, plan, customer, subscription string }

// plan works out every paying organization's credit for each of the outage's days. Paying: an active
// Stripe subscription (Pro or Enterprise); Free and hand-granted plans pay nothing, so get nothing.
func (s System) plan(ctx context.Context, o Outage) ([]Line, []Problem, map[string]payer, error) {
	rows, err := s.DB.Query(ctx, `SELECT org_id, COALESCE(plan, 'enterprise'), stripe_customer_id, stripe_subscription_id FROM org_billing
		WHERE stripe_status = 'active' AND stripe_customer_id IS NOT NULL AND stripe_subscription_id IS NOT NULL ORDER BY org_id`)
	if err != nil {
		return nil, nil, nil, err
	}
	payers, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (payer, error) {
		var p payer
		return p, row.Scan(&p.org, &p.plan, &p.customer, &p.subscription)
	})
	if err != nil {
		return nil, nil, nil, err
	}
	rows, err = s.DB.Query(ctx, `SELECT org_id, to_char(day, 'YYYY-MM-DD') FROM downtime_credits WHERE to_char(day, 'YYYY-MM-DD') = ANY($1)`, o.Days)
	if err != nil {
		return nil, nil, nil, err
	}
	done := map[string]bool{}
	for rows.Next() {
		var org, d string
		if err := rows.Scan(&org, &d); err != nil {
			return nil, nil, nil, err
		}
		done[org+" "+d] = true
	}
	lines, problems, byOrg := []Line{}, []Problem{}, map[string]payer{}
	for _, p := range payers {
		sub, err := s.Stripe.Subscription(ctx, p.subscription)
		if err == nil && (sub.Price.Amount <= 0 || sub.Price.Currency == "") {
			err = errors.New("its subscription has no price")
		}
		if err != nil {
			problems = append(problems, Problem{Org: p.org, Error: err.Error()})
			continue
		}
		byOrg[p.org] = p
		for _, d := range o.Days {
			lines = append(lines, Line{Org: p.org, Plan: p.plan, Day: d, Amount: DayPrice(sub.Price, d), Currency: sub.Price.Currency, Credited: done[p.org+" "+d]})
		}
	}
	return lines, problems, byOrg, nil
}

// Preview answers GET /api/staff/outages/{id}/preview: who'd be credited what, nothing changed.
func (s System) Preview(w http.ResponseWriter, r *http.Request) {
	if s.Stripe == nil {
		httpx.Error(w, r, http.StatusServiceUnavailable, ErrUnavailable)
		return
	}
	o, ok := s.outage(w, r)
	if !ok {
		return
	}
	lines, problems, _, err := s.plan(r.Context(), o)
	if err != nil {
		s.failed(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"outage": o, "credits": lines, "problems": problems})
}

// Apply answers POST /api/staff/outages/{id}/apply {include_short}: credits every line not credited
// yet. Each (org, day) is claimed in downtime_credits before Stripe is asked, so two runs can't both
// credit it, and a failed credit is released for a retry. include_short credits an outage of an hour
// or less: for a cluster staff decided on.
func (s System) Apply(w http.ResponseWriter, r *http.Request) {
	if s.Stripe == nil {
		httpx.Error(w, r, http.StatusServiceUnavailable, ErrUnavailable)
		return
	}
	var body struct {
		IncludeShort bool `json:"include_short"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&body)
	o, ok := s.outage(w, r)
	if !ok {
		return
	}
	if !o.Qualifies && !body.IncludeShort {
		httpx.Error(w, r, http.StatusBadRequest, ErrShort)
		return
	}
	ctx := context.WithoutCancel(r.Context()) // once started, a credit is finished or released
	lines, problems, payers, err := s.plan(ctx, o)
	if err != nil {
		s.failed(w, r, err)
		return
	}
	by := auth.ActorID(r.Context())
	credited := []Line{}
	days := map[string][]string{}
	for _, l := range lines {
		if l.Credited {
			continue
		}
		tag, err := s.DB.Exec(ctx, `INSERT INTO downtime_credits (org_id, day, outage_id, amount, currency, created_by) VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT DO NOTHING`, l.Org, l.Day, o.ID, l.Amount, l.Currency, by)
		if err != nil {
			problems = append(problems, Problem{Org: l.Org, Error: err.Error()})
			continue
		}
		if tag.RowsAffected() == 0 {
			continue // another run has it
		}
		// ponytail: a crash between this claim and Stripe leaves the row with stripe_id ''; it shows as
		// credited, so check that org in Stripe by hand (the idempotency key makes a retry within 24h safe).
		d, _ := time.Parse(day, l.Day)
		id, err := s.Stripe.Credit(ctx, payers[l.Org].customer, l.Amount, l.Currency, "Downtime credit: "+d.Format("2 Jan 2006")+" (UTC) was free",
			"tracklines-downtime-"+l.Org+"-"+l.Day)
		if err != nil {
			_, _ = s.DB.Exec(ctx, `DELETE FROM downtime_credits WHERE org_id = $1 AND day = $2 AND stripe_id = ''`, l.Org, l.Day)
			problems = append(problems, Problem{Org: l.Org, Error: fmt.Sprintf("%s: %v", l.Day, err)})
			continue
		}
		if _, err := s.DB.Exec(ctx, `UPDATE downtime_credits SET stripe_id = $3 WHERE org_id = $1 AND day = $2`, l.Org, l.Day, id); err != nil {
			logs.Errorf("downtime: record %s %s as %s: %v", l.Org, l.Day, id, err)
		}
		l.Credited = true
		credited = append(credited, l)
		days[l.Org] = append(days[l.Org], l.Day)
	}
	for org, ds := range days {
		s.tell(ctx, org, ds)
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"credited": credited, "problems": problems})
}

// tell lets the organization's admins know which days were made free.
func (s System) tell(ctx context.Context, org string, ds []string) {
	rows, err := s.DB.Query(ctx, `SELECT user_clerk_id FROM org_admins WHERE org_id = $1`, org)
	if err != nil {
		logs.Errorf("downtime: admins of %s: %v", org, err)
		return
	}
	admins, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		logs.Errorf("downtime: admins of %s: %v", org, err)
		return
	}
	notifications.Send(ctx, s.Notify, org, admins, notifications.Event{Kind: "downtime_credit",
		Text: "credited " + DayList(ds) + " for downtime: those days are free on your next bill"})
}

// DayList is "17 Oct", "17 and 18 Oct" or "30 Sep, 1 and 2 Oct".
func DayList(ds []string) string {
	slices.Sort(ds)
	var parts []string
	for i, d := range ds {
		t, _ := time.Parse(day, d)
		s := t.Format("2")
		if i == len(ds)-1 || ds[i+1][:7] != d[:7] {
			s = t.Format("2 Jan")
		}
		parts = append(parts, s)
	}
	if len(parts) == 1 {
		return parts[0]
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
}
