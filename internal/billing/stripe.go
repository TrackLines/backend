package billing

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Stripe is a minimal form-encoded client for the few Stripe endpoints billing needs
// (same approach as bugfixes/orchestrator). ponytail: no stripe-go dependency for a
// handful of calls; switch if the surface grows.
type Stripe struct {
	SecretKey string
	BaseURL   string // defaults to https://api.stripe.com
	Client    *http.Client
}

type stripeError struct {
	Status int
	Type   string
}

func (e stripeError) Error() string {
	return fmt.Sprintf("stripe returned HTTP %d (%s)", e.Status, e.Type)
}

func (s Stripe) call(ctx context.Context, method, path string, form url.Values, idempotencyKey string, out any) error {
	base := s.BaseURL
	if base == "" {
		base = "https://api.stripe.com"
	}
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.SecretKey)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("stripe request: %w", err)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("stripe response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var failure struct {
			Error struct {
				Type string `json:"type"`
			} `json:"error"`
		}
		_ = json.Unmarshal(payload, &failure)
		return stripeError{Status: resp.StatusCode, Type: failure.Error.Type}
	}
	return json.Unmarshal(payload, out)
}

// CreateCustomer is idempotent per clerk user so double clicks can't create two customers.
func (s Stripe) CreateCustomer(ctx context.Context, clerkID, email string) (string, error) {
	var customer struct {
		ID string `json:"id"`
	}
	form := url.Values{"metadata[clerk_id]": {clerkID}}
	if email != "" {
		form.Set("email", email)
	}
	if err := s.call(ctx, http.MethodPost, "/v1/customers", form, "tracklines-customer-"+clerkID, &customer); err != nil {
		return "", err
	}
	if customer.ID == "" {
		return "", errors.New("stripe returned no customer id")
	}
	return customer.ID, nil
}

func (s Stripe) CheckoutURL(ctx context.Context, clerkID, customerID, priceID, successURL, cancelURL string) (string, error) {
	form := url.Values{
		"mode":                                  {"subscription"},
		"allow_promotion_codes":                 {"true"},
		"customer":                              {customerID},
		"client_reference_id":                   {clerkID},
		"line_items[0][price]":                  {priceID},
		"line_items[0][quantity]":               {"1"},
		"subscription_data[metadata][clerk_id]": {clerkID},
		"success_url":                           {successURL},
		"cancel_url":                            {cancelURL},
	}
	return s.sessionURL(ctx, "/v1/checkout/sessions", form)
}

func (s Stripe) sessionURL(ctx context.Context, path string, form url.Values) (string, error) {
	var session struct {
		URL string `json:"url"`
	}
	if err := s.call(ctx, http.MethodPost, path, form, "", &session); err != nil {
		return "", err
	}
	if !strings.HasPrefix(session.URL, "https://") {
		return "", errors.New("stripe returned no session url")
	}
	return session.URL, nil
}

// Subscription is the part of a Stripe subscription billing stores.
type Subscription struct {
	ID         string
	CustomerID string
	Status     string
}

func (s Stripe) Subscription(ctx context.Context, id string) (Subscription, error) {
	var sub struct {
		ID       string `json:"id"`
		Customer string `json:"customer"`
		Status   string `json:"status"`
	}
	if err := s.call(ctx, http.MethodGet, "/v1/subscriptions/"+url.PathEscape(id), nil, "", &sub); err != nil {
		return Subscription{}, err
	}
	if sub.ID == "" || sub.Customer == "" || sub.Status == "" {
		return Subscription{}, errors.New("subscription is missing id, customer, or status")
	}
	return Subscription{ID: sub.ID, CustomerID: sub.Customer, Status: sub.Status}, nil
}

const webhookTolerance = 5 * time.Minute

// VerifyWebhook checks a Stripe-Signature header ("t=<unix>,v1=<hex>[,v1=...]")
// against HMAC-SHA256(secret, "<t>.<payload>") and rejects stale timestamps.
func VerifyWebhook(payload []byte, header, secret string, now time.Time) error {
	if secret == "" {
		return errors.New("webhook secret is not configured")
	}
	var timestamp int64
	var signatures [][]byte
	for _, part := range strings.Split(header, ",") {
		key, value, _ := strings.Cut(strings.TrimSpace(part), "=")
		switch key {
		case "t":
			timestamp, _ = strconv.ParseInt(value, 10, 64)
		case "v1":
			if sig, err := hex.DecodeString(value); err == nil {
				signatures = append(signatures, sig)
			}
		}
	}
	if timestamp == 0 || len(signatures) == 0 {
		return errors.New("malformed signature header")
	}
	if age := now.Sub(time.Unix(timestamp, 0)); age > webhookTolerance || age < -webhookTolerance {
		return errors.New("signature timestamp outside tolerance")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(timestamp, 10) + "."))
	mac.Write(payload)
	expected := mac.Sum(nil)
	for _, sig := range signatures {
		if hmac.Equal(sig, expected) {
			return nil
		}
	}
	return errors.New("signature mismatch")
}

func (s Stripe) PortalURL(ctx context.Context, customerID, returnURL string) (string, error) {
	return s.sessionURL(ctx, "/v1/billing_portal/sessions", url.Values{"customer": {customerID}, "return_url": {returnURL}})
}
