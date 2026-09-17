package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Inert-package lifecycle support.
//
// gno.land can run under an "inert" code_submission_policy: a new package
// submission is parked ("inert") rather than activated, and stays that way
// until an address in vm.params.pkg_approvers sends a MsgEnablePackage —
// usually the "gpao" oracle (github.com/gnolang/gno, contribs/gpao), a
// separate off-chain process that verifies parked packages and approves the
// ones that pass. Anyone — an approver, the package's own creator, or a live
// package's owner — can instead send MsgRejectPackage to drop a parked
// submission.
//
// gpao itself leaves no on-chain trace of *why* it approved or passed on a
// package: it is an off-chain process with its own local status API, not a
// realm, and this file cannot see into it. What the chain itself records,
// and what this file reads, is the current parked queue and each package's
// own status/reason (vm/qpkgmeta_json, vm/qinertpaths), plus the on-chain
// history of MsgAddPackage/MsgEnablePackage/MsgRejectPackage messages.
//
// See gnolang/gno's gno.land/pkg/sdk/vm/keeper_inert.go for the
// authoritative behavior this mirrors.

// Package statuses reported by vm/qpkgmeta_json, mirrored from
// gno.land/pkg/sdk/vm/keeper_inert.go's PackageStatus* constants.
const (
	PackageStatusLive   = "live"   // deployed and callable
	PackageStatusInert  = "inert"  // submitted, stored, awaiting an approver
	PackageStatusAbsent = "absent" // the chain holds nothing at this path
)

// InertPackage is one entry in the current parked queue, or one path's
// current status — the vm/qpkgmeta_json response shape, read live.
type InertPackage struct {
	Path   string `json:"path"`
	Status string `json:"status"`
	// Creator/Height/MaxDeposit describe the pending submission when Pending
	// is true, or the live package's own deploy when it is not.
	Creator string `json:"creator,omitempty"`
	Height  int    `json:"height,omitempty"`
	// SubmittedTime is resolved separately from Height by the API layer
	// (see HandleInertQueue) — this file has no indexer/DB access to look
	// block times up itself, only the RPC client.
	SubmittedTime string `json:"submitted_time,omitempty"`
	MaxDeposit    string `json:"max_deposit,omitempty"`
	// Reason explains why a parked (or pending-over-live) submission is not
	// callable yet, in terms its submitter can act on.
	Reason  string `json:"reason,omitempty"`
	Pending bool   `json:"pending,omitempty"`
}

// InertLifecycleEvent is one MsgEnablePackage or MsgRejectPackage,
// normalized to a common shape for a history feed.
type InertLifecycleEvent struct {
	Kind        string `json:"kind"` // "enabled" or "rejected"
	TxHash      string `json:"tx_hash"`
	BlockHeight int    `json:"block_height"`
	BlockTime   string `json:"block_time"`
	PkgPath     string `json:"pkg_path"`
	// Actor is the approver (enabled) or sender (rejected).
	Actor string `json:"actor"`
	// SubmittedHeight/SubmittedTime are set only for "enabled" events: the
	// block the approved submission was parked at
	// (MsgEnablePackage.PkgHeight), so a reader can see how long it waited.
	SubmittedHeight int    `json:"submitted_height,omitempty"`
	SubmittedTime   string `json:"submitted_time,omitempty"`
	// WaitBlocks/WaitSeconds are BlockHeight-SubmittedHeight and the
	// corresponding wall-clock gap, filled in once both timestamps are
	// known (see HandleInertHistory).
	WaitBlocks  int     `json:"wait_blocks,omitempty"`
	WaitSeconds float64 `json:"wait_seconds,omitempty"`
}

// InertStats summarizes queue depth and how quickly parked packages are
// getting approved.
type InertStats struct {
	QueueLength        int     `json:"queue_length"`
	EnabledCount       int     `json:"enabled_count"`
	RejectedCount      int     `json:"rejected_count"`
	MedianWaitBlocks   int     `json:"median_wait_blocks,omitempty"`
	AverageWaitBlocks  float64 `json:"average_wait_blocks,omitempty"`
	MedianWaitSeconds  float64 `json:"median_wait_seconds,omitempty"`
	AverageWaitSeconds float64 `json:"average_wait_seconds,omitempty"`
}

// fetchInertPaths lists paths currently parked, from vm/qinertpaths — bare
// paths, newline separated, empty data meaning "no prefix filter". limit
// mirrors gno.land's own ?limit= query-string convention on this path.
func fetchInertPaths(ctx context.Context, rpcURL string, limit int) ([]string, error) {
	md, err := fetchABCIQuery(ctx, rpcURL, fmt.Sprintf("vm/qinertpaths?limit=%d", limit), "")
	if err != nil {
		return nil, err
	}
	if md == "" {
		return nil, nil
	}
	lines := strings.Split(md, "\n")
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		if l != "" {
			out = append(out, l)
		}
	}
	return out, nil
}

// fetchPackageMeta reads vm/qpkgmeta_json for one path: live, parked, or
// absent, with whatever the chain stamped at submit time. An unknown path
// is a successful "absent" response, not an error.
func fetchPackageMeta(ctx context.Context, rpcURL, pkgPath string) (InertPackage, error) {
	data, err := fetchABCIQuery(ctx, rpcURL, "vm/qpkgmeta_json", pkgPath)
	if err != nil {
		return InertPackage{}, err
	}
	var meta InertPackage
	if err := json.Unmarshal([]byte(data), &meta); err != nil {
		return InertPackage{}, err
	}
	return meta, nil
}

const inertCacheTTL = 20 * time.Second

var inertQueueCache = struct {
	mu      sync.Mutex
	byNet   map[string][]InertPackage
	fetched map[string]time.Time
}{byNet: map[string][]InertPackage{}, fetched: map[string]time.Time{}}

// inertQueueFetchLimit bounds how many parked paths vm/qinertpaths returns.
// gnoland-1's queue has run into the dozens-to-hundreds before an operator
// noticed (see the gpao spend-bound bug this was written against) — high
// enough to cover that without paging, capped so a much larger queue cannot
// make this endpoint enumerate the whole thing on every cache miss.
const inertQueueFetchLimit = 2000

// FetchInertQueue lists every currently-parked package with its metadata,
// best-effort and cached. Each path needs its own vm/qpkgmeta_json round
// trip — vm/qinertpaths returns bare paths — so this fans them out
// concurrently rather than one at a time.
func FetchInertQueue(ctx context.Context, network, rpcURL string) ([]InertPackage, error) {
	inertQueueCache.mu.Lock()
	if cached, ok := inertQueueCache.byNet[network]; ok && time.Since(inertQueueCache.fetched[network]) < inertCacheTTL {
		inertQueueCache.mu.Unlock()
		return cached, nil
	}
	inertQueueCache.mu.Unlock()

	paths, err := fetchInertPaths(ctx, rpcURL, inertQueueFetchLimit)
	if err != nil {
		inertQueueCache.mu.Lock()
		cached := inertQueueCache.byNet[network]
		inertQueueCache.mu.Unlock()
		return cached, err
	}

	out := make([]InertPackage, len(paths))
	// 20 measured ~1.3s cold against gnoland-1's live queue (92 paths, 5
	// sequential rounds at the RPC's own per-call latency) — the dominant
	// cost switching to the "inert queue" tab pays. Each round trip is one
	// lightweight abci_query the node answers from local state, not a write,
	// so a wider fan-out is safe; this cuts the same queue to ~2 rounds.
	const maxConcurrent = 60
	sem := make(chan struct{}, maxConcurrent)
	var wg sync.WaitGroup
	for i, p := range paths {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, path string) {
			defer wg.Done()
			defer func() { <-sem }()
			meta, err := fetchPackageMeta(ctx, rpcURL, path)
			if err != nil {
				meta = InertPackage{Path: path, Status: PackageStatusInert}
			}
			out[i] = meta
		}(i, p)
	}
	wg.Wait()

	// Newest submission first — the order a reader wants for "what's
	// waiting on an approver right now".
	sort.Slice(out, func(i, j int) bool { return out[i].Height > out[j].Height })

	inertQueueCache.mu.Lock()
	inertQueueCache.byNet[network] = out
	inertQueueCache.fetched[network] = time.Now()
	inertQueueCache.mu.Unlock()
	return out, nil
}

// ComputeInertStats summarizes queue depth and approval speed from a queue
// snapshot and resolved enable/reject events (WaitBlocks/WaitSeconds already
// filled in — see HandleInertHistory).
func ComputeInertStats(queueLen int, enabled, rejected []InertLifecycleEvent) InertStats {
	stats := InertStats{
		QueueLength:   queueLen,
		EnabledCount:  len(enabled),
		RejectedCount: len(rejected),
	}
	var blocks []int
	var seconds []float64
	for _, e := range enabled {
		if e.WaitBlocks > 0 {
			blocks = append(blocks, e.WaitBlocks)
		}
		if e.WaitSeconds > 0 {
			seconds = append(seconds, e.WaitSeconds)
		}
	}
	if len(blocks) > 0 {
		sort.Ints(blocks)
		sum := 0
		for _, b := range blocks {
			sum += b
		}
		stats.AverageWaitBlocks = float64(sum) / float64(len(blocks))
		stats.MedianWaitBlocks = blocks[len(blocks)/2]
	}
	if len(seconds) > 0 {
		sort.Float64s(seconds)
		sum := 0.0
		for _, s := range seconds {
			sum += s
		}
		stats.AverageWaitSeconds = sum / float64(len(seconds))
		stats.MedianWaitSeconds = seconds[len(seconds)/2]
	}
	return stats
}
