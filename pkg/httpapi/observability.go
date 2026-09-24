package httpapi

import (
	"net/http"
	"strconv"
	"time"
)

// Making "it is slow" a number instead of an afternoon.
//
// Diagnosing the govdao page on 2026-09-24 took hours of curl because nothing
// the server sends says where time went. X-Cache says which path a response
// took but not what it cost, so a 47-second response carrying X-Cache: HIT
// looked like a broken cache when it was a busy process. Two headers and one
// endpoint would have answered it in a minute.

// WithServerTiming stamps every response with how long the handlers below it
// took, in the standard Server-Timing form that browser devtools already
// render in the network panel.
//
// It sits *inside* the response cache in the middleware chain, so the number is
// the cost of actually computing the answer. A cache hit never reaches it and
// therefore carries no app timing at all, which is exactly the signal wanted:
// Server-Timing present means somebody paid, absent means somebody did not.
func WithServerTiming(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(&timingWriter{ResponseWriter: w, start: start}, r)
	})
}

// timingWriter writes the header on the way out, at WriteHeader time, because
// that is the last moment it can still be set and the first moment the duration
// is meaningful.
type timingWriter struct {
	http.ResponseWriter
	start   time.Time
	written bool
}

func (w *timingWriter) stamp() {
	if w.written {
		return
	}
	w.written = true
	ms := float64(time.Since(w.start).Microseconds()) / 1000
	w.Header().Set("Server-Timing", "app;dur="+strconv.FormatFloat(ms, 'f', 1, 64))
}

func (w *timingWriter) WriteHeader(status int) {
	w.stamp()
	w.ResponseWriter.WriteHeader(status)
}

func (w *timingWriter) Write(b []byte) (int, error) {
	w.stamp()
	return w.ResponseWriter.Write(b)
}

// Flush passes through, so an SSE stream is not held back by this wrapper.
func (w *timingWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// SetResponseCache hands the API the cache wrapped around it, so it can report
// on it. The cache is built after the API in main, which is why this is a
// setter rather than a constructor argument.
func (a *API) SetResponseCache(c *responseCache) { a.responseCache = c }

// SetWarmer hands the API the warmer, for the same reason.
func (a *API) SetWarmer(w *Warmer) { a.warmer = w }

// CacheReport is what /api/cache/stats answers: the state of the two things
// that decide whether a reader waits.
type CacheReport struct {
	Cache  CacheStats  `json:"cache"`
	Warmer WarmerStats `json:"warmer"`
	// TTLSeconds and StaleGraceSeconds are echoed so a reader of this endpoint
	// can interpret the counters without reading the source.
	TTLSeconds        int `json:"ttl_seconds"`
	StaleGraceSeconds int `json:"stale_grace_seconds"`
}

// HandleCacheStats reports cache and warmer state.
//
// Read it when somebody says a page is slow. A large Misses against a small
// Entries means the warmer is not covering the keys people ask for; a Stale
// that dwarfs Hits means some endpoint costs more to recompute than its TTL
// allows, and belongs in endpointTTL; a Passes that has stopped climbing means
// the warmer is wedged on one target.
func (a *API) HandleCacheStats(w http.ResponseWriter, r *http.Request) {
	out := CacheReport{
		TTLSeconds:        int(CacheTTL / time.Second),
		StaleGraceSeconds: int(CacheStaleGrace / time.Second),
	}
	if a.responseCache != nil {
		out.Cache = a.responseCache.Stats()
	}
	if a.warmer != nil {
		out.Warmer = a.warmer.Stats()
	}
	JSONResponse(w, out)
}
