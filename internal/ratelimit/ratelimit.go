// Package ratelimit caps requests per minute: per API key (tl_), per client address on public
// routes (PerIP), and on invalid keys (BadKeys). Counted in Valkey so every backend instance shares
// the count, or in memory without it. Browser sessions aren't limited by key.
package ratelimit

import (
	"net/http"
	"strconv"
	"time"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/tracklines/backend/internal/auth"
	"github.com/valkey-io/valkey-go"
)

// Middleware limits API-key callers to perMinute requests in each clock minute (fixed window).
// It must run after the API-key middleware. perMinute <= 0 turns it off; a counter error lets the
// request through; without Valkey it counts per instance. /mcp itself isn't counted: each tool call
// it makes passes back through here and is counted then.
func Middleware(vk valkey.Client, perMinute int, now func() time.Time) func(http.Handler) http.Handler {
	c := NewCounter(vk)
	return func(next http.Handler) http.Handler {
		if perMinute <= 0 {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !auth.ViaAPIKey(r.Context()) || r.URL.Path == "/mcp" {
				next.ServeHTTP(w, r)
				return
			}
			t := now()
			key, retry := window("key:"+auth.OrgID(r.Context()), auth.ActorID(r.Context()), t)
			count, err := c.Add(r.Context(), key, 1, 70*time.Second) // a little past the window, then gone
			if err != nil {
				logs.Warnf("ratelimit: %v", err)
				next.ServeHTTP(w, r)
				return
			}
			w.Header().Set("X-RateLimit-Limit", strconv.Itoa(perMinute))
			w.Header().Set("X-RateLimit-Remaining", strconv.FormatInt(max(int64(perMinute)-count, 0), 10))
			if count > int64(perMinute) {
				tooMany(w, retry)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
