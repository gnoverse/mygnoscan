package httpapi

import (
	"context"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/moul/mygnoscan/pkg/analyzer"
	"github.com/moul/mygnoscan/pkg/config"
	"github.com/moul/mygnoscan/pkg/indexer"
	"github.com/moul/mygnoscan/pkg/store"
)

func stampPackageTimes(ctx context.Context, client *indexer.Client, pkgs []store.PackageInfo) {
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
		a.stampInertStatus(r.Context(), items)
		JSONResponse(w, map[string]any{"items": items, "total": total})
		return
	}

	// All networks: fetch per-network, stamp block times, merge, sort by time
	var merged []store.PackageInfo
	for _, items := range fanOut(r.Context(), a.networks, a.clients, a.health,
		func(ctx context.Context, n config.NetworkConfig, c *indexer.Client) ([]store.PackageInfo, error) {
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
		JSONResponse(w, map[string]any{"items": []store.PackageInfo{}, "total": total})
		return
	}
	end := offset + limit
	if end > len(merged) {
		end = len(merged)
	}
	page := merged[offset:end]
	a.stampInertStatus(r.Context(), page)
	JSONResponse(w, map[string]any{"items": page, "total": total})
}

// sortMergedPackages orders a cross-network list. Block heights from different
// chains are not comparable, so the default ordering is by timestamp with height
// only as a tiebreaker within rows that have none.

func sortMergedPackages(pkgs []store.PackageInfo, sortBy string) {
	switch sortBy {
	case "calls":
		sort.SliceStable(pkgs, func(i, j int) bool { return pkgs[i].Calls > pkgs[j].Calls })
	case "importers":
		sort.SliceStable(pkgs, func(i, j int) bool { return pkgs[i].Importers > pkgs[j].Importers })
	case "imports":
		sort.SliceStable(pkgs, func(i, j int) bool { return pkgs[i].Imports > pkgs[j].Imports })
	case "users":
		sort.SliceStable(pkgs, func(i, j int) bool { return pkgs[i].UniqueUsers > pkgs[j].UniqueUsers })
	case "last_call":
		sort.SliceStable(pkgs, func(i, j int) bool {
			ti, tj := pkgs[i].LastCallTime, pkgs[j].LastCallTime
			if ti != "" && tj != "" {
				return ti > tj
			}
			if ti != tj {
				return ti != "" // ever-called rows sort ahead of never-called ones
			}
			return pkgs[i].BlockHeight > pkgs[j].BlockHeight
		})
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

// stampInertStatus flags each row currently in its own network's parked
// queue, so a reader sees "parked" in a list itself rather than only after
// clicking through to the realm's own detail page — see #194's own
// complaint that a parked package "is indistinguishable from a live one
// everywhere except its own detail page".
//
// Grouped by each row's own Network rather than one network picked for the
// whole call: a caller can mix networks (search, the merged all-networks
// package list), and each row must be checked against its own chain's
// queue, not whichever one happens to be selected.
//
// Best-effort like the queue endpoint it reads: a network whose queue
// cannot be fetched right now just leaves that network's rows unstamped,
// not an error for the whole list.
func (a *API) stampInertStatus(ctx context.Context, items []store.PackageInfo) {
	byNetwork := map[string][]int{}
	for i, item := range items {
		byNetwork[item.Network] = append(byNetwork[item.Network], i)
	}
	for network, idxs := range byNetwork {
		parked := a.parkedPaths(ctx, network)
		if len(parked) == 0 {
			continue
		}
		for _, i := range idxs {
			if parked[items[i].Path] {
				items[i].Status = PackageStatusInert
			}
		}
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
	// Derived here rather than stored: it is a pure function of the source the
	// detail already carries, so persisting it would add a column that can go
	// stale against the files beside it.
	files := make([]indexer.MemFile, 0, len(detail.Files))
	for _, f := range detail.Files {
		files = append(files, indexer.MemFile(f))
	}
	detail.ExportedFuncs = analyzer.ExportedFunctions(files)
	// Symbols lives on a wrapper rather than store.PackageDetail: the store
	// package cannot import analyzer's richer type without a cycle (analyzer
	// already imports store for DB access), the same reason ExportedFuncs
	// stayed a plain []string instead of something structured.
	JSONResponse(w, &realmDetailResponse{
		PackageDetail: detail,
		Symbols:       analyzer.ExtractSymbols(files),
	})
}

// realmDetailResponse adds the package's parsed symbol table to
// store.PackageDetail's JSON shape without either package needing to know
// about the other's types.
type realmDetailResponse struct {
	*store.PackageDetail
	Symbols analyzer.PackageSymbols `json:"symbols"`
}

// normalizeTxHash accepts a transaction hash in either encoding in circulation
// and returns the base64 form the indexer stores.
//
// The same 32 bytes are printed two ways: gno tooling and this explorer use
// base64 ("e6ChL6Trihr1GABwAWTvGOAkCGtNtvfhr4ZkoixrBAg="), while gnoscan.io and
// Tendermint-style RPC use 64 hex characters
// ("7BA0A12FA4EB8A1AF51800700164EF18E024086B4DB6F7E1AF8664A22C6B0408"). They
// are the same transaction, so pasting either one must resolve — the hex form
// used to 404 on a transaction we were holding all along.
//
// The two forms cannot be confused: base64 of 32 bytes is always 43 characters
// and a pad, never 64, so a 64-character string that decodes as hex is
// unambiguous. Anything else is handed through untouched for the indexer to
// reject, rather than guessed at.

func (a *API) HandleInertQueue(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	cached, err := FetchInertQueue(r.Context(), network, a.rpcURLFor(network))
	if err != nil && len(cached) == 0 {
		jsonError(w, err.Error(), 502)
		return
	}

	// Copied, not enriched in place: FetchInertQueue's return shares its
	// cache's backing array, and this handler's own block-time lookups are
	// a per-request concern (they need a.db/the indexer client, which
	// inert.go's cache does not have), not something to mutate into a
	// value other concurrent requests may be reading.
	queue := make([]InertPackage, len(cached))
	copy(queue, cached)
	heights := make([]int, 0, len(queue))
	for _, p := range queue {
		if p.Height > 0 {
			heights = append(heights, p.Height)
		}
	}
	bt := a.blockTimesForHeights(r.Context(), network, a.clientFor(network), heights)
	for i := range queue {
		queue[i].SubmittedTime = bt[queue[i].Height]
	}

	JSONResponse(w, map[string]any{"queue": queue})
}

// HandleInertHistory serves recent package-approval activity (every
// MsgEnablePackage and MsgRejectPackage the indexer has) plus queue-depth
// and approval-speed stats. "Speed" is measured from a resolved
// MsgEnablePackage: BlockHeight (when the enable landed) minus PkgHeight
// (the submission it approved, which MsgEnablePackage itself pins) — the
// only place that pairing exists, since a parked submission is otherwise
// silent between AddPackage and whatever eventually resolves it.

func (a *API) HandleInertHistory(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	limit := eventTxLimit(r)
	enabled, rejected := a.inertLifecycleEvents(r.Context(), network, limit)

	queue, _ := FetchInertQueue(r.Context(), network, a.rpcURLFor(network))
	stats := ComputeInertStats(len(queue), enabled, rejected)

	JSONResponse(w, map[string]any{
		"enabled":  enabled,
		"rejected": rejected,
		"stats":    stats,
	})
}

// HandleInertPackage serves one path's inert-lifecycle detail: its current
// vm/qpkgmeta_json status plus every AddPackage/EnablePackage/RejectPackage
// transaction naming it, chronological — a redeploy parked while an earlier
// submission at the same path was still pending shows as two distinct
// "submitted" entries, exactly as the chain recorded it.

func (a *API) HandleInertPackage(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	path := "gno.land/" + r.PathValue("path")

	meta, metaErr := fetchPackageMeta(r.Context(), a.rpcURLFor(network), path)
	if metaErr != nil {
		meta = InertPackage{Path: path, Status: PackageStatusAbsent}
	}

	var history []InertLifecycleEvent
	client := a.clientFor(network)
	if client != nil {
		if txs, err := client.GetPackageLifecycleTransactions(r.Context(), path, 200); err == nil {
			a.stampBlockTimes(r.Context(), network, client, txs)
			history = buildLifecycleHistory(txs)
		}
	}

	JSONResponse(w, map[string]any{"meta": meta, "history": history})
}

// inertLifecycleEvents fetches every MsgEnablePackage/MsgRejectPackage,
// normalizes them, and — for enables — resolves each one's wait time by
// looking up the block_time at both the enable's own height and the
// submission height it names.

func (a *API) inertLifecycleEvents(ctx context.Context, network string, limit int) (enabled, rejected []InertLifecycleEvent) {
	client := a.clientFor(network)
	if client == nil {
		return nil, nil
	}

	enableTxs, _ := client.GetPackageEnableTransactions(ctx, limit)
	rejectTxs, _ := client.GetPackageRejectTransactions(ctx, limit)
	a.stampBlockTimes(ctx, network, client, enableTxs)
	a.stampBlockTimes(ctx, network, client, rejectTxs)

	// Submission heights (MsgEnablePackage.PkgHeight) are not any of these
	// transactions' own block_height, so stampBlockTimes cannot resolve
	// them — a second, explicit lookup over that separate set of heights.
	var subHeights []int
	for _, tx := range enableTxs {
		for _, m := range tx.Messages {
			if m.Value.Typename == "MsgEnablePackage" && m.Value.PkgHeight > 0 {
				subHeights = append(subHeights, m.Value.PkgHeight)
			}
		}
	}
	subTimes := a.blockTimesForHeights(ctx, network, client, subHeights)

	for _, tx := range enableTxs {
		for _, m := range tx.Messages {
			if m.Value.Typename != "MsgEnablePackage" {
				continue
			}
			ev := InertLifecycleEvent{
				Kind: "enabled", TxHash: tx.Hash, BlockHeight: tx.BlockHeight, BlockTime: tx.BlockTime,
				PkgPath: m.Value.PkgPath, Actor: m.Value.Approver, SubmittedHeight: m.Value.PkgHeight,
			}
			if t := subTimes[m.Value.PkgHeight]; t != "" {
				ev.SubmittedTime = t
			}
			if ev.SubmittedHeight > 0 {
				ev.WaitBlocks = tx.BlockHeight - ev.SubmittedHeight
			}
			if tx.BlockTime != "" && ev.SubmittedTime != "" {
				if d, ok := secondsBetween(ev.SubmittedTime, tx.BlockTime); ok {
					ev.WaitSeconds = d
				}
			}
			enabled = append(enabled, ev)
		}
	}
	for _, tx := range rejectTxs {
		for _, m := range tx.Messages {
			if m.Value.Typename != "MsgRejectPackage" {
				continue
			}
			rejected = append(rejected, InertLifecycleEvent{
				Kind: "rejected", TxHash: tx.Hash, BlockHeight: tx.BlockHeight, BlockTime: tx.BlockTime,
				PkgPath: m.Value.PkgPath, Actor: m.Value.Sender,
			})
		}
	}
	return enabled, rejected
}

// buildLifecycleHistory turns a mixed AddPackage/EnablePackage/RejectPackage
// transaction list (oldest-relevant-first is not assumed — the caller
// windows by height DESC) into a chronological set of normalized events.

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

	JSONResponse(w, map[string]any{
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

func (a *API) HandleTimeSeriesPackages(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	days, granularity := a.resolveTimeseriesParams(r, network)
	pts, err := a.db.GetPackageTimeSeries(network, granularity, days)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	if pts == nil {
		pts = []store.PkgTimePoint{}
	}
	JSONResponse(w, pts)
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
		pts = []store.StorageTimePoint{}
	}
	JSONResponse(w, pts)
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
	JSONResponse(w, paths)
}

// HandleTimeSeriesRealmShare answers "where is chain activity concentrating,
// and is that changing" — the question every existing rollup cannot, because
// they are all-time snapshots.

func (a *API) HandleTimeSeriesRealmShare(w http.ResponseWriter, r *http.Request) {
	network := a.networkParam(r)
	days, granularity := a.resolveTimeseriesParams(r, network)

	metric := r.URL.Query().Get("metric")
	if metric == "" {
		metric = "fee"
	}
	if metric != "fee" && metric != "storage" {
		jsonError(w, "metric must be fee or storage", 400)
		return
	}

	pts, err := a.db.GetRealmShareTimeSeries(network, metric, granularity, days)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	if pts == nil {
		pts = []store.RealmSharePoint{}
	}
	JSONResponse(w, pts)
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
	JSONResponse(w, paths)
}

// HandleFunctionCallHeatmap serves one realm's function x day call grid. The
// range is fixed at funcHeatmapDays; ?window= and ?days= are not honoured,
// because the y-axis is functions and the x-axis is days either way.
