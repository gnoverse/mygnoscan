package httpapi

import (
	"compress/gzip"
	"io"
	"net/http"
	"strings"
	"sync"
)

// Response compression for the read-only API.
//
// Nothing was compressed before this. The explorer ships JSON in the megabytes
// — /api/txs is 3.5 MB on mainnet, /api/allevents 1.3 MB, /api/contracts/map
// 110 KB — and every byte of it went over the wire raw. That is the dominant
// term in how long a page takes to appear on anything slower than a datacentre
// link, and it costs nothing to fix: this is line-oriented JSON with a small
// alphabet, so gzip takes roughly 85-90% of it away.
//
// The middleware sits *inside* the response cache (see WithResponseCache), so a
// cache hit serves bytes that were already compressed once rather than
// recompressing per request. That is why the cache key carries the negotiated
// encoding: two entries per hot endpoint at most, and a client that cannot
// accept gzip still gets correct bytes.

// compressMinBytes is the size below which compressing is not worth it. A gzip
// header and trailer are 18 bytes, and anything that fits in a single TCP
// segment arrives in one round trip whether or not it is compressed — so small
// responses only pay the CPU. /api/stats (244 B) and /api/networks (337 B) are
// in this class.
const compressMinBytes = 1024

// gzipLevel trades ratio for latency. BestSpeed is the right end of that curve
// here: on a 3.5 MB JSON body it is roughly 5x faster than DefaultCompression
// and still gets ~8x reduction, and the result is cached anyway so the ratio
// matters more than the one-off cost. Measured on /api/txs: 3.5 MB -> ~420 KB.
const gzipLevel = gzip.BestSpeed

var gzipPool = sync.Pool{
	New: func() any {
		w, _ := gzip.NewWriterLevel(io.Discard, gzipLevel)
		return w
	},
}

// compressibleType reports whether a Content-Type is worth gzipping.
//
// Deliberately an allow-list. Images and archives are already compressed and
// gzipping them wastes CPU for nothing, and text/event-stream must never be
// buffered: /api/live is an SSE stream that stays open for the life of the tab,
// so anything that holds bytes back holds the live feed back forever.
func compressibleType(ct string) bool {
	ct = strings.ToLower(strings.TrimSpace(strings.SplitN(ct, ";", 2)[0]))
	switch ct {
	case "application/json", "application/javascript", "image/svg+xml":
		return true
	}
	return strings.HasPrefix(ct, "text/") && ct != "text/event-stream"
}

// acceptsGzip reports whether the client advertised gzip.
//
// A bare substring check, not a full q-value parse: the only way to see
// "gzip;q=0" in the wild is a client deliberately disabling it, and the cost of
// honouring that is a header parser on every request. `identity` is not
// special-cased for the same reason — a client that wants it simply omits gzip.
func acceptsGzip(r *http.Request) bool {
	for _, v := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		name, _, _ := strings.Cut(strings.TrimSpace(v), ";")
		if strings.EqualFold(name, "gzip") {
			return true
		}
	}
	return false
}

// gzipWriter compresses a handler's output, deciding lazily.
//
// The first Write does not go to the compressor: it is held in `pending` until
// either compressMinBytes accumulates (compress, flush the buffer through) or
// the handler finishes (write it through raw). A handler cannot know its own
// body size before writing it, and Content-Length is only set for the ones that
// do — so the size decision has to be made here, from the bytes themselves.
type gzipWriter struct {
	http.ResponseWriter

	gz      *gzip.Writer
	pending []byte
	decided bool // whether the compress/passthrough choice has been made
	on      bool // and what it was
	status  int
}

func (w *gzipWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	// 204 and 304 carry no body, and 1xx is not a response yet. Committing an
	// encoding for them would describe a body that never arrives.
	if status < 200 || status == http.StatusNoContent || status == http.StatusNotModified {
		w.decided, w.on = true, false
		w.ResponseWriter.WriteHeader(status)
		return
	}
	// Header() must still be mutable when the decision lands, so the status
	// line is not sent yet — see commit().
}

func (w *gzipWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if w.decided {
		if w.on {
			return w.gz.Write(b)
		}
		return w.ResponseWriter.Write(b)
	}
	w.pending = append(w.pending, b...)
	if len(w.pending) >= compressMinBytes {
		w.commit(true)
	}
	return len(b), nil
}

// commit settles the compress-or-not choice, sends the headers, and drains
// whatever was held back. mayCompress is false when the handler finished under
// the size threshold.
func (w *gzipWriter) commit(mayCompress bool) {
	if w.decided {
		return
	}
	w.decided = true
	// A handler that encoded its own body (the response cache replaying a
	// stored gzip entry) has already done this work; wrapping it again would
	// produce gzip-inside-gzip and a body no browser can read.
	w.on = mayCompress &&
		w.Header().Get("Content-Encoding") == "" &&
		compressibleType(w.Header().Get("Content-Type"))

	if w.on {
		w.Header().Set("Content-Encoding", "gzip")
		// Content-Length, if a handler set one, describes the uncompressed
		// body and is now a lie. Range requests over a stream we re-encode
		// per request are not answerable either.
		w.Header().Del("Content-Length")
		w.Header().Del("Accept-Ranges")
		w.gz = gzipPool.Get().(*gzip.Writer)
		w.gz.Reset(w.ResponseWriter)
	}
	if w.status == 0 {
		w.status = http.StatusOK
	}
	w.ResponseWriter.WriteHeader(w.status)

	if len(w.pending) > 0 {
		if w.on {
			w.gz.Write(w.pending)
		} else {
			w.ResponseWriter.Write(w.pending)
		}
		w.pending = nil
	}
}

// close finishes the response. Safe to call on a handler that wrote nothing.
func (w *gzipWriter) close() {
	w.commit(false)
	if w.on && w.gz != nil {
		w.gz.Close()
		gzipPool.Put(w.gz)
		w.gz = nil
	}
}

// Flush passes a handler's flush through the compressor.
//
// Reaching here means the handler streams, which also means it is past the
// size-threshold guess: commit now rather than hold bytes back waiting for a
// buffer that may never fill. SSE never gets this far (compressibleType
// excludes text/event-stream), but progressively-rendered JSON could.
func (w *gzipWriter) Flush() {
	w.commit(true)
	if w.on && w.gz != nil {
		w.gz.Flush()
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// WithCompression gzips responses whose client asked for it and whose
// Content-Type is worth compressing.
//
// Vary is set on every response, compressed or not: a shared cache in front of
// this (Caddy, a CDN) must not hand a gzipped body to a client that did not ask
// for one.
func WithCompression(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", "Accept-Encoding")
		// A streaming route is never wrapped, not even to be passed through.
		// compressibleType already refuses text/event-stream, but that decision
		// is made from the Content-Type on the FIRST WRITE, so until then the
		// stream is running through a wrapper that holds bytes in `pending` and
		// hides the underlying ResponseWriter from http.ResponseController.
		//
		// This is also why the drop only ever reproduced in a browser: curl
		// sends no Accept-Encoding, so it was never wrapped, and the server
		// "looked healthy in isolation" while every tab saw
		// ERR_INCOMPLETE_CHUNKED_ENCODING.
		if !acceptsGzip(r) || streamingPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		gw := &gzipWriter{ResponseWriter: w}
		defer gw.close()
		next.ServeHTTP(gw, r)
	})
}

// streamingPath reports whether a route holds its response open indefinitely.
// Kept beside cacheable(), which excludes the same route for the same reason.
func streamingPath(p string) bool { return p == "/api/live" }
