package ratelimit

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type brokenCounter struct{}

func (brokenCounter) Add(context.Context, string, int64, time.Duration) (int64, error) {
	return 0, errors.New("valkey: connection refused")
}

// when Valkey fails, limits keep counting in memory instead of letting every request through (TL-251)
func TestFallbackCounterKeepsCounting(t *testing.T) {
	c := fallbackCounter{brokenCounter{}, &memoryCounter{counts: map[string]memoryCount{}}}
	var n int64
	var err error
	for range 3 {
		n, err = c.Add(context.Background(), "k", 1, time.Minute)
	}
	if err != nil || n != 3 {
		t.Fatalf("counted %d (%v), want 3", n, err)
	}
}

// Total caps a route whoever calls it: different addresses share one ceiling (TL-251)
func TestTotalCapsEveryone(t *testing.T) {
	now := func() time.Time { return time.Unix(1_000_000, 0) }
	h := Total(NewCounter(nil), "t", 2, now)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	codes := []int{}
	for _, ip := range []string{"203.0.113.1", "203.0.113.2", "203.0.113.3"} {
		r := httptest.NewRequest("POST", "/", nil)
		r.Header.Set("X-Forwarded-For", ip)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		codes = append(codes, rec.Code)
	}
	if codes[0] != 200 || codes[1] != 200 || codes[2] != 429 {
		t.Fatalf("codes %v, want the third caller refused", codes)
	}
}
