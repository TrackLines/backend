package config

import "testing"

func TestBuild(t *testing.T) {
	t.Setenv("STRIPE_PRICE_ID", "price_123")
	c, err := Build()
	if err != nil {
		t.Fatal(err)
	}
	if p := Get(c); p == nil || p.Stripe.PriceID != "price_123" {
		t.Fatalf("project config not parsed: %+v", p)
	}
}
