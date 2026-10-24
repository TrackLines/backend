package billing

import (
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/jackc/pgx/v5"
	"github.com/tracklines/backend/internal/httpx"
)

// ChangePlan: POST /api/subscription/plan {"plan": "pro" | "enterprise"} moves the org's subscription
// to that plan, prorated by Stripe (TL-290): upgrading Pro to Enterprise, or back. Org admins only; an
// org without a subscription checks out instead, and a plan granted by hand has no billing to change.
func (s Service) ChangePlan(w http.ResponseWriter, r *http.Request) {
	if !s.configured() {
		httpx.Error(w, r, http.StatusServiceUnavailable, httpx.Code("billing_not_configured", "billing is not configured"))
		return
	}
	org, _, ok := s.admin(w, r)
	if !ok {
		return
	}
	var in struct {
		Plan string `json:"plan"`
	}
	if !httpx.Decode(w, r, &in) {
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
	var sub *string
	err := s.DB.QueryRow(r.Context(), `SELECT stripe_subscription_id FROM org_billing WHERE org_id = $1 AND stripe_status IN ('active', 'trialing')`, org).Scan(&sub)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && sub == nil) {
		httpx.Error(w, r, http.StatusConflict, httpx.Code("no_subscription_to_change", "there's no subscription to change: upgrade with checkout, or this plan was granted by hand"))
		return
	}
	if err != nil {
		logs.Errorf("billing: change plan: %v", err)
		httpx.Error(w, r, http.StatusServiceUnavailable, httpx.Code("billing_unavailable", "billing unavailable"))
		return
	}
	current, err := s.Stripe.Subscription(r.Context(), *sub)
	if err == nil && current.ItemID == "" {
		err = errors.New("subscription has no item")
	}
	var price string
	if err == nil {
		price, err = s.Stripe.ResolvePrice(r.Context(), s.configuredPrice(in.Plan))
	}
	if err == nil && current.PriceID != price {
		err = s.Stripe.ChangePrice(r.Context(), current.ID, current.ItemID, price)
	}
	if err != nil {
		logs.Errorf("billing: change plan: %v", err)
		httpx.Error(w, r, http.StatusBadGateway, httpx.Code("billing_provider_unavailable", "billing provider unavailable"))
		return
	}
	// the webhook says so too; recording it now means the new limit applies straight away
	if _, err := s.DB.Exec(r.Context(), `UPDATE org_billing SET plan = $2, updated_at = now() WHERE org_id = $1`, org, in.Plan); err != nil {
		logs.Errorf("billing: change plan: %v", err)
	}
	if err := SyncMemberCap(r.Context(), s.DB, s.ClerkOrgs, org); err != nil { // Enterprise: unlimited members; Pro: 20 (TL-418)
		logs.Warnf("billing: member cap %s: %v", org, err)
	}
	w.WriteHeader(http.StatusNoContent)
}

// PlanInfo is one plan as the homepage and settings show it: its project limit (nil: unlimited) and,
// for paid plans on sale, its price from Stripe.
type PlanInfo struct {
	ID       string `json:"id"`
	Projects *int   `json:"projects"`
	Members  *int   `json:"members"` // nil: unlimited (TL-418)
	Price    *Price `json:"price"`
}

var plansCache struct {
	sync.Mutex
	at   time.Time
	list []PlanInfo
}

// Plans: GET /api/plans (public): Free, Pro and Enterprise with their limits and prices. Prices come
// from Stripe, cached for an hour; a plan whose price can't be read is listed without one.
func (s Service) Plans(w http.ResponseWriter, r *http.Request) {
	plansCache.Lock()
	defer plansCache.Unlock()
	if plansCache.list == nil || time.Since(plansCache.at) > time.Hour {
		free, pro, members := FreeProjectLimit, ProProjectLimit, MemberLimit
		list := []PlanInfo{{ID: Free, Projects: &free, Members: &members, Price: &Price{Amount: 0}}, {ID: Pro, Projects: &pro, Members: &members}, {ID: Enterprise}}
		for i := range list[1:] {
			p := &list[i+1]
			id := s.configuredPrice(p.ID)
			if id == "" || s.Stripe.SecretKey == "" {
				continue
			}
			resolved, err := s.Stripe.ResolvePrice(r.Context(), id)
			if err == nil {
				var price Price
				if price, err = s.Stripe.Price(r.Context(), resolved); err == nil {
					p.Price = &price
				}
			}
			if err != nil {
				logs.Warnf("billing: plan price %s: %v", p.ID, err)
			}
		}
		plansCache.list, plansCache.at = list, time.Now()
	}
	httpx.JSON(w, http.StatusOK, plansCache.list)
}
