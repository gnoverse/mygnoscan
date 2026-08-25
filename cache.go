package main

import (
	"bytes"
	"net/http"
	"sync"
	"time"
)

// Response caching for the read-only API.
//
// Nothing was cached before this: every request recomputed its aggregates from
// scratch, and the expensive ones are expensive every time. /api/analytics was
// 8.5s of SQL on the production database, repeated per visitor per page load.
//
// The data only moves when the sync loop writes, once every 30 seconds, so a
// short TTL costs no freshness that the pipeline could have delivered anyway.
const (
	// cacheTTL matches the sync interval: a shorter one would expire entries
	// that cannot have changed, a longer one would serve rows the syncer has
	// already replaced.
	cacheTTL = 30 * time.Second

	// cacheMaxEntries bounds memory. Keys are (path, query), and the query
	// carries network, limit, offset, sort and days — a crawler walking
	// pagination could otherwise grow this without limit.
	cacheMaxEntries = 512

	// cacheMaxBodyBytes keeps one oversized response from dominating the cache.
	// /api/txs without a limit can return the whole chain.
	cacheMaxBodyBytes = 8 << 20 // 8 MiB
)

type cacheEntry struct {
	body        []byte
	contentType string
	storedAt    time.Time
}

type responseCache struct {
	mu      sync.Mutex
	entries map[string]cacheEntry
	ttl     time.Duration

	hits, misses int
}

func newResponseCache(ttl time.Duration) *responseCache {
	return &responseCache{entries: map[string]cacheEntry{}, ttl: ttl}
}

func (c *responseCache) get(key string) (cacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || time.Since(e.storedAt) > c.ttl {
		c.misses++
		return cacheEntry{}, false
	}
	c.hits++
	return e, true
}

func (c *responseCache) put(key string, e cacheEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Drop anything already expired before deciding the map is full, so a burst
	// of distinct keys does not evict entries that are still good.
	if len(c.entries) >= cacheMaxEntries {
		for k, v := range c.entries {
			if time.Since(v.storedAt) > c.ttl {
				delete(c.entries, k)
			}
		}
	}
	// Still full: this is a pathological key space rather than normal traffic,
	// so start over rather than grow without bound.
	if len(c.entries) >= cacheMaxEntries {
		c.entries = map[string]cacheEntry{}
	}
	c.entries[key] = e
}

func (c *responseCache) stats() (hits, misses, size int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits, c.misses, len(c.entries)
}

// cachingWriter buffers a handler's response so it can be stored. It records the
// status so only successes are kept: caching a 500 would pin a transient indexer
// failure for the whole TTL.
type cachingWriter struct {
	http.ResponseWriter
	status int
	buf    bytes.Buffer
	tooBig bool
}

func (w *cachingWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *cachingWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if !w.tooBig {
		if w.buf.Len()+len(b) > cacheMaxBodyBytes {
			w.tooBig = true
			w.buf.Reset()
		} else {
			w.buf.Write(b)
		}
	}
	return w.ResponseWriter.Write(b)
}

// cacheable reports whether a request may be served from, and stored in, the
// cache.
//
// /api/live is a Server-Sent Events stream that never completes, so buffering it
// would hold the response open forever and leak the buffer. /api/version is
// constant and free to compute — caching it would only add bookkeeping.
func cacheable(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	switch r.URL.Path {
	case "/api/live", "/api/version":
		return false
	}
	return len(r.URL.Path) >= 5 && r.URL.Path[:5] == "/api/"
}

// withResponseCache serves repeated identical GETs from memory for the TTL.
//
// Deliberately keyed on path plus raw query, so ?network=sapphire and
// ?network=gnoland1 are different entries and a network cannot be served another
// one's data.
func withResponseCache(c *responseCache, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !cacheable(r) {
			next.ServeHTTP(w, r)
			return
		}

		key := r.URL.Path + "?" + r.URL.RawQuery
		if e, ok := c.get(key); ok {
			if e.contentType != "" {
				w.Header().Set("Content-Type", e.contentType)
			}
			w.Header().Set("X-Cache", "HIT")
			w.Write(e.body)
			return
		}

		w.Header().Set("X-Cache", "MISS")
		cw := &cachingWriter{ResponseWriter: w}
		next.ServeHTTP(cw, r)

		if cw.status == http.StatusOK && !cw.tooBig && cw.buf.Len() > 0 {
			c.put(key, cacheEntry{
				body:        append([]byte(nil), cw.buf.Bytes()...),
				contentType: cw.Header().Get("Content-Type"),
				storedAt:    time.Now(),
			})
		}
	})
}
