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
	c := NewResponseCache(10 * time.Millisecond)
	h := WithResponseCache(c, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) > 1 {
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
	if got := calls.Load(); got != 2 {
		t.Errorf("handler ran %d times for one stale entry, want 2 (the original plus one refresh)", got)
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
	if _, _, size := c.stats(); size > cacheMaxEntries {
		t.Errorf("cache holds %d entries, want at most %d", size, cacheMaxEntries)
	}
}
