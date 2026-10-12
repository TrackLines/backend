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
// so limits never switch off just because Valkey is missing.
func NewCounter(vk valkey.Client) Counter {
	if vk != nil {
		return valkeyCounter{vk}
	}
	return &memoryCounter{counts: map[string]memoryCount{}}
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
