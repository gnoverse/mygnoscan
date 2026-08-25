package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
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
			h := withResponseCache(newResponseCache(cacheTTL), countingHandler(&calls, tt.status, `{"ok":true}`))

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
	h := withResponseCache(newResponseCache(cacheTTL), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

func TestResponseCacheExpires(t *testing.T) {
	var calls atomic.Int32
	h := withResponseCache(newResponseCache(20*time.Millisecond), countingHandler(&calls, 200, `{"ok":true}`))

	req := func() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/stats", nil))
	}
	req()
	req()
	if calls.Load() != 1 {
		t.Fatalf("handler ran %d times before expiry, want 1", calls.Load())
	}
	time.Sleep(40 * time.Millisecond)
	req()
	if calls.Load() != 2 {
		t.Errorf("handler ran %d times after expiry, want 2 — the entry never expired", calls.Load())
	}
}

// The entry count is bounded: paginated URLs are unbounded in principle and a
// crawler should not be able to grow this without limit.
func TestResponseCacheIsBounded(t *testing.T) {
	c := newResponseCache(time.Hour)
	h := withResponseCache(c, countingHandler(new(atomic.Int32), 200, `{"ok":true}`))

	for i := 0; i < cacheMaxEntries*2; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", fmt.Sprintf("/api/txs?offset=%d", i), nil))
	}
	if _, _, size := c.stats(); size > cacheMaxEntries {
		t.Errorf("cache holds %d entries, want at most %d", size, cacheMaxEntries)
	}
}
