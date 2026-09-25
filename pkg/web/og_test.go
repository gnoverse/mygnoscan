package web

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestRealmPathFromURL(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		// The SPA's own form, and the pasted one carrying the prefix.
		{"/realm/r/gov/dao", "gno.land/r/gov/dao"},
		{"/realm/gno.land/r/gov/dao", "gno.land/r/gov/dao"},
		{"/realm/p/nt/avl/v0", "gno.land/p/nt/avl/v0"},
		{"/realm/r/gov/dao/", "gno.land/r/gov/dao"},
		// Not a realm URL: these take the ordinary document.
		{"/realm/", ""},
		{"/realm", ""},
		{"/txs", ""},
		{"/address/g1abc", ""},
		{"/realmish/r/x/y", ""},
		// Neither marker, so not a package path.
		{"/realm/x/gov/dao", ""},
		{"/realm/gno.land/x/gov/dao", ""},
		// Anything that could climb out or smuggle a second URL in.
		{"/realm/r/../../etc/passwd", ""},
		{"/realm/r//evil.example", ""},
	}
	for _, tt := range tests {
		if got := realmPathFromURL(tt.in); got != tt.want {
			t.Errorf("realmPathFromURL(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestRequestOrigin(t *testing.T) {
	t.Run("a proxy's forwarded scheme wins", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/realm/r/gov/dao", nil)
		r.Host = "mygnoscan.example"
		r.Header.Set("X-Forwarded-Proto", "https")
		if got := requestOrigin(r); got != "https://mygnoscan.example" {
			t.Errorf("origin = %q", got)
		}
	})
	t.Run("a host-shaped check, because the header is the client's", func(t *testing.T) {
		// A forged Host only poisons the forger's own preview, but a value
		// echoed into an HTML attribute is worth being strict about anyway.
		for _, bad := range []string{
			`evil"onload=alert(1)`,
			"host with spaces",
			"host/with/slashes",
			"a<b>c",
			strings.Repeat("x", 300),
			"",
		} {
			r := httptest.NewRequest("GET", "/realm/r/gov/dao", nil)
			r.Host = bad
			if got := requestOrigin(r); got != "" {
				t.Errorf("Host %q produced origin %q, want it refused", bad, got)
			}
		}
	})
}

func TestNetworkParamIsConstrained(t *testing.T) {
	// It lands in a query string this server builds, so it is constrained
	// rather than passed through.
	for _, tt := range []struct{ in, want string }{
		{"mainnet", "mainnet"},
		{"test6-x_1", "test6-x_1"},
		{"", ""},
		{"main net", ""},
		{`main"net`, ""},
		{"main/net", ""},
		{strings.Repeat("n", 40), ""},
	} {
		r := httptest.NewRequest("GET", "/realm/r/gov/dao?network="+url.QueryEscape(tt.in), nil)
		if got := networkParam(r); got != tt.want {
			t.Errorf("networkParam(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func realmDoc(t *testing.T, opts Options, target string) string {
	t.Helper()
	h, err := Handler(opts)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", target, nil)
	req.Host = "mygnoscan.example"
	req.Header.Set("X-Forwarded-Proto", "https")
	h(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	return rec.Body.String()
}

// The whole point: a crawler does not run the SPA, so the picture has to be in
// the bytes it is handed.
func TestRealmDocumentCarriesTheCaptureAsItsCard(t *testing.T) {
	body := realmDoc(t, Options{Shots: true}, "/realm/r/gov/dao?network=mainnet")

	for _, want := range []string{
		`property="og:image" content="https://mygnoscan.example/api/shot?`,
		`network=mainnet`,
		`path=gno.land%2Fr%2Fgov%2Fdao`,
		`size=og`,
		`property="og:image:width" content="1200"`,
		`name="twitter:card" content="summary_large_image"`,
		`property="og:title" content="gno.land/r/gov/dao on mygnoscan"`,
		`property="og:url" content="https://mygnoscan.example/realm/r/gov/dao"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the realm document is missing %s", want)
		}
	}
}

// No capture service, no picture, and no card claiming there is one: a
// summary_large_image with no image renders worse than a plain summary.
func TestRealmDocumentWithoutACaptureServiceClaimsNoImage(t *testing.T) {
	body := realmDoc(t, Options{}, "/realm/r/gov/dao")

	if strings.Contains(body, "og:image") {
		t.Error("an og:image was advertised with no capture service behind it")
	}
	if !strings.Contains(body, `name="twitter:card" content="summary"`) {
		t.Error("the card was not downgraded to a plain summary")
	}
	// The text half is still worth having on its own.
	if !strings.Contains(body, `property="og:title"`) {
		t.Error("no og:title")
	}
}

// The package path comes off the chain by way of the URL, so it reaches the
// attribute escaped and the query string encoded, never by concatenation.
func TestRealmDocumentEscapesTheChainChosenPath(t *testing.T) {
	// A path a deployer could actually choose, carrying the characters that
	// break an attribute and a query string.
	body := realmDoc(t, Options{Shots: true}, "/realm/"+url.PathEscape(`r/x/a"><script>alert(1)</script>`))

	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Fatal("a chain-chosen path reached the document unescaped")
	}
	if strings.Contains(body, `content="gno.land/r/x/a"><`) {
		t.Fatal("a chain-chosen path broke out of its attribute")
	}
}

// Two realms must never share an ETag, or a cache hands one realm's preview to
// another. The build still has to move it too.
func TestRealmETagsAreDistinct(t *testing.T) {
	h, err := Handler(Options{Shots: true})
	if err != nil {
		t.Fatal(err)
	}
	tag := func(target string) string {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", target, nil)
		req.Host = "mygnoscan.example"
		h(rec, req)
		return rec.Header().Get("ETag")
	}
	a, b := tag("/realm/r/gov/dao"), tag("/realm/r/demo/boards")
	if a == "" || b == "" {
		t.Fatal("a realm document served no ETag")
	}
	if a == b {
		t.Fatal("two realms share an ETag, so a cache can serve one the other's preview")
	}
	// And the base document's tag is not reused for either.
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest("GET", "/txs", nil))
	if base := rec.Header().Get("ETag"); base == a || base == b {
		t.Fatal("a realm shares the base document's ETag")
	}
}

func TestRealmDocumentRevalidates(t *testing.T) {
	h, err := Handler(Options{Shots: true})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/realm/r/gov/dao", nil)
	req.Host = "mygnoscan.example"
	h(rec, req)
	etag := rec.Header().Get("ETag")

	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("GET", "/realm/r/gov/dao", nil)
	req2.Host = "mygnoscan.example"
	req2.Header.Set("If-None-Match", etag)
	h(rec2, req2)
	if rec2.Code != http.StatusNotModified {
		t.Fatalf("revalidation got %d, want 304: a per-path body still has to cost nothing on a repeat visit", rec2.Code)
	}
	if rec2.Body.Len() != 0 {
		t.Fatalf("304 carried %d bytes", rec2.Body.Len())
	}
}

func TestRealmDocumentGzips(t *testing.T) {
	h, err := Handler(Options{Shots: true})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/realm/r/gov/dao", nil)
	req.Host = "mygnoscan.example"
	req.Header.Set("Accept-Encoding", "gzip")
	h(rec, req)

	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("Content-Encoding = %q", rec.Header().Get("Content-Encoding"))
	}
	if got, want := rec.Header().Get("Content-Length"), rec.Body.Len(); got != "" && got != itoa(want) {
		t.Fatalf("Content-Length = %s, body is %d", got, want)
	}
	// Compressed at all, which is what makes a per-request body affordable.
	index, _ := Index()
	if rec.Body.Len() >= len(index) {
		t.Fatalf("gzipped realm document is %d bytes against %d uncompressed", rec.Body.Len(), len(index))
	}
	if !bytes.HasPrefix(rec.Body.Bytes(), []byte{0x1f, 0x8b}) {
		t.Fatal("body is not gzip")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// og:url has to be a URL a crawler can follow back, not merely a string safe
// inside an attribute. html.EscapeString gives the second and not the first.
func TestRealmCanonicalURLIsEncodedNotJustEscaped(t *testing.T) {
	body := realmDoc(t, Options{Shots: true}, "/realm/"+url.PathEscape(`r/x/a"b<c`))

	i := strings.Index(body, `property="og:url" content="`)
	if i < 0 {
		t.Fatal("no og:url")
	}
	rest := body[i+len(`property="og:url" content="`):]
	raw := rest[:strings.Index(rest, `"`)]
	// The attribute is HTML-escaped, so undo that to get at the URL itself.
	unescaped := strings.NewReplacer("&#34;", `"`, "&gt;", ">", "&lt;", "<", "&amp;", "&").Replace(raw)
	if strings.ContainsAny(unescaped, `"<>`) {
		t.Fatalf("og:url carries raw URL-unsafe characters: %q", unescaped)
	}
	if _, err := url.Parse(unescaped); err != nil {
		t.Fatalf("og:url does not parse: %v", err)
	}
}

// networkParam constrains a value this server then puts into a query string it
// builds, so its accept set is security-relevant and worth pinning rather than
// reading. The condition was rewritten under De Morgan's law to satisfy
// staticcheck QF1001; this asserts the rewrite accepts and rejects exactly what
// the original did, since "it lints clean now" is not the property that matters.
func TestNetworkParamAcceptSet(t *testing.T) {
	// The original condition, verbatim, before QF1001 was applied.
	original := func(n string) string {
		if n == "" || len(n) > 32 {
			return ""
		}
		for _, c := range n {
			if !(c >= 'a' && c <= 'z') && !(c >= 'A' && c <= 'Z') && !(c >= '0' && c <= '9') && c != '-' && c != '_' {
				return ""
			}
		}
		return n
	}

	cases := []string{
		"", "mainnet", "pearl", "test-13", "gnoland_1", "ABC", "a1-B_2",
		"z", "Z", "0", "9", "-", "_",
		// the boundary characters on either side of each accepted range
		"`", "{", "@", "[", "/", ":",
		// the ones that would matter if any of them got through
		"a b", "a&b", "a?b", "a#b", "a/b", "a%2e", "a\"b", "a'b", "a<b",
		"../etc", "main net", "main\tnet", "main\nnet", "aéb", "你好",
		// length boundary: 32 accepted, 33 refused
		"abcdefghijklmnopqrstuvwxyz012345",
		"abcdefghijklmnopqrstuvwxyz0123456",
	}
	for _, in := range cases {
		got := networkParam(&http.Request{URL: &url.URL{RawQuery: "network=" + url.QueryEscape(in)}})
		want := original(in)
		if got != want {
			t.Errorf("networkParam(%q) = %q, the pre-rewrite condition gives %q", in, got, want)
		}
	}
}
