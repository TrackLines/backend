package billing

import (
	"errors"
	"net/http"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/jackc/pgx/v5"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/httpx"
)

// portal returns {"url": <Stripe billing portal>} for changing card, plan or cancelling. Only plans
// billed through Stripe (a subscription id from the webhook) have a portal; a plan granted by hand
// (stripe_status set directly, no subscription) has nothing to manage.
func (s Service) Portal(w http.ResponseWriter, r *http.Request) {
	if !s.configured() {
		http.Error(w, "billing is not configured", http.StatusServiceUnavailable)
		return
	}
	user, _ := auth.UserID(r.Context())
	var customer, subscription *string
	var paid bool
	err := s.DB.QueryRow(r.Context(), `SELECT stripe_customer_id, stripe_subscription_id, COALESCE(stripe_status IN ('active', 'trialing'), false)
		FROM users WHERE clerk_id = $1`, user).Scan(&customer, &subscription, &paid)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		logs.Errorf("billing: portal customer: %v", err)
		http.Error(w, "billing unavailable", http.StatusServiceUnavailable)
		return
	}
	if customer == nil || subscription == nil {
		if paid {
			http.Error(w, "this plan isn't billed through Stripe, so there's nothing to manage", http.StatusConflict)
			return
		}
		http.Error(w, "no subscription yet — upgrade first", http.StatusConflict)
		return
	}
	url, err := s.Stripe.PortalURL(r.Context(), *customer, s.ReturnURL)
	if err != nil {
		logs.Errorf("billing: portal: %v", err)
		http.Error(w, "billing provider unavailable", http.StatusBadGateway)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]string{"url": url})
}

// status reports the caller's plan for the frontend: {"paid": bool, "project_limit": n (-1 = unlimited),
// "billed": bool}. billed = paid through a Stripe subscription, so there's billing to manage.
func (s Service) Status(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserID(r.Context())
	limit, err := ProjectLimit(r.Context(), s.DB, user)
	if err != nil {
		logs.Errorf("billing: status: %v", err)
		http.Error(w, "billing unavailable", http.StatusServiceUnavailable)
		return
	}
	var billed bool
	if err := s.DB.QueryRow(r.Context(), `SELECT COALESCE(bool_or(stripe_subscription_id IS NOT NULL), false) FROM users WHERE clerk_id = $1`, user).Scan(&billed); err != nil {
		logs.Errorf("billing: status: %v", err)
		http.Error(w, "billing unavailable", http.StatusServiceUnavailable)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"paid": limit < 0, "project_limit": limit, "billed": billed})
}
