package httpapi

import (
	"bytes"
	"context"
	"net/http"
	"net/url"
	"strings"
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
	// ttl is this entry's own freshness window, stamped when it was stored.
	// Carried per entry rather than read from the cache, because how long an
	// answer stays good is a property of the question (see endpointTTL).
	ttl time.Duration
}

func (e cacheEntry) age() time.Duration { return time.Since(e.storedAt) }

// endpointTTL overrides CacheTTL for endpoints whose answer either changes
// more slowly than the sync loop, or costs more to recompute than the default
// TTL allows.
//
// The second case is the one that bites. A 30s TTL on a computation that takes
// 30 to 50s means the entry expires before its own replacement lands, so the
// endpoint sits permanently in stale-and-refreshing: measured over a 3-minute
// probe on 2026-09-24, /api/govdao/overview answered 56 STALE against 28 HIT
// and never once settled. Nobody waited for it, thanks to the stale window, but
// the process burned a full recompute every 30 seconds forever, per key, for
// data that changes when somebody votes.
//
// These are ceilings on staleness, not promises of it: the warmer refreshes on
// its own schedule, so a longer TTL here buys fewer redundant recomputes rather
// than older data.
//
// The rule for adding one: **measure the cold cost, and give it a TTL of at
// least ten times that**, or the process spends more than a tenth of its life
// recomputing a single endpoint for nobody. Measure with a cache-busting query
// parameter against a running instance:
//
//	curl -s -o /dev/null -w '%{time_total}\n' "$HOST/api/<path>?_p=$RANDOM"
//
// Every warm target was measured that way against production on 2026-09-24, and
// the result is why this map is short: /api/accounts came back in 0.82s,
// /api/analytics in 1.09s, /api/contracts/map in 0.48s, and thirteen others
// under 0.35s. An earlier measurement of the same endpoints had /api/accounts at
// 7.8s and /api/analytics at 8.5s; the balance cache and the rollups fixed those
// since, so the numbers to act on are the ones you just took, not the ones in an
// old issue.
var endpointTTL = map[string]time.Duration{
	// 24.8s to 51.1s cold, four runs. The list page runs the full detail-page
	// audit per row, and the audit re-walks every proposal creation.
	"/api/govdao":          2 * time.Minute,
	"/api/govdao/overview": 2 * time.Minute,
	"/api/govdao/voters":   2 * time.Minute,
	// 8.0s cold, and the only endpoint outside govdao that is not already
	// comfortably inside the default TTL.
	"/api/allevents": 2 * time.Minute,
}

// cacheTTLFor returns the freshness window for one path.
func (c *responseCache) cacheTTLFor(path string) time.Duration {
	if ttl, ok := endpointTTL[path]; ok {
		return ttl
	}
	return c.ttl
}

type responseCache struct {
	mu      sync.Mutex
	entries map[string]cacheEntry
	ttl     time.Duration
	grace   time.Duration

	// inflight holds one channel per key currently being computed, closed when
	// that computation lands. It covers both kinds of computation -- the
	// background refresh behind a stale entry, and the inline one a reader
	// waits on when there is nothing to serve -- because they are the same
	// work and running both at once is pure waste.
	//
	// It used to be a bool, and only the background refresh consulted it: the
	// MISS path had no dedupe at all. Measured against production on
	// 2026-09-24, four concurrent requests on one cold /api/govdao/overview
	// key returned four X-Cache: MISS, each having recomputed the whole thing,
	// and the latency grew with the concurrency (2.39s, 2.79s, 3.03s, 3.13s)
	// because they were competing for the same upstream node.
	//
	// Same map guarded by the same mutex as entries: the decision to start a
	// computation and the read of the entry it would replace have to be atomic
	// or two goroutines both conclude "nothing here, nobody working".
	inflight map[string]chan struct{}

	hits, misses, stale, coalesced int
}

func NewResponseCache(ttl time.Duration) *responseCache {
	return &responseCache{
		entries:  map[string]cacheEntry{},
		inflight: map[string]chan struct{}{},
		ttl:      ttl,
		grace:    CacheStaleGrace,
	}
}

// lookup classifies a key in one locked step and, when the caller is the one
// who has to do the work, hands it ownership of the computation.
//
// Four outcomes, which is what lets the handler stay flat:
//
//	fresh entry                 serve it, nothing else to do
//	stale entry, claimed        serve it, refresh it in the background
//	stale entry, not claimed    serve it, someone else is already refreshing
//	nothing usable              claimed says compute; otherwise wait on `wait`
//
// A caller that receives claimed must call done(key) when its computation
// lands, whatever the outcome, or every waiter on that key hangs until its
// own context expires.
func (c *responseCache) lookup(key string) (e cacheEntry, fresh, claimed bool, wait <-chan struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.entries[key]
	usable := ok && entry.age() <= entry.ttl+c.grace
	switch {
	case usable && entry.age() <= entry.ttl:
		c.hits++
		return entry, true, false, nil
	case usable:
		c.stale++
	default:
		c.misses++
		entry = cacheEntry{}
	}

	if done, running := c.inflight[key]; running {
		if !usable {
			c.coalesced++
		}
		return entry, false, false, done
	}
	done := make(chan struct{})
	c.inflight[key] = done
	return entry, false, true, done
}

// done releases a claimed key and wakes everything waiting on it. Safe to call
// twice; the second call is a no-op rather than a close of a closed channel.
func (c *responseCache) done(key string) {
	c.mu.Lock()
	if ch, ok := c.inflight[key]; ok {
		delete(c.inflight, key)
		close(ch)
	}
	c.mu.Unlock()
}

// get returns a still-servable entry, which is what a woken waiter needs: the
// leader has just stored one, and the waiter wants to serve it without going
// back through lookup and claiming a computation of its own.
func (c *responseCache) get(key string) (cacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || e.age() > e.ttl+c.grace {
		return cacheEntry{}, false
	}
	return e, true
}

func (c *responseCache) put(key string, e cacheEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Drop anything already beyond its serving life before deciding the map is
	// full, so a burst of distinct keys does not evict entries that are still
	// good — including the stale-but-servable ones, which are the whole point.
	if len(c.entries) >= cacheMaxEntries {
		for k, v := range c.entries {
			if v.age() > v.ttl+c.grace {
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

// CacheStats is what /api/cache/stats reports. It exists so "the site is slow"
// arrives with a cache state attached: a high miss count against a low entry
// count means the warmer is not covering the keys people actually ask for, and
// a stale count that dwarfs the hits means a refresh is slower than the TTL it
// is refreshing under.
type CacheStats struct {
	Hits      int `json:"hits"`
	Misses    int `json:"misses"`
	Stale     int `json:"stale"`
	Coalesced int `json:"coalesced"`
	Entries   int `json:"entries"`
	InFlight  int `json:"in_flight"`
}

func (c *responseCache) stats() CacheStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return CacheStats{
		Hits:      c.hits,
		Misses:    c.misses,
		Stale:     c.stale,
		Coalesced: c.coalesced,
		Entries:   len(c.entries),
		InFlight:  len(c.inflight),
	}
}

// Stats is the exported view, for the handler that serves it.
func (c *responseCache) Stats() CacheStats { return c.stats() }

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
	case "/api/cache/stats":
		// It reports the cache's own live counters. Cached, it would report
		// the state the cache was in thirty seconds ago, which is the one
		// question it exists to answer correctly.
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
//
// EscapedPath, not Path, because the two are not the same request. A third of
// transaction hashes are base64 containing a slash: `/api/tx/a%2Fb` routes to
// the transaction handler and answers JSON, while `/api/tx/a/b` matches no API
// route and falls through to the SPA's HTML. Path decodes both to the same
// string, so whichever arrived first was served to the other for the whole
// TTL: an agent asking for a transaction got a page of HTML because something
// had asked for the unescaped path a minute earlier.
func cacheKey(r *http.Request) string {
	key := r.URL.EscapedPath() + "?" + canonicalQuery(r.URL.RawQuery)
	if acceptsGzip(r) {
		return key + "\x00gzip"
	}
	return key
}

// trackingParams are query parameters that never reach a handler and exist
// only to tell somebody else where a click came from. Left in the key, a link
// shared on Twitter or in a newsletter is a different cache entry from the same
// link typed by hand, so the person arriving from the newsletter pays the cold
// price for a page that is already cached.
var trackingParams = map[string]bool{
	"fbclid": true, "gclid": true, "mc_cid": true, "mc_eid": true,
	"igshid": true, "msclkid": true, "twclid": true, "yclid": true,
	"ref_src": true, "ref_url": true, "_ga": true, "_gl": true,
}

// canonicalQuery normalizes a query string so two requests a handler cannot
// tell apart share one cache entry.
//
// Three normalizations, each one a case seen in production:
//
//   - network=all is dropped. networkParam maps both "all" and absent to the
//     empty string and RejectUnknownNetwork skips both, so ?network=all and no
//     network at all are one request computed and stored twice. The SPA sends
//     ?network=all by default, which is how the expensive half of this ended up
//     duplicated against the cheap half.
//   - parameters are sorted, so ?limit=10&network=mainnet and
//     ?network=mainnet&limit=10 are one entry.
//   - utm_* and the click-id parameters above are dropped.
//
// Deliberately conservative about everything else: the only way to know a
// parameter is ignored is to read the handler, and a key that over-shares
// serves one request's answer to a different question. Empty values are kept
// for that reason, since ?q= is not always ?q absent.
func canonicalQuery(raw string) string {
	if raw == "" {
		return ""
	}
	values, err := url.ParseQuery(raw)
	if err != nil {
		// Unparseable: leave it exactly as it arrived rather than guess. It
		// still keys correctly, it just does not get to share.
		return raw
	}
	for k := range values {
		if trackingParams[k] || strings.HasPrefix(k, "utm_") {
			delete(values, k)
		}
	}
	if values.Get("network") == "all" {
		values.Del("network")
	}
	// Encode sorts by key, which is the ordering guarantee this wants. It
	// leaves each key's values in the order they were given, so a repeated
	// parameter keeps its order and therefore its meaning.
	return values.Encode()
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
//   - nothing usable, and nobody computing it: the handler runs inline and the
//     reader waits. X-Cache: MISS.
//   - nothing usable, but someone is already computing this exact response:
//     the reader waits for *their* result instead of starting a second copy of
//     the same work. X-Cache: WAIT. Without this, a page that fetches one
//     endpoint twice, or two visitors arriving together, paid for it twice.
func WithResponseCache(c *responseCache, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !cacheable(r) {
			next.ServeHTTP(w, r)
			return
		}

		key := cacheKey(r)
		entry, fresh, claimed, wait := c.lookup(key)
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

		if !claimed {
			// Somebody else is already computing this exact response. Their
			// answer is our answer, so wait for it rather than run a second
			// copy of work that is expensive precisely when it is cold.
			select {
			case <-wait:
				if e, ok := c.get(key); ok {
					serveEntry(w, e, "WAIT")
					return
				}
				// The leader finished without storing anything: it errored,
				// streamed, or overflowed the body cap. Fall through and
				// compute, unclaimed. Rare, and a duplicate computation is a
				// better outcome here than a blank page.
			case <-r.Context().Done():
				return
			}
		} else {
			defer c.done(key)
		}

		w.Header().Set("X-Cache", "MISS")
		cw := &cachingWriter{ResponseWriter: w}
		next.ServeHTTP(cw, r)
		c.store(key, r.URL.Path, cw)
	})
}

// refresh recomputes one entry with nobody waiting on it. The request is
// already detached from the reader's (see the call site).
func (c *responseCache) refresh(key string, req *http.Request, next http.Handler) {
	defer c.done(key)
	cw := &cachingWriter{ResponseWriter: &discardWriter{header: http.Header{}}}
	next.ServeHTTP(cw, req)
	c.store(key, req.URL.Path, cw)
}

func (c *responseCache) store(key, path string, cw *cachingWriter) {
	if cw.status != http.StatusOK || cw.tooBig || cw.buf.Len() == 0 {
		return
	}
	c.put(key, cacheEntry{
		body:            append([]byte(nil), cw.buf.Bytes()...),
		contentType:     cw.Header().Get("Content-Type"),
		contentEncoding: cw.Header().Get("Content-Encoding"),
		storedAt:        time.Now(),
		ttl:             c.cacheTTLFor(path),
	})
}
