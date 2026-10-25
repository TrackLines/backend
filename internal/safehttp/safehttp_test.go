package safehttp

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"
)

// TL-161: a self-hosted Bugfixes URL can't make the server call internal addresses.
func TestPublicAddressesOnly(t *testing.T) {
	for addr, public := range map[string]bool{
		"8.8.8.8": true, "2606:4700:4700::1111": true,
		"127.0.0.1": false, "::1": false, // loopback
		"10.0.0.5": false, "172.16.3.4": false, "192.168.1.1": false, "fd12:3456::1": false, // private (Railway's network is fd…)
		"169.254.169.254": false, "fe80::1": false, // link-local: cloud metadata
		"100.64.0.1": false, // CGNAT
		"0.0.0.0":    false, "224.0.0.1": false, "::ffff:127.0.0.1": false,
	} {
		if got := PublicAddress(netip.MustParseAddr(addr)); got != public {
			t.Errorf("%s public = %v, want %v", addr, got, public)
		}
	}
}

func TestClientRefusesInternalAndRedirects(t *testing.T) {
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("secret")) }))
	defer internal.Close()
	_, err := Client(2 * time.Second).Get(internal.URL) // 127.0.0.1
	if !errors.Is(err, ErrPrivateAddress) {
		t.Fatalf("internal address: %v", err)
	}
	// redirects aren't followed (the hop could point inside)
	redirect := httptest.NewServer(http.RedirectHandler("http://169.254.169.254/latest/meta-data/", http.StatusFound))
	defer redirect.Close()
	c := Client(2 * time.Second)
	c.Transport = http.DefaultTransport // reach the local test server; only the redirect rule is under test
	resp, err := c.Get(redirect.URL)
	if err != nil || resp.StatusCode != http.StatusFound {
		t.Fatalf("redirect followed: %v %v", resp, err)
	}
	resp.Body.Close()
}
