package live

import (
	"errors"
	"testing"
	"time"
)

func TestConnectRetriesWithGrowingWaits(t *testing.T) {
	var waits []time.Duration
	tries := 0
	client, err := Connect(8, func(d time.Duration) { waits = append(waits, d) }, func() (string, error) {
		if tries++; tries < 6 {
			return "", errors.New("connection refused")
		}
		return "valkey", nil
	})
	if err != nil || client != "valkey" || tries != 6 {
		t.Fatalf("connect: %q %v after %d tries", client, err, tries)
	}
	want := []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}
	if len(waits) != len(want) {
		t.Fatalf("waits: %v", waits)
	}
	for i := range want {
		if waits[i] != want[i] {
			t.Fatalf("waits: %v, want %v", waits, want)
		}
	}

	// never reachable: gives up after the last attempt, waits capped at 10s, no wait after the last try
	waits, tries = nil, 0
	if _, err := Connect(7, func(d time.Duration) { waits = append(waits, d) }, func() (string, error) { tries++; return "", errors.New("down") }); err == nil || tries != 7 {
		t.Fatalf("unreachable: %v after %d tries", err, tries)
	}
	if len(waits) != 6 || waits[5] != 10*time.Second {
		t.Fatalf("capped waits: %v", waits)
	}
}
