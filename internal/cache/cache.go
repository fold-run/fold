// Package cache is a small TTL cache with single-flight refresh, used for
// upstream list results.
package cache

import (
	"context"
	"sync"
	"time"
)

type entry struct {
	value   any
	expires time.Time
	// inflight coalesces concurrent refreshes of the same key.
	inflight chan struct{}
	err      error
}

// Cache maps string keys to values with per-entry TTLs.
type Cache struct {
	mu      sync.Mutex
	entries map[string]*entry
	now     func() time.Time
}

// New returns an empty cache.
func New() *Cache {
	return &Cache{entries: map[string]*entry{}, now: time.Now}
}

// GetOrFill returns the cached value for key, or runs fill (once across
// concurrent callers) and caches its result for ttl. ttl <= 0 bypasses the
// cache entirely.
func (c *Cache) GetOrFill(ctx context.Context, key string, ttl time.Duration, fill func(context.Context) (any, error)) (any, error) {
	if ttl <= 0 {
		return fill(ctx)
	}
	c.mu.Lock()
	e := c.entries[key]
	if e != nil && e.inflight == nil && c.now().Before(e.expires) {
		v := e.value
		c.mu.Unlock()
		return v, nil
	}
	if e != nil && e.inflight != nil {
		ch := e.inflight
		c.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		c.mu.Lock()
		e = c.entries[key]
		if e != nil && e.inflight == nil && c.now().Before(e.expires) {
			v := e.value
			c.mu.Unlock()
			return v, nil
		}
		err := error(nil)
		if e != nil {
			err = e.err
		}
		c.mu.Unlock()
		if err != nil {
			return nil, err
		}
		// Filler raced with invalidation; fall through to a direct fill.
		return fill(ctx)
	}
	e = &entry{inflight: make(chan struct{})}
	c.entries[key] = e
	c.mu.Unlock()

	v, err := fill(ctx)

	c.mu.Lock()
	close(e.inflight)
	if err != nil {
		e.err = err
		delete(c.entries, key)
	} else {
		e.value = v
		e.err = nil
		e.expires = c.now().Add(ttl)
		e.inflight = nil
	}
	c.mu.Unlock()
	return v, err
}

// Invalidate drops all entries whose key has the given prefix.
func (c *Cache) Invalidate(prefix string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, e := range c.entries {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix && e.inflight == nil {
			delete(c.entries, k)
		}
	}
}
