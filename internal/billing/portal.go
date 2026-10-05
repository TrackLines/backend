package billing

import (
	"errors"
	"net/http"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/jackc/pgx/v5"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/httpx"
)

// portal returns {"url": <Stripe billing portal>} for changing card, plan or cancelling.
func (s Service) Portal(w http.ResponseWriter, r *http.Request) {
	if !s.configured() {
		http.Error(w, "billing is not configured", http.StatusServiceUnavailable)
		return
	}
	user, _ := auth.UserID(r.Context())
	var customer *string
	err := s.DB.QueryRow(r.Context(), `SELECT stripe_customer_id FROM users WHERE clerk_id = $1`, user).Scan(&customer)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		logs.Errorf("billing: portal customer: %v", err)
		http.Error(w, "billing unavailable", http.StatusServiceUnavailable)
		return
	}
	if customer == nil {
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

// status reports the caller's plan for the frontend badge: {"paid": bool, "board_limit": n (-1 = unlimited)}.
func (s Service) Status(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserID(r.Context())
	limit, err := BoardLimit(r.Context(), s.DB, user)
	if err != nil {
		logs.Errorf("billing: status: %v", err)
		http.Error(w, "billing unavailable", http.StatusServiceUnavailable)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"paid": limit < 0, "board_limit": limit})
}
