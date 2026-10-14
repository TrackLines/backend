package ratelimit

import (
	"context"
	"sync"
	"time"

	"github.com/valkey-io/valkey-go"
)

// Counter adds n to a key's count (n = 0 reads it), the key lasting ttl.
type Counter interface {
	Add(ctx context.Context, key string, n int64, ttl time.Duration) (int64, error)
}

// NewCounter counts in Valkey (shared by every instance) or, without it, in this instance's memory,
// so limits never switch off just because Valkey is missing. If Valkey fails mid-flight, that call is
// counted in memory too, rather than let through uncounted (TL-251).
func NewCounter(vk valkey.Client) Counter {
	mem := &memoryCounter{counts: map[string]memoryCount{}}
	if vk != nil {
		return fallbackCounter{valkeyCounter{vk}, mem}
	}
	return mem
}

// fallbackCounter counts in primary, or in backup when primary errors.
type fallbackCounter struct {
	primary, backup Counter
}

func (c fallbackCounter) Add(ctx context.Context, key string, n int64, ttl time.Duration) (int64, error) {
	if v, err := c.primary.Add(ctx, key, n, ttl); err == nil {
		return v, nil
	}
	return c.backup.Add(ctx, key, n, ttl)
}

type valkeyCounter struct{ vk valkey.Client }

func (c valkeyCounter) Add(ctx context.Context, key string, n int64, ttl time.Duration) (int64, error) {
	res := c.vk.DoMulti(ctx,
		c.vk.B().Incrby().Key(key).Increment(n).Build(),
		c.vk.B().Expire().Key(key).Seconds(int64(ttl.Seconds())).Build(),
	)
	return res[0].AsInt64()
}

// memoryCounter: per instance, so with N instances the effective limit is up to N times higher.
// ponytail: one mutex and a sweep of expired keys on each call; fine at our traffic, Valkey above it.
type memoryCounter struct {
	mu     sync.Mutex
	counts map[string]memoryCount
}

type memoryCount struct {
	n       int64
	expires time.Time
}

func (c *memoryCounter) Add(_ context.Context, key string, n int64, ttl time.Duration) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if len(c.counts) > 10_000 {
		for k, v := range c.counts {
			if now.After(v.expires) {
				delete(c.counts, k)
			}
		}
		// ponytail: under a flood of live keys, start afresh rather than sweep the whole map on every
		// call; counts reset, which is mild next to stalling every request behind the lock
		if len(c.counts) > 50_000 {
			c.counts = map[string]memoryCount{}
		}
	}
	v := c.counts[key]
	if now.After(v.expires) {
		v = memoryCount{}
	}
	v.n += n
	if v.expires.IsZero() || n > 0 {
		v.expires = now.Add(ttl)
	}
	c.counts[key] = v
	return v.n, nil
}
