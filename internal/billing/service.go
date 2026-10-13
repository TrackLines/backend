package billing

import (
	"context"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
)

// Service holds what the checkout, portal and webhook handlers share.
// Without a Stripe key (or price/webhook secret/return URL) every billing endpoint answers 503.
type Service struct {
	DB            *pgxpool.Pool
	Stripe        Stripe
	PriceID       string
	WebhookSecret string
	ReturnURL     string // frontend settings page; checkout/portal send the user back here
	ChewedFeed    ChewedFeed
	// IsAdmin: is user an admin of org (organizations.Admins.IsAdmin). Only admins pay for the org's plan.
	IsAdmin func(ctx context.Context, org, user string) (bool, error)
}

func (s Service) configured() bool {
	return s.Stripe.SecretKey != "" && s.PriceID != "" && s.WebhookSecret != "" && s.ReturnURL != ""
}

// admin returns the caller's org and email when they're a signed-in admin of it; otherwise it writes
// the refusal. The org's plan is bought and managed by its admins (TL-243).
func (s Service) admin(w http.ResponseWriter, r *http.Request) (org, email string, ok bool) {
	org = auth.OrgID(r.Context())
	user, signedIn := auth.UserID(r.Context())
	if auth.ViaAPIKey(r.Context()) || !signedIn || org == "" {
		http.Error(w, "sign in to an organization to manage its plan", http.StatusForbidden)
		return "", "", false
	}
	admin := false
	if s.IsAdmin != nil {
		var err error
		if admin, err = s.IsAdmin(r.Context(), org, user); err != nil {
			http.Error(w, "billing unavailable", http.StatusServiceUnavailable)
			return "", "", false
		}
	}
	if !admin {
		http.Error(w, "only organization admins can manage the plan", http.StatusForbidden)
		return "", "", false
	}
	_ = s.DB.QueryRow(r.Context(), `SELECT email FROM users WHERE clerk_id = $1`, user).Scan(&email)
	return org, email, true
}
