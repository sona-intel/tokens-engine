// Package cache keeps answers of slow sources in memory for a while, and joins callers that ask for the same key at once
package cache

import (
	"context"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

type entry[V any] struct {
	v       V
	err     error
	expires time.Time
}

// Cache holds values and errors under string keys until each one expires
type Cache[V any] struct {
	max     int
	timeout time.Duration
	now     func() time.Time

	mu    sync.Mutex
	items map[string]entry[V]
	group singleflight.Group
}

// New keeps at most max entries, and gives each fetch timeout to finish even when its caller stops waiting
func New[V any](maxEntries int, timeout time.Duration) *Cache[V] {
	return &Cache[V]{max: maxEntries, timeout: timeout, now: time.Now, items: map[string]entry[V]{}}
}

// TTL says how long a fetched answer stays, zero or less keeps it out
type TTL[V any] func(v V, err error) time.Duration

// Get answers from the cache or runs fetch once for every caller waiting on key.
// The fetch keeps running after ctx ends, so a slow source still fills the cache for the next caller.
func (c *Cache[V]) Get(ctx context.Context, key string, ttl TTL[V], fetch func(context.Context) (V, error)) (V, error) {
	if e, ok := c.lookup(key); ok {
		return e.v, e.err
	}
	ch := c.group.DoChan(key, func() (any, error) {
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.timeout)
		defer cancel()
		v, err := fetch(fctx)
		if d := ttl(v, err); d > 0 {
			c.store(key, entry[V]{v: v, err: err, expires: c.now().Add(d)})
		}
		return entry[V]{v: v, err: err}, nil
	})
	select {
	case res := <-ch:
		e := res.Val.(entry[V])
		return e.v, e.err
	case <-ctx.Done():
		var zero V
		return zero, ctx.Err()
	}
}

// Peek returns a live entry without fetching
func (c *Cache[V]) Peek(key string) (V, error, bool) {
	e, ok := c.lookup(key)
	return e.v, e.err, ok
}

func (c *Cache[V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items)
}

func (c *Cache[V]) lookup(key string) (entry[V], bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.items[key]
	if !ok {
		return entry[V]{}, false
	}
	if !c.now().Before(e.expires) {
		delete(c.items, key)
		return entry[V]{}, false
	}
	return e, true
}

func (c *Cache[V]) store(key string, e entry[V]) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items[key] = e
	if len(c.items) <= c.max {
		return
	}
	now := c.now()
	for k, it := range c.items {
		if !now.Before(it.expires) {
			delete(c.items, k)
		}
	}
	// still full: drop the entries closest to expiry until a tenth is free
	for len(c.items) > c.max-c.max/10 {
		var victim string
		var soonest time.Time
		for k, it := range c.items {
			if k != key && (victim == "" || it.expires.Before(soonest)) {
				victim, soonest = k, it.expires
			}
		}
		if victim == "" {
			return
		}
		delete(c.items, victim)
	}
}
