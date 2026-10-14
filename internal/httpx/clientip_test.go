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

// rate limits count an IPv6 caller by its /64: one machine usually holds a whole /64, and counting
// single addresses let it rotate past every limit (TL-251). IPv4 is counted per address.
func TestRateKey(t *testing.T) {
	key := func(xff string) string {
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("X-Forwarded-For", xff)
		return RateKey(r)
	}
	if a, b := key("2001:db8:1:2::1"), key("2001:db8:1:2:ffff:ffff:ffff:fffe"); a != b || a != "2001:db8:1:2::/64" {
		t.Fatalf("same /64 counted apart: %q %q", a, b)
	}
	if key("2001:db8:1:2::1") == key("2001:db8:1:3::1") {
		t.Fatal("different /64s counted together")
	}
	if key("203.0.113.7") != "203.0.113.7" || key("203.0.113.7") == key("203.0.113.8") {
		t.Fatal("IPv4 is per address")
	}
}
