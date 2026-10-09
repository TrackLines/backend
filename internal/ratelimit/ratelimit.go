// Package ratelimit caps how many requests an API key (tl_ or bf_) can make per minute, counted in
// Valkey so every backend instance shares the count. Browser sessions aren't limited.
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
// It must run after the API-key middleware. It fails open: with no Valkey, perMinute <= 0, or a
// Valkey error, requests go through. /mcp itself isn't counted: each tool call it makes passes
// back through here and is counted then.
func Middleware(vk valkey.Client, perMinute int, now func() time.Time) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if vk == nil || perMinute <= 0 {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !auth.ViaAPIKey(r.Context()) || r.URL.Path == "/mcp" {
				next.ServeHTTP(w, r)
				return
			}
			t := now()
			window := t.Unix() / 60
			key := "ratelimit:" + auth.OrgID(r.Context()) + ":" + auth.ActorID(r.Context()) + ":" + strconv.FormatInt(window, 10)
			res := vk.DoMulti(r.Context(),
				vk.B().Incr().Key(key).Build(),
				vk.B().Expire().Key(key).Seconds(70).Build(), // a little past the window, then gone
			)
			count, err := res[0].AsInt64()
			if err != nil {
				logs.Warnf("ratelimit: %v", err)
				next.ServeHTTP(w, r)
				return
			}
			w.Header().Set("X-RateLimit-Limit", strconv.Itoa(perMinute))
			w.Header().Set("X-RateLimit-Remaining", strconv.FormatInt(max(int64(perMinute)-count, 0), 10))
			if count > int64(perMinute) {
				w.Header().Set("Retry-After", strconv.FormatInt((window+1)*60-t.Unix(), 10))
				http.Error(w, "rate limit exceeded: try again shortly", http.StatusTooManyRequests)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
