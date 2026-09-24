package httpapi

import (
	"context"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/moul/mygnoscan/pkg/store"
)

// Counting reads, which is the only popularity signal that exists here.
//
// Everything this explorer ranks on is a write: calls, callers, gas, transfers.
// That is the right signal for anything transactional and it is blind by
// construction to a realm people *read*. A blog written to three times, a
// profile page deployed once and then only opened, a registry answering
// vm/qrender: gno.land records none of those reads, so no amount of indexing
// will ever surface them, and /apps quietly means "what is written to on
// gno.land" rather than "what is built on it".
//
// What this server can say honestly is which realms its own readers opened. So
// it counts that, and the number is labelled as this explorer's measurement
// rather than the chain's wherever it is shown. It is not a proxy for
// popularity on gno.land and must never be printed as one: gnoweb, Gnoscan and
// every wallet serve the same realms and none of that is visible here.

// viewFlushInterval is how often buffered counts reach the database. A read is
// the cheapest thing this server does, and paying for a write on each one would
// make looking at a realm cost more than the realm.
const viewFlushInterval = 30 * time.Second

// ViewCounter buffers read counts and flushes them in batches.
type ViewCounter struct {
	db *store.DB

	mu  sync.Mutex
	buf map[store.ViewKey]int
}

func NewViewCounter(db *store.DB) *ViewCounter {
	return &ViewCounter{db: db, buf: map[store.ViewKey]int{}}
}

// Record adds one read. Cheap and non-blocking: a map write under a mutex.
func (v *ViewCounter) Record(network, path string, now time.Time) {
	if v == nil || path == "" {
		return
	}
	// An absent ?network resolves to "every chain", which is not a chain a
	// realm can be read on. Counting those under "" would make a bucket nobody
	// can query per network and would double-count the same page open.
	if network == "" {
		network = "all"
	}
	key := store.ViewKey{Network: network, Path: path, Day: store.ViewDay(now)}
	v.mu.Lock()
	v.buf[key]++
	v.mu.Unlock()
}

// Flush writes what is buffered. Safe to call on an empty buffer.
func (v *ViewCounter) Flush() {
	if v == nil {
		return
	}
	v.mu.Lock()
	batch := v.buf
	v.buf = map[store.ViewKey]int{}
	v.mu.Unlock()
	if len(batch) == 0 {
		return
	}
	if err := v.db.AddRealmViews(batch); err != nil {
		log.Printf("realm views: flush: %v", err)
	}
}

// Run flushes on a timer until ctx is done, then flushes once more.
//
// The last flush is the point of the goroutine: a deploy restarts this process
// several times a day, and dropping up to thirty seconds of counts each time
// would bias the number toward whatever nobody was reading at deploy time.
func (v *ViewCounter) Run(ctx context.Context) {
	t := time.NewTicker(viewFlushInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			v.Flush()
			return
		case <-t.C:
			v.Flush()
		}
	}
}

// realmAPIPrefix is the one request that means "somebody opened a realm page".
//
// The detail read, not the tabs. A realm page fires several requests (usage,
// defi, state, deps) and counting all of them would rank a realm by how many
// tabs it has rather than by how many people opened it.
const realmAPIPrefix = "/api/realm/"

// WithRealmViews counts realm page opens.
//
// **Outside the response cache**, which is the whole reason this is middleware
// and not three lines in HandleRealm. A cached answer never reaches the
// handler, so counting there would count first readers and miss everyone who
// followed: the more a realm is read, the less it would appear to be read.
func WithRealmViews(v *ViewCounter, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if v != nil && r.Method == http.MethodGet && isRealmDetailPath(r.URL.Path) && !isRobot(r) {
			path := "gno.land/" + strings.TrimRight(strings.TrimPrefix(r.URL.Path, realmAPIPrefix), "/")
			v.Record(r.URL.Query().Get("network"), path, time.Now())
		}
		next.ServeHTTP(w, r)
	})
}

// isRealmDetailPath excludes the sub-routes that share the prefix. They are the
// realm page's other tabs, and a page open is one open.
func isRealmDetailPath(p string) bool {
	rest, ok := strings.CutPrefix(p, realmAPIPrefix)
	if !ok || rest == "" {
		return false
	}
	for _, sub := range []string{"usage/", "defi/", "cousage/"} {
		if strings.HasPrefix(rest, sub) {
			return false
		}
	}
	return true
}

// isRobot drops the readers that are not readers.
//
// Two kinds, and the second is the one that would have gone unnoticed: a
// crawler, and this server's own cache warmer, which replays real request paths
// through the real handler stack and would otherwise vote for whatever it warms.
// The warmer does not currently warm a realm path (see warmTargets) and this
// does not rely on that staying true.
func isRobot(r *http.Request) bool {
	ua := strings.ToLower(r.Header.Get("User-Agent"))
	if ua == "" {
		// No user agent at all is a script, and a script is not a reader. It is
		// also what an internal MCP call looks like.
		return true
	}
	for _, s := range []string{"mygnoscan-warmer", "bot", "crawl", "spider", "slurp", "headless", "curl", "wget", "python-requests", "go-http-client"} {
		if strings.Contains(ua, s) {
			return true
		}
	}
	return false
}

// HandleViews ranks what this explorer's readers opened.
//
// GET /api/views?network=&window=24h|7d|30d|90d&limit=
//
// Served separately from every other ranking on purpose. Mixing a read count
// into /apps or /realms would put this explorer's own measurement in a column
// beside figures the chain proves, where a reader has no way to tell them
// apart. Here the endpoint is the label.
func (a *API) HandleViews(w http.ResponseWriter, r *http.Request) {
	// Flush first. The buffer is at most one interval deep, but this is the
	// endpoint whose whole job is to answer "what are people reading", and
	// answering it from a database that is knowingly thirty seconds behind a
	// buffer in this same process is a worse trade than one small write. It is
	// also what makes the count testable without a sleep.
	a.views.Flush() // nil-safe
	network := a.networkParam(r)
	window := r.URL.Query().Get("window")
	if _, ok := usageWindows[window]; !ok {
		window = "30d"
	}
	since := store.ViewsSince(window, time.Now())

	// ?path= answers for one realm, which is what the realm page asks. It is a
	// separate request from the realm detail on purpose: that endpoint is
	// cached, and a count of how often a page is read cannot be served from a
	// cache that the reading fills. See realmDetailResponse.
	var rows []store.RealmView
	var err error
	if path := r.URL.Query().Get("path"); path != "" {
		var n map[string]int
		if n, err = a.db.RealmViewsFor(network, []string{path}, since); err == nil {
			rows = []store.RealmView{{Path: path, Views: n[path]}}
		}
	} else {
		rows, err = a.db.TopViewedRealms(network, since, intParam(r.URL.Query(), "limit", 50, 200))
	}
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	JSONResponse(w, map[string]any{
		"network": network,
		"window":  window,
		"realms":  rows,
		// Said in the payload, not only in the docs: a consumer that renders
		// this beside a call count has to be told which one the chain proves.
		"kind": "inferred",
		"why":  "how many times each realm was opened on this explorer. gno.land records no reads, so this is this explorer's own measurement and not a chain fact: it does not count gnoweb, Gnoscan, wallets or anything else serving the same realms.",
	})
}

// SetViewCounter hands the API the counter the middleware feeds, so the read
// endpoint can flush it before answering.
func (a *API) SetViewCounter(v *ViewCounter) { a.views = v }
