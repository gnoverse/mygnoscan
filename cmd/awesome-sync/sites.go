package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/moul/mygnoscan/pkg/registry"
)

// Finding the app behind an entry.
//
// An awesome list is a list of links, and most of its links are repositories.
// That is right for the list and wrong for a grid of pictures: a screenshot of
// a GitHub page tells a reader nothing they did not already know from the word
// "GitHub". So for every entry that could be an app, this resolves the page you
// would actually open, and then checks that the page is really there.
//
// Two sources, and they are recorded separately because they are not the same
// claim. Either the list points straight at the app, or it points at a
// repository and that repository declares a homepage. The second is the
// project's own assertion about where its app lives, which is why it is worth
// following and why it is worth labelling.

// notAnApp are hosts where a URL is a document, a conversation or source code
// rather than something to use.
//
// A denylist rather than an allowlist, because the point is to follow the
// community wherever they go: a new app on a host nobody has seen before should
// work on the day it is added, and a new *forum* should not.
//
// github.io is deliberately absent. It hosts source for some projects and the
// deployed app for others, and the deployed app is the common case for a link
// somebody put in an apps section.
var notAnApp = map[string]bool{
	"github.com":                true,
	"gist.github.com":           true,
	"raw.githubusercontent.com": true,
	"gitlab.com":                true,
	"www.youtube.com":           true,
	"youtube.com":               true,
	"youtu.be":                  true,
	"x.com":                     true,
	"twitter.com":               true,
	"t.me":                      true,
	"discord.com":               true,
	"discord.gg":                true,
	"medium.com":                true,
	"docs.gno.land":             true,
	"gno.link":                  true,
	"awesome.re":                true,
	"calendar.google.com":       true,
}

func isAppHost(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	h := strings.ToLower(u.Hostname())
	if h == "" || notAnApp[h] || strings.HasSuffix(h, ".hashnode.dev") {
		return false
	}
	// gno.land itself is the chain's own front end, not an app somebody built
	// on it. It arrives here as the declared homepage of the gnolang/gno
	// monorepo, which is the homepage of everything in that repository and
	// therefore of nothing in particular.
	if h == "gno.land" {
		return false
	}
	return true
}

// repoRoot reports whether a GitHub URL names a whole repository rather than a
// directory inside one.
//
// This is the guard that keeps a monorepo's homepage off its contents. gnodev,
// gnobro and the gnoclient package are all linked as
// github.com/gnolang/gno/tree/master/..., and gnolang/gno declares its homepage
// as gno.land, so following it gave three CLI tools a picture of the chain's
// website and each other. A link into a subdirectory is a link to source; only
// a link to the repository itself is a claim about a project.
func repoRoot(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return len(strings.Split(strings.Trim(u.Path, "/"), "/")) == 2
}

// repoHomepage asks GitHub what homepage a repository declares.
func repoHomepage(ctx context.Context, repoURL string) string {
	u, err := url.Parse(repoURL)
	if err != nil || strings.ToLower(u.Hostname()) != "github.com" {
		return ""
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 {
		return ""
	}
	body, err := get(fmt.Sprintf("https://api.github.com/repos/%s/%s", parts[0], parts[1]))
	if err != nil {
		return ""
	}
	var doc struct {
		Homepage string `json:"homepage"`
		Archived bool   `json:"archived"`
	}
	if err := json.Unmarshal(body, &doc); err != nil || doc.Archived {
		return ""
	}
	return strings.TrimSpace(doc.Homepage)
}

// siteClient follows redirects, which is most of the point: half of these
// homepages are written without the www or with a trailing slash the server
// moves, and the URL that answers is the one worth storing.
var siteClient = &http.Client{Timeout: 20 * time.Second}

// reachable checks that a candidate is a page a browser can open, and returns
// the URL it settled on.
//
// A screenshot service will happily photograph a 404, and the result is a card
// that looks deliberate and says "Not Found". gnockpit is the worked example:
// its repository still declares a homepage on a testnet that was retired, so
// without this the grid would carry a confident picture of a dead host.
func reachable(raw string) (string, bool) {
	req, err := http.NewRequest(http.MethodGet, raw, nil)
	if err != nil {
		return "", false
	}
	// Some of these serve a different page, or nothing at all, to a client that
	// does not look like a browser.
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; mygnoscan-awesome-sync/1)")
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	resp, err := siteClient.Do(req)
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", false
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" && !strings.Contains(ct, "html") {
		// A JSON API or a tarball is a real 200 and not a page. The picture
		// would be Chrome's own rendering of raw text.
		return "", false
	}
	return resp.Request.URL.String(), true
}

// resolveSites fills Site and SiteFrom on every entry that turns out to have an
// app behind it. Concurrent because it is dominated by waiting on other
// people's servers, bounded because several of them are one box.
func resolveSites(ctx context.Context, sections []registry.AwesomeSection) {
	type job struct{ s, e int }
	jobs := []job{}
	for si, s := range sections {
		if !registry.IsAwesomeAppSection(s.Slug) || s.Archived {
			continue
		}
		for ei := range s.Entries {
			jobs = append(jobs, job{si, ei})
		}
	}
	sem := make(chan struct{}, 6)
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, j := range jobs {
		wg.Add(1)
		go func(j job) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			e := sections[j.s].Entries[j.e]
			if e.Path != "" {
				return // a realm; the chain's own gnoweb photographs it
			}
			site, from := "", ""
			if isAppHost(e.URL) {
				site, from = e.URL, registry.SiteListed
			} else if repoRoot(e.URL) {
				if hp := repoHomepage(ctx, e.URL); hp != "" && isAppHost(hp) {
					site, from = hp, registry.SiteRepoHomepage
				}
			}
			if site == "" {
				return
			}
			final, ok := reachable(site)
			mu.Lock()
			defer mu.Unlock()
			if !ok {
				fmt.Fprintf(os.Stderr, "  skip %-28s %s does not answer\n", e.Name, site)
				return
			}
			sections[j.s].Entries[j.e].Site = final
			sections[j.s].Entries[j.e].SiteFrom = from
		}(j)
	}
	wg.Wait()
}
