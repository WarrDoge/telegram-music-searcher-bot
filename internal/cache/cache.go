// Package cache provides thread-safe caching with TTL support
package cache

import (
	"context"
	"sync"
	"time"
)

// Cache defines the interface for caching operations
type Cache interface {
	Get(key string) (string, bool)
	Set(key, value string, ttl time.Duration)
	StartCleanup(ctx context.Context, interval time.Duration)
}

// Entry represents a cached value with expiration
type Entry struct {
	Value     string
	ExpiresAt time.Time
}

// SimpleCache is a thread-safe in-memory cache with TTL support
type SimpleCache struct {
	mu    sync.RWMutex
	cache map[string]Entry
}

// New creates a new SimpleCache instance
func New() *SimpleCache {
	return &SimpleCache{
		cache: make(map[string]Entry),
	}
}

// Get retrieves a value from the cache
// Returns the value and true if found and not expired, otherwise empty string and false
func (c *SimpleCache) Get(key string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	entry, ok := c.cache[key]
	if !ok || time.Now().After(entry.ExpiresAt) {
		return "", false
	}
	return entry.Value, true
}

// Set stores a value in the cache with the specified TTL
func (c *SimpleCache) Set(key, value string, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.cache[key] = Entry{
		Value:     value,
		ExpiresAt: time.Now().Add(ttl),
	}
}

// StartCleanup starts a background goroutine that periodically removes expired entries
// The goroutine will stop when the context is canceled
func (c *SimpleCache) StartCleanup(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			c.cleanup()
		case <-ctx.Done():
			return
		}
	}
}

// cleanup removes all expired entries from the cache
func (c *SimpleCache) cleanup() {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	for k, v := range c.cache {
		if now.After(v.ExpiresAt) {
			delete(c.cache, k)
		}
	}
}

// Size returns the current number of entries in the cache (including expired ones)
func (c *SimpleCache) Size() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.cache)
}
