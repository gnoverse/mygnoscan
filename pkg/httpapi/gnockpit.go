package httpapi

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

// GnockpitValidator is one entry of gnockpit's live consensus validator set,
// keyed by consensus address. Voting power arrives as a string in gnockpit's
// own JSON (large chains would overflow a JS number), so it is kept as one
// here too rather than parsed into something that could silently truncate.
type GnockpitValidator struct {
	Name        string `json:"name"`
	Address     string `json:"address"`
	VotingPower string `json:"voting_power"`
	// SPOF marks a validator whose loss alone would drop the remaining voting
	// power below the BFT quorum (2/3+1 of total) — consensus cannot proceed
	// without it.
	SPOF       bool `json:"spof"`
	Missed100  int  `json:"missed_100"`
	Missed24h  int  `json:"missed_24h"`
	AvgBlockMs int  `json:"avg_block_ms"`
}

type gnockpitStatus struct {
	Validators []GnockpitValidator `json:"validators"`
}

// gnockpitCache holds the last successful fetch. gnockpit describes one
// chain (mainnet) regardless of which network mygnoscan is currently
// showing; an address from any other chain simply will not appear in it,
// which is a harmless miss rather than a wrong label, so the cache is not
// scoped per network.
var gnockpitCache = struct {
	mu         sync.Mutex
	validators []GnockpitValidator
	fetched    time.Time
}{}

// gnockpitCacheTTL trades freshness for not hammering a third party this
// instance does not control on every /api/validators/monikers request.
// Validator monikers change rarely enough that a few minutes of staleness
// costs nothing a reader would notice.
const gnockpitCacheTTL = 5 * time.Minute

// fetchGnockpitStatus returns gnockpit's live validator set, cached. Never
// errors outward: gnockpit is an optional enrichment from a service this
// instance does not run, so its absence must degrade to nothing shown, not a
// broken page.
func fetchGnockpitStatus(ctx context.Context) []GnockpitValidator {
	gnockpitCache.mu.Lock()
	if gnockpitCache.validators != nil && time.Since(gnockpitCache.fetched) < gnockpitCacheTTL {
		defer gnockpitCache.mu.Unlock()
		return gnockpitCache.validators
	}
	gnockpitCache.mu.Unlock()

	validators := fetchGnockpitValidators(ctx)

	gnockpitCache.mu.Lock()
	defer gnockpitCache.mu.Unlock()
	if validators != nil {
		gnockpitCache.validators = validators
		gnockpitCache.fetched = time.Now()
		return validators
	}
	// A failed refresh keeps serving the last good set rather than dropping
	// it because gnockpit had one bad moment — stale data beats none, and the
	// next request tries again since fetched is unchanged.
	return gnockpitCache.validators
}

// FetchGnockpitMonikers returns consensus-address -> moniker, best-effort.
func FetchGnockpitMonikers(ctx context.Context) map[string]string {
	validators := fetchGnockpitStatus(ctx)
	if validators == nil {
		return nil
	}
	out := make(map[string]string, len(validators))
	for _, v := range validators {
		if v.Address != "" && v.Name != "" {
			out[v.Address] = v.Name
		}
	}
	return out
}

// FetchGnockpitValidators returns gnockpit's full live consensus validator
// set, best-effort — voting power, missed-block counts and average block
// time alongside the name/address FetchGnockpitMonikers also derives from it.
func FetchGnockpitValidators(ctx context.Context) []GnockpitValidator {
	return fetchGnockpitStatus(ctx)
}

// gnockpitClient talks to one external dashboard, over the shared pool.
var gnockpitClient = sharedClient(5 * time.Second)

func fetchGnockpitValidators(ctx context.Context) []GnockpitValidator {
	req, err := http.NewRequestWithContext(ctx, "GET", gnockpitURL, nil)
	if err != nil {
		return nil
	}
	resp, err := gnockpitClient.Do(req)
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
	return status.Validators
}
