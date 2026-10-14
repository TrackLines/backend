package ratelimit

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/tracklines/backend/internal/httpx"
)

// window is this minute's bucket for key: name, who, and the minute.
func window(name, who string, t time.Time) (string, int64) {
	w := t.Unix() / 60
	return "ratelimit:" + name + ":" + who + ":" + strconv.FormatInt(w, 10), (w+1)*60 - t.Unix()
}

func tooMany(w http.ResponseWriter, retry int64) {
	w.Header().Set("Retry-After", strconv.FormatInt(retry, 10))
	http.Error(w, "rate limit exceeded: try again shortly", http.StatusTooManyRequests)
}

// PerIP limits a route to perMinute requests per client address (httpx.RateKey: IPv6 by /64), for
// routes anyone can call without signing in: the public request form, public roadmaps, webhooks.
// Counter errors let the request through (NewCounter falls back to memory first, so that's rare).
func PerIP(c Counter, name string, perMinute int, now func() time.Time) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t := now()
			key, retry := window(name, httpx.RateKey(r), t)
			n, err := c.Add(r.Context(), key, 1, 70*time.Second)
			if err != nil {
				logs.Warnf("ratelimit %s: %v", name, err)
			} else if n > int64(perMinute) {
				tooMany(w, retry)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// statusRecorder notes the status a handler wrote.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// BadKeys wraps the API-key middlewares: a client address that presents perMinute invalid tl_ (or retired bf_)
// keys in a minute is refused (429) before any further key lookups hit the database.
func BadKeys(c Counter, perMinute int, now func() time.Time) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !strings.HasPrefix(token, "tl_") && !strings.HasPrefix(token, "bf_") {
				next.ServeHTTP(w, r)
				return
			}
			key, retry := window("badkey", httpx.RateKey(r), now())
			if n, err := c.Add(r.Context(), key, 0, 70*time.Second); err == nil && n >= int64(perMinute) {
				tooMany(w, retry)
				return
			}
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)
			if rec.status == http.StatusUnauthorized { // the key was refused
				_, _ = c.Add(r.Context(), key, 1, 70*time.Second)
			}
		})
	}
}

// Flush and Unwrap keep streaming responses (the live board's server-sent events) working through it.
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// Total limits a route to perMinute requests in all, whoever sends them: a ceiling for public routes
// whose every request costs something elsewhere (enquiries land in a person's inbox), so spreading
// across addresses can't flood it (TL-251).
func Total(c Counter, name string, perMinute int, now func() time.Time) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key, retry := window(name, "all", now())
			if n, err := c.Add(r.Context(), key, 1, 70*time.Second); err == nil && n > int64(perMinute) {
				tooMany(w, retry)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
