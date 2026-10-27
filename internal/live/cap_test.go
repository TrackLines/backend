package live

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	tcvalkey "github.com/testcontainers/testcontainers-go/modules/valkey"
	"github.com/valkey-io/valkey-go"

	"github.com/tracklines/backend/internal/auth"
)

// TL-254: each person holds at most Max streams; one closing frees its place, once
func TestStreamCap(t *testing.T) {
	c := NewStreamCap(2)
	r1, ok1 := c.acquire("user_a")
	_, ok2 := c.acquire("user_a")
	if _, ok3 := c.acquire("user_a"); !ok1 || !ok2 || ok3 {
		t.Fatalf("cap of 2: %v %v %v", ok1, ok2, ok3)
	}
	if _, ok := c.acquire("user_b"); !ok {
		t.Fatal("someone else's streams count against user_a's")
	}
	r1()
	r1() // twice is still one
	if _, ok := c.acquire("user_a"); !ok {
		t.Fatal("a closed stream didn't free its place")
	}
	if _, ok := c.acquire("user_a"); ok {
		t.Fatal("a double release freed two places")
	}
}

// the stream handler refuses one over the cap with 429 live_streams, and takes it again once one closes
func TestStreamRefusedOverCap(t *testing.T) {
	ctx := context.Background()
	container, err := tcvalkey.Run(ctx, "valkey/valkey:8-alpine")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })
	url, err := container.ConnectionString(ctx)
	if err != nil {
		t.Fatal(err)
	}
	opt, err := valkey.ParseURL(url)
	if err != nil {
		t.Fatal(err)
	}
	vk, err := valkey.NewClient(opt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(vk.Close)
	s := System{VK: vk, Now: time.Now, Streams: NewStreamCap(1)}
	as := func(user string) context.Context { return auth.WithAPIKey(ctx, user, "org_x", "", "personal") }

	open, closeFirst := context.WithCancel(as("user_a"))
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.stream(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil).WithContext(open), "e", "c")
	}()
	refused := func(user string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		c, stop := context.WithTimeout(as(user), 50*time.Millisecond) // a stream it does get ends soon
		defer stop()
		req := httptest.NewRequest("GET", "/", nil).WithContext(c)
		req.Header.Set("Accept", "application/json")
		s.stream(rec, req, "e", "c")
		return rec
	}
	held := func() int { s.Streams.mu.Lock(); defer s.Streams.mu.Unlock(); return s.Streams.open["user_a"] }
	for deadline := time.Now().Add(5 * time.Second); held() == 0; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the first stream never opened")
		}
	}
	rec := refused("user_a")
	if rec.Code != 429 || !strings.Contains(rec.Body.String(), "live_streams") || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("over the cap: %d %s", rec.Code, rec.Body)
	}
	other, stop := context.WithTimeout(as("user_b"), 50*time.Millisecond)
	defer stop()
	other2 := httptest.NewRecorder()
	s.stream(other2, httptest.NewRequest("GET", "/", nil).WithContext(other), "e", "c")
	if other2.Code == 429 {
		t.Fatal("another person was refused")
	}
	closeFirst()
	<-done
	again, stop2 := context.WithTimeout(as("user_a"), 50*time.Millisecond)
	defer stop2()
	rec = httptest.NewRecorder()
	s.stream(rec, httptest.NewRequest("GET", "/", nil).WithContext(again), "e", "c")
	if rec.Code == 429 {
		t.Fatal("still refused after the first stream closed")
	}
}
