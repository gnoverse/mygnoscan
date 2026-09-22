package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/moul/mygnoscan/pkg/gnostate"
)

// The state endpoint: what a realm actually holds, decoded.
//
// Everything here is read live from a node. None of it is in SQLite, because
// realm state is not in the transaction stream: the indexer sees that a call
// happened, not what the call left behind. The only way to know a realm's
// current state is to ask a node for it.
//
// ⚠️ This reads the *current* state and can never read a past one. Realm
// objects live in an unversioned store and are overwritten in place, so a node
// cannot answer for a past height even though `abci_query` accepts one (it
// returns the tip, silently). Historical state has to be captured going
// forward by the syncer; it cannot be backfilled from here.

const (
	// stateFetchWorkers is deliberately modest. This runs against a public RPC
	// that nobody is paying for, and the measured gain flattens well before
	// the point where it would look like a load test: 40 objects took 15.5s at
	// 1 worker, 4.0s at 4, 1.5s at 16, with per-request latency unchanged.
	stateFetchWorkers = 12

	// stateFetchDeadline bounds the whole resolve, and is a deadline rather
	// than a fetch count on purpose: what a reader waiting on a page cares
	// about is seconds, and the same fetch count costs wildly different time
	// on a realm whose object graph is deep versus wide.
	//
	// 12s against the server's 30s WriteTimeout leaves room for the render and
	// for a slow first byte. A realm that does not fit comes back partial and
	// says so, which is the honest outcome: measured on mainnet, the median
	// realm has 10 top-level object refs and 150 of 203 have 20 or fewer, so
	// the deadline only bites on the handful of outliers.
	stateFetchDeadline = 12 * time.Second

	// The ?full=1 budget, for a reader who has seen the partial badge and
	// asked for the rest. Wider and longer, but still inside the server's 30s
	// WriteTimeout.
	//
	// Opt-in rather than default on purpose. r/gnoland/blog needs ~1,250
	// objects, which is ~39s at the default width: making every reader of
	// every realm pay that, against an RPC nobody is funding, to serve the
	// handful of realms that need it is the wrong trade. The response cache
	// then keeps the full answer warm for everyone who follows.
	stateFullWorkers  = 32
	stateFullDeadline = 24 * time.Second
)

// StateResponse is one realm's decoded state.
type StateResponse struct {
	Path    string          `json:"path"`
	Network string          `json:"network"`
	Nodes   []gnostate.Node `json:"nodes"`
	// Partial is true when the reader is not looking at the whole state:
	// either the walk hit a limit or the fetch deadline expired. It drives a
	// badge, and it is the difference between an explorer and a screenshot.
	Partial bool   `json:"partial"`
	Reason  string `json:"partial_reason,omitempty"`
	// CanRetryFull tells the UI a wider read is available, so it can offer it
	// rather than leaving the reader with a partial view and no way forward.
	CanRetryFull bool  `json:"can_retry_full,omitempty"`
	Objects      int   `json:"objects"`
	NodeCount    int   `json:"node_count"`
	FetchedAt    int64 `json:"fetched_at"`
}

// stateFetcher resolves object and type references through a bounded worker
// pool, and gives up politely when its deadline passes.
type stateFetcher struct {
	ctx     context.Context
	rpcURL  string
	workers int

	mu       sync.Mutex
	objects  int
	timedOut bool
}

func (f *stateFetcher) Objects(oids []string) (map[string][]byte, error) {
	return f.fanout("vm/qobject_json", oids, true)
}

func (f *stateFetcher) Types(tids []string) (map[string][]byte, error) {
	return f.fanout("vm/qtype_json", tids, false)
}

// fanout fetches a whole round in parallel.
//
// A key that fails, or that is requested after the deadline, is simply absent
// from the result. That is not an error: gnostate renders an unresolved
// reference as a followable link, so a partial answer degrades the page rather
// than replacing it with one. Returning an error here would throw away every
// object that *did* resolve.
func (f *stateFetcher) fanout(queryPath string, keys []string, count bool) (map[string][]byte, error) {
	out := make(map[string][]byte, len(keys))
	if f.expired() {
		return out, nil
	}

	var mu sync.Mutex
	sem := make(chan struct{}, f.workers)
	var wg sync.WaitGroup
	for _, k := range keys {
		wg.Add(1)
		go func(k string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if f.expired() {
				return
			}
			body, err := fetchABCIQuery(f.ctx, f.rpcURL, queryPath, k)
			if err != nil || body == "" {
				return
			}
			mu.Lock()
			out[k] = []byte(body)
			mu.Unlock()
		}(k)
	}
	wg.Wait()

	if count {
		f.mu.Lock()
		f.objects += len(out)
		f.mu.Unlock()
	}
	return out, nil
}

// expired reports whether the deadline has passed, recording the first time it
// has so the response can say *why* it is partial.
func (f *stateFetcher) expired() bool {
	select {
	case <-f.ctx.Done():
		f.mu.Lock()
		f.timedOut = true
		f.mu.Unlock()
		return true
	default:
		return false
	}
}

// HandleState serves a realm's decoded state.
//
// Path convention matches /api/realm: without the `gno.land/` prefix, with the
// `r/` or `p/` segment.
func (a *API) HandleState(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	path := strings.TrimRight("gno.land/"+r.PathValue("path"), "/")

	rpcURL := a.rpcURLFor(network)
	if rpcURL == "" {
		jsonError(w, "no verified RPC endpoint for network "+network, 503)
		return
	}

	full := r.URL.Query().Get("full") == "1"
	deadline, workers := stateFetchDeadline, stateFetchWorkers
	if full {
		deadline, workers = stateFullDeadline, stateFullWorkers
	}
	ctx, cancel := context.WithTimeout(r.Context(), deadline)
	defer cancel()

	pkg, err := fetchABCIQuery(ctx, rpcURL, "vm/qpkg_json", path)
	if err != nil {
		// Three different answers the reader must be able to tell apart, and
		// the raw abci error tells them apart badly: it surfaces as
		// `map[@type:/vm.InvalidPkgPathError] ()`, which reads like a bug in
		// this explorer rather than a fact about the chain.
		//
		// A path the chain does not know is a 404 and says so plainly. Anything
		// else (a node that is down, a query that was rejected) is a 502: it is
		// our problem or the node's, not the reader's.
		if strings.Contains(err.Error(), "InvalidPkgPath") {
			jsonError(w, path+" is not a live package on "+network, 404)
			return
		}
		jsonError(w, "could not read state for "+path+": "+err.Error(), 502)
		return
	}
	if pkg == "" {
		// An empty body where the query succeeded: the package is live but
		// holds nothing the VM will export. Distinct from absent, and it
		// renders as an empty state rather than as an error.
		jsonError(w, path+" is live but exports no package state", 404)
		return
	}

	f := &stateFetcher{ctx: ctx, rpcURL: rpcURL, workers: workers}
	tree, err := gnostate.DecodePackageWith([]byte(pkg), f, gnostate.Limits{
		// Generous, because the deadline above is the real bound. These exist
		// to stop a pathological realm rather than to shape the common case.
		MaxDepth: 64, MaxNodes: 60000, MaxFetch: 20000,
	})
	if err != nil {
		jsonError(w, "decode state: "+err.Error(), 502)
		return
	}

	resp := StateResponse{
		Path:      path,
		Network:   network,
		Nodes:     tree.Nodes,
		Objects:   f.objects,
		NodeCount: tree.Stats.Nodes,
		FetchedAt: time.Now().Unix(),
	}
	switch {
	case f.timedOut:
		resp.Partial = true
		resp.Reason = "the fetch deadline passed before every reference resolved"
		// Only worth offering when there is a bigger budget left to spend.
		resp.CanRetryFull = !full
	case tree.Stats.Truncated:
		resp.Partial = true
		resp.Reason = "the walk hit its size limit"
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
