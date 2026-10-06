package billing

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCreateCustomer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/customers" || r.Header.Get("Authorization") != "Bearer sk_test" ||
			!strings.HasPrefix(r.Header.Get("Idempotency-Key"), "tracklines-customer-user_1-") {
			t.Errorf("bad request: %s %v", r.URL.Path, r.Header)
		}
		_ = r.ParseForm()
		if r.Form.Get("metadata[clerk_id]") != "user_1" || r.Form.Get("email") != "a@b.c" {
			t.Errorf("bad form: %v", r.Form)
		}
		_, _ = w.Write([]byte(`{"id":"cus_123"}`))
	}))
	defer srv.Close()
	id, err := Stripe{SecretKey: "sk_test", BaseURL: srv.URL}.CreateCustomer(context.Background(), "user_1", "a@b.c")
	if err != nil || id != "cus_123" {
		t.Fatalf("got %q %v", id, err)
	}

	fail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = w.Write([]byte(`{"error":{"type":"card_error"}}`))
	}))
	defer fail.Close()
	if _, err := (Stripe{BaseURL: fail.URL}).CreateCustomer(context.Background(), "u", ""); err == nil || err.Error() != "stripe returned HTTP 402 (card_error)" {
		t.Fatalf("error path: %v", err)
	}
}

func TestResolvePrice(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/products/prod_str":
			_, _ = w.Write([]byte(`{"default_price":"price_1"}`))
		case "/v1/products/prod_obj":
			_, _ = w.Write([]byte(`{"default_price":{"id":"price_2"}}`))
		case "/v1/products/prod_none":
			_, _ = w.Write([]byte(`{"default_price":null}`))
		default:
			t.Errorf("unexpected call %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	s := Stripe{BaseURL: srv.URL}
	for in, want := range map[string]string{"price_9": "price_9", "prod_str": "price_1", "prod_obj": "price_2"} {
		if got, err := s.ResolvePrice(context.Background(), in); err != nil || got != want {
			t.Errorf("%s: got %q %v, want %q", in, got, err, want)
		}
	}
	if _, err := s.ResolvePrice(context.Background(), "prod_none"); err == nil {
		t.Error("product without default price should error")
	}
}
