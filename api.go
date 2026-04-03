package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
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
	jsonResponse(w, realms)
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
	jsonResponse(w, pkgs)
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
	txs, err := a.client.GetRecentTransactions(r.Context())
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	jsonResponse(w, txs)
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

	jsonResponse(w, map[string]any{
		"address":      addr,
		"transactions": txs,
		"packages":     pkgs,
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
