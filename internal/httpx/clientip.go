package httpx

import (
	"net"
	"net/http"
	"strings"
	"sync/atomic"
)

// trustedProxyHops is how many proxies sit in front of us, each appending the address it saw to
// X-Forwarded-For. Set once at startup from TRUSTED_PROXY_HOPS (SetTrustedProxyHops); defaults to 1,
// a single trusted edge, which is the behaviour before this was configurable.
var trustedProxyHops atomic.Int32

func init() { trustedProxyHops.Store(1) }

// SetTrustedProxyHops sets how many right-most forwarding hops we trust. Call once at startup, before
// serving. 0 trusts none: X-Forwarded-For is ignored and the connection address is used, which is
// what a directly reachable origin (local Compose, or any host that isn't only reached through its
// edge) must use so a caller can't make up its own address. Negative is treated as 0.
func SetTrustedProxyHops(n int) {
	if n < 0 {
		n = 0
	}
	trustedProxyHops.Store(int32(n))
}

// ClientIP is the caller's address. Requests reach us through trusted proxies (our hosting's edge and
// the web app's /api proxy), each appending the address it saw to X-Forwarded-For, so the client is
// the entry just left of the hops our infrastructure wrote. We trust exactly the configured number of
// right-most hops and no more, so a caller can't prepend a made-up address and be believed.
//
// This is only sound while the origin is reached *only* through that many trusted proxies: a directly
// reachable origin must set TRUSTED_PROXY_HOPS=0 (or not be exposed), since then the caller's own
// connection is the right-most hop and any X-Forwarded-For it sends is its own to choose.
func ClientIP(r *http.Request) string {
	remote := remoteHost(r)
	hops := int(trustedProxyHops.Load())
	if hops == 0 {
		return remote
	}
	// the forwarding chain, client-side first: the X-Forwarded-For entries, then the address our
	// server actually saw on the connection (always the right-most, always real).
	chain := make([]string, 0, 8)
	if f := r.Header.Get("X-Forwarded-For"); f != "" {
		for _, p := range strings.Split(f, ",") {
			if p = strings.TrimSpace(p); p != "" {
				chain = append(chain, p)
			}
		}
	}
	chain = append(chain, remote)
	// drop the trusted right-most hops; the client is the next one to the left. When the chain is
	// shorter than the hops we trust (fewer proxies than configured, or a stray direct call), fall
	// back to the left-most entry we have rather than reading off the end.
	i := len(chain) - 1 - hops
	if i < 0 {
		i = 0
	}
	return chain[i]
}

func remoteHost(r *http.Request) string {
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
