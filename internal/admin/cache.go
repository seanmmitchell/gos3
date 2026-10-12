package admin

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// maxCacheEntries bounds a ttlCache; expired entries are pruned first.
const maxCacheEntries = 4096

// ttlCache caches computed values for a fixed TTL and de-duplicates
// concurrent computations of the same key (singleflight): while a value is
// being computed, further callers wait for that computation instead of
// starting their own.
//
// Computations run detached from the caller's cancellation (a caller giving
// up does not fail the other waiters) but are bounded by computeTimeout, so
// they cannot leak.
type ttlCache[V any] struct {
	ttl            time.Duration
	computeTimeout time.Duration
	now            func() time.Time

	mu      sync.Mutex
	entries map[string]cacheEntry[V]
	calls   map[string]*cacheCall[V]
}

type cacheEntry[V any] struct {
	v  V
	at time.Time
}

type cacheCall[V any] struct {
	done chan struct{}
	v    V
	at   time.Time
	err  error
}

func newTTLCache[V any](ttl, computeTimeout time.Duration, now func() time.Time) *ttlCache[V] {
	return &ttlCache[V]{
		ttl:            ttl,
		computeTimeout: computeTimeout,
		now:            now,
		entries:        map[string]cacheEntry[V]{},
		calls:          map[string]*cacheCall[V]{},
	}
}

// get returns the cached value of key (and when it was computed), computing
// it with compute when missing or expired. Errors are not cached.
func (c *ttlCache[V]) get(ctx context.Context, key string, compute func(context.Context) (V, error)) (V, time.Time, error) {
	c.mu.Lock()
	if e, ok := c.entries[key]; ok && c.now().Sub(e.at) < c.ttl {
		c.mu.Unlock()
		return e.v, e.at, nil
	}
	call, ok := c.calls[key]
	if !ok {
		call = &cacheCall[V]{done: make(chan struct{})}
		c.calls[key] = call
		go c.run(context.WithoutCancel(ctx), key, call, compute)
	}
	c.mu.Unlock()

	select {
	case <-call.done:
		return call.v, call.at, call.err
	case <-ctx.Done():
		var zero V
		return zero, time.Time{}, ctx.Err()
	}
}

func (c *ttlCache[V]) run(ctx context.Context, key string, call *cacheCall[V], compute func(context.Context) (V, error)) {
	ctx, cancel := context.WithTimeout(ctx, c.computeTimeout)
	defer cancel()
	defer close(call.done)
	defer func() {
		if p := recover(); p != nil {
			call.err = fmt.Errorf("admin: panic computing %q: %v", key, p)
			c.mu.Lock()
			if c.calls[key] == call {
				delete(c.calls, key)
			}
			c.mu.Unlock()
		}
	}()
	v, err := compute(ctx)
	at := c.now()
	call.v, call.at, call.err = v, at, err

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.calls[key] != call {
		return // invalidated while computing: don't store a stale value
	}
	delete(c.calls, key)
	if err != nil {
		return
	}
	if len(c.entries) >= maxCacheEntries {
		c.pruneLocked()
	}
	c.entries[key] = cacheEntry[V]{v: v, at: at}
}

// pruneLocked drops expired entries and, if still full, arbitrary ones.
func (c *ttlCache[V]) pruneLocked() {
	now := c.now()
	for k, e := range c.entries {
		if now.Sub(e.at) >= c.ttl {
			delete(c.entries, k)
		}
	}
	for k := range c.entries {
		if len(c.entries) < maxCacheEntries {
			break
		}
		delete(c.entries, k)
	}
}

// invalidate forgets key; an in-flight computation still answers its
// waiters but its result is not stored.
func (c *ttlCache[V]) invalidate(key string) {
	c.mu.Lock()
	delete(c.entries, key)
	delete(c.calls, key)
	c.mu.Unlock()
}

// invalidateAll forgets every entry.
func (c *ttlCache[V]) invalidateAll() {
	c.mu.Lock()
	clear(c.entries)
	clear(c.calls)
	c.mu.Unlock()
}
