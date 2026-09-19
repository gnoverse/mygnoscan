package web

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

// The app is one 450 KB file re-served on every deep link and every cold tab.
// These are the three things that stop that being the dominant cost of loading
// the site, and each of them has a silent failure mode: a missing
// Content-Encoding just sends the big body, a missing ETag just re-sends it, and
// a wrong Content-Length truncates the page with no error anywhere.
func TestIndexIsCompressedAndRevalidatable(t *testing.T) {
	h, err := Handler()
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	index, err := Index()
	if err != nil {
		t.Fatalf("Index: %v", err)
	}

	t.Run("serves the raw body when gzip is not accepted", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest("GET", "/", nil))
		if got := rec.Header().Get("Content-Encoding"); got != "" {
			t.Errorf("Content-Encoding = %q", got)
		}
		if !bytes.Equal(rec.Body.Bytes(), index) {
			t.Error("body is not index.html")
		}
	})

	t.Run("gzips when accepted, and the bytes round-trip", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("Accept-Encoding", "gzip")
		rec := httptest.NewRecorder()
		h(rec, req)

		if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
			t.Fatalf("Content-Encoding = %q, want gzip", got)
		}
		if rec.Body.Len() >= len(index)/2 {
			t.Errorf("compressed to %d bytes from %d — barely compressed at all", rec.Body.Len(), len(index))
		}
		if got := rec.Header().Get("Content-Length"); got != strconv.Itoa(rec.Body.Len()) {
			t.Errorf("Content-Length = %q but body is %d bytes — a browser would truncate the page", got, rec.Body.Len())
		}
		zr, err := gzip.NewReader(bytes.NewReader(rec.Body.Bytes()))
		if err != nil {
			t.Fatalf("gzip reader: %v", err)
		}
		out, err := io.ReadAll(zr)
		if err != nil {
			t.Fatalf("gunzip: %v", err)
		}
		if !bytes.Equal(out, index) {
			t.Error("decompressed body is not index.html")
		}
	})

	t.Run("revalidates with an ETag", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest("GET", "/", nil))
		etag := rec.Header().Get("ETag")
		if etag == "" {
			t.Fatal("no ETag")
		}

		for _, inm := range []string{etag, "W/" + etag, `"other", ` + etag, "*"} {
			req := httptest.NewRequest("GET", "/", nil)
			req.Header.Set("If-None-Match", inm)
			rec := httptest.NewRecorder()
			h(rec, req)
			if rec.Code != http.StatusNotModified {
				t.Errorf("If-None-Match %q: status %d, want 304", inm, rec.Code)
			}
			if rec.Body.Len() != 0 {
				t.Errorf("If-None-Match %q: %d bytes of body on a 304", inm, rec.Body.Len())
			}
		}

		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("If-None-Match", `"stale-from-an-older-build"`)
		rec = httptest.NewRecorder()
		h(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("a non-matching ETag got %d, want 200 — a new build would never reach the reader", rec.Code)
		}
	})

	// A deep link is not a 404: the SPA routes on the URL, so every path that
	// is not an embedded file has to arrive at the same HTML.
	t.Run("deep links get the app", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest("GET", "/realm/gno.land/r/demo/boards", nil))
		if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), index) {
			t.Errorf("deep link got %d and %d bytes", rec.Code, rec.Body.Len())
		}
	})
}
