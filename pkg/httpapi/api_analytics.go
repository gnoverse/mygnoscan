package httpapi

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/moul/mygnoscan/pkg/config"
	"github.com/moul/mygnoscan/pkg/indexer"
	"github.com/moul/mygnoscan/pkg/store"
	"github.com/moul/mygnoscan/pkg/syncer"
)

func (a *API) HandleStats(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	stats, err := a.db.GetStats(network)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}

	// The live tip belongs to one chain. Asking for it without naming a network
	// used to return whichever chain clientFor happened to pick, so the home
	// page's block counter showed staging's height — a chain with seven
	// transactions — as if it were global. With several networks in play the
	// stored maximum stands instead: still a single number, but a deterministic
	// one derived from every configured chain rather than a coin flip.
	if client := a.clientFor(network); client != nil {
		height, err := client.LatestBlockHeight(r.Context())
		if err == nil {
			stats.LatestBlock = height
		}
	}

	JSONResponse(w, stats)
}

func (a *API) HandleGas(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)

	// Computed from stored transactions rather than by downloading the chain:
	// the numbers here are presented as all-time totals, so they cannot be
	// approximated from a recent window.
	stats, err := a.db.GetGasStats(network, 20)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}

	avgGasPerTx := 0
	if stats.TotalTxs > 0 {
		avgGasPerTx = stats.TotalGasUsed / stats.TotalTxs
	}

	JSONResponse(w, map[string]any{
		"total_txs":          stats.TotalTxs,
		"total_gas_used":     stats.TotalGasUsed,
		"total_gas_wanted":   stats.TotalGasWanted,
		"total_fees":         stats.TotalFees,
		"avg_gas_per_tx":     avgGasPerTx,
		"success_count":      stats.SuccessCount,
		"fail_count":         stats.FailCount,
		"total_source_bytes": a.db.TotalSourceBytes(network),
		"top_realms":         stats.TopRealms,
		"top_callers":        stats.TopCallers,
		"top_txs":            stats.TopTxs,
		// When the rollups behind these figures were built. Empty means they
		// were computed live, which happens before the first refresh.
		"computed_at": stats.ComputedAt,
	})
}

func (a *API) HandleAnalytics(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	analytics, err := a.db.GetAnalytics(network)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	JSONResponse(w, analytics)
}

// fetchBalance queries the gno.land RPC for bank balance.

func parseTimeseriesParams(r *http.Request) (days int, granularity string) {
	q := r.URL.Query()
	days, _ = strconv.Atoi(q.Get("days"))
	granularity = q.Get("granularity")

	if spec, ok := windowSpecs[strings.ToLower(q.Get("window"))]; ok {
		if days <= 0 {
			days = spec.days
		}
		if granularity == "" {
			granularity = spec.granularity
		}
	}

	if days <= 0 {
		days = 30
	}
	// The 365-day cap keeps hourly/daily/weekly bucket counts sane. The monthly
	// bucket exists precisely to span longer ranges, so it is exempt — but is
	// still bounded by allWindowDays.
	if days > 365 && granularity != "monthly" {
		days = 365
	}
	if days > allWindowDays {
		days = allWindowDays
	}

	switch granularity {
	case "hourly", "daily", "weekly", "monthly":
	default:
		granularity = "daily"
	}
	return
}

// Target point counts for granularityForSpan's bands, chosen so the bands
// are explainable without reference to any chain's current age (see that
// function's comment for why hardcoded day counts don't work here).
//
//   - targetHourlyMaxPoints: sized so an 8-day chain — the original bug
//     report — lands on hourly. resolveTimeseriesParams rounds a span up by
//     one day, so an 8-day history arrives here as 9 days (216 hourly
//     points); 250 clears that with room to spare while staying the same
//     order of magnitude as 7d's fixed 168 hourly points.
//   - targetDailyMaxPoints: sized in days directly (one point per day), set
//     to ~18 months so gno.land mainnet (~165 days as of 2026-08-14) has
//     roughly a year of headroom before this boundary, rather than the two
//     weeks a fixed 180-day ceiling gave it.
//   - targetWeeklyMaxPoints: sized in weeks, set to ~5 years so multi-year
//     spans still read as a weekly curve before falling back to monthly.

func (a *API) resolveTimeseriesParams(r *http.Request, network string) (int, string) {
	days, granularity := parseTimeseriesParams(r)

	q := r.URL.Query()
	if strings.ToLower(q.Get("window")) != "all" {
		return days, granularity
	}
	// Explicit values win, exactly as they do in parseTimeseriesParams. Compare
	// against the parsed value, not the raw query string: parseTimeseriesParams
	// treats unparseable days (e.g. "notanumber") as "not supplied" and falls
	// through to its own default, so garbage input here should fall through to
	// the sizing below too, rather than opting out of it into the old fixed
	// (allWindowDays, monthly) mapping.
	if explicitDays, err := strconv.Atoi(q.Get("days")); err == nil && explicitDays > 0 {
		return days, granularity
	}
	if q.Get("granularity") != "" {
		return days, granularity
	}

	start, ok, err := a.db.NetworkDataStart(network)
	if err != nil || !ok {
		// Nothing indexed, or the lookup failed: the fixed mapping is as good an
		// answer as any, since every window returns empty anyway.
		return days, granularity
	}

	spanDays := int(time.Since(start).Hours()/24) + 1
	if spanDays < 1 {
		spanDays = 1 // a start in the future means clock skew, not a negative range
	}
	if spanDays > allWindowDays {
		// A corrupt row (e.g. a year-1 timestamp) can otherwise produce a span of
		// tens of thousands of days, which fillBuckets would then iterate one
		// bucket at a time.
		spanDays = allWindowDays
	}
	return spanDays, granularityForSpan(spanDays)
}

func (a *API) HandleTimeSeriesTransactions(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	days, granularity := a.resolveTimeseriesParams(r, network)
	pts, err := a.db.GetTransactionTimeSeries(network, granularity, days)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	if pts == nil {
		pts = []store.TxTimePoint{}
	}
	JSONResponse(w, pts)
}

func (a *API) HandleTimeSeriesGas(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	days, granularity := a.resolveTimeseriesParams(r, network)
	pts, err := a.db.GetGasTimeSeries(network, granularity, days)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	if pts == nil {
		pts = []store.GasTimePoint{}
	}
	JSONResponse(w, pts)
}

func (a *API) HandleTimeSeriesCallers(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	days, granularity := a.resolveTimeseriesParams(r, network)
	pts, err := a.db.GetCallerTimeSeries(network, granularity, days)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	if pts == nil {
		pts = []store.CallerTimePoint{}
	}
	JSONResponse(w, pts)
}

// sanityResponse is the stored overview plus the two things that say whether
// *we* can read the chain, as opposed to whether the chain is running.
//
// Kept beside the overview rather than inside it: SanityLiveness describes a
// chain, and neither of these does. Sync health describes this process, and
// the endpoint describes our configuration.
type sanityResponse struct {
	*store.SanityOverview
	// Sync is keyed by network, and carries one entry per network that has
	// attempted a pass. Absent entirely when this process runs no sync loop.
	Sync map[string]syncer.NetworkHealth `json:"sync,omitempty"`
	// Indexers names, per network, the endpoint the pool is currently
	// selecting and the full set it may select from. A pool silently down to
	// its last working member is the failure this exists to make visible.
	Indexers map[string]endpointView `json:"indexers,omitempty"`
}

type endpointView struct {
	Active string   `json:"active"`
	Pool   []string `json:"pool"`
}

// endpointsFor reports which endpoint each network is being served by.
func (a *API) endpointsFor(networks []string) map[string]endpointView {
	out := map[string]endpointView{}
	for _, id := range networks {
		client := a.clientFor(id)
		if client == nil {
			continue
		}
		pool := client.Endpoints()
		if len(pool) == 0 {
			continue
		}
		out[id] = endpointView{Active: client.ActiveURL(), Pool: pool}
	}
	return out
}

func (a *API) HandleSanityOverview(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	ov, err := a.db.GetSanityOverview(network)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	resp := sanityResponse{SanityOverview: ov, Sync: a.syncHealth.Snapshot()}
	// Chain height, last block time and liveness always come from the live
	// indexer. They are also the figures that cannot be merged: there is no
	// such thing as the height of four chains at once.
	if network != "" {
		if client := a.clientFor(network); client != nil {
			live := livenessOf(r.Context(), client)
			ov.ChainHeight, ov.LastBlockTime = live.ChainHeight, live.LastBlockTime
			ov.SecondsSinceBlock, ov.IsAlive = live.SecondsSinceBlock, live.IsAlive
		}
		resp.SanityOverview = ov
		resp.Indexers = a.endpointsFor([]string{network})
		// One network selected, so the other chains' sync records are noise.
		if h, ok := resp.Sync[network]; ok {
			resp.Sync = map[string]syncer.NetworkHealth{network: h}
		} else {
			resp.Sync = nil
		}
		JSONResponse(w, resp)
		return
	}

	// All networks: report each chain rather than picking one. The top-level
	// liveness fields stay zero, because no single value could be right.
	type netLive struct {
		id   string
		live store.SanityLiveness
	}
	results := fanOut(r.Context(), a.networks, a.clients, a.health,
		func(ctx context.Context, n config.NetworkConfig, c *indexer.Client) (netLive, error) {
			return netLive{id: n.ID, live: livenessOf(ctx, c)}, nil
		})
	ov.ByNetwork = make(map[string]store.SanityLiveness, len(a.networks))
	for _, r := range results {
		ov.ByNetwork[r.id] = r.live
	}
	// fanOut drops networks it skipped — no client, or an open breaker — but
	// this is the page whose job is to report liveness, and a chain silently
	// missing from it is the one case a reader most needs to see. Fill the gaps
	// in as unreachable rather than letting them vanish.
	for _, n := range a.networks {
		if _, ok := ov.ByNetwork[n.ID]; !ok {
			ov.ByNetwork[n.ID] = store.SanityLiveness{}
		}
	}
	ids := make([]string, 0, len(a.networks))
	for _, n := range a.networks {
		ids = append(ids, n.ID)
	}
	resp.SanityOverview = ov
	resp.Indexers = a.endpointsFor(ids)
	JSONResponse(w, resp)
}

// livenessOf reads one chain's tip. An unreachable indexer reports Reachable
// false rather than a zero height, which would otherwise be indistinguishable
// from a chain sitting at genesis.

func (a *API) HandleTimeSeriesHealth(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	days, granularity := a.resolveTimeseriesParams(r, network)
	pts, err := a.db.GetHealthTimeSeries(network, granularity, days)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	if pts == nil {
		pts = []store.HealthTimePoint{}
	}
	JSONResponse(w, pts)
}

func (a *API) HandleTimeSeriesBlocks(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	days, granularity := a.resolveTimeseriesParams(r, network)
	pts, err := a.db.GetBlockTimeSeries(network, granularity, days)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	if pts == nil {
		pts = []store.BlockTimePoint{}
	}
	JSONResponse(w, pts)
}

func (a *API) HandleTimeSeriesActiveRolling(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	days, _ := a.resolveTimeseriesParams(r, network)
	// resolveTimeseriesParams' cap is 365 only when granularity != "monthly", and
	// this handler discards granularity entirely, so a request such as
	// ?days=3650&granularity=monthly (or window=all on an empty database, which
	// falls back to the fixed (allWindowDays, monthly) mapping) would otherwise
	// reach GetRollingActiveTimeSeries uncapped. The series is always daily, so
	// its own cap is independent of the granularity-aware one above.
	if days > store.RollingMaxDays {
		days = store.RollingMaxDays
	}
	pts, err := a.db.GetRollingActiveTimeSeries(network, days)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	if pts == nil {
		pts = []store.RollingActivePoint{}
	}
	JSONResponse(w, pts)
}

func (a *API) HandleGasPerTxHistogram(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	days, _ := a.resolveTimeseriesParams(r, network)
	bins, err := a.db.GetGasPerTxHistogram(network, days)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	if bins == nil {
		bins = []store.GasBin{}
	}
	JSONResponse(w, bins)
}
