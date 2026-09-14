package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"
)

// gnockpitURL is gnockpit's public, unauthenticated status endpoint — a
// real-time validator monitoring dashboard for gno.land
// (github.com/gnoverse/gnockpit). Its validator set is built from genesis
// data and live peer RPC queries, so it is keyed by *consensus* address —
// the same identity space as a block's proposer_address_raw, and disjoint
// from the *operator* address the valopers realm registers under (see the
// comment on proposerEl in frontend/index.html for why those two can never
// be joined). This is the one source that can label a proposer with a name.
// A var, not a const, so a test can point it at a fake server.
var gnockpitURL = "https://gnockpit.gno.land/api/status"

type gnockpitValidator struct {
	Name    string `json:"name"`
	Address string `json:"address"`
}

type gnockpitStatus struct {
	Validators []gnockpitValidator `json:"validators"`
}

// gnockpitCache holds the last successful fetch. gnockpit describes one
// chain (mainnet) regardless of which network mygnoscan is currently
// showing; an address from any other chain simply will not appear in it,
// which is a harmless miss rather than a wrong label, so the cache is not
// scoped per network.
var gnockpitCache = struct {
	mu       sync.Mutex
	monikers map[string]string
	fetched  time.Time
}{}

// gnockpitCacheTTL trades freshness for not hammering a third party this
// instance does not control on every /api/validators/monikers request.
// Validator monikers change rarely enough that a few minutes of staleness
// costs nothing a reader would notice.
const gnockpitCacheTTL = 5 * time.Minute

// FetchGnockpitMonikers returns consensus-address -> moniker, best-effort.
// Never errors outward: gnockpit is an optional enrichment from a service
// this instance does not run, so its absence must degrade to no monikers,
// not a broken page. Findings are cached across requests and networks.
func FetchGnockpitMonikers(ctx context.Context) map[string]string {
	gnockpitCache.mu.Lock()
	if gnockpitCache.monikers != nil && time.Since(gnockpitCache.fetched) < gnockpitCacheTTL {
		defer gnockpitCache.mu.Unlock()
		return gnockpitCache.monikers
	}
	gnockpitCache.mu.Unlock()

	monikers := fetchGnockpitMonikers(ctx)

	gnockpitCache.mu.Lock()
	defer gnockpitCache.mu.Unlock()
	if monikers != nil {
		gnockpitCache.monikers = monikers
		gnockpitCache.fetched = time.Now()
		return monikers
	}
	// A failed refresh keeps serving the last good map rather than dropping
	// every moniker because gnockpit had one bad moment — stale names beat
	// no names, and the next request tries again since fetched is unchanged.
	return gnockpitCache.monikers
}

func fetchGnockpitMonikers(ctx context.Context) map[string]string {
	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequestWithContext(ctx, "GET", gnockpitURL, nil)
	if err != nil {
		return nil
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil
	}
	var status gnockpitStatus
	if err := json.Unmarshal(body, &status); err != nil {
		return nil
	}
	out := make(map[string]string, len(status.Validators))
	for _, v := range status.Validators {
		if v.Address != "" && v.Name != "" {
			out[v.Address] = v.Name
		}
	}
	return out
}
