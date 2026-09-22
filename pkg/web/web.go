// Package web serves the single-file frontend embedded in the binary.
package web

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"fmt"
	"html"
	"io/fs"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

//go:embed frontend
var frontendFS embed.FS

// Options configures what gets served.
type Options struct {
	// AnalyticsScript is the URL of a third-party analytics script to load on
	// every page, or "" to load none. Off by default: an explorer that phones
	// a third party home without its operator having asked for it is not a
	// default anyone should inherit by upgrading.
	//
	// The deployment at mygnoscan.moul.p2p.team passes Simple Analytics'
	// https://scripts.simpleanalyticscdn.com/latest.js, which sets no cookie
	// and stores nothing on the device. Any provider serving a single
	// self-contained script works the same way.
	AnalyticsScript string

	// Shots is true when a capture service is configured, and it is injected
	// into the page rather than fetched because the answer has to be known
	// before the first row is drawn. /api/version arrives after the first
	// paint, so gating on it would mean a listing rendering without pictures
	// and then growing a column, which is worse than either outcome on its
	// own.
	Shots bool
}

// Handler serves the frontend: a static file when the path names one, and
// index.html for everything else.
//
// The fallback is what makes deep links work. The frontend is a single-page app
// that routes on the URL, so /realm/gno.land/r/demo/boards has to arrive at the
// same HTML as / — serving a 404 there would break every link anyone shares.
//
// The static attempt comes first and is an existence check rather than a path
// prefix: anything actually embedded is served as itself, and only genuine
// misses fall through to the app.
func Handler(opts Options) (http.HandlerFunc, error) {
	sub, err := fs.Sub(frontendFS, "frontend")
	if err != nil {
		return nil, fmt.Errorf("frontend fs: %w", err)
	}
	static := http.FileServer(http.FS(sub))
	index, err := fs.ReadFile(frontendFS, "frontend/index.html")
	if err != nil {
		return nil, fmt.Errorf("read index.html: %w", err)
	}

	// Before the ETag and the gzip below, so both describe what is actually
	// sent: turning analytics on changes the body, and a reader holding the
	// previous build's tag has to be told so.
	index, err = withAnalytics(index, opts.AnalyticsScript)
	if err != nil {
		return nil, err
	}
	index, err = withFeatures(index, opts)
	if err != nil {
		return nil, err
	}

	// The app is a single 450 KB HTML file, and every deep link serves the
	// whole thing again. It is also immutable for the life of the process — it
	// is compiled in — so its ETag can be computed once here, and a reader
	// navigating the site pays for it exactly once per deploy instead of once
	// per cold tab. `no-cache` is deliberate rather than a max-age: it means
	// "revalidate", so a new build is picked up on the next request, while an
	// unchanged one costs a 304 with no body.
	sum := sha256.Sum256(index)
	etag := `"` + base64.RawURLEncoding.EncodeToString(sum[:16]) + `"`

	// And compressed once, at the best ratio rather than the fastest, for the
	// same reason: this body never changes, so the cost is paid at startup and
	// the saving is paid back on every cold page load. 466 KB -> ~110 KB, where
	// the generic middleware's BestSpeed pass gets ~155 KB and spends a few
	// milliseconds of CPU per request to do it.
	indexGzip := gzipBytes(index)

	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			if f, err := sub.Open(r.URL.Path[1:]); err == nil {
				f.Close()
				static.ServeHTTP(w, r)
				return
			}
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("ETag", etag)
		w.Header().Set("Cache-Control", "no-cache")
		// Set, not Add: WithCompression has already added one on the way in,
		// and two identical values in a Vary header is noise a shared cache
		// has to parse.
		w.Header().Set("Vary", "Accept-Encoding")
		if matchesETag(r.Header.Get("If-None-Match"), etag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		body := index
		if indexGzip != nil && acceptsGzip(r) {
			w.Header().Set("Content-Encoding", "gzip")
			body = indexGzip
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.Write(body)
	}, nil
}

// gzipBytes compresses at the best available ratio, or returns nil if it
// cannot. A nil result is a supported outcome, not a failure: the caller simply
// serves the body uncompressed, and the generic middleware still gets a chance
// at it.
func gzipBytes(b []byte) []byte {
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil
	}
	if _, err := zw.Write(b); err != nil {
		return nil
	}
	if err := zw.Close(); err != nil {
		return nil
	}
	return buf.Bytes()
}

// acceptsGzip reports whether the client advertised gzip. Same rule as the
// httpapi middleware, duplicated rather than imported because pkg/web must not
// depend on pkg/httpapi — it is the leaf the whole binary can serve without.
func acceptsGzip(r *http.Request) bool {
	for _, v := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		name, _, _ := strings.Cut(strings.TrimSpace(v), ";")
		if strings.EqualFold(name, "gzip") {
			return true
		}
	}
	return false
}

// matchesETag reports whether an If-None-Match header names the given tag.
//
// The header is a comma-separated list and a revalidating browser may send the
// tag back weakened (`W/"..."`) even though it was issued strong, so a plain
// string comparison against the whole header misses the match and re-sends
// 450 KB for nothing.
func matchesETag(inm, etag string) bool {
	if inm == "" {
		return false
	}
	for _, candidate := range strings.Split(inm, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" {
			return true
		}
		if strings.TrimPrefix(candidate, "W/") == etag {
			return true
		}
	}
	return false
}

// withAnalytics returns index.html with an analytics script tag added just
// before </head>, or unchanged when src is empty.
//
// Injected here rather than written into index.html because the frontend is
// one file compiled into the binary and shared by every deployment, and a
// hardcoded tag would make anyone else running mygnoscan report to our account.
//
// The URL lands in an HTML attribute, so it is validated as an absolute http(s)
// URL and escaped. A misconfigured flag fails at startup rather than shipping
// a broken tag, or a javascript: URL, to every reader.
func withAnalytics(index []byte, src string) ([]byte, error) {
	if src == "" {
		return index, nil
	}
	u, err := url.Parse(src)
	if err != nil {
		return nil, fmt.Errorf("analytics script %q: %w", src, err)
	}
	if (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, fmt.Errorf("analytics script %q: want an absolute http(s) URL", src)
	}
	i := bytes.Index(index, []byte("</head>"))
	if i < 0 {
		return nil, fmt.Errorf("analytics script: no </head> in index.html")
	}
	tag := "<!-- Third-party analytics, off unless -analytics-script names one. -->\n" +
		`<script async src="` + html.EscapeString(u.String()) + `"></script>` + "\n"
	out := make([]byte, 0, len(index)+len(tag))
	out = append(out, index[:i]...)
	out = append(out, tag...)
	out = append(out, index[i:]...)
	return out, nil
}

// withFeatures injects the server-side feature flags the first paint depends on.
//
// A boolean literal built here, never a value echoed from a request: this is a
// <script> in the document, and the one rule it has to keep is that nothing
// attacker-influenced can reach it.
func withFeatures(index []byte, opts Options) ([]byte, error) {
	// Nothing to say when every flag is off, and saying nothing keeps the
	// default build byte-identical to the embedded file.
	if !opts.Shots {
		return index, nil
	}
	i := bytes.Index(index, []byte("</head>"))
	if i < 0 {
		return nil, fmt.Errorf("features: no </head> in index.html")
	}
	tag := "<script>window.FEATURES={shots:" + strconv.FormatBool(opts.Shots) + "};</script>\n"
	out := make([]byte, 0, len(index)+len(tag))
	out = append(out, index[:i]...)
	out = append(out, tag...)
	out = append(out, index[i:]...)
	return out, nil
}

// Index is the embedded index.html, for callers that need the bytes rather than
// a handler.
func Index() ([]byte, error) { return fs.ReadFile(frontendFS, "frontend/index.html") }
