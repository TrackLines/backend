package mcp

import (
	"context"
	"net/http"
	"testing"

	"github.com/tracklines/backend/internal/httpx"
)

// in-process tool calls keep the caller's address: per-address limits on public routes (get_roadmap)
// count each caller, not every MCP call in every org as one address (TL-251)
func TestRequesterKeepsTheCallersAddress(t *testing.T) {
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(httpx.ClientIP(r))) })
	outer := http.Header{}
	outer.Set("X-Forwarded-For", "203.0.113.9")
	if _, body := requester(api, outer)(context.Background(), "GET", "/api/roadmaps/x", nil); string(body) != "203.0.113.9" {
		t.Fatalf("tool call seen from %q", body)
	}
}
