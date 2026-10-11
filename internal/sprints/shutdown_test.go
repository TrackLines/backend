package sprints

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// On shutdown the loop stops when its context is cancelled, before main closes the pool; it used
// to keep ticking and log "auto-close: closed pool" on every deploy (Bugfixes report 15098).
func TestRunAutoCloseStopsOnShutdown(t *testing.T) {
	db, err := pgxpool.New(context.Background(), os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { Store{DB: db}.RunAutoClose(ctx, 10*time.Millisecond); close(done) }()
	time.Sleep(30 * time.Millisecond) // a few ticks
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunAutoClose kept running after shutdown")
	}
	db.Close() // what main does next: nothing is left using the pool
}
