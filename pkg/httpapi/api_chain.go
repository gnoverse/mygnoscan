package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/moul/mygnoscan/pkg/config"
	"github.com/moul/mygnoscan/pkg/indexer"
	"github.com/moul/mygnoscan/pkg/store"
)

func (a *API) stampBlockTimes(ctx context.Context, network string, client *indexer.Client, txs []indexer.Transaction) {
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

	bt := a.blockTimesForHeights(ctx, network, client, heights)
	for i := range txs {
		txs[i].BlockTime = bt[txs[i].BlockHeight]
	}
}

// blockTimesForHeights resolves height -> block_time for an arbitrary set of
// heights, preferring stored times over asking the indexer — the same
// lookup stampBlockTimes does for a transaction list, factored out for
// callers that need times for heights that are not necessarily any
// transaction's own block_height (e.g. an inert package's submission
// height, which is a value carried *inside* a later MsgEnablePackage, not
// the height of that message's own transaction).

func (a *API) blockTimesForHeights(ctx context.Context, network string, client *indexer.Client, heights []int) map[int]string {
	bt, err := a.db.BlockTimesForHeights(network, heights)
	if err != nil || bt == nil {
		bt = make(map[int]string, len(heights))
	}

	missing := make([]int, 0, len(heights))
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
	return bt
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

func normalizeTxHash(s string) string {
	s = strings.TrimSpace(s)
	if len(s) == 66 && (strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X")) {
		s = s[2:]
	}
	if len(s) != 64 {
		return s
	}
	raw, err := hex.DecodeString(s)
	if err != nil {
		return s
	}
	return base64.StdEncoding.EncodeToString(raw)
}

func (a *API) HandleTx(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	hash := normalizeTxHash(r.PathValue("hash"))

	type txDetail struct {
		*indexer.Transaction
		BlockTime string `json:"block_time,omitempty"`
		ChainID   string `json:"chain_id,omitempty"`
		Network   string `json:"network,omitempty"`
	}

	tryClient := func(ctx context.Context, netID string, client *indexer.Client) (*txDetail, error) {
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
		JSONResponse(w, resp)
		return
	}

	// Ask every network at once; a hash lives on at most one, and the others
	// answering "not found" should not be paid for serially.
	found := fanOut(r.Context(), a.networks, a.clients, a.health,
		func(ctx context.Context, n config.NetworkConfig, c *indexer.Client) (*txDetail, error) {
			return tryClient(ctx, n.ID, c)
		})
	if len(found) > 0 {
		JSONResponse(w, found[0])
		return
	}
	jsonError(w, "transaction not found", 404)
}

// Bounds for the transaction list. "No limit" used to mean `where: {}` — every
// transaction the chain has ever had — which returned 500 after ten seconds on a
// busy chain because it could not finish inside the client timeout. The indexer
// exposes no way to make that query cheap, so the endpoint bounds it instead.

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

	// A type filter is served from storage, not the indexer.
	//
	// The indexer has no index for message type, so asking it for deploys walks
	// the chain until it finds enough — 12s for a 50-row page on sapphire,
	// against 0.5s unfiltered. The syncer already writes one row per message
	// into a per-type table indexed by (network, block_height), which answers
	// the same question with a real offset.
	//
	// A status filter alone still goes to the indexer: `success` is a column on
	// every transaction there, so it costs nothing extra.
	msgType := r.URL.Query().Get("type")
	if _, known := store.TxSources[msgType]; known {
		var success *bool
		switch r.URL.Query().Get("success") {
		case "true":
			t := true
			success = &t
		case "false":
			f := false
			success = &f
		}
		rows, total, err := a.db.FilteredTransactions(network, msgType, success, windowed, offset)
		if err != nil {
			jsonError(w, err.Error(), 500)
			return
		}
		JSONResponse(w, map[string]any{"items": rows, "total": total, "from_storage": true})
		return
	}

	success := r.URL.Query().Get("success")
	fetch := func(ctx context.Context, c *indexer.Client) ([]indexer.Transaction, error) {
		return c.GetRecentTransactionsFiltered(ctx, need, "", success)
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
		JSONResponse(w, map[string]any{"items": page, "total": total})
		return
	}

	// Fan-out to all clients, merge and sort
	type netTx struct {
		indexer.Transaction
		Network string `json:"network,omitempty"`
	}
	var merged []netTx
	seen := make(map[string]bool)
	perNetwork := fanOut(r.Context(), a.networks, a.clients, a.health,
		func(ctx context.Context, n config.NetworkConfig, c *indexer.Client) ([]netTx, error) {
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
	JSONResponse(w, map[string]any{"items": merged[offset:end], "total": total})
}

// addressTxPage bounds an address page. Its history can be enormous — the
// busiest account on sapphire has half a million calls — and the view shows
// recent activity, not an archive.

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
	JSONResponse(w, results)
}

// Event listing bounds. `limit=-1` still means "everything" for callers that
// genuinely want it; the default keeps the events page from pulling the whole
// chain's event history on load.

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

func sortTransactionsByTime(txs []indexer.Transaction) {
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
	TxHash      string            `json:"tx_hash"`
	BlockHeight int               `json:"block_height"`
	BlockTime   string            `json:"block_time,omitempty"`
	Success     bool              `json:"success"`
	Network     string            `json:"network,omitempty"`
	Events      []indexer.TxEvent `json:"events"`
}

// gnoEvents keeps the GnoEvents of each transaction, dropping transactions that
// emitted none.

func gnoEvents(txs []indexer.Transaction, network string) []EventResult {
	out := make([]EventResult, 0, len(txs))
	for _, tx := range txs {
		if tx.Response == nil {
			continue
		}
		var matched []indexer.TxEvent
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

func gnoEventsForPath(txs []indexer.Transaction, network, path string) []EventResult {
	out := make([]EventResult, 0, len(txs))
	for _, tx := range txs {
		if tx.Response == nil {
			continue
		}
		var matched []indexer.TxEvent
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
		JSONResponse(w, results)
		return
	}

	// All networks means every network, not whichever one clientFor happened to
	// return. It used to call clientFor("") — which hands back an arbitrary entry
	// of a Go map — so this endpoint silently served a single chain's events
	// under an "all networks" heading, and the busiest chain was often the one
	// left out.
	merged := []EventResult{}
	for _, batch := range fanOut(r.Context(), a.networks, a.clients, a.health,
		func(ctx context.Context, n config.NetworkConfig, c *indexer.Client) ([]EventResult, error) {
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
	JSONResponse(w, merged)
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
			func(ctx context.Context, nc config.NetworkConfig, c *indexer.Client) ([]EventResult, error) {
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
		JSONResponse(w, merged)
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
	JSONResponse(w, gnoEventsForPath(txs, network, path))
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
		JSONResponse(w, blocks)
		return
	}

	// Fan-out: merge blocks from all networks, sort by time
	type netBlock struct {
		indexer.Block
		Network string `json:"network,omitempty"`
	}
	var merged []netBlock
	for _, blocks := range fanOut(r.Context(), a.networks, a.clients, a.health,
		func(ctx context.Context, n config.NetworkConfig, c *indexer.Client) ([]netBlock, error) {
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
	JSONResponse(w, merged)
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
	if block == nil {
		jsonError(w, fmt.Sprintf("block not found: %d", height), 404)
		return
	}
	// Also get transactions in this block
	txs, _ := client.GetTransactionsByBlock(r.Context(), height)
	JSONResponse(w, map[string]any{
		"block":        block,
		"transactions": txs,
	})
}

func (a *API) fetchGovDAOTransactions(ctx context.Context, network string) ([]indexer.Transaction, bool) {
	client := a.clientFor(network)
	if client == nil {
		return nil, false
	}
	txs, err := client.GetGovDAOTransactions(ctx, 500)
	if err != nil {
		return nil, true
	}
	a.stampBlockTimes(ctx, network, client, txs)
	return txs, true
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
		bins = []store.BlockTimeBin{}
	}
	JSONResponse(w, bins)
}

func (a *API) HandleBlockCoverage(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	cov, err := a.db.GetBlockCoverage(network)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	JSONResponse(w, cov)
}

// --- batch 2b handlers ---

// funcHeatmapDays pins the function-call heatmap's range. Daily columns past
// about a fortnight stop being legible, and the chart is about the shape of a
// realm's recent function mix, not its history.
