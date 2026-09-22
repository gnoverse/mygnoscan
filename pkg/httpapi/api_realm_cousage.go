package httpapi

import (
	"net/http"
	"strings"
)

// HandleRealmCoUsage answers "what else did the addresses that called this
// realm call", which is the contracts map's shared-caller edge kind asked from
// one realm rather than over the whole chain.
//
// Per chain, not per union, for the reason the contracts map documents: 193
// package paths exist on more than one network, and an address is a different
// actor on each. An absent network resolves to the first configured one and the
// response says which it picked, rather than blending two chains' callers into
// a relationship neither of them has.
func (a *API) HandleRealmCoUsage(w http.ResponseWriter, r *http.Request) {
	network := a.singleNetwork(r)
	path := strings.TrimRight("gno.land/"+r.PathValue("path"), "/")

	windowName := r.URL.Query().Get("window")
	since, ok := parseContractWindow(windowName)
	if !ok {
		jsonError(w, "unknown window: use all, 24h, 7d, 30d or 90d", 400)
		return
	}
	if windowName == "" {
		windowName = "all"
	}

	limit := clampParam(r, "limit", 0, 1000)
	usage, err := a.db.RealmCoUsagePartners(network, path, since, limit)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	usage.Window = windowName
	JSONResponse(w, usage)
}
