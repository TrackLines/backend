package internal

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

// A request still running when the server is told to stop finishes before serveOn returns, because
// returning is what lets main close the database (audit, 2026-10-17).
func TestServeDrainsInFlightRequests(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	started, finished := make(chan struct{}), false
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		time.Sleep(300 * time.Millisecond)
		finished = true
		w.Write([]byte("done"))
	})
	ctx, stop := context.WithCancel(context.Background())
	returned := make(chan error, 1)
	go func() { returned <- serveOn(ctx, ln, h, 5*time.Second) }()

	got := make(chan string, 1)
	go func() {
		res, err := http.Get("http://" + ln.Addr().String() + "/")
		if err != nil {
			got <- "error: " + err.Error()
			return
		}
		b, _ := io.ReadAll(res.Body)
		got <- string(b)
	}()
	<-started
	stop() // a deploy, mid-request
	if err := <-returned; err != nil {
		t.Fatal(err)
	}
	if !finished {
		t.Fatal("serveOn returned before the request finished: the database would close under it")
	}
	if body := <-got; body != "done" {
		t.Fatalf("the request was cut off: %q", body)
	}
}
