package web

import (
	"bytes"
	"html"
	"net/http"
	"net/url"
	"strings"
)

// Link previews for a realm.
//
// Every realm on the chain now has a captured picture of itself, served by
// /api/shot, and a link to one still previewed in Slack, Discord or X as a bare
// text card: the document carries no og: tags at all, so there is nothing for a
// crawler to read.
//
// This is the one place the app serves a per-path body. Handler's comments
// explain at length why there is otherwise exactly one document for every deep
// link, and that stays true for every route but this one: a crawler does not
// run the SPA, so the only way it can learn what a realm looks like is from the
// bytes it is handed.

// realmPathFromURL returns the package path a /realm/ URL names, or "".
//
// The SPA navigates to /realm/r/gov/dao, dropping the gno.land prefix, but a
// pasted link may carry it. Both resolve to the same package, so both are
// accepted and normalised to the form /api/shot expects.
func realmPathFromURL(p string) string {
	rest, ok := strings.CutPrefix(p, "/realm/")
	if !ok {
		return ""
	}
	rest = strings.Trim(rest, "/")
	if rest == "" {
		return ""
	}
	// A path element that is not a path: reject anything that could climb out
	// of the namespace or smuggle a second URL in.
	if strings.Contains(rest, "..") || strings.Contains(rest, "//") {
		return ""
	}
	rest = strings.TrimPrefix(rest, "gno.land/")
	// r/ and p/ are the only two markers a package path starts with. Anything
	// else is not a realm URL and gets the ordinary document.
	if !strings.HasPrefix(rest, "r/") && !strings.HasPrefix(rest, "p/") {
		return ""
	}
	return "gno.land/" + rest
}

// requestOrigin is the scheme and host a crawler should be sent back to.
//
// og:image has to be absolute, and the only source for the public origin is the
// request itself. Behind a reverse proxy the scheme is in X-Forwarded-Proto;
// without it, TLS on the connection is the answer.
//
// Host is client-supplied, so it is validated to something host-shaped rather
// than trusted. The blast radius of a forged Host is only the forger's own
// preview, but a header echoed into an HTML attribute is worth being strict
// about regardless.
func requestOrigin(r *http.Request) string {
	host := r.Host
	if host == "" || len(host) > 255 || strings.ContainsAny(host, "/\\ \t\"'<>") {
		return ""
	}
	scheme := "http"
	if proto := r.Header.Get("X-Forwarded-Proto"); proto == "https" {
		scheme = "https"
	} else if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + host
}

// ogTags renders the preview block for one realm path.
//
// Everything interpolated here is either built from a constant or escaped: the
// package path comes off the chain by way of the URL, so it reaches the
// attribute through html.EscapeString and the query string through
// url.Values.Encode, never by concatenation.
func ogTags(origin, pkgPath, network string, withImage bool) []byte {
	title := pkgPath + " on mygnoscan"
	desc := "What " + pkgPath + " looks like, what it holds, and who has been calling it."

	var b bytes.Buffer
	b.WriteString("<!-- Per-path link preview. Handler serves one document for every other route. -->\n")
	meta := func(attr, key, value string) {
		b.WriteString(`<meta ` + attr + `="` + key + `" content="` + html.EscapeString(value) + `">` + "\n")
	}
	meta("property", "og:type", "website")
	meta("property", "og:site_name", "mygnoscan")
	meta("property", "og:title", title)
	meta("property", "og:description", desc)
	meta("name", "description", desc)
	if origin != "" {
		// Built through url.URL, not concatenated: html.EscapeString stops a
		// chain-chosen path breaking out of the attribute, and would happily
		// leave a quote or an angle bracket sitting raw inside the URL itself,
		// which is not a URL a crawler can follow back.
		if u, err := url.Parse(origin); err == nil {
			u.Path = "/realm/" + strings.TrimPrefix(pkgPath, "gno.land/")
			meta("property", "og:url", u.String())
		}
	}

	// No capture service, no picture, and no card claiming there is one: a
	// summary_large_image card with no image renders worse than a plain one.
	if withImage && origin != "" {
		q := url.Values{}
		q.Set("path", pkgPath)
		q.Set("size", "og")
		if network != "" {
			q.Set("network", network)
		}
		img := origin + "/api/shot?" + q.Encode()
		meta("property", "og:image", img)
		meta("property", "og:image:width", "1200")
		meta("property", "og:image:height", "630")
		meta("property", "og:image:alt", "A screenshot of "+pkgPath)
		meta("name", "twitter:card", "summary_large_image")
		meta("name", "twitter:image", img)
	} else {
		meta("name", "twitter:card", "summary")
	}
	meta("name", "twitter:title", title)
	meta("name", "twitter:description", desc)
	return b.Bytes()
}

// networkParam echoes a network back only if it looks like one of ours.
//
// It lands in a query string this server builds, so it is constrained rather
// than passed through: a network id is a short configured label, never
// punctuation.
func networkParam(r *http.Request) string {
	n := r.URL.Query().Get("network")
	if n == "" || len(n) > 32 {
		return ""
	}
	for _, c := range n {
		// Named rather than negated inline: QF1001 fires on a negated binary
		// expression whichever way it is written, so !(a && b) && !(c && d)
		// and !(a || b || c) are both flagged and "applying De Morgan's law"
		// only moves the complaint. Negating a single identifier ends it, and
		// the accept set reads as a list, which is what it is.
		allowed := c >= 'a' && c <= 'z' ||
			c >= 'A' && c <= 'Z' ||
			c >= '0' && c <= '9' ||
			c == '-' || c == '_'
		if !allowed {
			return ""
		}
	}
	return n
}
