package httpx

import (
	"context"
	"net/http"
	"time"
)

// ZoneHeader carries the caller's IANA timezone (e.g. Europe/London). Data is always stored in UTC;
// the zone only decides day-based rules, like a sprint's last day, in the caller's local time.
const ZoneHeader = "Tracklines-Timezone"

type zoneKey struct{}

// WithZone puts the caller's timezone in the request context. Missing or unknown zones are UTC
// (MCP, API keys, scripts).
func WithZone(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		loc := time.UTC
		// "Local" would be the server's zone, not the caller's
		if name := r.Header.Get(ZoneHeader); name != "" && name != "Local" {
			if l, err := time.LoadLocation(name); err == nil {
				loc = l
			}
		}
		next.ServeHTTP(w, r.WithContext(WithZoneContext(r.Context(), loc)))
	})
}

func WithZoneContext(ctx context.Context, loc *time.Location) context.Context {
	return context.WithValue(ctx, zoneKey{}, loc)
}

// Zone is the caller's timezone, UTC when none was given.
func Zone(ctx context.Context) *time.Location {
	if loc, ok := ctx.Value(zoneKey{}).(*time.Location); ok {
		return loc
	}
	return time.UTC
}
