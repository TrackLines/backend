package httpx

import (
	"net/http/httptest"
	"testing"
)

func TestClientIP(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.9:5555"
	if got := ClientIP(r); got != "10.0.0.9" {
		t.Fatalf("no header: %q", got)
	}
	// the client can put anything on the left; the right-most hop is what our edge wrote
	r.Header.Set("X-Forwarded-For", "1.2.3.4, 6.6.6.6, 203.0.113.7")
	if got := ClientIP(r); got != "203.0.113.7" {
		t.Fatalf("spoofed left hops: %q", got)
	}
	r.Header.Set("X-Forwarded-For", "203.0.113.8")
	if got := ClientIP(r); got != "203.0.113.8" {
		t.Fatalf("single hop: %q", got)
	}
}
