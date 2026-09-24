package httpapi

import (
	"context"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/moul/mygnoscan/pkg/syncer"
)

// The cache warmer: keep the answers hot so no reader is ever the first one.
//
// The response cache serves a stale entry instantly and refreshes it behind the
// reader, which covers the gap between two visitors. The gap that actually hurt
// is the one *before* the first visitor. CacheTTL is 30s and CacheStaleGrace is
// 15 minutes, so a reader is served instantly only if somebody else asked for
// the same thing in the last 15 and a half minutes, and this explorer does not
// get a visitor every fifteen minutes. Measured against production on
// 2026-09-24, /api/govdao/overview?network=mainnet answered in 24.8s, 30.5s,
// 48.1s and 51.1s cold, and in 27ms warm over 84 consecutive samples. Nothing
// was wrong with the warm path. Nothing kept anything warm.
//
// So the warmer walks a fixed list of the requests the site actually serves and
// re-issues them through the same middleware a browser goes through. The cache
// stops being a cache of whatever somebody happened to ask for and becomes a
// cache of what the site offers.

// WarmInterval is how long the warmer waits between passes.
//
// Measured from the end of a pass, not its start: a pass that overruns delays
// the next one instead of overlapping with it. Overlapping passes are how a
// warmer turns into the load it was meant to prevent.
const WarmInterval = 25 * time.Second

// warmConcurrency bounds how many targets a pass has in flight. The work is
// almost entirely waiting on somebody else's node or our own database, so a
// little concurrency shortens a pass a lot, and a lot of concurrency just moves
// the queue upstream where we cannot see it.
const warmConcurrency = 4

// warmRequestTimeout bounds one target. Generous, because the targets that most
// need warming are the slow ones -- that is the whole point -- and a target that
// cannot finish in three minutes is a bug to find rather than a deadline to
// tighten.
const warmRequestTimeout = 3 * time.Minute

// warmTargets are the requests the warmer keeps hot.
//
// The list is deliberately the landing state of each section rather than every
// route: a visitor lands on a page, and what makes the site feel slow is the
// first screen, not the fourth filter they apply. Routes that take a path
// argument (/api/realm/{path}, /api/tx/{hash}) are unbounded and are left to
// the stale-while-revalidate path they already have.
//
// No ?network is set on purpose. networkParam maps both "all" and absent to the
// empty string, canonicalQuery drops network=all, and the SPA sends network=all
// by default, so this one spelling is what nearly every real request keys to.
// Per-network variants are warmed only for the networks named in WarmNetworks,
// since those are paid for per network and most visitors never switch.
var warmTargets = []string{
	// The reported case, and the most expensive thing on the site.
	"/api/govdao/overview",
	"/api/govdao",
	"/api/govdao/voters",
	// Home.
	"/api/analytics",
	"/api/sanity/overview",
	"/api/blocks",
	"/api/bankstats",
	"/api/labels",
	"/api/validators/monikers",
	"/api/graph/active",
	// The section landings.
	"/api/accounts",
	"/api/accounts/rich",
	"/api/accounts/population",
	"/api/apps",
	"/api/assets",
	"/api/realms",
	"/api/namespaces",
	"/api/params",
	"/api/gas",
	"/api/calls/realms",
	"/api/contracts/map",
	"/api/storage/map",
	"/api/allevents",
}

// networkScopedWarmTargets are the subset worth warming per network as well as
// network-wide. govdao is here because its network selector is the one people
// actually use: mainnet is where the proposals are, and it is also the variant
// that measured 51s cold.
var networkScopedWarmTargets = []string{
	"/api/govdao/overview",
	"/api/govdao",
	"/api/govdao/voters",
}

// WarmPlan is the full list of request paths one pass issues.
func WarmPlan(networks []string) []string {
	plan := append([]string(nil), warmTargets...)
	for _, n := range networks {
		if n == "" || n == "all" {
			continue
		}
		for _, t := range networkScopedWarmTargets {
			plan = append(plan, t+"?network="+url.QueryEscape(n))
		}
	}
	sort.Strings(plan)
	return plan
}

// Warmer re-issues a fixed set of requests through the serving stack so their
// answers are already in the response cache when somebody asks.
type Warmer struct {
	handler  http.Handler
	plan     []string
	interval time.Duration

	mu     sync.Mutex
	passes int
	last   time.Time
	lastMS int64
}

// NewWarmer builds a warmer over the handler the public listener serves.
//
// It must be the handler *including* WithResponseCache, not the bare mux: the
// point is to fill that cache under exactly the keys a browser will produce,
// and a warmer that bypasses the middleware fills nothing.
func NewWarmer(handler http.Handler, networks []string, interval time.Duration) *Warmer {
	if interval <= 0 {
		interval = WarmInterval
	}
	return &Warmer{handler: handler, plan: WarmPlan(networks), interval: interval}
}

// WarmReadyGrace bounds how long WaitReady waits for a sync pass before
// warming anyway. A deployment with -sync=false never records one, and refusing
// to warm at all there would be worse than warming against whatever the
// database already holds.
const WarmReadyGrace = 2 * time.Minute

// WaitReady blocks until this process is serving answers worth storing.
//
// Warming a database that is still filling caches the half-empty answer, and
// CacheStaleGrace then serves it for up to fifteen minutes after it became
// wrong. That is not hypothetical: it is what this warmer did to the browser
// suite the first time it ran, where the binary starts against an empty file
// and the fixture is written behind it. /api/accounts is first in the sorted
// plan, so it was warmed empty while later targets were warmed after the rows
// landed, and exactly one test failed.
//
// A completed sync pass is the cheapest honest signal that the database holds
// what it is going to hold. It is not a guarantee, and it cannot be: nothing
// the process can observe covers a database being written behind its back. A
// harness that does that should pass -warm-interval=0 and say why.
func (wm *Warmer) WaitReady(ctx context.Context, health *syncer.Registry, grace time.Duration) {
	if health == nil {
		return
	}
	if grace <= 0 {
		grace = WarmReadyGrace
	}
	deadline := time.Now().Add(grace)
	for {
		for _, h := range health.Snapshot() {
			if h.LastSuccessAt != "" {
				return
			}
		}
		if time.Now().After(deadline) {
			log.Printf("warmer: no sync pass in %s, warming against the database as it stands", grace)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

// Run warms continuously until ctx is done. Blocks; call it in a goroutine.
func (wm *Warmer) Run(ctx context.Context) {
	log.Printf("warmer: %d targets every %s", len(wm.plan), wm.interval)
	for {
		start := time.Now()
		wm.Pass(ctx)
		if ctx.Err() != nil {
			return
		}
		wm.mu.Lock()
		wm.passes++
		wm.last = time.Now()
		wm.lastMS = time.Since(start).Milliseconds()
		n := wm.passes
		took := wm.lastMS
		wm.mu.Unlock()
		// One line per pass is too much noise at 25s; the first few are worth
		// having in the log, and after that /api/cache/stats is the answer.
		if n <= 3 {
			log.Printf("warmer: pass %d warmed %d targets in %dms", n, len(wm.plan), took)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wm.interval):
		}
	}
}

// Pass issues every target once.
func (wm *Warmer) Pass(ctx context.Context) {
	sem := make(chan struct{}, warmConcurrency)
	var wg sync.WaitGroup
	for _, target := range wm.plan {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(target string) {
			defer wg.Done()
			defer func() { <-sem }()
			wm.warmOne(ctx, target)
		}(target)
	}
	wg.Wait()
}

func (wm *Warmer) warmOne(ctx context.Context, target string) {
	ctx, cancel := context.WithTimeout(ctx, warmRequestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://warmer"+target, nil)
	if err != nil {
		return
	}
	// gzip, because every real browser sends it and the cache keys on the
	// negotiated encoding. Warming the identity variant too would double the
	// work to serve the handful of clients that cannot decode gzip, and those
	// still get the stale-while-revalidate path.
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("User-Agent", "mygnoscan-warmer")

	wm.handler.ServeHTTP(&discardWriter{header: http.Header{}}, req)
}

// Stats reports what the warmer has been doing, for /api/cache/stats.
type WarmerStats struct {
	Targets    int    `json:"targets"`
	Passes     int    `json:"passes"`
	LastPassAt string `json:"last_pass_at,omitempty"`
	LastPassMS int64  `json:"last_pass_ms"`
	IntervalS  int    `json:"interval_seconds"`
}

func (wm *Warmer) Stats() WarmerStats {
	wm.mu.Lock()
	defer wm.mu.Unlock()
	out := WarmerStats{
		Targets:    len(wm.plan),
		Passes:     wm.passes,
		LastPassMS: wm.lastMS,
		IntervalS:  int(wm.interval / time.Second),
	}
	if !wm.last.IsZero() {
		out.LastPassAt = wm.last.UTC().Format(time.RFC3339)
	}
	return out
}

// ParseWarmNetworks reads the -warm-networks flag: a comma-separated list, or
// "all" for every configured network.
func ParseWarmNetworks(raw string, configured []string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	if raw == "all" {
		return configured
	}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}
