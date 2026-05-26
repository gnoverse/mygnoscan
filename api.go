package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
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
}

func NewAPI(db *DB, clients map[string]*IndexerClient, networks []NetworkConfig, analyzer *Analyzer) *API {
	return &API{db: db, clients: clients, networks: networks, analyzer: analyzer}
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

// stampBlockTimes fetches block times for the given transactions and sets BlockTime on each.
func stampBlockTimes(ctx context.Context, client *IndexerClient, txs []Transaction) {
	if len(txs) == 0 {
		return
	}
	minH, maxH := txs[0].BlockHeight, txs[0].BlockHeight
	for _, tx := range txs {
		if tx.BlockHeight < minH {
			minH = tx.BlockHeight
		}
		if tx.BlockHeight > maxH {
			maxH = tx.BlockHeight
		}
	}
	blocks, err := client.GetBlocksInRange(ctx, minH, maxH)
	if err != nil {
		return
	}
	bt := make(map[int]string, len(blocks))
	for _, b := range blocks {
		bt[b.Height] = b.Time
	}
	for i := range txs {
		txs[i].BlockTime = bt[txs[i].BlockHeight]
	}
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
	total, _ := a.db.CountPackages(network, realmOnly)

	if network != "" {
		items, err := a.db.ListPackages(network, realmOnly, limit, offset)
		if err != nil {
			jsonError(w, err.Error(), 500)
			return
		}
		jsonResponse(w, map[string]any{"items": items, "total": total})
		return
	}

	// All networks: fetch per-network, stamp block times, merge, sort by time
	var merged []PackageInfo
	for _, n := range a.networks {
		items, err := a.db.ListPackages(n.ID, realmOnly, limit+offset, 0)
		if err != nil {
			continue
		}
		if client := a.clients[n.ID]; client != nil {
			stampPackageTimes(r.Context(), client, items)
		}
		merged = append(merged, items...)
	}
	sort.Slice(merged, func(i, j int) bool {
		if merged[i].BlockTime != "" && merged[j].BlockTime != "" {
			return merged[i].BlockTime > merged[j].BlockTime
		}
		if merged[i].BlockTime != "" {
			return true
		}
		if merged[j].BlockTime != "" {
			return false
		}
		return merged[i].BlockHeight > merged[j].BlockHeight
	})
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

	tryClient := func(netID string, client *IndexerClient) (*txDetail, error) {
		tx, err := client.GetTransactionByHash(r.Context(), hash)
		if err != nil {
			return nil, err
		}
		resp := &txDetail{Transaction: tx, Network: netID}
		if block, berr := client.GetBlock(r.Context(), tx.BlockHeight); berr == nil && block != nil {
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
		resp, err := tryClient(network, client)
		if err != nil {
			jsonError(w, err.Error(), 404)
			return
		}
		jsonResponse(w, resp)
		return
	}

	// Try all clients
	for _, n := range a.networks {
		client := a.clients[n.ID]
		if client == nil {
			continue
		}
		if resp, err := tryClient(n.ID, client); err == nil {
			jsonResponse(w, resp)
			return
		}
	}
	jsonError(w, "transaction not found", 404)
}

func (a *API) HandleTxs(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))

	if network != "" {
		client := a.clientFor(network)
		if client == nil {
			jsonError(w, "network not found", 404)
			return
		}
		txs, err := client.GetRecentTransactions(r.Context(), 0)
		if err != nil {
			jsonError(w, err.Error(), 500)
			return
		}
		total := len(txs)
		if limit <= 0 {
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
		jsonResponse(w, map[string]any{"items": txs[offset:end], "total": total})
		return
	}

	// Fan-out to all clients, merge and sort
	type netTx struct {
		Transaction
		Network string `json:"network,omitempty"`
	}
	var merged []netTx
	seen := make(map[string]bool)
	for _, n := range a.networks {
		client := a.clients[n.ID]
		if client == nil {
			continue
		}
		txs, err := client.GetRecentTransactions(r.Context(), 0)
		if err != nil {
			continue
		}
		stampBlockTimes(r.Context(), client, txs)
		for _, tx := range txs {
			if seen[tx.Hash] {
				continue
			}
			seen[tx.Hash] = true
			tx.Network = n.ID
			merged = append(merged, netTx{Transaction: tx, Network: n.ID})
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
		for _, n := range a.networks {
			c := a.clients[n.ID]
			if c == nil {
				continue
			}
			netTxs, err := c.GetTransactionsByAddress(r.Context(), addr)
			if err != nil {
				continue
			}
			stampBlockTimes(r.Context(), c, netTxs)
			for i := range netTxs {
				netTxs[i].Network = n.ID
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

func (a *API) HandleAllEvents(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	client := a.clientFor(network)
	if client == nil {
		jsonError(w, "no client available", 500)
		return
	}
	// Recent transactions that have GnoEvents
	txs, err := client.GetRecentTransactionsWithEvents(r.Context())
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
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
	for _, n := range a.networks {
		client := a.clients[n.ID]
		if client == nil {
			continue
		}
		blocks, err := client.GetRecentBlocks(r.Context(), limit)
		if err != nil {
			continue
		}
		for _, b := range blocks {
			merged = append(merged, netBlock{Block: b, Network: n.ID})
		}
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
	client := a.clientFor(network)
	if client == nil {
		jsonError(w, "no client available", 500)
		return
	}
	txs, err := client.GetRecentTransactions(r.Context(), 0)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}

	var totalGasUsed, totalGasWanted, totalFees int
	var successCount, failCount int
	type RealmGas struct {
		Path    string `json:"path"`
		Gas     int    `json:"gas"`
		Fees    int    `json:"fees"`
		TxCount int    `json:"tx_count"`
	}
	type TopTx struct {
		Hash        string `json:"hash"`
		BlockHeight int    `json:"block_height"`
		GasUsed     int    `json:"gas_used"`
		GasWanted   int    `json:"gas_wanted"`
		Fee         int    `json:"fee"`
		Type        string `json:"type"`
		Detail      string `json:"detail"`
		Success     bool   `json:"success"`
	}
	realmMap := make(map[string]*RealmGas)

	for _, tx := range txs {
		totalGasUsed += tx.GasUsed
		totalGasWanted += tx.GasWanted
		if tx.GasFee != nil {
			totalFees += tx.GasFee.Amount
		}
		if tx.Success {
			successCount++
		} else {
			failCount++
		}
		for _, m := range tx.Messages {
			path := m.Value.PkgPath
			if path == "" && m.Value.Package != nil {
				path = m.Value.Package.Path
			}
			// Ephemeral packages (MsgRun): aggregate by caller address
			if strings.Contains(path, "/e/") {
				caller := m.Value.Caller
				if caller == "" && m.Value.Creator != "" {
					caller = m.Value.Creator
				}
				if caller != "" {
					path = "MsgRun by " + caller
				}
			}
			if path != "" {
				rg, ok := realmMap[path]
				if !ok {
					rg = &RealmGas{Path: path}
					realmMap[path] = rg
				}
				rg.Gas += tx.GasUsed
				rg.TxCount++
				if tx.GasFee != nil {
					rg.Fees += tx.GasFee.Amount
				}
			}
		}
	}

	// Sort realms by gas
	var topRealms []RealmGas
	for _, rg := range realmMap {
		topRealms = append(topRealms, *rg)
	}
	sort.Slice(topRealms, func(i, j int) bool { return topRealms[i].Gas > topRealms[j].Gas })
	if len(topRealms) > 20 {
		topRealms = topRealms[:20]
	}

	// Top txs by gas
	sorted := make([]Transaction, len(txs))
	copy(sorted, txs)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].GasUsed > sorted[j].GasUsed })
	var topTxs []TopTx
	for _, tx := range sorted {
		if len(topTxs) >= 20 {
			break
		}
		typ := ""
		detail := ""
		for _, m := range tx.Messages {
			typ = m.Value.Typename
			if m.Value.PkgPath != "" {
				detail = m.Value.PkgPath
				if m.Value.Func != "" {
					detail += "::" + m.Value.Func
				}
			} else if m.Value.Package != nil {
				p := m.Value.Package.Path
				if strings.Contains(p, "/e/") {
					// Ephemeral: show caller instead
					caller := m.Value.Caller
					if caller != "" {
						detail = "MsgRun by " + caller
					} else {
						detail = p
					}
				} else {
					detail = p
				}
			}
		}
		fee := 0
		if tx.GasFee != nil {
			fee = tx.GasFee.Amount
		}
		topTxs = append(topTxs, TopTx{
			Hash: tx.Hash, BlockHeight: tx.BlockHeight,
			GasUsed: tx.GasUsed, GasWanted: tx.GasWanted,
			Fee: fee, Type: typ, Detail: detail, Success: tx.Success,
		})
	}

	// Total source bytes from DB
	totalStorageBytes := a.db.TotalSourceBytes(network)

	avgGasPerTx := 0
	if len(txs) > 0 {
		avgGasPerTx = totalGasUsed / len(txs)
	}

	jsonResponse(w, map[string]any{
		"total_txs":          len(txs),
		"total_gas_used":     totalGasUsed,
		"total_gas_wanted":   totalGasWanted,
		"total_fees":         totalFees,
		"avg_gas_per_tx":     avgGasPerTx,
		"success_count":      successCount,
		"fail_count":         failCount,
		"total_source_bytes": totalStorageBytes,
		"top_realms":         topRealms,
		"top_txs":            topTxs,
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
