package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/moul/mygnoscan/pkg/analyzer"
	"github.com/moul/mygnoscan/pkg/config"
	"github.com/moul/mygnoscan/pkg/indexer"
	"github.com/moul/mygnoscan/pkg/store"
	"github.com/moul/mygnoscan/pkg/syncer"
)

type API struct {
	db       *store.DB
	clients  map[string]*indexer.Client
	networks []config.NetworkConfig
	analyzer *analyzer.Analyzer
	health   *healthTracker

	// syncHealth is how the sanity page answers "are our sync passes
	// succeeding", which chain liveness cannot: a chain can be producing
	// blocks perfectly while every query we send about it fails. Nil in the
	// tools and tests that run no sync loop, and nil-safe for that reason.
	syncHealth *syncer.Registry

	// rpcOK records, per network, whether its RPC has been confirmed to serve
	// the same chain as its indexer. Written by a background re-check and read
	// by every request that wants a balance, hence the mutex.
	//
	// Absent means unverified, which is treated as unusable — see rpcURLFor.
	rpcMu   sync.RWMutex
	rpcPick map[string]string
}

func NewAPI(db *store.DB, clients map[string]*indexer.Client, networks []config.NetworkConfig, analyzer *analyzer.Analyzer) *API {
	return &API{
		db:       db,
		clients:  clients,
		networks: networks,
		analyzer: analyzer,
		health:   newHealthTracker(),
	}
}

// SetSyncHealth hands the API the registry the sync goroutines write to.
//
// A setter rather than a constructor argument: the API is built before the
// sync loops start, and every test and tool that constructs one runs no sync
// at all.
func (a *API) SetSyncHealth(r *syncer.Registry) { a.syncHealth = r }

func JSONResponse(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(emptyNotNull(data))
}

// emptyNotNull turns a nil slice or map into an empty one.
//
// Go marshals a nil slice as `null`, and every list view in the frontend
// iterates what it gets — `for (const r of rows)` throws on null, so a query
// that legitimately matched nothing rendered as a broken page rather than an
// empty one. /api/search did exactly that for any query with no hits.
//
// Individual handlers had been guarding this one at a time as each was noticed.
// Doing it where the response is written covers the ones nobody has hit yet:
// most list endpoints only look safe because the chain they were tried against
// happened to have data.

func emptyNotNull(data any) any {
	v := reflect.ValueOf(data)
	switch v.Kind() {
	case reflect.Slice:
		if v.IsNil() {
			return []any{}
		}
	case reflect.Map:
		if v.IsNil() {
			return map[string]any{}
		}
	}
	return data
}

func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// stampPackageTimes fetches block times for each unique block height in parallel.

const (
	breakerThreshold = 2
	breakerCooldown  = 60 * time.Second
)

// healthTracker trips a per-network breaker after repeated failures.

func (h *healthTracker) shouldSkip(id string) bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.state[id]
	return s != nil && time.Now().Before(s.skipUntil)
}

// record updates the breaker after an attempt.

func (h *healthTracker) record(id string, err error) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.state[id]
	if s == nil {
		s = &netHealth{}
		h.state[id] = s
	}
	if err == nil {
		if s.failures >= breakerThreshold {
			log.Printf("[%s] reachable again, resuming", id)
		}
		s.failures, s.skipUntil = 0, time.Time{}
		return
	}
	s.failures++
	if s.failures >= breakerThreshold {
		if s.skipUntil.IsZero() || time.Now().After(s.skipUntil) {
			log.Printf("[%s] unreachable, pausing for %s: %v", id, breakerCooldown, err)
		}
		s.skipUntil = time.Now().Add(breakerCooldown)
	}
}

// fanOut queries every configured network concurrently, returning results in
// configured order. Networks that error or time out are skipped: a merged view
// is best-effort by nature, and one bad network must not take the others down
// with it.
//
// Sequential fan-out made an all-networks response cost the sum of every
// network's latency, which is also what made adding an unreachable network
// dangerous.

func fanOut[T any](
	ctx context.Context,
	networks []config.NetworkConfig,
	clients map[string]*indexer.Client,
	health *healthTracker,
	fn func(ctx context.Context, net config.NetworkConfig, client *indexer.Client) (T, error),
) []T {
	type slot struct {
		val T
		ok  bool
	}
	slots := make([]slot, len(networks))

	var wg sync.WaitGroup
	for i, n := range networks {
		client := clients[n.ID]
		if client == nil {
			continue
		}
		if health.shouldSkip(n.ID) {
			continue
		}
		wg.Add(1)
		go func(i int, n config.NetworkConfig, c *indexer.Client) {
			defer wg.Done()
			nctx, cancel := context.WithTimeout(ctx, perNetworkDeadline)
			defer cancel()
			v, err := fn(nctx, n, c)
			// A miss is an answer, not a failure. Every unfiltered lookup of a
			// hash, block or realm asks all networks and expects all but one to
			// say no; charging those to the breaker takes healthy chains
			// offline for browsing the site normally.
			if errors.Is(err, indexer.ErrNotFound) {
				health.record(n.ID, nil)
			} else {
				health.record(n.ID, err)
			}
			if err != nil {
				return
			}
			slots[i] = slot{val: v, ok: true}
		}(i, n, client)
	}
	wg.Wait()

	out := make([]T, 0, len(slots))
	for _, s := range slots {
		if s.ok {
			out = append(out, s.val)
		}
	}
	return out
}

// networkParam reads ?network from request. Returns "" for "all" (no filter), or specific network ID.

func (a *API) networkParam(r *http.Request) string {
	n := r.URL.Query().Get("network")
	if n == "" || n == "all" {
		return ""
	}
	return n
}

// RejectUnknownNetwork turns `?network=` naming an unconfigured network into a
// 404, for API routes only.
//
// Left alone, an unconfigured network is not an error anywhere: handlers pass
// the string straight through to the database, which still holds rows for every
// network ever synced, and clientFor falls back to an arbitrary client for the
// parts that need a live chain. A retired testnet therefore keeps answering with
// stale local rows stamped with an unrelated chain's height — worse than a 404,
// because it looks like data.
//
// Non-API routes are left alone so the SPA still loads on a stale bookmark and
// can say so itself, rather than the browser being handed a JSON error.

func RejectUnknownNetwork(networks []config.NetworkConfig, next http.Handler) http.Handler {
	known := make(map[string]bool, len(networks))
	for _, n := range networks {
		known[n.ID] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := r.URL.Query().Get("network")
		if strings.HasPrefix(r.URL.Path, "/api/") && n != "" && n != "all" && !known[n] {
			jsonError(w, "network not found", 404)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// clientFor returns the Client for a network, or nil when there is no
// single right answer.
//
// It used to fall back to an arbitrary entry of a.clients — Go map iteration
// order — so "all networks" silently became "one chain, chosen at random". That
// is not a degraded answer, it is a wrong one that looks right: the home page
// reported staging's block height (a chain with seven transactions) as the
// global figure, /api/govdao returned null, /api/allevents served a single
// chain's events, and the sanity dashboard presented one chain's liveness as
// everyone's.
//
// Callers that need a live chain must either have a network or fan out.

func (a *API) clientFor(network string) *indexer.Client {
	if network == "" {
		return nil
	}
	return a.clients[network]
}

// rpcURLFor returns the RPC URL for a network, but only once that RPC has been
// confirmed to serve the same chain as the network's indexer.
//
// Unverified is treated as unusable rather than as probably-fine: the cost of
// withholding a balance is a missing figure, and the cost of trusting a
// mismatched one is a number from a different chain shown beside this chain's
// history. See VerifyRPCChains.

func (a *API) rpcURLFor(network string) string {
	for _, n := range a.networks {
		if network == "" || n.ID == network {
			if url := a.rpcVerified(n.ID); url != "" {
				return url
			}
		}
	}
	return ""
}

const (
	defaultTxs = 500
	// 5000 was still eight seconds against a busy chain — close enough to the
	// server's write timeout to fail under load. 2000 matches maxEventTxs and
	// lands around three.
	maxTxs = 2000
)

const (
	defaultEventTxs = 200
	maxEventTxs     = 2000
)

// eventTxLimit reads the caller's `limit`, falling back to a default and capped
// at a maximum. Every event view goes through it so none of them can ask the
// indexer for a chain's entire history.

func newerFirst(timeA, timeB string, heightA, heightB int) bool {
	if timeA != "" && timeB != "" {
		return timeA > timeB
	}
	if timeA != timeB {
		return timeA != ""
	}

	return heightA > heightB
}

// sortTransactionsByTime orders newest first.
//
// Heights are per-chain and not comparable: gnoland1 sits near 3.1M while
// sapphire is near 400k, so a height comparison lets the chain with the largest
// numbers win every time. Dropped into a merged list that is then truncated to a
// page, that does not merely mis-order — it deletes a chain. It did exactly that
// to sapphire's events before the block-time stamping was added.
//
// So: timestamp first, then rows that have one ahead of rows that do not, and
// only then height — by which point both rows are undated and any order is a
// guess, but at least a stable one.

const (
	defaultAccounts = 100
	maxAccounts     = 500
)

// HandleLabels serves display names for addresses, derived from on-chain data.
//
// Kept separate from the rows that mention an address so it can be fetched once
// and applied everywhere, rather than repeating a label on every transaction in
// a list.

func (a *API) HandleLabels(w http.ResponseWriter, r *http.Request) {
	labels, err := a.db.DerivedAddressLabels(a.networkParam(r))
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	JSONResponse(w, labels)
}

// maxWatchItems bounds a watchlist request. Each item is a handful of indexed
// counts, so the cost is linear — but the parameters come from a URL and this is
// the one endpoint whose size a caller controls directly.

func (a *API) HandleGovDAO(w http.ResponseWriter, r *http.Request) {
	calls, err := a.db.GovDAOCalls(a.networkParam(r), eventTxLimit(r))
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	JSONResponse(w, calls)
}

// HandleGovDAOOverview serves the /govdao landing page: the live proposal
// list plus the memberstore's tiers and members, read straight from gov/dao's
// own Render() output over RPC (see govdao.go) rather than reimplemented
// against its storage — the realm is the source of truth for its own rules,
// including ones mygnoscan does not know about (a tier threshold changing,
// say).
// HandleInertQueue serves the current parked-package queue: every path
// vm/qinertpaths reports, enriched with each one's own vm/qpkgmeta_json
// metadata (creator, submission height, why it is stuck). See inert.go.

func buildLifecycleHistory(txs []indexer.Transaction) []InertLifecycleEvent {
	var out []InertLifecycleEvent
	for _, tx := range txs {
		for _, m := range tx.Messages {
			switch m.Value.Typename {
			case "MsgAddPackage":
				out = append(out, InertLifecycleEvent{
					Kind: "submitted", TxHash: tx.Hash, BlockHeight: tx.BlockHeight, BlockTime: tx.BlockTime,
					PkgPath: pkgPathOf(m.Value), Actor: m.Value.Creator,
				})
			case "MsgEnablePackage":
				out = append(out, InertLifecycleEvent{
					Kind: "enabled", TxHash: tx.Hash, BlockHeight: tx.BlockHeight, BlockTime: tx.BlockTime,
					PkgPath: m.Value.PkgPath, Actor: m.Value.Approver, SubmittedHeight: m.Value.PkgHeight,
				})
			case "MsgRejectPackage":
				out = append(out, InertLifecycleEvent{
					Kind: "rejected", TxHash: tx.Hash, BlockHeight: tx.BlockHeight, BlockTime: tx.BlockTime,
					PkgPath: m.Value.PkgPath, Actor: m.Value.Sender,
				})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].BlockHeight < out[j].BlockHeight })
	return out
}

func pkgPathOf(v indexer.MessageValue) string {
	if v.Package != nil {
		return v.Package.Path
	}
	return ""
}

// secondsBetween parses two RFC3339 timestamps and returns b-a in seconds.
// The bool is false when either fails to parse, so a caller can leave the
// wait time unset rather than report a nonsense duration.

func secondsBetween(a, b string) (float64, bool) {
	ta, err1 := time.Parse(time.RFC3339, a)
	tb, err2 := time.Parse(time.RFC3339, b)
	if err1 != nil || err2 != nil {
		return 0, false
	}
	return tb.Sub(ta).Seconds(), true
}

func (a *API) HandleGovDAOOverview(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	rpcURL := a.rpcURLFor(network)
	overview := FetchGovDAOOverview(r.Context(), network, rpcURL)
	a.enrichGovDAOProposals(r.Context(), network, rpcURL, overview.Proposals)
	JSONResponse(w, overview)
}

// enrichGovDAOProposals fills in each summary's vote percentages (from its
// own cached detail render — one RPC round trip per proposal, but every
// result is independently cached for govDAOCacheTTL and gov/dao's proposal
// count is small, so this stays cheap) and its approximate creation/last-
// activity dates (from one bulk indexer query, bucketed by proposal ID —
// see govDAORelatedCalls for why this cannot come from the local calls
// table). Mutates in place; best-effort, so a failure here just leaves a
// row's extra fields blank rather than failing the whole overview.

func (a *API) enrichGovDAOProposals(ctx context.Context, network, rpcURL string, proposals []GovDAOProposalSummary) {
	if len(proposals) == 0 {
		return
	}
	var wg sync.WaitGroup
	for i := range proposals {
		wg.Add(1)
		go func(p *GovDAOProposalSummary) {
			defer wg.Done()
			detail := FetchGovDAOProposal(ctx, network, rpcURL, p.ID)
			p.YesPercent = detail.YesPercent
			p.NoPercent = detail.NoPercent
			p.AbstainPercent = detail.AbstainPercent
			p.AuthorAddress = resolveGnoUsernameCached(ctx, rpcURL, p.Author)
		}(&proposals[i])
	}

	byID := make(map[int]*GovDAOProposalSummary, len(proposals))
	for i := range proposals {
		byID[proposals[i].ID] = &proposals[i]
	}
	if txs, ok := a.fetchGovDAOTransactions(ctx, network); ok {
		for _, tx := range txs {
			for _, m := range tx.Messages {
				v := m.Value
				if v.Typename != "MsgCall" || len(v.Args) == 0 {
					continue
				}
				id, err := strconv.Atoi(v.Args[0])
				if err != nil {
					continue
				}
				p, ok := byID[id]
				if !ok {
					continue
				}
				if p.CreatedHeight == 0 || tx.BlockHeight < p.CreatedHeight {
					p.CreatedHeight, p.CreatedTime = tx.BlockHeight, tx.BlockTime
				}
				if tx.BlockHeight > p.LastActivityHeight {
					p.LastActivityHeight, p.LastActivityTime = tx.BlockHeight, tx.BlockTime
				}
			}
		}
	}

	wg.Wait()
}

// HandleGovDAOProposal serves one proposal's detail page: the parsed render
// (description, executor, status, vote percentages, per-address votes) plus
// two independently sourced "how did this happen" trails — related MsgCalls
// (vote/execute transactions naming this proposal ID, found live on the
// indexer since the locally synced calls table does not keep arguments) and
// related MsgRuns (maketx-run scripts that plausibly created it, found by
// searching locally synced script source).

func (a *API) HandleGovDAOProposal(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id < 0 {
		jsonError(w, "invalid proposal id", 400)
		return
	}
	rpcURL := a.rpcURLFor(network)
	detail := FetchGovDAOProposal(r.Context(), network, rpcURL, id)
	detail.RelatedCalls = a.govDAORelatedCalls(r.Context(), network, id)
	if runs, err := a.db.GovDAORelatedMsgRuns(network, detail.ExecutorPkgPath); err == nil {
		detail.RelatedMsgRuns = runs
	}
	detail.AuthorAddress = resolveGnoUsernameCached(r.Context(), rpcURL, detail.Author)
	for i := range detail.Votes {
		detail.Votes[i].VoterAddress = resolveGnoUsernameCached(r.Context(), rpcURL, detail.Votes[i].Voter)
	}
	JSONResponse(w, detail)
}

// govDAORelatedCalls finds MsgCalls into gov/dao naming this proposal ID as
// their first argument — votes, execution, and (if ever issued directly
// rather than via a MsgRun script) creation. Live on the indexer, not the
// local calls table: the syncer never persists call arguments, so this is
// the only place that ID lives.
//
// Matching on "args[0] == id" without also pinning the function name is
// deliberate: gov/dao's set of proposal-related functions is not something
// this file should have to keep in sync with the realm's own source, and a
// false positive here is just an unrelated call briefly listed for a
// human to judge, not a wrong balance or a broken page.

func (a *API) govDAORelatedCalls(ctx context.Context, network string, id int) []GovDAORelatedCall {
	txs, ok := a.fetchGovDAOTransactions(ctx, network)
	if !ok {
		return nil
	}
	idStr := strconv.Itoa(id)
	var out []GovDAORelatedCall
	for _, tx := range txs {
		for _, m := range tx.Messages {
			v := m.Value
			if v.Typename != "MsgCall" || len(v.Args) == 0 || v.Args[0] != idStr {
				continue
			}
			out = append(out, GovDAORelatedCall{
				TxHash:      tx.Hash,
				BlockHeight: tx.BlockHeight,
				BlockTime:   tx.BlockTime,
				Caller:      v.Caller,
				Func:        v.Func,
				Success:     tx.Success,
			})
		}
	}
	return out
}

// fetchGovDAOTransactions fetches every gov/dao MsgCall, with block times
// filled in (see stampBlockTimes) so callers can show a date without a
// second round trip. The bool return is whether an indexer client exists for
// the network at all, distinct from a zero-length result — a network with a
// client but genuinely no gov/dao activity should not look identical to one
// this instance cannot reach.

func (a *API) HandleDeps(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	path := "gno.land/" + r.PathValue("path")
	path = strings.TrimRight(path, "/")
	direction := r.URL.Query().Get("dir") // "imports" or "dependents"

	var graph map[string][]string
	var err error

	switch direction {
	case "dependents":
		graph, err = a.db.GetReverseGraph(network, path)
	default:
		graph, err = a.db.GetDependencyGraph(network, path)
	}

	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	JSONResponse(w, graph)
}

func fetchBalance(ctx context.Context, addr, rpcURL string) string {
	if rpcURL == "" {
		return ""
	}
	url := fmt.Sprintf("%s/abci_query?path=%%22bank/balances/%s%%22&data=0x", rpcURL, addr)
	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return ""
	}
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return ""
	}
	var result struct {
		Result struct {
			Response struct {
				ResponseBase struct {
					Data string `json:"Data"`
				} `json:"ResponseBase"`
			} `json:"response"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return ""
	}
	data := result.Result.Response.ResponseBase.Data
	if data == "" {
		return ""
	}
	decoded, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return ""
	}
	// Strip quotes: "754954090ugnot" -> 754954090ugnot
	return strings.Trim(string(decoded), "\"")
}

// allWindowDays bounds the "all" window. gno.land's genesis is comfortably
// inside this, and a finite bound keeps the monthly bucket loop terminating.

const allWindowDays = 3650

// windowSpecs maps a spec §8 window name onto the (days, granularity) pair the
// time-series queries already take. See the design doc's window table.

var windowSpecs = map[string]struct {
	days        int
	granularity string
}{
	"24h": {1, "hourly"},
	"7d":  {7, "hourly"},
	"30d": {30, "daily"},
	"90d": {90, "daily"},
	"1y":  {365, "weekly"},
	"all": {allWindowDays, "monthly"},
}

// parseTimeseriesParams resolves the time range for a time-series request.
// ?window= is the current contract; ?days= and ?granularity= predate it and
// still work, and win when both are supplied.

const (
	targetHourlyMaxPoints = 250 // ~10.4 days of hourly points
	targetDailyMaxPoints  = 550 // ~18 months of daily points
	targetWeeklyMaxPoints = 260 // ~5 years of weekly points
)

// granularityForSpan picks a bucket that keeps an "all" series readable, by
// keeping each candidate granularity's point count under its target. The
// targets are expressed as point counts, not day counts, because a day-count
// boundary is really a stand-in for "how many points does this produce" —
// naming the point count directly means the boundary doesn't need re-tuning
// as a specific chain (e.g. gno.land mainnet) ages past whatever day count
// happened to work when it was chosen.

func granularityForSpan(days int) string {
	switch {
	case days*24 <= targetHourlyMaxPoints:
		return "hourly"
	case days <= targetDailyMaxPoints:
		return "daily"
	case days/7 <= targetWeeklyMaxPoints:
		return "weekly"
	default:
		return "monthly"
	}
}

// resolveTimeseriesParams is parseTimeseriesParams plus the one thing a pure
// function cannot do: size the "all" window against the data that exists.
//
// windowSpecs maps "all" to a fixed (allWindowDays, monthly) because the window
// table assumed a chain with years of history. No gno chain is that old —
// mainnet is ~165 days — so a fixed monthly bucket flattens the whole history
// into a handful of points, and on a chain younger than a calendar month into
// exactly one, which draws as a lone dot instead of a curve. Measuring the
// network's real span fixes both the bucket and the range, the latter also
// sparing fillBuckets ~120 dead leading buckets on every "all" request.

func livenessOf(ctx context.Context, client *indexer.Client) store.SanityLiveness {
	blocks, err := client.GetRecentBlocks(ctx, 1)
	if err != nil || len(blocks) == 0 {
		return store.SanityLiveness{}
	}

	b := blocks[0]
	live := store.SanityLiveness{ChainHeight: b.Height, LastBlockTime: b.Time, Reachable: true}
	if t, err := time.Parse(time.RFC3339, b.Time); err == nil {
		live.SecondsSinceBlock = int(time.Since(t).Seconds())
		live.IsAlive = live.SecondsSinceBlock < 120
	}
	return live
}

const funcHeatmapDays = 14

func (a *API) HandleActivityHeatmap(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	days, _ := a.resolveTimeseriesParams(r, network)
	cells, err := a.db.GetActivityHeatmap(network, days)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	if cells == nil {
		cells = []store.ActivityCell{}
	}
	JSONResponse(w, cells)
}

func (a *API) HandleFunctionCallHeatmap(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	realm := r.URL.Query().Get("realm")
	if realm == "" {
		jsonError(w, "realm is required", 400)
		return
	}
	cells, err := a.db.GetFunctionCallHeatmap(network, realm, funcHeatmapDays)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	if cells == nil {
		cells = []store.FuncCallCell{}
	}
	JSONResponse(w, cells)
}

// RegisterRoutes wires every API endpoint onto a mux.
//
// These lived inline in run(), which meant nothing could reach them: a test
// calling a handler directly gets a request with no path values, so `{hash}`
// and `{height}` arrive empty and the handler rejects its own input. Route
// patterns are part of the endpoint's behaviour and belong somewhere testable.
//
// Endpoints that close over build-time values (/api/version) or over process
// state (/api/live, the SPA) stay in run().

func (a *API) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/stats", a.HandleStats)
	mux.HandleFunc("GET /api/realms", a.HandleRealms)
	mux.HandleFunc("GET /api/realm/{path...}", a.HandleRealm)
	mux.HandleFunc("GET /api/packages", a.HandlePackages)
	mux.HandleFunc("GET /api/tx/{hash}", a.HandleTx)
	mux.HandleFunc("GET /api/txs", a.HandleTxs)
	mux.HandleFunc("GET /api/address/{addr}", a.HandleAddress)
	mux.HandleFunc("GET /api/search", a.HandleSearch)
	mux.HandleFunc("GET /api/deps/{path...}", a.HandleDeps)
	mux.HandleFunc("GET /api/analytics", a.HandleAnalytics)
	mux.HandleFunc("GET /api/timeseries/transactions", a.HandleTimeSeriesTransactions)
	mux.HandleFunc("GET /api/timeseries/packages", a.HandleTimeSeriesPackages)
	mux.HandleFunc("GET /api/timeseries/callers", a.HandleTimeSeriesCallers)
	mux.HandleFunc("GET /api/timeseries/gas", a.HandleTimeSeriesGas)
	mux.HandleFunc("GET /api/timeseries/storage", a.HandleTimeSeriesStorage)
	mux.HandleFunc("GET /api/timeseries/storage/realms", a.HandleStorageRealms)
	mux.HandleFunc("GET /api/timeseries/storage/deltas", a.HandleTimeSeriesStorageDeltas)
	mux.HandleFunc("GET /api/storage/realms", a.HandleStorageEventRealms)
	mux.HandleFunc("GET /api/storage/consumers", a.HandleStorageConsumers)
	mux.HandleFunc("GET /api/graph/transfers", a.HandleGraphTransfers)
	mux.HandleFunc("GET /api/graph/callers", a.HandleGraphCallers)
	mux.HandleFunc("GET /api/sanity/overview", a.HandleSanityOverview)
	mux.HandleFunc("GET /api/timeseries/health", a.HandleTimeSeriesHealth)
	mux.HandleFunc("GET /api/timeseries/active-addresses", a.HandleTimeSeriesActiveAddresses)
	mux.HandleFunc("GET /api/timeseries/blocks", a.HandleTimeSeriesBlocks)
	mux.HandleFunc("GET /api/blocks/time-histogram", a.HandleBlockTimeHistogram)
	mux.HandleFunc("GET /api/blocks/proposers", a.HandleBlockProposers)
	mux.HandleFunc("GET /api/blocks/coverage", a.HandleBlockCoverage)
	mux.HandleFunc("GET /api/activity/heatmap", a.HandleActivityHeatmap)
	mux.HandleFunc("GET /api/timeseries/new-addresses", a.HandleTimeSeriesNewAddresses)
	mux.HandleFunc("GET /api/timeseries/active-rolling", a.HandleTimeSeriesActiveRolling)
	mux.HandleFunc("GET /api/gas/per-tx-histogram", a.HandleGasPerTxHistogram)
	mux.HandleFunc("GET /api/calls/realms", a.HandleCallRealms)
	mux.HandleFunc("GET /api/calls/function-heatmap", a.HandleFunctionCallHeatmap)
	mux.HandleFunc("GET /api/gas", a.HandleGas)
	mux.HandleFunc("GET /api/bankstats", a.HandleBankStats)
	mux.HandleFunc("GET /api/timeseries/realm-share", a.HandleTimeSeriesRealmShare)
	mux.HandleFunc("GET /api/namespaces", a.HandleNamespaces)
	mux.HandleFunc("GET /api/storage/{path...}", a.HandleStorage)
	mux.HandleFunc("GET /api/allevents", a.HandleAllEvents)
	mux.HandleFunc("GET /api/events/{path...}", a.HandleEvents)
	mux.HandleFunc("GET /api/blocks", a.HandleBlocks)
	mux.HandleFunc("GET /api/block/{height}", a.HandleBlock)
	mux.HandleFunc("GET /api/validators", a.HandleValidators)
	mux.HandleFunc("GET /api/validators/monikers", a.HandleValidatorMonikers)
	mux.HandleFunc("GET /api/validators/live", a.HandleValidatorsLive)
	mux.HandleFunc("GET /api/tokens", a.HandleTokens)
	mux.HandleFunc("GET /api/accounts", a.HandleAccounts)
	mux.HandleFunc("GET /api/labels", a.HandleLabels)
	mux.HandleFunc("GET /api/watch", a.HandleWatch)
	mux.HandleFunc("GET /api/govdao", a.HandleGovDAO)
	mux.HandleFunc("GET /api/govdao/overview", a.HandleGovDAOOverview)
	mux.HandleFunc("GET /api/govdao/proposals/{id}", a.HandleGovDAOProposal)
	mux.HandleFunc("GET /api/params", a.HandleParameters)
	mux.HandleFunc("GET /api/contracts/map", a.HandleContractsMap)
	mux.HandleFunc("GET /api/contracts/edges", a.HandleContractsEdges)
	mux.HandleFunc("GET /api/inert/queue", a.HandleInertQueue)
	mux.HandleFunc("GET /api/inert/history", a.HandleInertHistory)
	mux.HandleFunc("GET /api/inert/package/{path...}", a.HandleInertPackage)
}

// --- RPC / indexer chain agreement -----------------------------------------

// RPCChainRecheckInterval is how often the indexer/RPC pairing is re-verified.
//
// An endpoint can be repointed under a running process, so checking only at
// startup would make the guard depend on when the process happened to restart.

const RPCChainRecheckInterval = 10 * time.Minute

// rpcStatus reports the chain an RPC serves and how far along it is.
//
// The endpoint is the same one a node operator uses to check liveness. The
// height matters as much as the identity. rpc.gno.land kept answering
// /status with the right chain ID while frozen 500 blocks behind the network,
// so "does it respond and is it the right chain" is not enough to choose by.

func rpcStatus(ctx context.Context, rpcURL string) (string, int, error) {
	if rpcURL == "" {
		return "", 0, errors.New("no rpc url")
	}
	req, err := http.NewRequestWithContext(ctx, "GET", rpcURL+"/status", nil)
	if err != nil {
		return "", 0, err
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("rpc returned %s", resp.Status)
	}

	var out struct {
		Result struct {
			NodeInfo struct {
				Network string `json:"network"`
			} `json:"node_info"`
			SyncInfo struct {
				LatestBlockHeight string `json:"latest_block_height"`
			} `json:"sync_info"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", 0, err
	}
	if out.Result.NodeInfo.Network == "" {
		return "", 0, errors.New("rpc reported no chain id")
	}
	height, _ := strconv.Atoi(out.Result.SyncInfo.LatestBlockHeight)
	return out.Result.NodeInfo.Network, height, nil
}

// VerifyRPCChains records which RPCs are serving the same chain as the indexer
// they are configured beside.
//
// A network is a *pair*: an indexer supplying history and an RPC supplying live
// balance. Nothing previously checked they agreed. The chain-reset detection
// fingerprints the indexer's block 1, so it sees a replaced indexer — it cannot
// see an RPC repointed underneath, because it never asks the RPC anything.
//
// That is not hypothetical. gno.land's mainnet launched as a fresh chain,
// `gnoland-1`, on the endpoint `rpc.gno.land` — one hyphen away from the
// long-running `gnoland1` this instance indexes locally, and configured as its
// RPC. Once mainnet answered there, an address page would have served the old
// chain's transactions beside mainnet's balance, with nothing to notice.
//
// Refusing the RPC costs a balance figure. Trusting it costs a number that is
// wrong in a way no one can see.
//
// The verdict is kept beside the config rather than by editing it: a refusal has
// to be reversible, or an RPC that was merely down at startup stays disabled for
// the life of the process.

func (a *API) VerifyRPCChains(ctx context.Context) {
	for _, n := range a.networks {
		rpcs := n.RPCs()
		if len(rpcs) == 0 {
			continue
		}
		client := a.clients[n.ID]
		if client == nil {
			continue
		}

		block, err := client.GetBlock(ctx, 1)
		if err != nil || block == nil || block.ChainID == "" {
			// Unknown, not mismatched. Leave the previous verdict alone rather
			// than changing it because an indexer was briefly unreachable.
			continue
		}

		// Among the RPCs that serve the right chain, take the one furthest
		// along. Being on the right chain is a floor, not a recommendation:
		// rpc.gno.land reported the correct chain ID for 44 minutes while
		// frozen behind the network, and the balances it served were as stale
		// as the tip it reported.
		var (
			best       string
			bestHeight = -1
			lastErr    error
			mismatch   string
		)
		for _, rpcURL := range rpcs {
			rpcChain, height, err := rpcStatus(ctx, rpcURL)
			switch {
			case err != nil:
				lastErr = err
			case rpcChain != block.ChainID:
				mismatch = fmt.Sprintf("%s serves chain %q", rpcURL, rpcChain)
				log.Printf("[%s] REFUSING RPC %s: it serves chain %q while the indexer serves %q — "+
					"balances would come from a different chain than the history beside them",
					n.ID, rpcURL, rpcChain, block.ChainID)
			case height > bestHeight:
				best, bestHeight = rpcURL, height
			}
		}

		switch best {
		case "":
			if lastErr != nil {
				log.Printf("[%s] rpc chain unverified (%v); balances stay off until it answers", n.ID, lastErr)
			} else if mismatch != "" {
				log.Printf("[%s] no usable RPC: %s", n.ID, mismatch)
			}
			a.setRPCVerified(n.ID, "")
		default:
			if prev := a.rpcVerified(n.ID); prev != best {
				log.Printf("[%s] using RPC %s (chain %q, height %d)", n.ID, best, block.ChainID, bestHeight)
			}
			a.setRPCVerified(n.ID, best)
		}
	}
}

func (a *API) setRPCVerified(network, url string) {
	a.rpcMu.Lock()
	defer a.rpcMu.Unlock()
	if a.rpcPick == nil {
		a.rpcPick = map[string]string{}
	}
	a.rpcPick[network] = url
}

func (a *API) rpcVerified(network string) string {
	a.rpcMu.RLock()
	defer a.rpcMu.RUnlock()
	return a.rpcPick[network]
}
