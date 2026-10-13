package live

import (
	"time"

	"github.com/bugfixes/go-bugfixes/logs"
)

// Connect retries dialing Valkey with a growing wait (0.5s, 1s, 2s … capped at 10s) for up to
// attempts tries, so a backend that starts before Valkey is ready (a deploy race) still gets it.
// Live views (standup, refinement) and the caches need it; without it they answer 503 or skip caching.
// ponytail: retried at startup only; every system is handed the client once, so connecting later would
// mean swapping it under them. Add a reconnecting wrapper if Valkey outages after startup ever matter.
func Connect[C any](attempts int, sleep func(time.Duration), dial func() (C, error)) (C, error) {
	wait := 500 * time.Millisecond
	var client C
	var err error
	for i := 1; i <= attempts; i++ {
		if client, err = dial(); err == nil {
			if i > 1 {
				logs.Infof("valkey: connected after %d attempts", i)
			}
			return client, nil
		}
		if i < attempts {
			sleep(wait)
			wait = min(wait*2, 10*time.Second)
		}
	}
	return client, err
}
