package billing

import (
	"net/http"
	"strings"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/httpx"
)

// checkout returns {"url": <Stripe Checkout>} for the paid plan.
func (s Service) Checkout(w http.ResponseWriter, r *http.Request) {
	if !s.configured() {
		http.Error(w, "billing is not configured", http.StatusServiceUnavailable)
		return
	}
	user, _ := auth.UserID(r.Context())
	paid, err := IsPaid(r.Context(), s.DB, user)
	if err != nil {
		logs.Errorf("billing: tier: %v", err)
		http.Error(w, "billing unavailable", http.StatusServiceUnavailable)
		return
	}
	if paid {
		http.Error(w, "already subscribed — manage it from the billing portal", http.StatusConflict)
		return
	}
	customer, err := Customer(r.Context(), s.DB, s.Stripe, user)
	if err != nil {
		logs.Errorf("billing: customer: %v", err)
		http.Error(w, "billing provider unavailable", http.StatusBadGateway)
		return
	}
	ret := strings.TrimRight(s.ReturnURL, "/")
	url, err := s.Stripe.CheckoutURL(r.Context(), user, customer, s.PriceID, ret+"?billing=success", ret+"?billing=cancelled")
	if err != nil {
		logs.Errorf("billing: checkout: %v", err)
		http.Error(w, "billing provider unavailable", http.StatusBadGateway)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]string{"url": url})
}
