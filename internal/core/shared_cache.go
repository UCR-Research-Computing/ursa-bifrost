package core

import (
	"context"
	"sync"
	"time"
)

// SharedCache holds command output that is the same for every user (node
// state, the whole queue, partitions, the site catalog: backend.Command.Public),
// so the hosted server asks the login node once for everyone instead of once
// per person. It also runs identical commands in flight only once
// (single-flight), for public and per-person commands alike.
//
// Per-person output is never stored here: a Service keeps its own cache for
// that, keyed under its own identity.
type SharedCache struct {
	mu      sync.Mutex
	entries map[string]cacheEntry
	flight  map[string]*flightCall
	now     func() time.Time
	Hits    int64
	Runs    int64
	Joined  int64
}

type flightCall struct {
	done chan struct{}
	data []byte
	err  error
}

// NewSharedCache returns an empty shared cache.
func NewSharedCache() *SharedCache {
	return &SharedCache{entries: map[string]cacheEntry{}, flight: map[string]*flightCall{}, now: time.Now}
}

// get returns a fresh stored value.
func (c *SharedCache) get(key string, ttl time.Duration) ([]byte, bool) {
	if c == nil || ttl <= 0 {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if ok && c.now().Sub(e.at) < ttl {
		c.Hits++
		return e.data, true
	}
	return nil, false
}

// do runs fn once for every concurrent caller with the same key and returns
// its result to all of them. When store is true a successful result is kept
// for later callers (public output only). Errors are never stored.
func (c *SharedCache) do(ctx context.Context, key string, store bool, fn func(context.Context) ([]byte, error)) ([]byte, error) {
	c.mu.Lock()
	if f, ok := c.flight[key]; ok {
		c.Joined++
		c.mu.Unlock()
		select {
		case <-f.done:
			return f.data, f.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f := &flightCall{done: make(chan struct{})}
	c.flight[key] = f
	c.Runs++
	c.mu.Unlock()

	// The command runs detached from this caller's cancellation, so one caller
	// giving up does not fail everyone else waiting on the same answer. The
	// backend applies its own per-command timeout.
	f.data, f.err = fn(context.WithoutCancel(ctx))

	c.mu.Lock()
	delete(c.flight, key)
	if store && f.err == nil {
		c.entries[key] = cacheEntry{at: c.now(), data: f.data}
	}
	c.mu.Unlock()
	close(f.done)
	return f.data, f.err
}

// Sweep drops entries older than maxAge (called periodically by the server).
func (c *SharedCache) Sweep(maxAge time.Duration) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, e := range c.entries {
		if c.now().Sub(e.at) > maxAge {
			delete(c.entries, k)
		}
	}
}
