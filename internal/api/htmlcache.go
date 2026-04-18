package api

import (
	"sync"
	"time"
)

// htmlCache is a tiny in-process TTL cache for rendered HTML fragments.
// Keys are constructed per-handler (usually "handler-name:param1=v1:param2=v2").
// Invalidate() bumps an internal version counter so all in-flight entries
// become stale instantly — cheaper than iterating the map.
type htmlCache struct {
	mu      sync.Mutex
	entries map[string]htmlCacheEntry
	ver     uint64
}

type htmlCacheEntry struct {
	body    []byte
	ver     uint64
	expires time.Time
}

func newHTMLCache() *htmlCache {
	return &htmlCache{entries: make(map[string]htmlCacheEntry)}
}

// Get returns the cached body for key if it exists, is not expired, and
// was stored at the current version. Returns (nil, false) otherwise.
func (c *htmlCache) Get(key string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || e.ver != c.ver || time.Now().After(e.expires) {
		return nil, false
	}
	// Return a copy-safe reference; the caller only reads.
	return e.body, true
}

// Set stores body for key with the given TTL, tagged with the current
// version. Overwrites any prior entry for the same key.
func (c *htmlCache) Set(key string, body []byte, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = htmlCacheEntry{
		body:    body,
		ver:     c.ver,
		expires: time.Now().Add(ttl),
	}
}

// Invalidate bumps the internal version counter so all previously-stored
// entries are considered stale on next Get. O(1). Use after any mutation
// that could affect cached output.
func (c *htmlCache) Invalidate() {
	c.mu.Lock()
	c.ver++
	c.mu.Unlock()
}
