package httpapi

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCompressionNegotiation(t *testing.T) {
	big := strings.Repeat(`{"pad":"xxxxxxxxxxxxxxxx"},`, 200)

	tests := []struct {
		name         string
		accept       string
		contentType  string
		body         string
		wantEncoding string
	}{
		{
			name: "gzips a large json body", accept: "gzip",
			contentType: "application/json", body: big, wantEncoding: "gzip",
		},
		{
			name: "gzips html too", accept: "gzip, deflate, br",
			contentType: "text/html; charset=utf-8", body: big, wantEncoding: "gzip",
		},
		{
			// The gzip header and trailer alone are 18 bytes, and anything
			// this small already fits in one segment.
			name: "leaves a small body alone", accept: "gzip",
			contentType: "application/json", body: `{"ok":true}`, wantEncoding: "",
		},
		{
			name: "leaves a client that did not ask alone", accept: "",
			contentType: "application/json", body: big, wantEncoding: "",
		},
		{
			// Re-encoding an already-compressed format spends CPU to make the
			// body very slightly larger.
			name: "leaves an image alone", accept: "gzip",
			contentType: "image/png", body: big, wantEncoding: "",
		},
		{
			// /api/live must never be buffered: it is an open stream, and
			// holding bytes back holds the live feed back for the life of
			// the tab.
			name: "leaves an event stream alone", accept: "gzip",
			contentType: "text/event-stream", body: big, wantEncoding: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := WithCompression(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tt.contentType)
				fmt.Fprint(w, tt.body)
			}))
			req := httptest.NewRequest("GET", "/api/stats", nil)
			if tt.accept != "" {
				req.Header.Set("Accept-Encoding", tt.accept)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if got := rec.Header().Get("Content-Encoding"); got != tt.wantEncoding {
				t.Fatalf("Content-Encoding = %q, want %q", got, tt.wantEncoding)
			}
			got := rec.Body.String()
			if tt.wantEncoding == "gzip" {
				if len(rec.Body.Bytes()) >= len(tt.body) {
					t.Errorf("compressed body is %d bytes, not smaller than the %d raw", rec.Body.Len(), len(tt.body))
				}
				got = gunzip(t, rec.Body.Bytes())
			}
			if got != tt.body {
				t.Errorf("body round-tripped to something else (%d bytes vs %d)", len(got), len(tt.body))
			}
			if !strings.Contains(rec.Header().Get("Vary"), "Accept-Encoding") {
				t.Error("Vary does not name Accept-Encoding — a shared cache could hand gzip to a client that cannot decode it")
			}
		})
	}
}

// A handler's status has to survive the deferred commit, or every error
// response would arrive as a 200 with an error body — which reads as success
// to the frontend's `if (!r.ok)` check.
func TestCompressionPreservesStatus(t *testing.T) {
	h := WithCompression(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(404)
		fmt.Fprint(w, strings.Repeat(`{"error":"nope"},`, 200))
	}))
	req := httptest.NewRequest("GET", "/api/stats", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 404 {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Errorf("Content-Encoding = %q, want gzip", got)
	}
}

// An empty response must not become a gzip stream with no members, and must not
// panic on the way out.
func TestCompressionEmptyBody(t *testing.T) {
	h := WithCompression(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(204)
	}))
	req := httptest.NewRequest("GET", "/api/stats", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 204 {
		t.Errorf("status = %d, want 204", rec.Code)
	}
	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Errorf("Content-Encoding = %q on a bodyless response", got)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("body is %d bytes on a 204", rec.Body.Len())
	}
}

// A streaming handler must keep streaming. Flush is where a buffering wrapper
// turns a live feed into a page that never paints.
func TestCompressionFlushPassesThrough(t *testing.T) {
	h := WithCompression(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		f, ok := w.(http.Flusher)
		if !ok {
			t.Error("handler did not see a Flusher through the compression wrapper")
			return
		}
		fmt.Fprint(w, "data: one\n\n")
		f.Flush()
		fmt.Fprint(w, "data: two\n\n")
		f.Flush()
	}))
	req := httptest.NewRequest("GET", "/api/live", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Body.String(); got != "data: one\n\ndata: two\n\n" {
		t.Errorf("stream body = %q", got)
	}
	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Errorf("Content-Encoding = %q on an event stream", got)
	}
}

func TestAcceptsGzip(t *testing.T) {
	tests := []struct {
		header string
		want   bool
	}{
		{"", false},
		{"gzip", true},
		{"gzip, deflate, br", true},
		{"deflate, gzip;q=1.0, *;q=0.5", true},
		{"br", false},
		{"deflate", false},
		// "x-gzip" and "notgzip" must not match on a substring check.
		{"notgzip", false},
	}
	for _, tt := range tests {
		r := httptest.NewRequest("GET", "/api/stats", nil)
		if tt.header != "" {
			r.Header.Set("Accept-Encoding", tt.header)
		}
		if got := acceptsGzip(r); got != tt.want {
			t.Errorf("acceptsGzip(%q) = %v, want %v", tt.header, got, tt.want)
		}
	}
}
