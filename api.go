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
