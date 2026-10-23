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
		httpx.Error(w, r, http.StatusServiceUnavailable, httpx.Code("billing_not_configured", "billing is not configured"))
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
		httpx.Error(w, r, http.StatusBadRequest, httpx.Code("bad_plan", "plan must be pro or enterprise"))
		return
	}
	if s.configuredPrice(in.Plan) == "" {
		httpx.Error(w, r, http.StatusServiceUnavailable, httpx.Code("plan_not_on_sale", in.Plan+" isn't on sale yet"))
		return
	}
	paid, err := IsPaid(r.Context(), s.DB, org)
	if err != nil {
		logs.Errorf("billing: tier: %v", err)
		httpx.Error(w, r, http.StatusServiceUnavailable, httpx.Code("billing_unavailable", "billing unavailable"))
		return
	}
	if paid {
		httpx.Error(w, r, http.StatusConflict, httpx.Code("already_subscribed", "already subscribed — manage it from the billing portal"))
		return
	}
	customer, err := Customer(r.Context(), s.DB, s.Stripe, org, email)
	if err != nil {
		logs.Errorf("billing: customer: %v", err)
		httpx.Error(w, r, http.StatusBadGateway, httpx.Code("billing_provider_unavailable", "billing provider unavailable"))
		return
	}
	ret := strings.TrimRight(s.ReturnURL, "/")
	price, err := s.Stripe.ResolvePrice(r.Context(), s.configuredPrice(in.Plan))
	if err != nil {
		logs.Errorf("billing: price: %v", err)
		httpx.Error(w, r, http.StatusBadGateway, httpx.Code("billing_provider_unavailable", "billing provider unavailable"))
		return
	}
	url, err := s.Stripe.CheckoutURL(r.Context(), org, customer, price, ret+"?billing=success", ret+"?billing=cancelled")
	if err != nil {
		logs.Errorf("billing: checkout: %v", err)
		httpx.Error(w, r, http.StatusBadGateway, httpx.Code("billing_provider_unavailable", "billing provider unavailable"))
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]string{"url": url})
}
