package httpx

import (
	"net/http"
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

// TrustedProxyHops decides how far left of the connection we believe X-Forwarded-For. It's global
// (set once at startup), so these don't run in parallel and each resets to the default of 1.
func TestClientIPTrustedHops(t *testing.T) {
	t.Cleanup(func() { SetTrustedProxyHops(1) })
	req := func(xff, remote string) *http.Request {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = remote
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		return r
	}

	// 0 hops: a directly reachable origin (local Compose). X-Forwarded-For is a caller-supplied header
	// here, so it's ignored and the connection address is used — this is the demonstrated rate-limit
	// bypass (rotating X-Forwarded-For) being closed.
	SetTrustedProxyHops(0)
	if got := ClientIP(req("10.0.0.1", "198.51.100.5:4444")); got != "198.51.100.5" {
		t.Fatalf("hops=0 should ignore X-Forwarded-For, got %q", got)
	}
	if got := ClientIP(req("10.0.0.1, 10.0.0.2, 10.0.0.3", "198.51.100.5:4444")); got != "198.51.100.5" {
		t.Fatalf("hops=0 should ignore every X-Forwarded-For hop, got %q", got)
	}

	// 1 hop: one trusted edge. An attacker prepends a fake address; the edge appends the real one it
	// saw, so the real one is the trusted right-most X-Forwarded-For hop.
	SetTrustedProxyHops(1)
	if got := ClientIP(req("1.1.1.1, 203.0.113.9", "10.0.0.1:1234")); got != "203.0.113.9" {
		t.Fatalf("hops=1 forged left hops: %q", got)
	}

	// 2 hops: two trusted proxies in front, so the client is the third address from the right.
	SetTrustedProxyHops(2)
	if got := ClientIP(req("203.0.113.9, 10.0.0.2", "10.0.0.1:1234")); got != "203.0.113.9" {
		t.Fatalf("hops=2 should skip two trusted hops: %q", got)
	}
	// forged: attacker prepends a fake; still skipped past the two trusted hops.
	if got := ClientIP(req("1.1.1.1, 203.0.113.9, 10.0.0.2", "10.0.0.1:1234")); got != "203.0.113.9" {
		t.Fatalf("hops=2 forged: %q", got)
	}
	// chain shorter than the trusted hops (fewer proxies than configured): fall back to the left-most
	// we have rather than reading off the end.
	if got := ClientIP(req("203.0.113.9", "10.0.0.1:1234")); got != "203.0.113.9" {
		t.Fatalf("hops=2 short chain should clamp left: %q", got)
	}

	// negative is treated as 0.
	SetTrustedProxyHops(-3)
	if got := ClientIP(req("10.0.0.1", "198.51.100.5:4444")); got != "198.51.100.5" {
		t.Fatalf("negative hops should behave as 0, got %q", got)
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
