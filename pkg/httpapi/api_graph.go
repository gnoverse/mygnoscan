package httpapi

import (
	"net/http"
	"strconv"
)

// The two edge-graph endpoints are per-chain and say so rather than answering
// for none.
//
// transfer_edges holds denominated values, which are never summed across
// chains, and an address is a different actor on each chain. The underlying
// queries filter on an exact network, so an empty one matches nothing: without
// this guard an all-networks request returns an empty graph, which reads as "no
// activity" rather than "ask a different question".
func (a *API) graphNetwork(w http.ResponseWriter, r *http.Request) (string, bool) {
	network := a.networkParam(r)
	if network == "" {
		jsonError(w, "graphs are per-chain: add ?network=", 400)
		return "", false
	}
	return network, true
}

func (a *API) HandleGraphTransfers(w http.ResponseWriter, r *http.Request) {
	network, ok := a.graphNetwork(w, r)
	if !ok {
		return
	}
	days, _ := a.resolveTimeseriesParams(r, network)
	topN, _ := strconv.Atoi(r.URL.Query().Get("topN"))
	minValue, _ := strconv.ParseInt(r.URL.Query().Get("min_value"), 10, 64)

	g, err := a.db.GetTransferGraph(network, days, topN, minValue, r.URL.Query().Get("ego"))
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	JSONResponse(w, g)
}

func (a *API) HandleGraphCallers(w http.ResponseWriter, r *http.Request) {
	network, ok := a.graphNetwork(w, r)
	if !ok {
		return
	}
	days, _ := a.resolveTimeseriesParams(r, network)
	topN, _ := strconv.Atoi(r.URL.Query().Get("topN"))
	minCalls, _ := strconv.Atoi(r.URL.Query().Get("min_calls"))

	g, err := a.db.GetCallerGraph(network, days, topN, minCalls)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	JSONResponse(w, g)
}

type activePathsResponse struct {
	Network string   `json:"network"`
	Window  string   `json:"window"`
	Paths   []string `json:"paths"`
}

// HandleGraphActive answers "which contracts has anyone touched lately", as a
// path set every graph on the site can filter itself against. Touched means
// called in the window or deployed in it, the same two tests the contracts map
// applies to its own nodes.
//
// It exists because the answer is not derivable from what the graph endpoints
// already return. /api/deps draws the dependency graph out of `dependencies`,
// a table that records what a package imports and nothing about whether the
// chain still runs it; the contracts map computes windowed call counts for its
// own nodes and cannot lend them to anyone else. One indexed DISTINCT serves
// both, and anything added later.
//
// Not routed through graphNetwork: this returns paths, not per-chain
// quantities, so an absent network is the union over every configured chain
// rather than an error. That matches /api/deps, which is the caller and which
// answers for the union too.
func (a *API) HandleGraphActive(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	windowName := r.URL.Query().Get("window")
	since, ok := parseContractWindow(windowName)
	if !ok {
		jsonError(w, "unknown window: use all, 24h, 7d, 30d or 90d", 400)
		return
	}
	if windowName == "" {
		windowName = "all"
	}

	paths, err := a.db.ActivePaths(network, since)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	JSONResponse(w, activePathsResponse{Network: network, Window: windowName, Paths: paths})
}
