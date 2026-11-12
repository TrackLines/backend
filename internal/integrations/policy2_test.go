package integrations

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// policy2 says which notification types are urgent (TL-882): asked with {"data": {"type": …}}, answered once a minute per type.
func TestUrgentPolicy(t *testing.T) {
	var asked atomic.Int32
	down := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		if r.URL.Path != "/policy/pol-1" || r.Header.Get("X-API-Key") != "key-1" {
			t.Errorf("request: %s %q", r.URL.Path, r.Header.Get("X-API-Key"))
		}
		if down {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		var body struct {
			Data struct {
				Type string `json:"type"`
			} `json:"data"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		_ = json.NewEncoder(w).Encode(map[string]any{"result": body.Data.Type == "offer"})
	}))
	defer srv.Close()

	if p, err := NewUrgentPolicy("", "pol-1", srv.URL); p != nil || err != nil {
		t.Fatalf("no key: %v %v", p, err)
	}
	var none *UrgentPolicy
	if none.Urgent(context.Background(), "offer") {
		t.Fatal("no policy: urgent")
	}
	p, err := NewUrgentPolicy("key-1", "pol-1", srv.URL)
	if err != nil || p == nil {
		t.Fatalf("NewUrgentPolicy: %v", err)
	}
	ctx := context.Background()
	if !p.Urgent(ctx, "offer") || p.Urgent(ctx, "mention") {
		t.Fatal("offer is urgent, mention isn't")
	}
	if p.Urgent(ctx, "offer"); asked.Load() != 2 {
		t.Fatalf("cached: asked %d times", asked.Load())
	}
	down = true
	if p.Urgent(ctx, "assigned") { // can't ask: not urgent, the notice still goes
		t.Fatal("down: urgent")
	}
}
