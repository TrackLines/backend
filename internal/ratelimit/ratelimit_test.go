package ratelimit

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	tcvalkey "github.com/testcontainers/testcontainers-go/modules/valkey"
	"github.com/tracklines/backend/internal/auth"
	"github.com/valkey-io/valkey-go"
)

func TestMiddleware(t *testing.T) {
	ctx := context.Background()
	container, err := tcvalkey.Run(ctx, "valkey/valkey:8-alpine")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = container.Terminate(ctx) })
	url, _ := container.ConnectionString(ctx)
	opt, _ := valkey.ParseURL(url)
	vk, err := valkey.NewClient(opt)
	if err != nil {
		t.Fatal(err)
	}

	clock := time.Unix(1_800_000_000, 0) // start of a minute
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	h := Middleware(vk, 3, func() time.Time { return clock })(ok)
	call := func(path, agent string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		if agent != "" {
			req = req.WithContext(auth.WithAPIKey(ctx, "user", "org_rl", agent, "ai"))
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	for i := range 3 {
		if rec := call("/api/projects", "claude"); rec.Code != 204 {
			t.Fatalf("request %d: %d", i+1, rec.Code)
		}
	}
	rec := call("/api/projects", "claude")
	if rec.Code != 429 || rec.Header().Get("Retry-After") != "60" || rec.Header().Get("X-RateLimit-Remaining") != "0" {
		t.Fatalf("over the limit: %d retry %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	if rec := call("/api/projects", "codex"); rec.Code != 204 {
		t.Fatalf("another key has its own budget: %d", rec.Code)
	}
	if rec := call("/mcp", "claude"); rec.Code != 204 {
		t.Fatalf("/mcp itself isn't counted: %d", rec.Code)
	}
	if rec := call("/api/projects", ""); rec.Code != 204 {
		t.Fatalf("browser sessions aren't limited: %d", rec.Code)
	}
	clock = clock.Add(time.Minute)
	if rec := call("/api/projects", "claude"); rec.Code != 204 {
		t.Fatalf("next minute resets: %d", rec.Code)
	}

	// fail open: Valkey gone, or not configured
	vk.Close()
	clock = clock.Add(-time.Minute) // the window that was full
	if rec := call("/api/projects", "claude"); rec.Code != 204 {
		t.Fatalf("valkey down should let requests through: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	Middleware(nil, 3, time.Now)(ok).ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != 204 {
		t.Fatalf("no valkey: %d", rec.Code)
	}
}
