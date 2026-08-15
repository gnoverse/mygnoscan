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

// clientFor returns the IndexerClient for a specific network, or the first available one if not found.
func (a *API) clientFor(network string) *IndexerClient {
	if network != "" {
		if c, ok := a.clients[network]; ok {
			return c
		}
	}
	// fallback: first client
	for _, c := range a.clients {
		return c
	}
	return nil
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

	// Also get latest block from indexer
	client := a.clientFor(network)
	if client != nil {
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

func (a *API) HandleTxs(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))

	// With a limit we only need enough rows to fill the page. Without one the
	// caller is asking for everything, which stays expensive by definition.
	need := 0
	if limit > 0 {
		need = offset + limit
	}
	fetch := func(ctx context.Context, c *IndexerClient) ([]Transaction, error) {
		if need > 0 {
			return c.GetRecentTransactionsPage(ctx, need)
		}
		return c.GetRecentTransactions(ctx, 0)
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
		total := len(txs)
		if limit <= 0 {
			a.stampBlockTimes(r.Context(), network, client, txs)
			jsonResponse(w, map[string]any{"items": txs, "total": total})
			return
		}
		if offset > total {
			offset = total
		}
		end := offset + limit
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
	sort.Slice(merged, func(i, j int) bool {
		if merged[i].BlockTime != "" && merged[j].BlockTime != "" {
			return merged[i].BlockTime > merged[j].BlockTime
		}
		return merged[i].BlockHeight > merged[j].BlockHeight
	})
	total := len(merged)
	if limit <= 0 {
		jsonResponse(w, map[string]any{"items": merged, "total": total})
		return
	}
	if offset > total {
		offset = total
	}
	end := offset + limit
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
		sort.Slice(txs, func(i, j int) bool {
			if txs[i].BlockTime != "" && txs[j].BlockTime != "" {
				return txs[i].BlockTime > txs[j].BlockTime
			}
			return txs[i].BlockHeight > txs[j].BlockHeight
		})
	} else {
		c := a.clientFor(network)
		if c == nil {
			jsonError(w, "no client available", 500)
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

func (a *API) HandleAllEvents(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	client := a.clientFor(network)
	if client == nil {
		jsonError(w, "no client available", 500)
		return
	}
	// Recent transactions that have GnoEvents. Bounded by default: unbounded,
	// this returns every event-emitting transaction the chain ever had.
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit == 0 {
		limit = defaultEventTxs
	}
	if limit > maxEventTxs {
		limit = maxEventTxs
	}
	txs, err := client.GetRecentTransactionsWithEvents(r.Context(), limit)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	if limit > 0 && len(txs) > limit {
		txs = txs[:limit]
	}
	type EventResult struct {
		TxHash      string    `json:"tx_hash"`
		BlockHeight int       `json:"block_height"`
		Success     bool      `json:"success"`
		Events      []TxEvent `json:"events"`
	}
	var results []EventResult
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
		if len(matched) > 0 {
			results = append(results, EventResult{
				TxHash:      tx.Hash,
				BlockHeight: tx.BlockHeight,
				Success:     tx.Success,
				Events:      matched,
			})
		}
	}
	jsonResponse(w, results)
}

func (a *API) HandleEvents(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	client := a.clientFor(network)
	if client == nil {
		jsonError(w, "no client available", 500)
		return
	}
	path := "gno.land/" + r.PathValue("path")
	path = strings.TrimRight(path, "/")
	txs, err := client.GetEventsByPkgPath(r.Context(), path)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	// Extract just the events for this path
	type EventResult struct {
		TxHash      string    `json:"tx_hash"`
		BlockHeight int       `json:"block_height"`
		Success     bool      `json:"success"`
		Events      []TxEvent `json:"events"`
	}
	var results []EventResult
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
		if len(matched) > 0 {
			results = append(results, EventResult{
				TxHash:      tx.Hash,
				BlockHeight: tx.BlockHeight,
				Success:     tx.Success,
				Events:      matched,
			})
		}
	}
	jsonResponse(w, results)
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
			jsonError(w, "no client available", 500)
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
	sort.Slice(merged, func(i, j int) bool {
		if merged[i].Time != "" && merged[j].Time != "" {
			return merged[i].Time > merged[j].Time
		}
		return merged[i].Height > merged[j].Height
	})
	if len(merged) > limit {
		merged = merged[:limit]
	}
	jsonResponse(w, merged)
}

func (a *API) HandleBlock(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	client := a.clientFor(network)
	if client == nil {
		jsonError(w, "no client available", 500)
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
	if block == nil {
		jsonError(w, fmt.Sprintf("block not found: %d", height), 404)
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
	client := a.clientFor(network)
	if client == nil {
		jsonError(w, "no client available", 500)
		return
	}
	// Get validator registrations from gno.land/r/gnops/valopers
	txs, err := client.GetTransactionsByPkgPath(r.Context(), "gno.land/r/gnops/valopers")
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	jsonResponse(w, txs)
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
	client := a.clientFor(network)
	if client == nil {
		jsonError(w, "no client available", 500)
		return
	}
	txs, err := client.GetGovDAOTransactions(r.Context())
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
	client := a.clientFor(network)
	if client == nil {
		jsonError(w, "no client available", 500)
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
		pts = []TxTimePoint{}
	}
	jsonResponse(w, pts)
}

func (a *API) HandleTimeSeriesPackages(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	days, granularity := a.resolveTimeseriesParams(r, network)
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
	days, granularity := a.resolveTimeseriesParams(r, network)
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
	days, _ := a.resolveTimeseriesParams(r, network)
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
	days, granularity := a.resolveTimeseriesParams(r, network)
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
	days, granularity := a.resolveTimeseriesParams(r, network)
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
	days, granularity := a.resolveTimeseriesParams(r, network)
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
	days, granularity := a.resolveTimeseriesParams(r, network)
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

func (a *API) HandleTimeSeriesBlocks(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	days, granularity := a.resolveTimeseriesParams(r, network)
	pts, err := a.db.GetBlockTimeSeries(network, granularity, days)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	if pts == nil {
		pts = []BlockTimePoint{}
	}
	jsonResponse(w, pts)
}

func (a *API) HandleBlockTimeHistogram(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	days, _ := a.resolveTimeseriesParams(r, network)
	bins, err := a.db.GetBlockTimeHistogram(network, days)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	if bins == nil {
		bins = []BlockTimeBin{}
	}
	jsonResponse(w, bins)
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
		props = []ProposerCount{}
	}
	jsonResponse(w, props)
}

func (a *API) HandleBlockCoverage(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	cov, err := a.db.GetBlockCoverage(network)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	jsonResponse(w, cov)
}

// --- batch 2b handlers ---

// funcHeatmapDays pins the function-call heatmap's range. Daily columns past
// about a fortnight stop being legible, and the chart is about the shape of a
// realm's recent function mix, not its history.
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
		cells = []ActivityCell{}
	}
	jsonResponse(w, cells)
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
		pts = []NewAddressPoint{}
	}
	jsonResponse(w, pts)
}

// HandleTimeSeriesActiveRolling ignores ?granularity= on purpose: DAU/WAU/MAU
// are trailing *day* windows, so the series is daily whatever the caller asks.
func (a *API) HandleTimeSeriesActiveRolling(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	days, _ := a.resolveTimeseriesParams(r, network)
	// resolveTimeseriesParams' cap is 365 only when granularity != "monthly", and
	// this handler discards granularity entirely, so a request such as
	// ?days=3650&granularity=monthly (or window=all on an empty database, which
	// falls back to the fixed (allWindowDays, monthly) mapping) would otherwise
	// reach GetRollingActiveTimeSeries uncapped. The series is always daily, so
	// its own cap is independent of the granularity-aware one above.
	if days > rollingMaxDays {
		days = rollingMaxDays
	}
	pts, err := a.db.GetRollingActiveTimeSeries(network, days)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	if pts == nil {
		pts = []RollingActivePoint{}
	}
	jsonResponse(w, pts)
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
		bins = []GasBin{}
	}
	jsonResponse(w, bins)
}

func (a *API) HandleCallRealms(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	paths, err := a.db.GetRealmsWithCalls(network, funcHeatmapDays, limit)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	if paths == nil {
		paths = []string{}
	}
	jsonResponse(w, paths)
}

// HandleFunctionCallHeatmap serves one realm's function x day call grid. The
// range is fixed at funcHeatmapDays; ?window= and ?days= are not honoured,
// because the y-axis is functions and the x-axis is days either way.
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
		cells = []FuncCallCell{}
	}
	jsonResponse(w, cells)
}
