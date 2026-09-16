package httpapi

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/moul/mygnoscan/pkg/store"
)

const addressTxPage = 200

// HandleAddress serves an address's activity from storage.
//
// It used to ask the indexer, which cannot answer at chain scale: the query is
// five address predicates over fields it has no index for, so it scans.
// Windowing it from the tip (#121) bought time and the chain outgrew it — the
// busiest account went back to a 500 at 13.9s.
//
// Every message the syncer decodes is already written to a per-type table keyed
// by the address involved, all indexed. Balance still comes from RPC, which is
// the only place it exists.

func (a *API) HandleAddress(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	addr := r.PathValue("addr")

	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > addressTxPage {
		limit = addressTxPage
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if offset < 0 {
		offset = 0
	}

	txs, total, err := a.db.AddressTransactions(network, addr, limit, offset)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}

	pkgs, err := a.db.Search(network, addr)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}

	// The oldest block on this page, not the address's first ever: paging back
	// would otherwise make "first seen" wander. Named accordingly.
	oldestOnPage := -1
	for _, tx := range txs {
		if oldestOnPage < 0 || tx.BlockHeight < oldestOnPage {
			oldestOnPage = tx.BlockHeight
		}
	}

	// Balance is per chain and lives only at the RPC, so it is reported only
	// when one chain is selected. Summing balances across chains would repeat
	// the category error of adding ugnot from different networks.
	balance := ""
	if network != "" {
		balance = fetchBalance(r.Context(), addr, a.rpcURLFor(network))
	}

	JSONResponse(w, map[string]any{
		"address":        addr,
		"transactions":   txs,
		"total":          total,
		"packages":       pkgs,
		"oldest_on_page": oldestOnPage,
		"balance":        balance,
	})
}

func (a *API) HandleValidators(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	// Served entirely from storage since #83, so no indexer client is needed —
	// and requiring one would have made this 500 in all-networks mode.
	regs, err := a.db.ValoperRegistrations(network)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	JSONResponse(w, regs)
}

// HandleValidatorMonikers serves consensus-address -> name, sourced from
// gnockpit (see gnockpit.go) rather than this chain's own data: a block
// proposer is identified by its consensus key, which the valopers realm
// never records (it registers the *operator* key instead — see the comment
// on proposerEl in frontend/index.html), so nothing indexed here can answer
// this. Best-effort: an empty map means gnockpit could not be reached, not
// an error, since a page that can label proposers most of the time is
// better than one that breaks whenever a third party is briefly down.

func (a *API) HandleValidatorMonikers(w http.ResponseWriter, r *http.Request) {
	monikers := FetchGnockpitMonikers(r.Context())
	if monikers == nil {
		monikers = map[string]string{}
	}
	JSONResponse(w, monikers)
}

func (a *API) HandleTokens(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	// Get all packages that look like token contracts (import grc20)
	tokens, err := a.db.GetTokenPackages(network)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	JSONResponse(w, tokens)
}

// Account listing bounds. The default matches the previous fixed top 100, so an
// existing caller passing nothing sees no change.

const maxWatchItems = 100

// watchTimelineLimit bounds the merged recent-activity timeline HandleWatch
// returns alongside the digest. A fixed cap, not a query parameter: the
// timeline is a "what just happened" glance, not a paged history browser.

const watchTimelineLimit = 50

// HandleWatch summarises activity for the realms and addresses a caller watches.
//
// Items arrive as repeated `realm=` and `address=` parameters, each optionally
// carrying the height the caller last saw as `path@height`. That height is what
// turns a list into a digest: it is what "12 new calls since you last looked"
// counts against.
//
// Height rather than a timestamp because it is exact and monotonic per chain,
// where comparing wall-clock time against block time drifts.

func (a *API) HandleWatch(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	q := r.URL.Query()

	parse := func(values []string) []store.WatchRequest {
		out := make([]store.WatchRequest, 0, len(values))
		for _, v := range values {
			if len(out) >= maxWatchItems {
				break
			}
			id, since := v, 0
			if at := strings.LastIndex(v, "@"); at > 0 {
				if n, err := strconv.Atoi(v[at+1:]); err == nil {
					id, since = v[:at], n
				}
			}
			if id == "" {
				continue
			}
			out = append(out, store.WatchRequest{ID: id, Since: since})
		}
		return out
	}

	realmReqs := parse(q["realm"])
	addressReqs := parse(q["address"])

	realms, err := a.db.WatchRealms(network, realmReqs)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	addresses, err := a.db.WatchAddresses(network, addressReqs)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}

	ids := func(reqs []store.WatchRequest) []string {
		out := make([]string, len(reqs))
		for i, r := range reqs {
			out[i] = r.ID
		}
		return out
	}
	// A timeline alongside the digest: the digest says how much changed,
	// this is the actual activity behind that count. Capped independently of
	// maxWatchItems — that bounds how many realms/addresses can be watched,
	// this bounds how many rows their combined history returns.
	txs, err := a.db.WatchTransactions(network, ids(realmReqs), ids(addressReqs), watchTimelineLimit)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}

	JSONResponse(w, map[string]any{"realms": realms, "addresses": addresses, "transactions": txs})
}

func (a *API) HandleAccounts(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)

	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = defaultAccounts
	}
	if limit > maxAccounts {
		limit = maxAccounts
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if offset < 0 {
		offset = 0
	}

	accounts, err := a.db.GetActiveAccounts(network, r.URL.Query().Get("sort"), limit, offset)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	a.fillAccountBalances(r.Context(), accounts)
	JSONResponse(w, accounts)
}

// fillAccountBalances fetches each row's balance over RPC, one call per
// address since there is no batch endpoint for it. Bounded concurrency
// keeps a page of maxAccounts rows from firing that many requests at once
// against a single RPC node.
func (a *API) fillAccountBalances(ctx context.Context, accounts []store.AccountInfo) {
	const maxConcurrent = 20
	sem := make(chan struct{}, maxConcurrent)
	var wg sync.WaitGroup
	for i := range accounts {
		wg.Add(1)
		sem <- struct{}{}
		go func(acc *store.AccountInfo) {
			defer wg.Done()
			defer func() { <-sem }()
			acc.Balance = fetchBalance(ctx, acc.Address, a.rpcURLFor(acc.Network))
		}(&accounts[i])
	}
	wg.Wait()
}

func (a *API) HandleBankStats(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	stats, err := a.db.GetBankStats(network)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	JSONResponse(w, stats)
}

// HandleGovDAO lists governance calls, from storage.
//
// This used to ask the indexer, which cannot answer it: the filter is a
// substring match over a field it has no index for, so on a chain with no
// governance activity it widened its window until the deadline and returned a
// 500 — 12 seconds on sapphire. On pearl it returned a row that was not a
// governance call at all, because the predicate matched a message carrying no
// pkg_path.
//
// The syncer already records every MsgCall with its path, indexed by
// (network, pkg_path), so this is a prefix scan that answers instantly and
// cannot match a non-call.

func (a *API) HandleTimeSeriesActiveAddresses(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	days, granularity := a.resolveTimeseriesParams(r, network)
	pts, err := a.db.GetActiveAddressTimeSeries(network, granularity, days)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	if pts == nil {
		pts = []store.ActiveAddressTimePoint{}
	}
	JSONResponse(w, pts)
}

func (a *API) HandleBlockProposers(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	days, _ := a.resolveTimeseriesParams(r, network)
	topN, _ := strconv.Atoi(r.URL.Query().Get("topN"))
	props, err := a.db.GetBlockProposers(network, days, topN)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	if props == nil {
		props = []store.ProposerCount{}
	}
	JSONResponse(w, props)
}

func (a *API) HandleTimeSeriesNewAddresses(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	days, granularity := a.resolveTimeseriesParams(r, network)
	pts, err := a.db.GetNewAddressTimeSeries(network, granularity, days)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	if pts == nil {
		pts = []store.NewAddressPoint{}
	}
	JSONResponse(w, pts)
}

// HandleTimeSeriesActiveRolling ignores ?granularity= on purpose: DAU/WAU/MAU
// are trailing *day* windows, so the series is daily whatever the caller asks.
