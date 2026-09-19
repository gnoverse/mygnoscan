package httpapi

import (
	"bytes"
	"context"
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
//
// A plain TTL cache still hands the full cost to whoever arrives first after it
// expires, which on a low-traffic explorer is *most* visitors: measured against
// production, /api/govdao/overview was 1.8s cold and 50ms warm, /api/accounts
// 7.8s cold — and with a 30s TTL, every reader who shows up more than 30s after
// the last one pays the cold price. That is the slowness people actually report.
// So the cache serves stale entries immediately and refreshes them behind the
// reader (see WithResponseCache), and only a genuinely empty cache blocks.
const (
	// CacheTTL matches the sync interval: a shorter one would expire entries
	// that cannot have changed, a longer one would serve rows the syncer has
	// already replaced.
	CacheTTL = 30 * time.Second

	// CacheStaleGrace is how far past the TTL an entry may still be served
	// while its replacement is computed in the background. Sized in minutes,
	// not seconds, on purpose: its job is to cover the gap between two
	// visitors, and on this explorer that gap is minutes. An entry older than
	// TTL+grace is treated as absent — a reader returning after an hour away
	// should not be shown an hour-old chain tip, however fast.
	CacheStaleGrace = 15 * time.Minute

	// cacheMaxEntries bounds memory. Keys are (path, query, encoding), and the
	// query carries network, limit, offset, sort and days — a crawler walking
	// pagination could otherwise grow this without limit.
	cacheMaxEntries = 512

	// cacheMaxBodyBytes keeps one oversized response from dominating the cache.
	// /api/txs without a limit can return the whole chain.
	cacheMaxBodyBytes = 8 << 20 // 8 MiB

	// backgroundRefreshTimeout bounds a refresh that nobody is waiting on. The
	// slowest endpoint measured in production is ~8s; this leaves room for a
	// bad day without letting a wedged indexer pin a goroutine forever.
	backgroundRefreshTimeout = 60 * time.Second
)

type cacheEntry struct {
	body            []byte
	contentType     string
	contentEncoding string
	storedAt        time.Time
}

func (e cacheEntry) age() time.Duration { return time.Since(e.storedAt) }

type responseCache struct {
	mu      sync.Mutex
	entries map[string]cacheEntry
	ttl     time.Duration
	grace   time.Duration

	// refreshing marks keys with a background refresh already in flight, so a
	// burst of readers on one stale entry triggers one recompute, not one per
	// reader. Same map guarded by the same mutex as entries: the decision to
	// start a refresh and the read of the entry it refreshes have to be atomic
	// or two goroutines both see "stale, nobody refreshing".
	refreshing map[string]bool

	hits, misses, stale int
}

func NewResponseCache(ttl time.Duration) *responseCache {
	return &responseCache{
		entries:    map[string]cacheEntry{},
		refreshing: map[string]bool{},
		ttl:        ttl,
		grace:      CacheStaleGrace,
	}
}

// lookup classifies a key in one locked step and, when the answer is "stale",
// claims the background refresh for the caller.
//
// The three-way return is what lets the handler stay branch-free: fresh serves,
// stale serves and refreshes, absent blocks. `claimed` is only ever true
// alongside a stale entry, and the caller must release it when done.
func (c *responseCache) lookup(key string) (e cacheEntry, fresh bool, claimed bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok || entry.age() > c.ttl+c.grace {
		c.misses++
		return cacheEntry{}, false, false
	}
	if entry.age() <= c.ttl {
		c.hits++
		return entry, true, false
	}
	c.stale++
	if !c.refreshing[key] {
		c.refreshing[key] = true
		claimed = true
	}
	return entry, false, claimed
}

func (c *responseCache) releaseRefresh(key string) {
	c.mu.Lock()
	delete(c.refreshing, key)
	c.mu.Unlock()
}

func (c *responseCache) put(key string, e cacheEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Drop anything already beyond its serving life before deciding the map is
	// full, so a burst of distinct keys does not evict entries that are still
	// good — including the stale-but-servable ones, which are the whole point.
	if len(c.entries) >= cacheMaxEntries {
		for k, v := range c.entries {
			if v.age() > c.ttl+c.grace {
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

// Flush passes through, and marks the response unstorable.
//
// A handler that flushes is streaming; its body is not a value with a size, and
// buffering one to store it is how a never-ending stream turns into an
// ever-growing buffer. cacheable() already keeps /api/live out, but a future
// streaming endpoint should degrade to "not cached" rather than to a leak.
func (w *cachingWriter) Flush() {
	w.tooBig = true
	w.buf.Reset()
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// discardWriter collects a background refresh's response. Nobody is waiting on
// it, so there is nowhere to write: the point is purely the side effect of the
// handler running and its result landing in the cache via the cachingWriter
// wrapped around this.
type discardWriter struct {
	header http.Header
}

func (w *discardWriter) Header() http.Header         { return w.header }
func (w *discardWriter) Write(b []byte) (int, error) { return len(b), nil }
func (w *discardWriter) WriteHeader(int)             {}

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

// cacheKey identifies one stored response.
//
// Path plus raw query, so ?network=sapphire and ?network=gnoland1 are different
// entries and a network cannot be served another one's data — plus the
// negotiated content encoding, because WithCompression runs *inside* this cache
// and the stored bytes are therefore whatever that produced. Without the
// encoding in the key, the first gzip-accepting reader would poison the entry
// for every client that cannot decode it.
func cacheKey(r *http.Request) string {
	key := r.URL.Path + "?" + r.URL.RawQuery
	if acceptsGzip(r) {
		return key + "\x00gzip"
	}
	return key
}

func serveEntry(w http.ResponseWriter, e cacheEntry, state string) {
	if e.contentType != "" {
		w.Header().Set("Content-Type", e.contentType)
	}
	if e.contentEncoding != "" {
		w.Header().Set("Content-Encoding", e.contentEncoding)
	}
	w.Header().Add("Vary", "Accept-Encoding")
	w.Header().Set("X-Cache", state)
	w.Write(e.body)
}

// WithResponseCache serves repeated identical GETs from memory, and keeps
// serving them while they are refreshed.
//
// The three paths, in the order they matter to a reader:
//
//   - fresh entry: served as-is. X-Cache: HIT.
//   - stale entry, still within the grace window: served *immediately*, and one
//     background refresh is started for it. X-Cache: STALE. This is what turns
//     "every visitor after a 30s lull waits 1.8s" into "nobody waits".
//   - nothing usable: the handler runs inline and the reader waits, as before.
//     X-Cache: MISS.
func WithResponseCache(c *responseCache, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !cacheable(r) {
			next.ServeHTTP(w, r)
			return
		}

		key := cacheKey(r)
		entry, fresh, claimed := c.lookup(key)
		if fresh {
			serveEntry(w, entry, "HIT")
			return
		}
		if entry.body != nil {
			serveEntry(w, entry, "STALE")
			if claimed {
				// Cloned here, not in the goroutine: net/http may reuse the
				// *http.Request once the handler returns, and the refresh
				// outlives this handler by design.
				//
				// On a fresh background context, too. The reader whose visit
				// triggered this already has their (stale) answer and may
				// close the tab a millisecond later, which would cancel the
				// very refresh that was meant to serve the next reader.
				ctx, cancel := context.WithTimeout(context.Background(), backgroundRefreshTimeout)
				req := r.Clone(ctx)
				req.Body = http.NoBody
				go func() {
					defer cancel()
					c.refresh(key, req, next)
				}()
			}
			return
		}

		w.Header().Set("X-Cache", "MISS")
		cw := &cachingWriter{ResponseWriter: w}
		next.ServeHTTP(cw, r)
		c.store(key, cw)
	})
}

// refresh recomputes one entry with nobody waiting on it. The request is
// already detached from the reader's (see the call site).
func (c *responseCache) refresh(key string, req *http.Request, next http.Handler) {
	defer c.releaseRefresh(key)
	cw := &cachingWriter{ResponseWriter: &discardWriter{header: http.Header{}}}
	next.ServeHTTP(cw, req)
	c.store(key, cw)
}

func (c *responseCache) store(key string, cw *cachingWriter) {
	if cw.status != http.StatusOK || cw.tooBig || cw.buf.Len() == 0 {
		return
	}
	c.put(key, cacheEntry{
		body:            append([]byte(nil), cw.buf.Bytes()...),
		contentType:     cw.Header().Get("Content-Type"),
		contentEncoding: cw.Header().Get("Content-Encoding"),
		storedAt:        time.Now(),
	})
}
