package httpapi

import (
	"net/http"
	"strconv"
)

// The two graph endpoints are per-chain and say so rather than answering for
// none.
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
