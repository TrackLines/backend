package billing

import (
	"net/http"
	"strings"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/tracklines/backend/internal/httpx"
)

// checkout returns {"url": <Stripe Checkout>} for a paid plan: {"plan": "pro" | "enterprise"}, Pro
// when left out. An org already paying changes plan instead (ChangePlan), so it never pays twice.
func (s Service) Checkout(w http.ResponseWriter, r *http.Request) {
	if !s.configured() {
		http.Error(w, "billing is not configured", http.StatusServiceUnavailable)
		return
	}
	org, email, ok := s.admin(w, r)
	if !ok {
		return
	}
	in := struct {
		Plan string `json:"plan"`
	}{Plan: Pro}
	if r.ContentLength != 0 && !httpx.Decode(w, r, &in) {
		return
	}
	if in.Plan != Pro && in.Plan != Enterprise {
		http.Error(w, "plan must be pro or enterprise", http.StatusBadRequest)
		return
	}
	if s.configuredPrice(in.Plan) == "" {
		http.Error(w, in.Plan+" isn't on sale yet", http.StatusServiceUnavailable)
		return
	}
	paid, err := IsPaid(r.Context(), s.DB, org)
	if err != nil {
		logs.Errorf("billing: tier: %v", err)
		http.Error(w, "billing unavailable", http.StatusServiceUnavailable)
		return
	}
	if paid {
		http.Error(w, "already subscribed — manage it from the billing portal", http.StatusConflict)
		return
	}
	customer, err := Customer(r.Context(), s.DB, s.Stripe, org, email)
	if err != nil {
		logs.Errorf("billing: customer: %v", err)
		http.Error(w, "billing provider unavailable", http.StatusBadGateway)
		return
	}
	ret := strings.TrimRight(s.ReturnURL, "/")
	price, err := s.Stripe.ResolvePrice(r.Context(), s.configuredPrice(in.Plan))
	if err != nil {
		logs.Errorf("billing: price: %v", err)
		http.Error(w, "billing provider unavailable", http.StatusBadGateway)
		return
	}
	url, err := s.Stripe.CheckoutURL(r.Context(), org, customer, price, ret+"?billing=success", ret+"?billing=cancelled")
	if err != nil {
		logs.Errorf("billing: checkout: %v", err)
		http.Error(w, "billing provider unavailable", http.StatusBadGateway)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]string{"url": url})
}
