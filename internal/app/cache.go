package app

import (
	"context"
	"sync"
	"time"
)

// cache holds expensive library scans for a while so that paging between
// tabs, or returning from a confirmation page, does not re-read every
// service. Concurrent requests for the same key share one load.
type cache[T any] struct {
	ttl     time.Duration
	mu      sync.Mutex
	entries map[string]*cacheEntry[T]
}

type cacheEntry[T any] struct {
	mu    sync.Mutex
	value T
	at    time.Time
	ok    bool
}

func newCache[T any](ttl time.Duration) *cache[T] {
	return &cache[T]{ttl: ttl, entries: map[string]*cacheEntry[T]{}}
}

// get returns the cached value, loading it when missing, stale or forced.
func (c *cache[T]) get(ctx context.Context, key string, force bool, now time.Time, load func(context.Context) (T, error)) (T, time.Time, error) {
	c.mu.Lock()
	e, ok := c.entries[key]
	if !ok {
		e = &cacheEntry[T]{}
		c.entries[key] = e
	}
	c.mu.Unlock()

	e.mu.Lock()
	defer e.mu.Unlock()

	if e.ok && !force && now.Sub(e.at) < c.ttl {
		return e.value, e.at, nil
	}

	v, err := load(ctx)
	if err != nil {
		var zero T
		return zero, time.Time{}, err
	}
	e.value, e.at, e.ok = v, now, true
	return v, now, nil
}

// update mutates a cached value in place, e.g. to drop deleted items, so the
// page stays honest without another full scan.
func (c *cache[T]) update(key string, fn func(T) T) {
	c.mu.Lock()
	e, ok := c.entries[key]
	c.mu.Unlock()
	if !ok {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ok {
		e.value = fn(e.value)
	}
}

// clear drops everything, e.g. after settings change.
func (c *cache[T]) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = map[string]*cacheEntry[T]{}
}

// clearPrefix drops keys for one feature.
func (c *cache[T]) clearMatching(match func(string) bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.entries {
		if match(k) {
			delete(c.entries, k)
		}
	}
}
