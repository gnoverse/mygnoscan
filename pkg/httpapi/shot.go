package httpapi

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Realm screenshots.
//
// The pictures themselves are taken by gnoshot (github.com/gnoverse/gnoshot),
// which drives a headless Chrome over gnoweb and crops to the realm's own
// render. This file is the explorer's side of that: one endpoint, on the
// explorer's own origin, that turns a package path into an image URL.
//
// Same origin on purpose, and it is the one decision here worth defending. A
// listing page opens fifty thumbnails; pointing them at a fourth host costs a
// DNS lookup and a TLS handshake before the first byte of the first one, on a
// page that already carries too much third-party JavaScript. It is also the
// only place the allowlist and the parameter validation can live, because a
// browser will happily ask any origin for anything.

// shotUpstream is the gnoshot base URL. Empty disables the feature entirely:
// an explorer with no capture service behind it serves no image URLs at all,
// rather than serving ones that will fail.
func (a *API) SetShotUpstream(base string) {
	a.shotUpstream = strings.TrimSuffix(base, "/")
}

// ShotsEnabled reports whether a capture service is configured. The frontend
// reads this and decides whether to put any <img> on the page at all, which is
// what keeps a deployment without gnoshot from drawing fifty broken tiles.
func (a *API) ShotsEnabled() bool { return a.shotUpstream != "" }

// shotModes and shotSizes are closed sets, validated here rather than passed
// through. The parameters arrive from a page that renders chain-chosen strings,
// so "whatever the caller sent" is not an acceptable value for any of them.
var (
	shotModes = map[string]bool{"render": true, "page": true}
	shotSizes = map[string]bool{"og": true, "hero": true, "thumb": true}
)

// gnowebURLFor builds the page URL a capture is taken of.
//
// The package path is chain-chosen and can contain anything a deployer typed,
// so it goes through url.URL rather than into a format string: a path holding a
// "?" or a "#" would otherwise smuggle query parameters into the upstream call.
func (a *API) gnowebURLFor(network, pkgPath string) (string, error) {
	var base string
	for _, n := range a.networks {
		if n.ID == network {
			base = n.GnowebURL
			break
		}
	}
	if base == "" {
		return "", fmt.Errorf("network %q has no gnoweb", network)
	}
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	p := strings.TrimPrefix(pkgPath, "gno.land")
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if strings.Contains(p, "..") {
		return "", fmt.Errorf("bad path")
	}
	u.Path = p
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

// shotClient is separate from the indexer clients: a capture that is still
// being taken answers in milliseconds with a placeholder, so a long timeout
// here would only ever be waiting on something that has already gone wrong.
var shotClient = &http.Client{Timeout: 15 * time.Second}

// HandleShot proxies one image.
//
// GET /api/shot?network=&path=&mode=render|page&size=og|hero|thumb&dpr=1|2&v=
//
// The v parameter is not read here. It is the caller's cache key — a realm's
// last call height is the usual choice — and its only job is to make the URL
// change when the picture might have. Passing it through to an immutable
// response is the whole point of it existing.
func (a *API) HandleShot(w http.ResponseWriter, r *http.Request) {
	if !a.ShotsEnabled() {
		http.Error(w, "screenshots are not configured", http.StatusNotFound)
		return
	}
	q := r.URL.Query()
	network := q.Get("network")
	if network == "" {
		network = defaultShotNetwork(a)
	}
	pageURL, err := a.gnowebURLFor(network, q.Get("path"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	mode := q.Get("mode")
	if mode == "" {
		mode = "render"
	}
	size := q.Get("size")
	if size == "" {
		size = "thumb"
	}
	if !shotModes[mode] || !shotSizes[size] {
		http.Error(w, "unknown mode or size", http.StatusBadRequest)
		return
	}
	dpr := q.Get("dpr")
	if dpr != "1" && dpr != "2" {
		dpr = "2"
	}

	up := url.Values{}
	up.Set("url", pageURL)
	up.Set("mode", mode)
	up.Set("size", size)
	up.Set("dpr", dpr)
	if v := q.Get("v"); v != "" {
		up.Set("v", v)
	}
	a.proxyShot(w, r, "/shot?"+up.Encode())
}

// HandleShotMeta proxies the manifest entry: which selector matched, the box,
// whether the capture was truncated. A consumer that wants to say "view the
// full page" needs the truncated flag, and nothing else can tell it.
func (a *API) HandleShotMeta(w http.ResponseWriter, r *http.Request) {
	if !a.ShotsEnabled() {
		http.Error(w, "screenshots are not configured", http.StatusNotFound)
		return
	}
	q := r.URL.Query()
	network := q.Get("network")
	if network == "" {
		network = defaultShotNetwork(a)
	}
	pageURL, err := a.gnowebURLFor(network, q.Get("path"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	up := url.Values{}
	up.Set("url", pageURL)
	a.proxyShot(w, r, "/meta?"+up.Encode())
}

func (a *API) proxyShot(w http.ResponseWriter, r *http.Request, path string) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, a.shotUpstream+path, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if m := r.Header.Get("If-None-Match"); m != "" {
		req.Header.Set("If-None-Match", m)
	}
	resp, err := shotClient.Do(req)
	if err != nil {
		// The capture service being down is not the explorer being down. Say so
		// briefly and let the page draw its own placeholder; a 500 here would
		// make a listing look broken over a decoration.
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, "capture service unavailable", http.StatusServiceUnavailable)
		return
	}
	defer resp.Body.Close()

	for _, h := range []string{
		"Content-Type", "Cache-Control", "ETag", "Vary",
		"X-Gnoshot-Matched", "X-Gnoshot-State", "X-Gnoshot-Truncated",
	} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if resp.StatusCode == http.StatusNotModified {
		return
	}
	// Bounded: an upstream that starts streaming gigabytes at us is a bug
	// somewhere, and the biggest thing it can legitimately send is a page
	// master at a couple of megabytes.
	_, _ = io.Copy(w, io.LimitReader(resp.Body, 16<<20))
}

// defaultShotNetwork picks the first configured network that has a gnoweb, so
// a caller that omits the parameter gets the obvious answer rather than an
// error.
func defaultShotNetwork(a *API) string {
	for _, n := range a.networks {
		if n.GnowebURL != "" {
			return n.ID
		}
	}
	return ""
}

// HandleShotSite proxies a screenshot of a page that is not on a chain.
//
// GET /api/shot/site?url=&size=og|hero|thumb&dpr=1|2
//
// Most of what is built on gno.land is not a realm, so most of it cannot be
// photographed through gnoweb. The community's own list names those projects
// and, after `make awesome`, where each one lives; this is how the app hub puts
// a picture on them.
//
// The gate is the whole design. An unbounded ?url= on a capture service is an
// open proxy and a way to spend someone else's CPU on headless Chrome, so a URL
// is accepted only if it appears **verbatim** in the vendored snapshot: not a
// host match, the exact string a human merged. gnoshot has its own host
// allowlist behind this one and refuses anything not on it, which is the second
// gate and the one that survives a bug in the first.
func (a *API) HandleShotSite(w http.ResponseWriter, r *http.Request) {
	if !a.ShotsEnabled() {
		http.Error(w, "screenshots are not configured", http.StatusNotFound)
		return
	}
	q := r.URL.Query()
	site := q.Get("url")
	if !a.registry.AllowsSite(site) {
		// Deliberately the same answer as an unknown route: this endpoint has
		// nothing to say about URLs it does not serve, and enumerating what it
		// would accept is not its job.
		http.Error(w, "not a listed site", http.StatusBadRequest)
		return
	}
	size := q.Get("size")
	if size == "" {
		size = "thumb"
	}
	if !shotSizes[size] {
		http.Error(w, "unknown size", http.StatusBadRequest)
		return
	}
	dpr := q.Get("dpr")
	if dpr != "1" && dpr != "2" {
		dpr = "2"
	}

	up := url.Values{}
	up.Set("url", site)
	// Never `page`, which would serve the full-height master: several of these
	// sites are eight thousand pixels tall and the master is close to a
	// megabyte. The rung is derived from that master by gnoshot, cropped to the
	// top, which is the part of a landing page that identifies it.
	up.Set("mode", "render")
	up.Set("size", size)
	up.Set("dpr", dpr)
	a.proxyShot(w, r, "/shot?"+up.Encode())
}
