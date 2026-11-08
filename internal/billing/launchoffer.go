package billing

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/httpx"
)

// The launch offer (TL-773): when testing ends, staff send chosen organizations a free year of Pro
// (the admin panel, TL-778) and an admin of each claims it.
// It's a Stripe Pro subscription on a 365-day trial with no card, so the webhook and PlanOf treat it
// like any other ('trialing' is live), Clerk's member cap follows, and Stripe ends it: with a card
// added it carries on as paid Pro, without one it's cancelled and the org is back on Free.

const launchOfferDays = 365

// LaunchOfferLink is where the offer's notices open: the plan card on the settings page.
const LaunchOfferLink = "/settings?offer=1"

// LaunchOffer is what GET /api/subscription/launch-offer says about the caller's organization.
type LaunchOffer struct {
	Eligible    bool       `json:"eligible"`      // it was sent to this org and can be claimed now (by an admin)
	Claimed     bool       `json:"claimed"`       // it was claimed: never again for this org
	Admin       bool       `json:"admin"`         // the caller can claim it; members ask an admin
	TrialEndsAt *time.Time `json:"trial_ends_at"` // when the free year ends, once claimed
}

// launchOfferOn is the flags.gg flag launch-offer: off, the offer doesn't exist (404).
func (s Service) launchOfferOn() bool { return s.LaunchOfferOn != nil && s.LaunchOfferOn() }

func (s Service) offer(ctx context.Context, org string) (LaunchOffer, error) {
	var o LaunchOffer
	err := s.DB.QueryRow(ctx, `SELECT claimed_at IS NOT NULL, trial_ends_at FROM launch_offers WHERE org_id = $1`, org).Scan(&o.Claimed, &o.TrialEndsAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return o, nil // not sent to this org
	}
	if err != nil || o.Claimed {
		return o, err
	}
	paid, err := IsPaid(ctx, s.DB, org)
	o.Eligible = !paid
	return o, err
}

// LaunchOfferStatus: GET /api/subscription/launch-offer, for anyone in the org.
func (s Service) LaunchOfferStatus(w http.ResponseWriter, r *http.Request) {
	if !s.launchOfferOn() {
		http.NotFound(w, r)
		return
	}
	org := auth.OrgID(r.Context())
	user, signedIn := auth.UserID(r.Context())
	if auth.ViaAPIKey(r.Context()) || !signedIn || org == "" {
		httpx.Error(w, r, http.StatusForbidden, httpx.Code("sign_in_to_manage_plan", "sign in to an organization to manage its plan"))
		return
	}
	o, err := s.offer(r.Context(), org)
	if err == nil && s.IsAdmin != nil {
		o.Admin, err = s.IsAdmin(r.Context(), org, user)
	}
	if err != nil {
		if httpx.Canceled(err) || r.Context().Err() != nil {
			return // the caller left before it was answered (TL-825): nobody to tell, nothing wrong
		}
		logs.Errorf("billing: launch offer %s: %v", org, err)
		httpx.Error(w, r, http.StatusServiceUnavailable, httpx.Code("billing_unavailable", "billing unavailable"))
		return
	}
	httpx.JSON(w, http.StatusOK, o)
}

// ClaimLaunchOffer: POST /api/subscription/launch-offer, admins only. The org goes on Pro at once.
func (s Service) ClaimLaunchOffer(w http.ResponseWriter, r *http.Request) {
	if !s.launchOfferOn() {
		http.NotFound(w, r)
		return
	}
	if !s.configured() {
		httpx.Error(w, r, http.StatusServiceUnavailable, httpx.Code("billing_not_configured", "billing is not configured"))
		return
	}
	org, email, ok := s.admin(w, r)
	if !ok {
		return
	}
	user, _ := auth.UserID(r.Context())
	ctx := r.Context()
	o, err := s.offer(ctx, org)
	if err != nil {
		logs.Errorf("billing: launch offer %s: %v", org, err)
		httpx.Error(w, r, http.StatusServiceUnavailable, httpx.Code("billing_unavailable", "billing unavailable"))
		return
	}
	switch {
	case o.Claimed:
		httpx.Error(w, r, http.StatusConflict, httpx.Code("launch_offer_claimed", "this organization has already claimed its free year"))
		return
	case !o.Eligible:
		paid, _ := IsPaid(ctx, s.DB, org)
		if paid {
			httpx.Error(w, r, http.StatusConflict, httpx.Code("already_subscribed", "already subscribed — manage it from the billing portal"))
			return
		}
		httpx.Error(w, r, http.StatusForbidden, httpx.Code("launch_offer_unavailable", "the free year isn't available to this organization"))
		return
	}
	// claiming is the lock: a second admin's click (or a double click) finds it claimed and stops here
	tag, err := s.DB.Exec(ctx, `UPDATE launch_offers SET claimed_at = now(), claimed_by = $2 WHERE org_id = $1 AND claimed_at IS NULL`, org, user)
	if err != nil {
		logs.Errorf("billing: launch offer claim %s: %v", org, err)
		httpx.Error(w, r, http.StatusServiceUnavailable, httpx.Code("billing_unavailable", "billing unavailable"))
		return
	}
	if tag.RowsAffected() == 0 {
		httpx.Error(w, r, http.StatusConflict, httpx.Code("launch_offer_claimed", "this organization has already claimed its free year"))
		return
	}
	sub, err := s.startTrial(ctx, org, email)
	if err != nil {
		logs.Errorf("billing: launch offer trial %s: %v", org, err)
		// nothing was given, so the claim is still there to make
		if _, derr := s.DB.Exec(ctx, `UPDATE launch_offers SET claimed_at = NULL, claimed_by = NULL WHERE org_id = $1 AND subscription_id IS NULL`, org); derr != nil {
			logs.Errorf("billing: launch offer unclaim %s: %v", org, derr)
		}
		httpx.Error(w, r, http.StatusBadGateway, httpx.Code("billing_provider_unavailable", "billing provider unavailable"))
		return
	}
	// Pro now, not when the webhook arrives (it does the same again)
	if _, err := applySubscription(ctx, s, sub.Subscription); err != nil {
		logs.Warnf("billing: launch offer apply %s: %v", org, err)
	}
	httpx.JSON(w, http.StatusOK, LaunchOffer{Claimed: true, Admin: true, TrialEndsAt: &sub.trialEnd})
}

type trial struct {
	Subscription
	trialEnd time.Time
}

func (s Service) startTrial(ctx context.Context, org, email string) (trial, error) {
	customer, err := Customer(ctx, s.DB, s.Stripe, org, email)
	if err != nil {
		return trial{}, err
	}
	price, err := s.Stripe.ResolvePrice(ctx, s.ProPriceID)
	if err != nil {
		return trial{}, err
	}
	// ponytail: minute bucket like CreateCustomer, so a retry after a failure isn't Stripe's replay of it
	key := fmt.Sprintf("tracklines-launch-offer-%s-%d", org, time.Now().Unix()/60)
	id, end, err := s.Stripe.TrialSubscription(ctx, org, customer, price, launchOfferDays, key)
	if err != nil {
		return trial{}, err
	}
	if _, err := s.DB.Exec(ctx, `UPDATE launch_offers SET subscription_id = $2, trial_ends_at = $3 WHERE org_id = $1`, org, id, end); err != nil {
		return trial{}, err
	}
	return trial{Subscription{ID: id, CustomerID: customer, Status: "trialing", PriceID: price}, end}, nil
}

// TrialReminders keeps the price-increase promise for the launch offer: each org's admins are told
// 30 days and again 7 days before the free year ends, while it's still a trial (cancelled or already
// paying: nothing to say). It runs whether or not the offer's flag is on.
type TrialReminders struct{ DB *pgxpool.Pool }

func (t TrialReminders) Run(ctx context.Context, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		if err := t.Once(ctx); err != nil && ctx.Err() == nil {
			logs.Errorf("billing: trial reminders: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// Once sends the reminders that are due. A missed 30-day one isn't sent inside the last 7 days:
// the 7-day one says it.
func (t TrialReminders) Once(ctx context.Context) error {
	for _, r := range []struct {
		column string
		within string
		after  string
		text   string
	}{
		{"reminded_30_at", "30 days", "7 days", "Your free year of Pro ends in 30 days. Add a card to keep Pro, or the organization moves to Free. Already added one? You're all set."},
		{"reminded_7_at", "7 days", "0 days", "Your free year of Pro ends in 7 days. Add a card to keep Pro, or the organization moves to Free. Already added one? You're all set."},
	} {
		// one statement: the reminder is marked sent only with its notices written
		_, err := t.DB.Exec(ctx, `WITH due AS (
				UPDATE launch_offers lo SET `+r.column+` = now()
				FROM org_billing b
				WHERE b.org_id = lo.org_id AND b.stripe_subscription_id = lo.subscription_id AND b.stripe_status = 'trialing'
				  AND lo.`+r.column+` IS NULL
				  AND lo.trial_ends_at <= now() + $1::interval AND lo.trial_ends_at > now() + $2::interval
				RETURNING lo.org_id)
			INSERT INTO notifications (org_id, user_id, kind, text, link)
			SELECT due.org_id, a.user_clerk_id, 'billing', $3, $4 FROM due JOIN org_admins a ON a.org_id = due.org_id`,
			r.within, r.after, r.text, LaunchOfferLink)
		if err != nil {
			return err
		}
	}
	return nil
}
