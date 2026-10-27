package live

import (
	"sync"

	"github.com/tracklines/backend/internal/httpx"
)

// MaxStreams is how many live streams one person (their sessions and API keys together) may hold open
// on an instance at once (TL-254): plenty for a board or two in several tabs, and few enough that a
// leaked key can't use up the instance's connections.
const MaxStreams = 10

var ErrTooManyStreams = httpx.Code("live_streams", "too many live views open: close some tabs and try again")

// StreamCap counts the open streams per person, on this instance: running out of connections and
// goroutines is per instance too, and an in-memory count can't outlive a stream the way a shared one can.
// ponytail: per instance; a shared Valkey count only if a person's streams must be capped across instances.
type StreamCap struct {
	mu   sync.Mutex
	open map[string]int
	Max  int
}

func NewStreamCap(max int) *StreamCap { return &StreamCap{open: map[string]int{}, Max: max} }

// acquire takes one of who's streams; false when they're all in use. release gives it back.
func (c *StreamCap) acquire(who string) (release func(), ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.open[who] >= c.Max {
		return nil, false
	}
	c.open[who]++
	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			if c.open[who]--; c.open[who] <= 0 {
				delete(c.open, who)
			}
		})
	}, true
}
