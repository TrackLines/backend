// Package safehttp is the HTTP client for URLs an org admin typed in (a self-hosted Bugfixes, a
// webhook endpoint): it only ever connects to public addresses.
package safehttp

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"
)

var ErrPrivateAddress = errors.New("refusing to connect to a private or internal address")

var cgnat = netip.MustParsePrefix("100.64.0.0/10") // carrier-grade NAT, also used for internal networks

// PublicAddress reports whether ip is on the public internet: not loopback, private, link-local
// (which includes cloud metadata at 169.254.169.254), CGNAT, multicast or unspecified.
func PublicAddress(ip netip.Addr) bool {
	ip = ip.Unmap()
	return ip.IsValid() && !ip.IsLoopback() && !ip.IsPrivate() && !ip.IsLinkLocalUnicast() && !ip.IsLinkLocalMulticast() &&
		!ip.IsMulticast() && !ip.IsUnspecified() && !ip.IsInterfaceLocalMulticast() && !cgnat.Contains(ip)
}

// Client only connects to public addresses, checked after DNS resolves (so rebinding can't sneak
// past), and never follows redirects (TL-161).
func Client(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: timeout, Control: func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		ip, err := netip.ParseAddr(host)
		if err != nil || !PublicAddress(ip) {
			return fmt.Errorf("%w: %s", ErrPrivateAddress, host)
		}
		return nil
	}}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = dialer.DialContext
	transport.Proxy = nil // a proxy would dial on our behalf and skip the address check
	return &http.Client{Timeout: timeout, Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
