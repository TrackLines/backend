package billing

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/jackc/pgx/v5"
	"github.com/tracklines/backend/internal/httpx"
)

// webhook accepts only signed events and re-reads the subscription from Stripe, so
// out-of-order events can't leave stale state. A 5xx makes Stripe retry.
func (s Service) Webhook(now func() time.Time) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.configured() {
			http.Error(w, "billing is not configured", http.StatusServiceUnavailable)
			return
		}
		payload, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		if err := VerifyWebhook(payload, r.Header.Get("Stripe-Signature"), s.WebhookSecret, now()); err != nil {
			http.Error(w, "invalid signature", http.StatusBadRequest)
			return
		}
		var event struct {
			Type string `json:"type"`
			Data struct {
				Object struct {
					ID           string `json:"id"`
					Subscription string `json:"subscription"`
				} `json:"object"`
			} `json:"data"`
		}
		if err := json.Unmarshal(payload, &event); err != nil {
			http.Error(w, "invalid event", http.StatusBadRequest)
			return
		}
		var subID string
		switch event.Type {
		case "checkout.session.completed":
			subID = event.Data.Object.Subscription
		case "customer.subscription.created", "customer.subscription.updated", "customer.subscription.deleted":
			subID = event.Data.Object.ID
		}
		if subID != "" {
			sub, err := s.Stripe.Subscription(r.Context(), subID)
			if err != nil {
				logs.Errorf("billing: webhook fetch %s: %v", subID, err)
				http.Error(w, "billing provider unavailable", http.StatusBadGateway)
				return
			}
			customer, err := applySubscription(r.Context(), s, sub)
			if err != nil {
				logs.Errorf("billing: webhook apply %s: %v", subID, err)
				http.Error(w, "billing unavailable", http.StatusServiceUnavailable)
				return
			}
			// ponytail: best effort, the next subscription event re-syncs; add a retry queue if gaps show up in HQ
			if customer != nil {
				if err := s.ChewedFeed.Sync(r.Context(), *customer); err != nil {
					logs.Errorf("billing: chewedfeed sync %s: %v", subID, err)
				}
			}
		}
		httpx.JSON(w, http.StatusOK, map[string]bool{"received": true})
	}
}

// applySubscription stores the subscription on the user owning its customer. A stale
// non-live subscription can't overwrite a different live one. Unknown customers
// (not from our checkout) match no row and are acknowledged so Stripe stops retrying.
// It returns the updated user for ChewedFeed, or nil when no row changed.
func applySubscription(ctx context.Context, s Service, sub Subscription) (*chewedFeedCustomer, error) {
	c := chewedFeedCustomer{BillingStatus: sub.Status, Plan: plan(sub.Status)}
	err := s.DB.QueryRow(ctx, `UPDATE users SET stripe_subscription_id = $2, stripe_status = $3, updated_at = now()
		WHERE stripe_customer_id = $1
		  AND (stripe_subscription_id IS NULL OR stripe_subscription_id = $2 OR $3 IN ('active', 'trialing'))
		RETURNING clerk_id, COALESCE(name, ''), email, created_at`,
		sub.CustomerID, sub.ID, sub.Status).Scan(&c.ExternalCustomerID, &c.Name, &c.Email, &c.SignedUpAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}
