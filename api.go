package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type API struct {
	db       *DB
	client   *IndexerClient
	analyzer *Analyzer
}

func NewAPI(db *DB, client *IndexerClient, analyzer *Analyzer) *API {
	return &API{db: db, client: client, analyzer: analyzer}
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

func (a *API) HandleStats(w http.ResponseWriter, r *http.Request) {
	stats, err := a.db.GetStats()
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}

	// Also get latest block from indexer
	height, err := a.client.LatestBlockHeight(r.Context())
	if err == nil {
		stats.LatestBlock = height
	}

	jsonResponse(w, stats)
}

func (a *API) HandleRealms(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if limit == 0 {
		limit = 50
	}

	realms, err := a.db.ListPackages(true, limit, offset)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	total, _ := a.db.CountPackages(true)
	jsonResponse(w, map[string]any{"items": realms, "total": total})
}

func (a *API) HandlePackages(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if limit == 0 {
		limit = 50
	}

	pkgs, err := a.db.ListPackages(false, limit, offset)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	total, _ := a.db.CountPackages(false)
	jsonResponse(w, map[string]any{"items": pkgs, "total": total})
}

func (a *API) HandleRealm(w http.ResponseWriter, r *http.Request) {
	path := "gno.land/" + r.PathValue("path")
	// Remove trailing slash
	path = strings.TrimRight(path, "/")

	detail, err := a.db.GetPackageDetail(path)
	if err != nil {
		jsonError(w, "package not found: "+path, 404)
		return
	}
	jsonResponse(w, detail)
}

func (a *API) HandleTx(w http.ResponseWriter, r *http.Request) {
	hash := r.PathValue("hash")
	tx, err := a.client.GetTransactionByHash(r.Context(), hash)
	if err != nil {
		jsonError(w, err.Error(), 404)
		return
	}
	jsonResponse(w, tx)
}

func (a *API) HandleTxs(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	txs, err := a.client.GetRecentTransactions(r.Context(), 0)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	total := len(txs)
	// limit=0 means return all
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
	jsonResponse(w, map[string]any{
		"items": txs[offset:end],
		"total": total,
	})
}

func (a *API) HandleAddress(w http.ResponseWriter, r *http.Request) {
	addr := r.PathValue("addr")

	// Get transactions for this address
	txs, err := a.client.GetTransactionsByAddress(r.Context(), addr)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}

	// Get packages created by this address
	pkgs, err := a.db.Search(addr)
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
	balance := fetchBalance(r.Context(), addr)

	jsonResponse(w, map[string]any{
		"address":      addr,
		"transactions": txs,
		"packages":     pkgs,
		"first_block":  firstBlock,
		"balance":      balance,
	})
}

func (a *API) HandleSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if q == "" {
		jsonError(w, "missing q parameter", 400)
		return
	}

	results, err := a.db.Search(q)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	jsonResponse(w, results)
}

func (a *API) HandleAllEvents(w http.ResponseWriter, r *http.Request) {
	// Recent transactions that have GnoEvents
	txs, err := a.client.GetRecentTransactionsWithEvents(r.Context())
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
	path := "gno.land/" + r.PathValue("path")
	path = strings.TrimRight(path, "/")
	txs, err := a.client.GetEventsByPkgPath(r.Context(), path)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	// Extract just the events for this path
	type EventResult struct {
		TxHash      string     `json:"tx_hash"`
		BlockHeight int        `json:"block_height"`
		Success     bool       `json:"success"`
		Events      []TxEvent  `json:"events"`
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
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit == 0 {
		limit = 50
	}
	blocks, err := a.client.GetRecentBlocks(r.Context(), limit)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	jsonResponse(w, blocks)
}

func (a *API) HandleBlock(w http.ResponseWriter, r *http.Request) {
	height, err := strconv.Atoi(r.PathValue("height"))
	if err != nil {
		jsonError(w, "invalid block height", 400)
		return
	}
	block, err := a.client.GetBlock(r.Context(), height)
	if err != nil {
		jsonError(w, err.Error(), 404)
		return
	}
	// Also get transactions in this block
	txs, _ := a.client.GetTransactionsByBlock(r.Context(), height)
	jsonResponse(w, map[string]any{
		"block":        block,
		"transactions": txs,
	})
}

func (a *API) HandleValidators(w http.ResponseWriter, r *http.Request) {
	// Get validator registrations from gno.land/r/gnops/valopers
	txs, err := a.client.GetTransactionsByPkgPath(r.Context(), "gno.land/r/gnops/valopers")
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	jsonResponse(w, txs)
}

func (a *API) HandleTokens(w http.ResponseWriter, r *http.Request) {
	// Get all packages that look like token contracts (import grc20)
	tokens, err := a.db.GetTokenPackages()
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	jsonResponse(w, tokens)
}

func (a *API) HandleAccounts(w http.ResponseWriter, r *http.Request) {
	accounts, err := a.db.GetActiveAccounts()
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	jsonResponse(w, accounts)
}

func (a *API) HandleGovDAO(w http.ResponseWriter, r *http.Request) {
	txs, err := a.client.GetGovDAOTransactions(r.Context())
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	jsonResponse(w, txs)
}

func (a *API) HandleDeps(w http.ResponseWriter, r *http.Request) {
	path := "gno.land/" + r.PathValue("path")
	path = strings.TrimRight(path, "/")
	direction := r.URL.Query().Get("dir") // "imports" or "dependents"

	var graph map[string][]string
	var err error

	switch direction {
	case "dependents":
		graph, err = a.db.GetReverseGraph(path)
	default:
		graph, err = a.db.GetDependencyGraph(path)
	}

	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	jsonResponse(w, graph)
}

func (a *API) HandleStorage(w http.ResponseWriter, r *http.Request) {
	path := "gno.land/" + r.PathValue("path")
	path = strings.TrimRight(path, "/")

	storageTxs, _ := a.client.GetStorageEvents(r.Context(), path)
	gasTxs, _ := a.client.GetGasUsageForRealm(r.Context(), path)

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
	txs, err := a.client.GetRecentTransactions(r.Context(), 0)
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
	// Sort descending by gas
	for i := 0; i < len(topRealms); i++ {
		for j := i + 1; j < len(topRealms); j++ {
			if topRealms[j].Gas > topRealms[i].Gas {
				topRealms[i], topRealms[j] = topRealms[j], topRealms[i]
			}
		}
	}
	if len(topRealms) > 20 {
		topRealms = topRealms[:20]
	}

	// Top txs by gas
	// Sort txs copy by gas_used desc
	type byGas []Transaction
	sorted := make([]Transaction, len(txs))
	copy(sorted, txs)
	for i := 0; i < len(sorted); i++ {
		for j := i + 1; j < len(sorted); j++ {
			if sorted[j].GasUsed > sorted[i].GasUsed {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}
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

	// Storage totals from DB
	var totalStorageBytes int
	a.db.db.QueryRow(`SELECT COALESCE(SUM(bytes_delta), 0) FROM (
		SELECT bytes_delta FROM package_files pf
		JOIN packages p ON p.path = pf.package_path
	)`).Scan(&totalStorageBytes)
	// Approximate: count total source bytes
	a.db.db.QueryRow(`SELECT COALESCE(SUM(LENGTH(body)), 0) FROM package_files`).Scan(&totalStorageBytes)

	avgGasPerTx := 0
	if len(txs) > 0 {
		avgGasPerTx = totalGasUsed / len(txs)
	}

	jsonResponse(w, map[string]any{
		"total_txs":        len(txs),
		"total_gas_used":   totalGasUsed,
		"total_gas_wanted": totalGasWanted,
		"total_fees":       totalFees,
		"avg_gas_per_tx":   avgGasPerTx,
		"success_count":    successCount,
		"fail_count":       failCount,
		"total_source_bytes": totalStorageBytes,
		"top_realms":       topRealms,
		"top_txs":          topTxs,
	})
}

// fetchBalance queries the gno.land RPC for bank balance.
func fetchBalance(ctx context.Context, addr string) string {
	rpcURL := fmt.Sprintf("https://rpc.gno.land/abci_query?path=%%22bank/balances/%s%%22&data=0x", addr)
	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequestWithContext(ctx, "GET", rpcURL, nil)
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
