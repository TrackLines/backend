package billing

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCreateCustomer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/customers" || r.Header.Get("Authorization") != "Bearer sk_test" ||
			r.Header.Get("Idempotency-Key") != "tracklines-customer-user_1" {
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
