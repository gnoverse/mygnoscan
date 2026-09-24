package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// A streaming route must reach the handler unwrapped, even when the client
// advertises gzip. compressibleType already refuses text/event-stream, but it
// decides from the Content-Type on the first write, and until then the stream
// is running through a wrapper that buffers and that hides the real
// ResponseWriter from http.ResponseController.
//
// This is the difference the issue could not explain: curl sends no
// Accept-Encoding and was never wrapped, so the server looked healthy while
// every browser tab dropped.
func TestCompressionDoesNotWrapTheLiveStream(t *testing.T) {
	var got http.ResponseWriter
	h := WithCompression(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = w
	}))

	req := httptest.NewRequest("GET", "/api/live", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if _, wrapped := got.(*gzipWriter); wrapped {
		t.Fatal("/api/live reached the handler wrapped in a gzipWriter")
	}
}

// Everything else still gets compressed: the exemption is one route, not a
// regression in the middleware.
func TestCompressionStillWrapsOrdinaryRoutes(t *testing.T) {
	var got http.ResponseWriter
	h := WithCompression(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = w
	}))

	req := httptest.NewRequest("GET", "/api/txs", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if _, wrapped := got.(*gzipWriter); !wrapped {
		t.Fatal("/api/txs should still be wrapped for compression")
	}
}

// A client that cannot take gzip is never wrapped, whatever the route.
func TestCompressionSkipsWithoutAcceptEncoding(t *testing.T) {
	var got http.ResponseWriter
	h := WithCompression(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = w
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/txs", nil))

	if _, wrapped := got.(*gzipWriter); wrapped {
		t.Fatal("a client that did not advertise gzip must not be wrapped")
	}
}

// The two exclusion lists answer the same question about the same route and
// have to agree: one buffers a response, the other holds it open.
func TestStreamingPathAndCacheableAgree(t *testing.T) {
	req := httptest.NewRequest("GET", "/api/live", nil)
	if cacheable(req) {
		t.Error("/api/live must not be cacheable")
	}
	if !streamingPath("/api/live") {
		t.Error("/api/live must be a streaming path")
	}
	if streamingPath("/api/txs") {
		t.Error("/api/txs is not a streaming path")
	}
}
