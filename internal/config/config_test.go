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
	// there's one ChewedFeed API; only the enquiry key turns customer sync and enquiries on
	if got := Get(c).ChewedFeedURL; got != "https://api.chewedfeed.com" {
		t.Fatalf("default ChewedFeed URL = %q", got)
	}
	if got, want := Get(c).AuthorizedParties(), []string{"https://tracklin.es"}; !equalStrings(got, want) {
		t.Fatalf("default authorized parties = %q, want %q", got, want)
	}
}

func TestAuthorizedPartiesFromConfig(t *testing.T) {
	p := &Project{ClerkAuthorizedParties: " https://app.example, http://localhost:3000 ,,"}
	if got, want := p.AuthorizedParties(), []string{"https://app.example", "http://localhost:3000"}; !equalStrings(got, want) {
		t.Fatalf("authorized parties = %q, want %q", got, want)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
