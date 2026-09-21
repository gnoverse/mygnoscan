package web

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The app is one 450 KB file re-served on every deep link and every cold tab.
// These are the three things that stop that being the dominant cost of loading
// the site, and each of them has a silent failure mode: a missing
// Content-Encoding just sends the big body, a missing ETag just re-sends it, and
// a wrong Content-Length truncates the page with no error anywhere.
func TestIndexIsCompressedAndRevalidatable(t *testing.T) {
	h, err := Handler(Options{})
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

// Two `function` declarations sharing a name are not an error in JavaScript.
// The later one wins, silently, for every caller including the earlier one's
// own — so a function written for one page starts answering calls made by
// another, with a different signature, and the first symptom is a TypeError
// deep inside unrelated rendering.
//
// That is not hypothetical. `paramValueEl` was written twice: once by the
// /params page for a parameter object, once by the govdao proposal diff for a
// bare string. Each pull request was green on its own base. They merged eight
// seconds apart, and /params was dead on main from that moment, because the
// diff's one-argument version was hoisted over the page's own.
//
// Nothing else catches this. The frontend is one file in one global scope with
// no build step, no module boundary and no bundler to warn; the Go tests never
// execute the JavaScript, and a browser test only finds it if a test happens to
// exercise the losing caller. So the guard is textual and deliberately cheap.
//
// `const` and `let` need no guard: redeclaring either is a SyntaxError, which
// takes the whole script down and cannot reach main unnoticed.
func TestFrontendHasNoDuplicateTopLevelDeclarations(t *testing.T) {
	index, err := Index()
	if err != nil {
		t.Fatalf("Index: %v", err)
	}

	// Anchored at column zero on purpose: that is what "top level" means in
	// this file, and it keeps every nested closure and method out.
	decl := regexp.MustCompile(`(?m)^(?:function|var)\s+([A-Za-z0-9_$]+)`)
	seen := map[string]int{}
	for _, m := range decl.FindAllSubmatch(index, -1) {
		seen[string(m[1])]++
	}

	var dupes []string
	for name, n := range seen {
		if n > 1 {
			dupes = append(dupes, fmt.Sprintf("%s (%d declarations)", name, n))
		}
	}
	sort.Strings(dupes)
	if len(dupes) > 0 {
		t.Errorf("declared more than once at the top level of index.html, so the last one silently wins everywhere: %s",
			strings.Join(dupes, ", "))
	}
}

// Analytics is off by default and on by flag, and the two states are not the
// same document. Three things go wrong quietly here: a tag that ships to every
// deployment because it was written into index.html instead of injected, a URL
// dropped straight into an attribute unescaped, and a body that changes while
// the ETag does not, which serves the old page from cache until the reader
// hard-reloads.
func TestAnalyticsScript(t *testing.T) {
	const sa = "https://scripts.simpleanalyticscdn.com/latest.js"

	body := func(t *testing.T, opts Options) string {
		t.Helper()
		h, err := Handler(opts)
		if err != nil {
			t.Fatalf("Handler: %v", err)
		}
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest("GET", "/", nil))
		return rec.Body.String()
	}

	t.Run("off by default", func(t *testing.T) {
		if got := body(t, Options{}); strings.Contains(got, "simpleanalytics") {
			t.Error("the default build ships an analytics tag")
		}
		index, err := Index()
		if err != nil {
			t.Fatalf("Index: %v", err)
		}
		if bytes.Contains(index, []byte("simpleanalyticscdn")) {
			t.Error("index.html carries a hardcoded analytics tag, so every deployment reports to one account")
		}
	})

	t.Run("injected into the head when configured", func(t *testing.T) {
		got := body(t, Options{AnalyticsScript: sa})
		tag := `<script async src="` + sa + `"></script>`
		i := strings.Index(got, tag)
		if i < 0 {
			t.Fatalf("no %s in the served page", tag)
		}
		if head := strings.Index(got, "</head>"); i > head {
			t.Errorf("tag at %d is after </head> at %d", i, head)
		}
	})

	t.Run("the ETag follows the body", func(t *testing.T) {
		etag := func(opts Options) string {
			h, err := Handler(opts)
			if err != nil {
				t.Fatalf("Handler: %v", err)
			}
			rec := httptest.NewRecorder()
			h(rec, httptest.NewRequest("GET", "/", nil))
			return rec.Header().Get("ETag")
		}
		if on, off := etag(Options{AnalyticsScript: sa}), etag(Options{}); on == off {
			t.Errorf("same ETag %s with and without analytics, so caches keep serving the old page", on)
		}
	})

	t.Run("a URL that cannot go in an attribute is a startup error", func(t *testing.T) {
		for _, src := range []string{
			"javascript:alert(1)",
			"latest.js",
			"//scripts.simpleanalyticscdn.com/latest.js",
			"https://x.example/a.js\" onload=\"alert(1)",
		} {
			t.Run(src, func(t *testing.T) {
				h, err := Handler(Options{AnalyticsScript: src})
				if err != nil {
					return // rejected outright, which is the preferred outcome
				}
				// Accepted, so it must at least be inert in the attribute it
				// landed in.
				rec := httptest.NewRecorder()
				h(rec, httptest.NewRequest("GET", "/", nil))
				if strings.Contains(rec.Body.String(), `onload="alert(1)`) {
					t.Error("the URL broke out of the src attribute")
				}
			})
		}
	})
}

// The rail is static HTML and the NAV table beside it is JavaScript, and
// they describe the same navigation twice: the markup so the chrome paints
// without waiting on two render-blocking CDN scripts, the table so route()
// knows which parent to light up and pageNav() can build each section's pill
// strip.
//
// Two copies means one can go stale, and the stale one fails quietly. Adding
// a rail entry and forgetting the table gives a page that navigates fine and
// has no section strip, which reads as a page that is simply not in a
// section. Adding it to the table only gives a strip linking to a rail entry
// that never highlights. Neither throws, neither shows up in a browser test
// that only checks the page renders.
//
// So: same ids, same order, same paths. Parsed textually, which is enough
// because both sides are written by hand in a fixed shape and a shape change
// fails here loudly rather than silently.
func TestRailMatchesNavTable(t *testing.T) {
	index, err := Index()
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	html := string(index)

	rail := between(t, html, "<nav>", "</nav>")
	navTable := between(t, html, "const NAV = [", "\n];")

	// id and path, in document order, from each side. The rail's <a> carries
	// both as `navigate('<path>')` and `id="nav-<id>"`; NAV's entries carry
	// them as `id: '<id>'` and `path: '<path>'`.
	railRe := regexp.MustCompile(`navigate\('([^']+)'\)" id="nav-([A-Za-z0-9-]+)"`)
	tableRe := regexp.MustCompile(`id: '([A-Za-z0-9-]+)',[^\n]*?path: '([^']+)'`)

	type entry struct{ id, path string }
	var fromRail, fromTable []entry
	for _, m := range railRe.FindAllStringSubmatch(rail, -1) {
		fromRail = append(fromRail, entry{id: m[2], path: m[1]})
	}
	for _, m := range tableRe.FindAllStringSubmatch(navTable, -1) {
		fromTable = append(fromTable, entry{id: m[1], path: m[2]})
	}

	if len(fromRail) == 0 || len(fromTable) == 0 {
		t.Fatalf("parsed %d rail entries and %d table entries — the shape of one of them changed",
			len(fromRail), len(fromTable))
	}
	if len(fromRail) != len(fromTable) {
		t.Fatalf("the rail has %d entries and NAV has %d:\nrail:  %v\ntable: %v",
			len(fromRail), len(fromTable), fromRail, fromTable)
	}
	for i := range fromRail {
		if fromRail[i] != fromTable[i] {
			t.Errorf("entry %d: the rail says %+v, NAV says %+v", i, fromRail[i], fromTable[i])
		}
	}
}

func between(t *testing.T, s, openTag, closeTag string) string {
	t.Helper()
	i := strings.Index(s, openTag)
	if i < 0 {
		t.Fatalf("index.html has no %q", openTag)
	}
	rest := s[i+len(openTag):]
	j := strings.Index(rest, closeTag)
	if j < 0 {
		t.Fatalf("index.html has no %q after %q", closeTag, openTag)
	}
	return rest[:j]
}

// The icon sprite is a block of <symbol> definitions at the top of
// index.html, referenced by <use href="#i-name">. Nothing links the two: an
// orphaned symbol ships to every reader forever, and a reference to a symbol
// that is not there renders as an empty 15px box with no error anywhere.
//
// Both happen for the same reason, which is that the sprite is edited from
// the other end of a 13,000-line file. Nesting the nav orphaned nine of them
// in one commit: every icon that belonged to an entry which became a child,
// since children are indented text and carry none.
func TestFrontendSpriteHasNoOrphansOrDanglingRefs(t *testing.T) {
	index, err := Index()
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	html := string(index)

	defined := map[string]bool{}
	for _, m := range regexp.MustCompile(`<symbol id="(i-[a-z0-9-]+)"`).FindAllStringSubmatch(html, -1) {
		defined[m[1]] = true
	}
	used := map[string]bool{}
	for _, m := range regexp.MustCompile(`href="#(i-[a-z0-9-]+)"`).FindAllStringSubmatch(html, -1) {
		used[m[1]] = true
	}
	if len(defined) == 0 || len(used) == 0 {
		t.Fatalf("found %d symbols and %d references — the shape of the sprite changed", len(defined), len(used))
	}

	var orphans, dangling []string
	for name := range defined {
		if !used[name] {
			orphans = append(orphans, name)
		}
	}
	for name := range used {
		if !defined[name] {
			dangling = append(dangling, name)
		}
	}
	sort.Strings(orphans)
	sort.Strings(dangling)

	if len(orphans) > 0 {
		t.Errorf("defined but never referenced, so they are dead bytes in every response: %s",
			strings.Join(orphans, ", "))
	}
	if len(dangling) > 0 {
		t.Errorf("referenced but not defined, so they render as an empty box: %s",
			strings.Join(dangling, ", "))
	}
}
