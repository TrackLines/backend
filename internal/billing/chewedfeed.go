package billing

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ChewedFeed pushes customers to ChewedFeed HQ (POST /api/product-customers/sync, keyed by
// the tracklines project's enquiry key, same endpoint bugfixes uses). Empty URL or Key = off.
type ChewedFeed struct {
	URL    string
	Key    string
	Client *http.Client
}

type chewedFeedCustomer struct {
	ExternalCustomerID string     `json:"external_customer_id"`
	Name               string     `json:"name"`
	Email              string     `json:"email"`
	Plan               string     `json:"plan"`
	BillingStatus      string     `json:"billing_status"`
	SignedUpAt         *time.Time `json:"signed_up_at,omitempty"` // nil: ChewedFeed keeps what it has
}

func (c ChewedFeed) Sync(ctx context.Context, customer chewedFeedCustomer) error {
	if c.URL == "" || c.Key == "" {
		return nil
	}
	body, err := json.Marshal(customer)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.URL, "/")+"/api/product-customers/sync", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Enquiry-Key", c.Key)
	client := c.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("chewedfeed returned HTTP %d", resp.StatusCode)
	}
	return nil
}

// Push sends one organization to ChewedFeed as a customer: its plan (free, pro or enterprise, hand
// grants included), its Stripe status ("none" without billing) and a contact email: the billing
// email, else its creator's, else an admin's. With no email known yet it's skipped; RunSync retries.
// name, creator and created may be empty; ChewedFeed keeps what it already has.
func (c ChewedFeed) Push(ctx context.Context, db *pgxpool.Pool, org, name, creator string, created *time.Time) error {
	if c.URL == "" || c.Key == "" || org == "" {
		return nil
	}
	plan, err := PlanOf(ctx, db, org)
	if err != nil {
		return err
	}
	customer := chewedFeedCustomer{ExternalCustomerID: org, Name: name, Plan: plan, SignedUpAt: created}
	if err := db.QueryRow(ctx, `SELECT
		COALESCE((SELECT stripe_status FROM org_billing WHERE org_id = $1), 'none'),
		COALESCE((SELECT NULLIF(billing_email, '') FROM org_billing WHERE org_id = $1),
			(SELECT email FROM users WHERE clerk_id = $2),
			(SELECT u.email FROM org_admins a JOIN users u ON u.clerk_id = a.user_clerk_id WHERE a.org_id = $1 ORDER BY a.created_at LIMIT 1),
			'')`, org, creator).Scan(&customer.BillingStatus, &customer.Email); err != nil {
		return err
	}
	if customer.Email == "" {
		return nil
	}
	return c.Sync(ctx, customer)
}

// RunSync pushes every Clerk organization to ChewedFeed now and then every interval: organizations
// from before the sync, plans granted by hand (no Stripe event) and anything a webhook missed.
// ponytail: one POST per org per pass; a batch endpoint in ChewedFeed if orgs reach the thousands
func (c ChewedFeed) RunSync(ctx context.Context, db *pgxpool.Pool, orgs OrgLister, every time.Duration) {
	if c.URL == "" || c.Key == "" {
		return
	}
	for {
		list, err := allOrgs(ctx, orgs)
		if err != nil {
			logs.Warnf("billing: chewedfeed sync: %v", err)
		}
		for _, org := range list {
			created := time.UnixMilli(org.CreatedAt).UTC()
			if err := c.Push(ctx, db, org.ID, org.Name, org.CreatedBy, &created); err != nil {
				logs.Warnf("billing: chewedfeed sync %s: %v", org.ID, err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}
