package httpx

import (
	"net"
	"net/http"
	"strings"
)

// ClientIP is the caller's address. Requests reach us through our hosting's edge (which appends the
// address it saw to X-Forwarded-For) and the web app's /api proxy (which passes headers through), so
// the right-most hop is the one our infrastructure wrote; anything to its left came from the client
// and can be made up. Without the header (direct calls, tests) it's the connection's address.
func ClientIP(r *http.Request) string {
	if f := r.Header.Get("X-Forwarded-For"); f != "" {
		hops := strings.Split(f, ",")
		if ip := strings.TrimSpace(hops[len(hops)-1]); ip != "" {
			return ip
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// RateKey is who a rate limit counts: the client's address, but an IPv6 address by its /64. One
// machine is usually given a whole /64, so counting single addresses let one caller rotate through
// billions of them and never be limited (TL-251).
func RateKey(r *http.Request) string {
	ip := net.ParseIP(ClientIP(r))
	if ip == nil || ip.To4() != nil {
		return ClientIP(r)
	}
	return ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
}
