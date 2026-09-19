package httpapi

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/moul/mygnoscan/pkg/store"
)

// The contracts map: one bubble per deployed package, and lines between them.
//
// Two endpoints rather than one. Every metric travels with the nodes on first
// paint, because switching what sizes a bubble is the main thing a reader does
// here and it should not cost a round trip. Edges are fetched separately
// because the two kinds are different queries and most readers never switch
// away from the default, so shipping both would double the payload for a
// control nobody touched.

const (
	// defaultContractEdgeLimit and maxContractEdgeLimit bound how many edges
	// one request can produce. The default is what a force layout still lays
	// out legibly at a thousand nodes; the maximum is what keeps a crafted
	// query string from asking the server to build every pair on the chain.
	defaultContractEdgeLimit = 1500
	maxContractEdgeLimit     = 20000

	// defaultContractMinWeight: an edge needs two addresses in common. One
	// address touching two contracts once is a coincidence, not a relationship,
	// and at min=1 the map is mostly coincidences.
	defaultContractMinWeight = 2

	// defaultContractMaxFanout excludes addresses that touched more than this
	// many contracts from edge generation. A bot that called fifty contracts
	// links all of them to each other, which is how this picture turns to mud.
	// Its calls still count toward every node's metrics.
	defaultContractMaxFanout = 30
	maxContractMaxFanout     = 500
)

type contractsMapResponse struct {
	Network string               `json:"network"`
	Window  string               `json:"window"`
	Nodes   []store.ContractNode `json:"nodes"`
}

type contractsEdgesResponse struct {
	Network string               `json:"network"`
	Kind    string               `json:"kind"`
	Window  string               `json:"window"`
	Min     int                  `json:"min"`
	Limit   int                  `json:"limit"`
	Fanout  int                  `json:"max_fanout"`
	Edges   []store.ContractEdge `json:"edges"`
}

// singleNetwork resolves the network for an endpoint that, unlike most, may
// not answer for "all".
//
// Two kinds of endpoint need this. The contracts map, because a bubble is
// identified by its path and 193 paths exist on more than one chain: an
// all-networks map would draw one bubble carrying two chains' traffic and one
// edge joining callers who never shared anything. And chain configuration,
// because there is no such thing as the parameters of three chains at once.
//
// In both cases an absent or "all" network resolves to the first configured one
// and the response says which it picked, rather than blending.
func (a *API) singleNetwork(r *http.Request) string {
	if n := a.networkParam(r); n != "" {
		return n
	}
	if len(a.networks) > 0 {
		return a.networks[0].ID
	}
	return ""
}

// parseContractWindow maps the window names the UI offers to a cutoff.
//
// Unknown values are rejected rather than treated as all time: a typo that
// silently widens the window produces a plausible map of the wrong period,
// which is worse than an error because nothing about it looks wrong.
func parseContractWindow(s string) (time.Time, bool) {
	switch s {
	case "", "all":
		return time.Time{}, true
	case "24h":
		return time.Now().UTC().Add(-24 * time.Hour), true
	case "7d":
		return time.Now().UTC().AddDate(0, 0, -7), true
	case "30d":
		return time.Now().UTC().AddDate(0, 0, -30), true
	}
	return time.Time{}, false
}

// clampParam reads a positive integer from the query string, falling back to
// def when absent or unparseable and to max when over.
func clampParam(r *http.Request, name string, def, max int) int {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 {
		return def
	}
	if v > max {
		return max
	}
	return v
}

func (a *API) HandleContractsMap(w http.ResponseWriter, r *http.Request) {
	network := a.singleNetwork(r)
	windowName := r.URL.Query().Get("window")
	since, ok := parseContractWindow(windowName)
	if !ok {
		http.Error(w, "unknown window: use all, 24h, 7d or 30d", http.StatusBadRequest)
		return
	}
	if windowName == "" {
		windowName = "all"
	}

	nodes, err := a.db.ContractMapNodes(network, since)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	a.stampParked(r.Context(), network, nodes)

	JSONResponse(w, contractsMapResponse{Network: network, Window: windowName, Nodes: nodes})
}

func (a *API) HandleContractsEdges(w http.ResponseWriter, r *http.Request) {
	network := a.singleNetwork(r)
	windowName := r.URL.Query().Get("window")
	since, ok := parseContractWindow(windowName)
	if !ok {
		http.Error(w, "unknown window: use all, 24h, 7d or 30d", http.StatusBadRequest)
		return
	}
	if windowName == "" {
		windowName = "all"
	}

	kind := r.URL.Query().Get("kind")
	if kind == "" {
		kind = "callers"
	}

	resp := contractsEdgesResponse{Network: network, Kind: kind, Window: windowName}
	switch kind {
	case "imports":
		// Imports are a property of the deployed source, not of traffic, so
		// they ignore the window and every bound: the graph is as big as the
		// chain's dependency graph and no bigger.
		edges, err := a.db.ContractImportEdges(network)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		resp.Edges = edges
	case "callers":
		resp.Min = clampParam(r, "min", defaultContractMinWeight, 1000)
		resp.Limit = clampParam(r, "limit", defaultContractEdgeLimit, maxContractEdgeLimit)
		resp.Fanout = clampParam(r, "maxFanout", defaultContractMaxFanout, maxContractMaxFanout)
		edges, err := a.db.ContractCallerEdges(network, since, resp.Min, resp.Limit, resp.Fanout)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		resp.Edges = edges
	default:
		http.Error(w, "unknown kind: use callers or imports", http.StatusBadRequest)
		return
	}

	JSONResponse(w, resp)
}

// stampParked marks the nodes currently sitting in the chain's inert queue:
// submitted and stored, but never enabled, and therefore invisible to every
// liveness probe on chain. A map that quietly omitted them would repeat the
// same silence.
//
// Live RPC state rather than anything SQL can answer, which is why it is
// stamped here instead of being selected in the store, the same split
// PackageInfo.Status follows. Best-effort for the same reason: a chain whose
// queue cannot be read right now leaves every node unstamped rather than
// failing the whole map.
func (a *API) stampParked(ctx context.Context, network string, nodes []store.ContractNode) {
	parked := a.parkedPaths(ctx, network)
	if len(parked) == 0 {
		return
	}
	for i := range nodes {
		if parked[nodes[i].Path] {
			nodes[i].Parked = true
		}
	}
}

// parkedPaths reads one network's inert queue into a set.
func (a *API) parkedPaths(ctx context.Context, network string) map[string]bool {
	rpcURL := a.rpcURLFor(network)
	if rpcURL == "" {
		return nil
	}
	queue, err := FetchInertQueue(ctx, network, rpcURL)
	if err != nil || len(queue) == 0 {
		return nil
	}
	parked := make(map[string]bool, len(queue))
	for _, q := range queue {
		parked[q.Path] = true
	}
	return parked
}
