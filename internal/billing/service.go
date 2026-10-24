package billing

import (
	"context"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/httpx"
)

// Service holds what the checkout, portal and webhook handlers share.
// Without a Stripe key (or price/webhook secret/return URL) every billing endpoint answers 503.
type Service struct {
	DB                *pgxpool.Pool
	Stripe            Stripe
	ProPriceID        string // Stripe price (or product) for Pro: 5 projects (TL-290)
	EnterprisePriceID string // and for Enterprise: unlimited; "" while it isn't on sale
	WebhookSecret     string
	ReturnURL         string // frontend settings page; checkout/portal send the user back here
	ChewedFeed        ChewedFeed
	// IsAdmin: is user an admin of org (organizations.Admins.IsAdmin). Only admins pay for the org's plan.
	IsAdmin func(ctx context.Context, org, user string) (bool, error)
	// ClerkOrgs keeps Clerk's member cap in step with the plan (TL-418); nil: not synced
	ClerkOrgs OrgUpdater
}

func (s Service) configured() bool {
	return s.Stripe.SecretKey != "" && s.ProPriceID != "" && s.WebhookSecret != "" && s.ReturnURL != ""
}

// admin returns the caller's org and email when they're a signed-in admin of it; otherwise it writes
// the refusal. The org's plan is bought and managed by its admins (TL-243).
func (s Service) admin(w http.ResponseWriter, r *http.Request) (org, email string, ok bool) {
	org = auth.OrgID(r.Context())
	user, signedIn := auth.UserID(r.Context())
	if auth.ViaAPIKey(r.Context()) || !signedIn || org == "" {
		httpx.Error(w, r, http.StatusForbidden, httpx.Code("sign_in_to_manage_plan", "sign in to an organization to manage its plan"))
		return "", "", false
	}
	admin := false
	if s.IsAdmin != nil {
		var err error
		if admin, err = s.IsAdmin(r.Context(), org, user); err != nil {
			httpx.Error(w, r, http.StatusServiceUnavailable, httpx.Code("billing_unavailable", "billing unavailable"))
			return "", "", false
		}
	}
	if !admin {
		httpx.Error(w, r, http.StatusForbidden, httpx.Code("admins_manage_plan", "only organization admins can manage the plan"))
		return "", "", false
	}
	_ = s.DB.QueryRow(r.Context(), `SELECT email FROM users WHERE clerk_id = $1`, user).Scan(&email)
	return org, email, true
}

// configuredPrice is the Stripe price (or product) id configured for a paid plan, "" if none.
func (s Service) configuredPrice(plan string) string {
	switch plan {
	case Pro:
		return s.ProPriceID
	case Enterprise:
		return s.EnterprisePriceID
	}
	return ""
}

// planFor is the paid plan a Stripe price is, or "" when it's neither of ours.
func (s Service) planFor(ctx context.Context, price string) string {
	for _, plan := range []string{Enterprise, Pro} {
		if id := s.configuredPrice(plan); id != "" {
			if resolved, err := s.Stripe.ResolvePrice(ctx, id); err == nil && resolved == price {
				return plan
			}
		}
	}
	return ""
}
