package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type API struct {
	db       *DB
	clients  map[string]*IndexerClient
	networks []NetworkConfig
	analyzer *Analyzer
	health   *healthTracker
}

func NewAPI(db *DB, clients map[string]*IndexerClient, networks []NetworkConfig, analyzer *Analyzer) *API {
	return &API{
		db:       db,
		clients:  clients,
		networks: networks,
		analyzer: analyzer,
		health:   newHealthTracker(),
	}
}

func jsonResponse(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}

func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// stampPackageTimes fetches block times for each unique block height in parallel.
func stampPackageTimes(ctx context.Context, client *IndexerClient, pkgs []PackageInfo) {
	if len(pkgs) == 0 {
		return
	}
	// Collect unique heights
	seen := make(map[int]bool)
	var heights []int
	for _, p := range pkgs {
		if !seen[p.BlockHeight] {
			seen[p.BlockHeight] = true
			heights = append(heights, p.BlockHeight)
		}
	}
	// Fetch block times in parallel (max 5 concurrent)
	bt := make(map[int]string, len(heights))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 5)
	for _, h := range heights {
		wg.Add(1)
		go func(height int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			block, err := client.GetBlock(ctx, height)
			if err == nil && block != nil {
				mu.Lock()
				bt[height] = block.Time
				mu.Unlock()
			}
		}(h)
	}
	wg.Wait()
	for i := range pkgs {
		pkgs[i].BlockTime = bt[pkgs[i].BlockHeight]
	}
}

// stampBlockTimes sets BlockTime on each transaction, preferring stored times.
//
// Asking the indexer for each block is the dominant cost of list endpoints — a
// public indexer answers in roughly a quarter second, so a page spanning 50
// distinct blocks spends over a second on timestamps alone. The syncer already
// stores block_time, so the indexer is only consulted for whatever is not
// already known locally.
func (a *API) stampBlockTimes(ctx context.Context, network string, client *IndexerClient, txs []Transaction) {
	if len(txs) == 0 {
		return
	}
	// Deduplicate: many transactions share a block, and a naive min..max range
	// over a sparse set would pull every block in between.
	seen := make(map[int]bool, len(txs))
	heights := make([]int, 0, len(txs))
	for _, tx := range txs {
		if !seen[tx.BlockHeight] {
			seen[tx.BlockHeight] = true
			heights = append(heights, tx.BlockHeight)
		}
	}

	bt, err := a.db.BlockTimesForHeights(network, heights)
	if err != nil || bt == nil {
		bt = make(map[int]string, len(heights))
	}

	missing := heights[:0:0]
	for _, h := range heights {
		if bt[h] == "" {
			missing = append(missing, h)
		}
	}
	if len(missing) > 0 && client != nil {
		if fetched, err := client.GetBlockTimesForHeights(ctx, missing); err == nil {
			for h, t := range fetched {
				bt[h] = t
			}
		}
	}

	for i := range txs {
		txs[i].BlockTime = bt[txs[i].BlockHeight]
	}
}

// perNetworkDeadline bounds how long a single network may hold up a merged
// response. A configured-but-unreachable network — one that is down, or a
// testnet configured ahead of its launch — must degrade to missing data rather
// than to a hung page. The HTTP client's own timeout is far too long to serve
// as this bound.
const perNetworkDeadline = 8 * time.Second

// Circuit breaker settings. Without one, a configured network that is down costs
// every merged request the full perNetworkDeadline, forever. With one it costs
// that once, then nothing until the cooldown expires and it is retried — so a
// network can be configured before it launches and starts working on its own.
const (
	breakerThreshold = 2
	breakerCooldown  = 60 * time.Second
)

// healthTracker trips a per-network breaker after repeated failures.
type healthTracker struct {
	mu    sync.Mutex
	state map[string]*netHealth
}

type netHealth struct {
	failures  int
	skipUntil time.Time
}

func newHealthTracker() *healthTracker {
	return &healthTracker{state: map[string]*netHealth{}}
}

// shouldSkip reports whether the breaker for this network is currently open.
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
	networks []NetworkConfig,
	clients map[string]*IndexerClient,
	health *healthTracker,
	fn func(ctx context.Context, net NetworkConfig, client *IndexerClient) (T, error),
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
		go func(i int, n NetworkConfig, c *IndexerClient) {
			defer wg.Done()
			nctx, cancel := context.WithTimeout(ctx, perNetworkDeadline)
			defer cancel()
			v, err := fn(nctx, n, c)
			health.record(n.ID, err)
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

// rejectUnknownNetwork turns `?network=` naming an unconfigured network into a
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
func rejectUnknownNetwork(networks []NetworkConfig, next http.Handler) http.Handler {
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

// clientFor returns the IndexerClient for a network, or nil when there is no
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
func (a *API) clientFor(network string) *IndexerClient {
	if network == "" {
		return nil
	}
	return a.clients[network]
}

// rpcURLFor returns the RPC URL for a network (or first network with an RPC URL).
func (a *API) rpcURLFor(network string) string {
	for _, n := range a.networks {
		if network == "" || n.ID == network {
			if n.RPCURL != "" {
				return n.RPCURL
			}
		}
	}
	return ""
}

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

	jsonResponse(w, stats)
}

func (a *API) handleListPackages(w http.ResponseWriter, r *http.Request, realmOnly bool) {
	network := a.networkParam(r)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if limit == 0 {
		limit = 50
	}
	sortBy := r.URL.Query().Get("sort")
	total, _ := a.db.CountPackages(network, realmOnly)

	if network != "" {
		items, err := a.db.ListPackages(network, realmOnly, limit, offset, sortBy)
		if err != nil {
			jsonError(w, err.Error(), 500)
			return
		}
		jsonResponse(w, map[string]any{"items": items, "total": total})
		return
	}

	// All networks: fetch per-network, stamp block times, merge, sort by time
	var merged []PackageInfo
	for _, items := range fanOut(r.Context(), a.networks, a.clients, a.health,
		func(ctx context.Context, n NetworkConfig, c *IndexerClient) ([]PackageInfo, error) {
			items, err := a.db.ListPackages(n.ID, realmOnly, limit+offset, 0, sortBy)
			if err != nil {
				return nil, err
			}
			stampPackageTimes(ctx, c, items)
			return items, nil
		}) {
		merged = append(merged, items...)
	}
	sortMergedPackages(merged, sortBy)
	if offset >= len(merged) {
		jsonResponse(w, map[string]any{"items": []PackageInfo{}, "total": total})
		return
	}
	end := offset + limit
	if end > len(merged) {
		end = len(merged)
	}
	jsonResponse(w, map[string]any{"items": merged[offset:end], "total": total})
}

// sortMergedPackages orders a cross-network list. Block heights from different
// chains are not comparable, so the default ordering is by timestamp with height
// only as a tiebreaker within rows that have none.
func sortMergedPackages(pkgs []PackageInfo, sortBy string) {
	switch sortBy {
	case "calls":
		sort.SliceStable(pkgs, func(i, j int) bool { return pkgs[i].Calls > pkgs[j].Calls })
	case "importers":
		sort.SliceStable(pkgs, func(i, j int) bool { return pkgs[i].Importers > pkgs[j].Importers })
	case "imports":
		sort.SliceStable(pkgs, func(i, j int) bool { return pkgs[i].Imports > pkgs[j].Imports })
	case "name":
		sort.SliceStable(pkgs, func(i, j int) bool { return pkgs[i].Path < pkgs[j].Path })
	default:
		sort.SliceStable(pkgs, func(i, j int) bool {
			ti, tj := pkgs[i].BlockTime, pkgs[j].BlockTime
			if ti != "" && tj != "" {
				return ti > tj
			}
			if ti != tj {
				return ti != "" // rows with a timestamp sort ahead of rows without
			}
			return pkgs[i].BlockHeight > pkgs[j].BlockHeight
		})
	}
}

func (a *API) HandleRealms(w http.ResponseWriter, r *http.Request) {
	a.handleListPackages(w, r, true)
}

func (a *API) HandlePackages(w http.ResponseWriter, r *http.Request) {
	a.handleListPackages(w, r, false)
}

func (a *API) HandleRealm(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	path := "gno.land/" + r.PathValue("path")
	// Remove trailing slash
	path = strings.TrimRight(path, "/")

	detail, err := a.db.GetPackageDetail(network, path)
	if err != nil {
		jsonError(w, "package not found: "+path, 404)
		return
	}
	jsonResponse(w, detail)
}

func (a *API) HandleTx(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	hash := r.PathValue("hash")

	type txDetail struct {
		*Transaction
		BlockTime string `json:"block_time,omitempty"`
		ChainID   string `json:"chain_id,omitempty"`
		Network   string `json:"network,omitempty"`
	}

	tryClient := func(ctx context.Context, netID string, client *IndexerClient) (*txDetail, error) {
		tx, err := client.GetTransactionByHash(ctx, hash)
		if err != nil {
			return nil, err
		}
		resp := &txDetail{Transaction: tx, Network: netID}
		if block, berr := client.GetBlock(ctx, tx.BlockHeight); berr == nil && block != nil {
			resp.BlockTime = block.Time
			resp.ChainID = block.ChainID
		}
		return resp, nil
	}

	if network != "" {
		client := a.clientFor(network)
		if client == nil {
			jsonError(w, "network not found", 404)
			return
		}
		resp, err := tryClient(r.Context(), network, client)
		if err != nil {
			jsonError(w, err.Error(), 404)
			return
		}
		jsonResponse(w, resp)
		return
	}

	// Ask every network at once; a hash lives on at most one, and the others
	// answering "not found" should not be paid for serially.
	found := fanOut(r.Context(), a.networks, a.clients, a.health,
		func(ctx context.Context, n NetworkConfig, c *IndexerClient) (*txDetail, error) {
			return tryClient(ctx, n.ID, c)
		})
	if len(found) > 0 {
		jsonResponse(w, found[0])
		return
	}
	jsonError(w, "transaction not found", 404)
}

// Bounds for the transaction list. "No limit" used to mean `where: {}` — every
// transaction the chain has ever had — which returned 500 after ten seconds on a
// busy chain because it could not finish inside the client timeout. The indexer
// exposes no way to make that query cheap, so the endpoint bounds it instead.
const (
	defaultTxs = 500
	// 5000 was still eight seconds against a busy chain — close enough to the
	// server's write timeout to fail under load. 2000 matches maxEventTxs and
	// lands around three.
	maxTxs = 2000
)

func (a *API) HandleTxs(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))

	// An absent or non-positive limit is a request for "recent transactions",
	// not for the whole chain. Callers that genuinely want more say so, up to
	// the cap; beyond it the indexer's own element cap takes over anyway.
	windowed := limit
	if windowed <= 0 {
		windowed = defaultTxs
	}
	if windowed > maxTxs {
		windowed = maxTxs
	}
	need := offset + windowed

	fetch := func(ctx context.Context, c *IndexerClient) ([]Transaction, error) {
		return c.GetRecentTransactionsPage(ctx, need)
	}

	if network != "" {
		client := a.clientFor(network)
		if client == nil {
			jsonError(w, "network not found", 404)
			return
		}
		txs, err := fetch(r.Context(), client)
		if err != nil {
			jsonError(w, err.Error(), 500)
			return
		}
		// total is what was fetched, not what the chain holds. It never could
		// be: the indexer caps a result set and exposes no count, so this is
		// the size of the recent window and the frontend labels it as such.
		total := len(txs)
		if offset > total {
			offset = total
		}
		end := offset + windowed
		if end > total {
			end = total
		}
		page := txs[offset:end]
		// Stamp only what is being returned, not everything that was fetched.
		a.stampBlockTimes(r.Context(), network, client, page)
		jsonResponse(w, map[string]any{"items": page, "total": total})
		return
	}

	// Fan-out to all clients, merge and sort
	type netTx struct {
		Transaction
		Network string `json:"network,omitempty"`
	}
	var merged []netTx
	seen := make(map[string]bool)
	perNetwork := fanOut(r.Context(), a.networks, a.clients, a.health,
		func(ctx context.Context, n NetworkConfig, c *IndexerClient) ([]netTx, error) {
			txs, err := fetch(ctx, c)
			if err != nil {
				return nil, err
			}
			// Block times are needed before sorting: across networks, heights
			// from different chains are not comparable and only the timestamp
			// orders them.
			a.stampBlockTimes(ctx, n.ID, c, txs)
			out := make([]netTx, 0, len(txs))
			for _, tx := range txs {
				tx.Network = n.ID
				out = append(out, netTx{Transaction: tx, Network: n.ID})
			}
			return out, nil
		})
	for _, txs := range perNetwork {
		for _, tx := range txs {
			if seen[tx.Hash] {
				continue
			}
			seen[tx.Hash] = true
			merged = append(merged, tx)
		}
	}
	sort.SliceStable(merged, func(i, j int) bool {
		return newerFirst(merged[i].BlockTime, merged[j].BlockTime,
			merged[i].BlockHeight, merged[j].BlockHeight)
	})
	total := len(merged)
	if offset > total {
		offset = total
	}
	end := offset + windowed
	if end > total {
		end = total
	}
	jsonResponse(w, map[string]any{"items": merged[offset:end], "total": total})
}

func (a *API) HandleAddress(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	addr := r.PathValue("addr")

	// Get transactions for this address (fan-out to all networks when unfiltered)
	var txs []Transaction
	if network == "" {
		seen := make(map[string]bool)
		for _, netTxs := range fanOut(r.Context(), a.networks, a.clients, a.health,
			func(ctx context.Context, n NetworkConfig, c *IndexerClient) ([]Transaction, error) {
				netTxs, err := c.GetTransactionsByAddress(ctx, addr)
				if err != nil {
					return nil, err
				}
				a.stampBlockTimes(ctx, n.ID, c, netTxs)
				for i := range netTxs {
					netTxs[i].Network = n.ID
				}
				return netTxs, nil
			}) {
			for i := range netTxs {
				if !seen[netTxs[i].Hash] {
					seen[netTxs[i].Hash] = true
					txs = append(txs, netTxs[i])
				}
			}
		}
		sortTransactionsByTime(txs)
	} else {
		c := a.clientFor(network)
		if c == nil {
			jsonError(w, "network not found", 404)
			return
		}
		var err error
		txs, err = c.GetTransactionsByAddress(r.Context(), addr)
		if err != nil {
			jsonError(w, err.Error(), 500)
			return
		}
	}

	// Get packages created by this address
	pkgs, err := a.db.Search(network, addr)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}

	// First block seen
	firstBlock := -1
	for _, tx := range txs {
		if firstBlock < 0 || tx.BlockHeight < firstBlock {
			firstBlock = tx.BlockHeight
		}
	}

	// Bank balance via RPC
	rpcURL := a.rpcURLFor(network)
	balance := fetchBalance(r.Context(), addr, rpcURL)

	jsonResponse(w, map[string]any{
		"address":      addr,
		"transactions": txs,
		"packages":     pkgs,
		"first_block":  firstBlock,
		"balance":      balance,
	})
}

func (a *API) HandleSearch(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	q := r.URL.Query().Get("q")
	if q == "" {
		jsonError(w, "missing q parameter", 400)
		return
	}

	results, err := a.db.Search(network, q)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	jsonResponse(w, results)
}

// Event listing bounds. `limit=-1` still means "everything" for callers that
// genuinely want it; the default keeps the events page from pulling the whole
// chain's event history on load.
const (
	defaultEventTxs = 200
	maxEventTxs     = 2000
)

// eventTxLimit reads the caller's `limit`, falling back to a default and capped
// at a maximum. Every event view goes through it so none of them can ask the
// indexer for a chain's entire history.
func eventTxLimit(r *http.Request) int {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = defaultEventTxs
	}
	if limit > maxEventTxs {
		limit = maxEventTxs
	}
	return limit
}

// newerFirst is the ordering every cross-network list uses.
//
// Timestamp wins. A row that has one sorts ahead of a row that does not, so
// undated rows collect at the end rather than being interleaved by a number that
// means nothing. Height is the last resort, and by then both rows are undated.
//
// Heights are per-chain: gnoland1 sits near 3.1M while sapphire is near 400k.
// Comparing them across networks lets the chain with the largest numbers win
// every comparison — and in a list that is then truncated to a page, that does
// not mis-order, it deletes a chain. It did exactly that to sapphire's events.
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
func sortTransactionsByTime(txs []Transaction) {
	sort.SliceStable(txs, func(i, j int) bool {
		return newerFirst(txs[i].BlockTime, txs[j].BlockTime, txs[i].BlockHeight, txs[j].BlockHeight)
	})
}

// sortEventResultsByTime orders newest first, on the same rules as
// sortTransactionsByTime.
func sortEventResultsByTime(rows []EventResult) {
	sort.SliceStable(rows, func(i, j int) bool {
		return newerFirst(rows[i].BlockTime, rows[j].BlockTime, rows[i].BlockHeight, rows[j].BlockHeight)
	})
}

// EventResult is one transaction's GnoEvents, tagged with the chain it came from.
type EventResult struct {
	TxHash      string    `json:"tx_hash"`
	BlockHeight int       `json:"block_height"`
	BlockTime   string    `json:"block_time,omitempty"`
	Success     bool      `json:"success"`
	Network     string    `json:"network,omitempty"`
	Events      []TxEvent `json:"events"`
}

// gnoEvents keeps the GnoEvents of each transaction, dropping transactions that
// emitted none.
func gnoEvents(txs []Transaction, network string) []EventResult {
	out := make([]EventResult, 0, len(txs))
	for _, tx := range txs {
		if tx.Response == nil {
			continue
		}
		var matched []TxEvent
		for _, ev := range tx.Response.Events {
			if ev.Typename == "GnoEvent" {
				matched = append(matched, ev)
			}
		}
		if len(matched) == 0 {
			continue
		}
		out = append(out, EventResult{
			TxHash:      tx.Hash,
			BlockHeight: tx.BlockHeight,
			BlockTime:   tx.BlockTime,
			Success:     tx.Success,
			Network:     network,
			Events:      matched,
		})
	}
	return out
}

// gnoEventsForPath keeps only the events a given realm emitted. The transaction
// may carry events from several realms; the realm view wants one realm's.
func gnoEventsForPath(txs []Transaction, network, path string) []EventResult {
	out := make([]EventResult, 0, len(txs))
	for _, tx := range txs {
		if tx.Response == nil {
			continue
		}
		var matched []TxEvent
		for _, ev := range tx.Response.Events {
			if ev.PkgPath == path {
				matched = append(matched, ev)
			}
		}
		if len(matched) == 0 {
			continue
		}
		out = append(out, EventResult{
			TxHash:      tx.Hash,
			BlockHeight: tx.BlockHeight,
			BlockTime:   tx.BlockTime,
			Success:     tx.Success,
			Network:     network,
			Events:      matched,
		})
	}
	return out
}

func (a *API) HandleAllEvents(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	// Bounded by default: unbounded, this returns every event-emitting
	// transaction the chain ever had.
	limit := eventTxLimit(r)

	if network != "" {
		client := a.clientFor(network)
		if client == nil {
			jsonError(w, "network not found", 404)
			return
		}
		txs, err := client.GetRecentTransactionsWithEvents(r.Context(), limit)
		if err != nil {
			jsonError(w, err.Error(), 500)
			return
		}
		// Same stamping as the merged path, so a row carries a timestamp
		// whichever way it was fetched.
		a.stampBlockTimes(r.Context(), network, client, txs)
		results := gnoEvents(txs, network)
		if len(results) > limit {
			results = results[:limit]
		}
		jsonResponse(w, results)
		return
	}

	// All networks means every network, not whichever one clientFor happened to
	// return. It used to call clientFor("") — which hands back an arbitrary entry
	// of a Go map — so this endpoint silently served a single chain's events
	// under an "all networks" heading, and the busiest chain was often the one
	// left out.
	merged := []EventResult{}
	for _, batch := range fanOut(r.Context(), a.networks, a.clients, a.health,
		func(ctx context.Context, n NetworkConfig, c *IndexerClient) ([]EventResult, error) {
			txs, err := c.GetRecentTransactionsWithEvents(ctx, limit)
			if err != nil {
				return nil, err
			}
			// Timestamps are load-bearing here, not decoration. txFieldsLight
			// carries no block_time, so without this every row sorts on raw
			// height — and heights are not comparable across chains. gnoland1
			// sits near 3.1M while sapphire is near 400k, so gnoland1 would win
			// every comparison and the truncation below would drop sapphire
			// entirely. Measured: 100 rows returned, 100 of them gnoland1.
			a.stampBlockTimes(ctx, n.ID, c, txs)
			return gnoEvents(txs, n.ID), nil
		}) {
		merged = append(merged, batch...)
	}

	// Interleave by time. Heights are not comparable across chains, so they are
	// only a fallback for rows the block-time backfill has not reached.
	sortEventResultsByTime(merged)
	if len(merged) > limit {
		merged = merged[:limit]
	}
	jsonResponse(w, merged)
}

func (a *API) HandleEvents(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	path := "gno.land/" + r.PathValue("path")
	path = strings.TrimRight(path, "/")

	// Same treatment as /api/allevents: query every chain rather than whichever
	// one clientFor used to return.
	if network == "" {
		limit := eventTxLimit(r)
		merged := []EventResult{}
		for _, batch := range fanOut(r.Context(), a.networks, a.clients, a.health,
			func(ctx context.Context, nc NetworkConfig, c *IndexerClient) ([]EventResult, error) {
				txs, err := c.GetEventsByPkgPath(ctx, path, limit)
				if err != nil {
					return nil, err
				}
				a.stampBlockTimes(ctx, nc.ID, c, txs)
				return gnoEventsForPath(txs, nc.ID, path), nil
			}) {
			merged = append(merged, batch...)
		}
		sortEventResultsByTime(merged)
		if len(merged) > limit {
			merged = merged[:limit]
		}
		jsonResponse(w, merged)
		return
	}

	client := a.clientFor(network)
	if client == nil {
		jsonError(w, "network not found", 404)
		return
	}
	// Bounded like /api/allevents, and for the same reason: unbounded, this
	// filter scans the chain's whole history and takes ~34s on a busy one.
	txs, err := client.GetEventsByPkgPath(r.Context(), path, eventTxLimit(r))
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	a.stampBlockTimes(r.Context(), network, client, txs)
	jsonResponse(w, gnoEventsForPath(txs, network, path))
}

func (a *API) HandleBlocks(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit == 0 {
		limit = 50
	}

	if network != "" {
		client := a.clientFor(network)
		if client == nil {
			jsonError(w, "network not found", 404)
			return
		}
		blocks, err := client.GetRecentBlocks(r.Context(), limit)
		if err != nil {
			jsonError(w, err.Error(), 500)
			return
		}
		jsonResponse(w, blocks)
		return
	}

	// Fan-out: merge blocks from all networks, sort by time
	type netBlock struct {
		Block
		Network string `json:"network,omitempty"`
	}
	var merged []netBlock
	for _, blocks := range fanOut(r.Context(), a.networks, a.clients, a.health,
		func(ctx context.Context, n NetworkConfig, c *IndexerClient) ([]netBlock, error) {
			blocks, err := c.GetRecentBlocks(ctx, limit)
			if err != nil {
				return nil, err
			}
			out := make([]netBlock, 0, len(blocks))
			for _, b := range blocks {
				out = append(out, netBlock{Block: b, Network: n.ID})
			}
			return out, nil
		}) {
		merged = append(merged, blocks...)
	}
	sort.SliceStable(merged, func(i, j int) bool {
		return newerFirst(merged[i].Time, merged[j].Time, merged[i].Height, merged[j].Height)
	})
	if len(merged) > limit {
		merged = merged[:limit]
	}
	jsonResponse(w, merged)
}

func (a *API) HandleBlock(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	// A height does not identify a block on its own: every chain has one. This
	// used to answer from an arbitrary chain, so the same URL could return a
	// different block on consecutive requests.
	if network == "" {
		jsonError(w, "a block height needs a network: add ?network=", 400)
		return
	}
	client := a.clientFor(network)
	if client == nil {
		jsonError(w, "network not found", 404)
		return
	}
	height, err := strconv.Atoi(r.PathValue("height"))
	if err != nil {
		jsonError(w, "invalid block height", 400)
		return
	}
	block, err := client.GetBlock(r.Context(), height)
	if err != nil {
		jsonError(w, err.Error(), 404)
		return
	}
	// Also get transactions in this block
	txs, _ := client.GetTransactionsByBlock(r.Context(), height)
	jsonResponse(w, map[string]any{
		"block":        block,
		"transactions": txs,
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
	jsonResponse(w, regs)
}

func (a *API) HandleTokens(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	// Get all packages that look like token contracts (import grc20)
	tokens, err := a.db.GetTokenPackages(network)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	jsonResponse(w, tokens)
}

func (a *API) HandleAccounts(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	accounts, err := a.db.GetActiveAccounts(network)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	jsonResponse(w, accounts)
}

func (a *API) HandleBankStats(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	stats, err := a.db.GetBankStats(network)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	jsonResponse(w, stats)
}

func (a *API) HandleGovDAO(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	limit := eventTxLimit(r)

	// Previously this called clientFor("") for all networks, which returns an
	// arbitrary client — and in practice returned a null body.
	if network == "" {
		// Non-nil so an empty result serialises as [] rather than null; the
		// frontend iterates this and null is what it used to receive.
		merged := []Transaction{}
		for _, txs := range fanOut(r.Context(), a.networks, a.clients, a.health,
			func(ctx context.Context, nc NetworkConfig, c *IndexerClient) ([]Transaction, error) {
				txs, err := c.GetGovDAOTransactions(ctx, limit)
				if err != nil {
					return nil, err
				}
				a.stampBlockTimes(ctx, nc.ID, c, txs)
				for i := range txs {
					txs[i].Network = nc.ID
				}
				return txs, nil
			}) {
			merged = append(merged, txs...)
		}
		sortTransactionsByTime(merged)
		if len(merged) > limit {
			merged = merged[:limit]
		}
		jsonResponse(w, merged)
		return
	}

	client := a.clientFor(network)
	if client == nil {
		jsonError(w, "network not found", 404)
		return
	}
	txs, err := client.GetGovDAOTransactions(r.Context(), limit)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	jsonResponse(w, txs)
}

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
	jsonResponse(w, graph)
}

func (a *API) HandleStorage(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	// This totals storage deposits and refunds. Those are denominated amounts,
	// and adding one chain's to another's gives a figure that describes nothing
	// — the same category error as the summed fee totals in #86. Rather than
	// blend them, or answer from whichever chain clientFor used to pick, ask for
	// a network. A realm path lives on one chain in practice, so the caller
	// always has one to give.
	if network == "" {
		jsonError(w, "storage figures are per-chain: add ?network=", 400)
		return
	}
	client := a.clientFor(network)
	if client == nil {
		jsonError(w, "network not found", 404)
		return
	}
	path := "gno.land/" + r.PathValue("path")
	path = strings.TrimRight(path, "/")

	storageTxs, _ := client.GetStorageEvents(r.Context(), path)
	gasTxs, _ := client.GetGasUsageForRealm(r.Context(), path)

	// Aggregate storage
	var totalBytesDeposit, totalBytesUnlock int
	var totalFeeDeposit, totalFeeRefund int
	type StorageEntry struct {
		TxHash      string `json:"tx_hash"`
		BlockHeight int    `json:"block_height"`
		Type        string `json:"type"`
		BytesDelta  int    `json:"bytes_delta"`
		FeeAmount   int    `json:"fee_amount"`
		FeeDenom    string `json:"fee_denom"`
	}
	var entries []StorageEntry
	for _, tx := range storageTxs {
		if tx.Response == nil {
			continue
		}
		for _, ev := range tx.Response.Events {
			if ev.Typename == "StorageDepositEvent" && ev.PkgPath == path {
				totalBytesDeposit += ev.BytesDelta
				fee := 0
				denom := ""
				if ev.FeeDelta != nil {
					fee = ev.FeeDelta.Amount
					denom = ev.FeeDelta.Denom
					totalFeeDeposit += fee
				}
				entries = append(entries, StorageEntry{tx.Hash, tx.BlockHeight, "deposit", ev.BytesDelta, fee, denom})
			} else if ev.Typename == "StorageUnlockEvent" && ev.PkgPath == path {
				totalBytesUnlock += ev.BytesDelta
				fee := 0
				denom := ""
				if ev.FeeRefund != nil {
					fee = ev.FeeRefund.Amount
					denom = ev.FeeRefund.Denom
					totalFeeRefund += fee
				}
				entries = append(entries, StorageEntry{tx.Hash, tx.BlockHeight, "unlock", ev.BytesDelta, fee, denom})
			}
		}
	}

	// Aggregate gas
	var totalGasUsed, totalGasWanted, totalGasFee int
	type GasEntry struct {
		TxHash      string `json:"tx_hash"`
		BlockHeight int    `json:"block_height"`
		GasUsed     int    `json:"gas_used"`
		GasWanted   int    `json:"gas_wanted"`
		GasFee      int    `json:"gas_fee"`
		Func        string `json:"func"`
		Success     bool   `json:"success"`
	}
	var gasEntries []GasEntry
	for _, tx := range gasTxs {
		totalGasUsed += tx.GasUsed
		totalGasWanted += tx.GasWanted
		fee := 0
		if tx.GasFee != nil {
			fee = tx.GasFee.Amount
			totalGasFee += fee
		}
		fn := ""
		if len(tx.Messages) > 0 {
			fn = tx.Messages[0].Value.Func
			if fn == "" {
				fn = tx.Messages[0].Value.Typename
			}
		}
		gasEntries = append(gasEntries, GasEntry{tx.Hash, tx.BlockHeight, tx.GasUsed, tx.GasWanted, fee, fn, tx.Success})
	}

	jsonResponse(w, map[string]any{
		"storage": map[string]any{
			"total_bytes_deposited": totalBytesDeposit,
			"total_bytes_unlocked":  totalBytesUnlock,
			"net_bytes":             totalBytesDeposit - totalBytesUnlock,
			"total_fee_deposited":   totalFeeDeposit,
			"total_fee_refunded":    totalFeeRefund,
			"entries":               entries,
		},
		"gas": map[string]any{
			"total_gas_used":   totalGasUsed,
			"total_gas_wanted": totalGasWanted,
			"total_gas_fee":    totalGasFee,
			"tx_count":         len(gasEntries),
			"entries":          gasEntries,
		},
	})
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

	jsonResponse(w, map[string]any{
		"total_txs":          stats.TotalTxs,
		"total_gas_used":     stats.TotalGasUsed,
		"total_gas_wanted":   stats.TotalGasWanted,
		"total_fees":         stats.TotalFees,
		"avg_gas_per_tx":     avgGasPerTx,
		"success_count":      stats.SuccessCount,
		"fail_count":         stats.FailCount,
		"total_source_bytes": a.db.TotalSourceBytes(network),
		"top_realms":         stats.TopRealms,
		"top_txs":            stats.TopTxs,
	})
}

func (a *API) HandleAnalytics(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	analytics, err := a.db.GetAnalytics(network)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	jsonResponse(w, analytics)
}

// fetchBalance queries the gno.land RPC for bank balance.
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

func parseTimeseriesParams(r *http.Request) (days int, granularity string) {
	days, _ = strconv.Atoi(r.URL.Query().Get("days"))
	if days <= 0 {
		days = 30
	}
	if days > 365 {
		days = 365
	}
	granularity = r.URL.Query().Get("granularity")
	switch granularity {
	case "hourly", "daily", "weekly":
	default:
		granularity = "daily"
	}
	return
}

func (a *API) HandleTimeSeriesTransactions(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	days, granularity := parseTimeseriesParams(r)
	pts, err := a.db.GetTransactionTimeSeries(network, granularity, days)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	if pts == nil {
		pts = []TxTimePoint{}
	}
	jsonResponse(w, pts)
}

func (a *API) HandleTimeSeriesPackages(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	days, granularity := parseTimeseriesParams(r)
	pts, err := a.db.GetPackageTimeSeries(network, granularity, days)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	if pts == nil {
		pts = []PkgTimePoint{}
	}
	jsonResponse(w, pts)
}

func (a *API) HandleTimeSeriesStorage(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	days, granularity := parseTimeseriesParams(r)
	realmPath := r.URL.Query().Get("realm")
	pts, err := a.db.GetStorageTimeSeries(network, realmPath, granularity, days)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	if pts == nil {
		pts = []StorageTimePoint{}
	}
	jsonResponse(w, pts)
}

func (a *API) HandleStorageRealms(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	days, _ := parseTimeseriesParams(r)
	paths, err := a.db.GetRealmsWithStorage(network, days)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	if paths == nil {
		paths = []string{}
	}
	jsonResponse(w, paths)
}

func (a *API) HandleTimeSeriesGas(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	days, granularity := parseTimeseriesParams(r)
	pts, err := a.db.GetGasTimeSeries(network, granularity, days)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	if pts == nil {
		pts = []GasTimePoint{}
	}
	jsonResponse(w, pts)
}

func (a *API) HandleTimeSeriesCallers(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	days, granularity := parseTimeseriesParams(r)
	pts, err := a.db.GetCallerTimeSeries(network, granularity, days)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	if pts == nil {
		pts = []CallerTimePoint{}
	}
	jsonResponse(w, pts)
}

func (a *API) HandleSanityOverview(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	ov, err := a.db.GetSanityOverview(network)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	// Chain height, last block time and liveness always come from the live indexer.
	if client := a.clientFor(network); client != nil {
		if blocks, err := client.GetRecentBlocks(r.Context(), 1); err == nil && len(blocks) > 0 {
			b := blocks[0]
			ov.ChainHeight = b.Height
			ov.LastBlockTime = b.Time
			if t, err := time.Parse(time.RFC3339, b.Time); err == nil {
				ov.SecondsSinceBlock = int(time.Since(t).Seconds())
				ov.IsAlive = ov.SecondsSinceBlock < 120
			}
		}
	}
	jsonResponse(w, ov)
}

func (a *API) HandleTimeSeriesHealth(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	days, granularity := parseTimeseriesParams(r)
	pts, err := a.db.GetHealthTimeSeries(network, granularity, days)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	if pts == nil {
		pts = []HealthTimePoint{}
	}
	jsonResponse(w, pts)
}

func (a *API) HandleTimeSeriesActiveAddresses(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	days, granularity := parseTimeseriesParams(r)
	pts, err := a.db.GetActiveAddressTimeSeries(network, granularity, days)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	if pts == nil {
		pts = []ActiveAddressTimePoint{}
	}
	jsonResponse(w, pts)
}
