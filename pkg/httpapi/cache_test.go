package httpapi

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// countingHandler records how often the expensive work behind the cache ran.
func countingHandler(calls *atomic.Int32, status int, body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	})
}

func TestResponseCache(t *testing.T) {
	tests := []struct {
		name      string
		path      string
		status    int
		requests  int
		wantCalls int32
		wantCache string
	}{
		{
			name:      "repeated reads hit the cache",
			path:      "/api/analytics?network=sapphire",
			status:    200,
			requests:  3,
			wantCalls: 1,
			wantCache: "HIT",
		},
		{
			// A cached 500 would pin a transient indexer failure for the whole
			// TTL, turning a blip into half a minute of outage.
			name:      "errors are never cached",
			path:      "/api/gas",
			status:    500,
			requests:  3,
			wantCalls: 3,
			wantCache: "MISS",
		},
		{
			// Buffering a stream that never ends would hold the response open
			// and grow the buffer forever.
			name:      "the live stream is not cached",
			path:      "/api/live",
			status:    200,
			requests:  2,
			wantCalls: 2,
			wantCache: "",
		},
		{
			name:      "version is not worth caching",
			path:      "/api/version",
			status:    200,
			requests:  2,
			wantCalls: 2,
			wantCache: "",
		},
		{
			name:      "non-api routes pass through",
			path:      "/realms",
			status:    200,
			requests:  2,
			wantCalls: 2,
			wantCache: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			h := WithResponseCache(NewResponseCache(CacheTTL), countingHandler(&calls, tt.status, `{"ok":true}`))

			var last *httptest.ResponseRecorder
			for i := 0; i < tt.requests; i++ {
				last = httptest.NewRecorder()
				h.ServeHTTP(last, httptest.NewRequest("GET", tt.path, nil))
			}

			if got := calls.Load(); got != tt.wantCalls {
				t.Errorf("handler ran %d times, want %d", got, tt.wantCalls)
			}
			if got := last.Header().Get("X-Cache"); got != tt.wantCache {
				t.Errorf("X-Cache = %q, want %q", got, tt.wantCache)
			}
			if last.Body.String() != `{"ok":true}` {
				t.Errorf("body = %q", last.Body.String())
			}
		})
	}
}

// Two networks must never share an entry — serving one chain's data under
// another's name is the failure this whole area has been full of.
func TestResponseCacheKeysOnQuery(t *testing.T) {
	var calls atomic.Int32
	h := WithResponseCache(NewResponseCache(CacheTTL), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"network":%q}`, r.URL.Query().Get("network"))
	}))

	get := func(path string) string {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		return rec.Body.String()
	}

	if got := get("/api/stats?network=sapphire"); got != `{"network":"sapphire"}` {
		t.Fatalf("got %s", got)
	}
	if got := get("/api/stats?network=gnoland1"); got != `{"network":"gnoland1"}` {
		t.Errorf("got %s — a different network was served from the cache", got)
	}
	if got := get("/api/stats"); got != `{"network":""}` {
		t.Errorf("got %s — all-networks was served a single network's entry", got)
	}
	if calls.Load() != 3 {
		t.Errorf("handler ran %d times, want 3 distinct keys", calls.Load())
	}
}

// An expired entry is refreshed. The reader who triggers it is served the old
// value immediately rather than waiting for the new one — that is the whole
// point of the grace window — so the second run is observed by waiting for it,
// not by reading the counter on the way out.
func TestResponseCacheRefreshesAfterTTL(t *testing.T) {
	var calls atomic.Int32
	c := NewResponseCache(20 * time.Millisecond)
	h := WithResponseCache(c, countingHandler(&calls, 200, `{"ok":true}`))

	req := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/stats", nil))
		return rec
	}
	req()
	req()
	if calls.Load() != 1 {
		t.Fatalf("handler ran %d times before expiry, want 1", calls.Load())
	}
	time.Sleep(40 * time.Millisecond)
	if got := req().Header().Get("X-Cache"); got != "STALE" {
		t.Errorf("X-Cache = %q after the TTL, want STALE — the reader waited on the refresh", got)
	}
	waitFor(t, func() bool { return calls.Load() == 2 }, "background refresh after the TTL")
}

// Past TTL+grace an entry stops being servable at all: a reader coming back
// after a long absence must not be handed a long-dead chain tip, however fast.
func TestResponseCacheStopsServingBeyondGrace(t *testing.T) {
	var calls atomic.Int32
	c := NewResponseCache(10 * time.Millisecond)
	c.grace = 10 * time.Millisecond
	h := WithResponseCache(c, countingHandler(&calls, 200, `{"ok":true}`))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/stats", nil))
	time.Sleep(50 * time.Millisecond)

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/stats", nil))
	if got := rec.Header().Get("X-Cache"); got != "MISS" {
		t.Errorf("X-Cache = %q beyond the grace window, want MISS", got)
	}
	if calls.Load() != 2 {
		t.Errorf("handler ran %d times, want 2 — the stale entry was served past its grace", calls.Load())
	}
}

// A burst of readers on one stale entry must produce one refresh, not one per
// reader: the expensive endpoints are the ones that go stale under load, and
// fanning out N recomputes of an 8-second query is worse than the cold miss it
// replaces.
func TestResponseCacheRefreshesOnlyOncePerBurst(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	// refreshing is signalled by the refresh goroutine as it enters the
	// handler. Without it this test asserts on work that has been scheduled
	// and not yet run: the refresh is deliberately detached from the request
	// that triggered it (see WithResponseCache), so the burst returning says
	// nothing about whether the refresh has started. Buffered so a second,
	// unwanted refresh cannot deadlock the assertion it is there to fail.
	refreshing := make(chan struct{}, 8)
	c := NewResponseCache(10 * time.Millisecond)
	h := WithResponseCache(c, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) > 1 {
			refreshing <- struct{}{}
			<-release // hold the refresh open so the burst overlaps it
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/stats", nil))
	time.Sleep(30 * time.Millisecond)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/stats", nil))
			if rec.Body.String() != `{"ok":true}` {
				t.Errorf("stale read got %q", rec.Body.String())
			}
		}()
	}
	wg.Wait()

	// Wait for the one refresh the burst is allowed to trigger to actually
	// reach the handler. Only then is the call count meaningful.
	select {
	case <-refreshing:
	case <-time.After(5 * time.Second):
		t.Fatal("no refresh started within 5s for a stale entry")
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("handler ran %d times for one stale entry, want 2 (the original plus one refresh)", got)
	}
	// A second refresh cannot legitimately start while the first is still in
	// flight: the claim in lookup is single-flight under the cache lock, and
	// the first refresh is blocked on release. So anything queued here is a
	// coalescing bug, which is the whole point of the test.
	select {
	case <-refreshing:
		t.Error("a second refresh started while the first was still running; the burst was not coalesced")
	default:
	}
	close(release)
}

// A refresh outlives the request that triggered it. The reader already has
// their answer and may close the tab immediately; cancelling the refresh with
// their context would mean the entry never gets replaced under exactly the
// traffic pattern this cache exists for.
func TestResponseCacheRefreshSurvivesClientCancel(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{}, 4)
	c := NewResponseCache(10 * time.Millisecond)
	h := WithResponseCache(c, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n > 1 {
			started <- struct{}{}
			time.Sleep(30 * time.Millisecond)
			if err := r.Context().Err(); err != nil {
				t.Errorf("refresh context cancelled: %v", err)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/stats", nil))
	time.Sleep(30 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/stats", nil).WithContext(ctx))
	cancel() // the reader leaves the moment they have the stale body

	<-started
	waitFor(t, func() bool { return calls.Load() == 2 }, "refresh completing after the reader left")
}

// A gzip-accepting reader and one that cannot decode it must not share an
// entry. Compression runs inside the cache, so whichever arrives first decides
// what the stored bytes are — the key has to carry that.
func TestResponseCacheKeysOnEncoding(t *testing.T) {
	var calls atomic.Int32
	body := strings.Repeat(`{"pad":"x"},`, 400)
	h := WithResponseCache(NewResponseCache(time.Hour),
		WithCompression(countingHandler(&calls, 200, body)))

	plain := httptest.NewRecorder()
	h.ServeHTTP(plain, httptest.NewRequest("GET", "/api/stats", nil))
	if got := plain.Header().Get("Content-Encoding"); got != "" {
		t.Errorf("Content-Encoding = %q for a client that did not ask for gzip", got)
	}
	if plain.Body.String() != body {
		t.Error("plain client got something other than the raw body")
	}

	req := httptest.NewRequest("GET", "/api/stats", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	zipped := httptest.NewRecorder()
	h.ServeHTTP(zipped, req)
	if got := zipped.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	if got := gunzip(t, zipped.Body.Bytes()); got != body {
		t.Error("gzip client decoded to something other than the raw body")
	}
	if calls.Load() != 2 {
		t.Errorf("handler ran %d times, want 2 — one entry per encoding", calls.Load())
	}
}

// waitFor polls a condition rather than sleeping a fixed amount: the work it
// waits on is a goroutine, and a fixed sleep is either flaky or slow.
func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Errorf("timed out waiting for %s", what)
}

func gunzip(t *testing.T, b []byte) string {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	defer zr.Close()
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	return string(out)
}

// The entry count is bounded: paginated URLs are unbounded in principle and a
// crawler should not be able to grow this without limit.
func TestResponseCacheIsBounded(t *testing.T) {
	c := NewResponseCache(time.Hour)
	h := WithResponseCache(c, countingHandler(new(atomic.Int32), 200, `{"ok":true}`))

	for i := 0; i < cacheMaxEntries*2; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", fmt.Sprintf("/api/txs?offset=%d", i), nil))
	}
	if size := c.stats().Entries; size > cacheMaxEntries {
		t.Errorf("cache holds %d entries, want at most %d", size, cacheMaxEntries)
	}
}

// An escaped path and an unescaped one are different requests, and the cache
// must not merge them.
//
// A third of gno transaction hashes are base64 containing a slash.
// `/api/tx/a%2Fb` routes to the transaction handler and answers JSON;
// `/api/tx/a/b` matches no API route and falls through to the SPA, which
// answers 200 with HTML. Keyed on the decoded path the two collide, and
// whichever arrived first was served to the other for the whole TTL.
func TestResponseCacheKeysOnTheEscapedPath(t *testing.T) {
	var calls atomic.Int32
	h := WithResponseCache(NewResponseCache(CacheTTL), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		// What the real chain does: one path matches a route, the other does
		// not and is answered by the single-page app.
		if r.URL.EscapedPath() == "/api/tx/a%2Fb" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"hash":"a/b"}`)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<!DOCTYPE html>")
	}))

	get := func(path string) string {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		return rec.Body.String()
	}

	// The unescaped one first, which is what poisoned the entry.
	if got := get("/api/tx/a/b"); got != "<!DOCTYPE html>" {
		t.Fatalf("got %q", got)
	}
	if got := get("/api/tx/a%2Fb"); got != `{"hash":"a/b"}` {
		t.Errorf("got %q, want the transaction; the two paths shared a cache entry", got)
	}
	if calls.Load() != 2 {
		t.Errorf("handler ran %d times, want 2 distinct keys", calls.Load())
	}
}

// Concurrent readers of one cold key produce one computation, not one each.
//
// Measured against production on 2026-09-24, four concurrent requests on a cold
// /api/govdao/overview key all answered X-Cache: MISS, each having recomputed
// the whole thing, and the latency grew with the concurrency (2.39s, 2.79s,
// 3.03s, 3.13s) because they were competing for the same upstream node. The
// stale path was already single-flighted; this is the path that was not.
func TestResponseCacheCoalescesConcurrentMisses(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	h := WithResponseCache(NewResponseCache(time.Hour), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		<-release
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))

	const readers = 6
	states := make([]string, readers)
	bodies := make([]string, readers)
	var wg sync.WaitGroup
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/govdao/overview", nil))
			states[i] = rec.Header().Get("X-Cache")
			bodies[i] = rec.Body.String()
		}(i)
	}
	// Let every reader reach the cache before the leader may finish.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if n := calls.Load(); n != 1 {
		t.Errorf("handler ran %d times for %d concurrent readers, want 1", n, readers)
	}
	misses, waits := 0, 0
	for i, s := range states {
		switch s {
		case "MISS":
			misses++
		case "WAIT":
			waits++
		default:
			t.Errorf("reader %d got X-Cache %q", i, s)
		}
		if bodies[i] != `{"ok":true}` {
			t.Errorf("reader %d got body %q", i, bodies[i])
		}
	}
	if misses != 1 {
		t.Errorf("%d readers computed, want exactly 1", misses)
	}
	if waits != readers-1 {
		t.Errorf("%d readers waited, want %d", waits, readers-1)
	}
}

// A waiter whose client goes away stops waiting, and does not wedge the key.
func TestResponseCacheWaiterHonoursItsContext(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	var once sync.Once
	h := WithResponseCache(NewResponseCache(time.Hour), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(started) })
		<-release
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))

	go h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/accounts", nil))
	<-started

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/accounts", nil).WithContext(ctx))
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a waiter with a cancelled context never returned")
	}
	close(release)
}

// Two spellings of one request share one entry, so the reader who arrived from
// a shared link does not pay the cold price for a page that is already cached.
func TestCanonicalQuery(t *testing.T) {
	for _, tt := range []struct{ name, in, want string }{
		{"empty", "", ""},
		{"network=all is the same as no network", "network=all", ""},
		{"a real network is kept", "network=mainnet", "network=mainnet"},
		{"parameters are sorted", "network=mainnet&limit=10", "limit=10&network=mainnet"},
		{"utm is dropped", "utm_source=twitter&network=mainnet", "network=mainnet"},
		{"click ids are dropped", "fbclid=abc&gclid=def", ""},
		{"an empty value is kept", "q=", "q="},
		{"an unknown parameter is kept", "days=30", "days=30"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := canonicalQuery(tt.in); got != tt.want {
				t.Errorf("canonicalQuery(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// ?network=all and no network reach the same entry end to end.
func TestResponseCacheSharesNetworkAllWithNoNetwork(t *testing.T) {
	var calls atomic.Int32
	h := WithResponseCache(NewResponseCache(time.Hour), countingHandler(&calls, 200, `{"ok":true}`))
	for _, u := range []string{"/api/analytics?network=all", "/api/analytics", "/api/analytics?utm_source=x"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", u, nil))
		if rec.Body.String() != `{"ok":true}` {
			t.Errorf("%s: body = %q", u, rec.Body.String())
		}
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("handler ran %d times for three spellings of one request, want 1", n)
	}
}

// An endpoint whose recompute costs more than the default TTL gets its own,
// or it never settles: measured on 2026-09-24, /api/govdao/overview answered
// 56 STALE against 28 HIT over three minutes and was refreshing continuously.
func TestEndpointTTLOverridesTheDefault(t *testing.T) {
	c := NewResponseCache(CacheTTL)
	if got := c.cacheTTLFor("/api/govdao/overview"); got != endpointTTL["/api/govdao/overview"] {
		t.Errorf("govdao TTL = %v, want its override", got)
	}
	if got := c.cacheTTLFor("/api/txs"); got != CacheTTL {
		t.Errorf("default TTL = %v, want %v", got, CacheTTL)
	}
}
