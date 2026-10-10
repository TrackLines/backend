package billing

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// ChewedFeed pushes customers to ChewedFeed HQ (POST /api/product-customers/sync, keyed by
// the tracklines project's enquiry key, same endpoint bugfixes uses). Empty URL or Key = off.
type ChewedFeed struct {
	URL    string
	Key    string
	Client *http.Client
}

type chewedFeedCustomer struct {
	ExternalCustomerID string    `json:"external_customer_id"`
	Name               string    `json:"name"`
	Email              string    `json:"email"`
	Plan               string    `json:"plan"`
	BillingStatus      string    `json:"billing_status"`
	SignedUpAt         time.Time `json:"signed_up_at"`
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

// plan maps a Stripe status onto ChewedFeed's plan column (it counts plan <> 'free' as paid).
func plan(status string) string {
	switch status {
	case "active", "trialing", "past_due":
		return "paid"
	}
	return "free"
}
