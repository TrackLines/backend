package ratelimit

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TL-169: limits hold without Valkey (memory counter), by trusted client address.
func TestPerIPAndBadKeys(t *testing.T) {
	c := NewCounter(nil)
	clock := time.Unix(1_800_000_000, 0)
	now := func() time.Time { return clock }
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	send := func(h http.Handler, xff, auth string) int {
		req := httptest.NewRequest("POST", "/", nil)
		if xff != "" {
			req.Header.Set("X-Forwarded-For", xff)
		}
		if auth != "" {
			req.Header.Set("Authorization", "Bearer "+auth)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	// public route: 3 a minute per address; spoofing the left of X-Forwarded-For doesn't help
	form := PerIP(c, "intake", 3, now)(ok)
	for i := range 3 {
		if code := send(form, "fake-"+string(rune('a'+i))+", 203.0.113.7", ""); code != 204 {
			t.Fatalf("request %d: %d", i+1, code)
		}
	}
	if code := send(form, "fake-z, 203.0.113.7", ""); code != 429 {
		t.Fatalf("4th from the same address: %d", code)
	}
	if code := send(form, "203.0.113.8", ""); code != 204 {
		t.Fatalf("another address: %d", code)
	}
	clock = clock.Add(time.Minute)
	if code := send(form, "203.0.113.7", ""); code != 204 {
		t.Fatalf("next minute: %d", code)
	}

	// invalid keys: after 2 refused keys an address is stopped before the key check runs
	checked := 0
	keyCheck := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checked++
		if a := r.Header.Get("Authorization"); a == "" || a == "Bearer tl_good" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.Error(w, "invalid api key", http.StatusUnauthorized)
	})
	guard := BadKeys(c, 2, now)(keyCheck)
	for range 2 {
		if code := send(guard, "198.51.100.1", "tl_guess"); code != 401 {
			t.Fatalf("bad key: %d", code)
		}
	}
	if code, before := send(guard, "198.51.100.1", "tl_guess"), checked; code != 429 || checked != before {
		t.Fatalf("third bad key should be refused without a lookup: %d (checked %d)", code, checked)
	}
	if code := send(guard, "198.51.100.2", "tl_good"); code != 204 {
		t.Fatalf("another address with a good key: %d", code)
	}
	if code := send(guard, "198.51.100.1", ""); code != 204 {
		t.Fatalf("no key (sessions) isn't affected: %d", code)
	}
	// good keys aren't counted as failures
	for range 5 {
		if code := send(guard, "198.51.100.3", "tl_good"); code != 204 {
			t.Fatalf("good key: %d", code)
		}
	}
}
