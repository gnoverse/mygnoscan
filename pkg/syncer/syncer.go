package syncer

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/moul/mygnoscan/pkg/analyzer"
	"github.com/moul/mygnoscan/pkg/indexer"
	"github.com/moul/mygnoscan/pkg/store"
)

type Syncer struct {
	client      *indexer.Client
	db          *store.DB
	analyzer    *analyzer.Analyzer
	networkID   string
	proposerIDs map[string]int64 // address -> interned id, memoised across passes

	// Block paging budget. Fields rather than constants so a test can bound a
	// pass to a handful of blocks: the paging behaviour under test is about the
	// budget being respected, not about its production size, and seeding 100k
	// real blocks to observe it is not a unit test.
	blockPageSize     int
	blockPagesPerPass int

	// blockHistoryDays caps how far back syncBlocks backfills. See the flag of
	// the same name in main.go for the sentinels. The zero value means
	// unlimited, so a Syncer built without setting it behaves exactly as it did
	// before the cap existed.
	blockHistoryDays int
}

func NewSyncer(client *indexer.Client, db *store.DB, analyzer *analyzer.Analyzer, networkID string) *Syncer {
	return &Syncer{
		client: client, db: db, analyzer: analyzer, networkID: networkID,
		blockPageSize: defaultBlockPageSize, blockPagesPerPass: defaultBlockPagesPerPass,
	}
}

// SetBlockHistoryDays caps how far back the block backfill reaches. Zero or
// less means "no cap", the same reading the field has always had.
//
// A setter rather than a constructor argument: every caller but one wants the
// default, and threading it through NewSyncer would put a parameter nobody
// reads at every call site.
func (s *Syncer) SetBlockHistoryDays(days int) { s.blockHistoryDays = days }

// fingerprintKeyPrefix namespaces the stored chain fingerprint per network.
const fingerprintKeyPrefix = "chain_fingerprint:"

// SyncAll fetches all data from the indexer and processes it.
func (s *Syncer) SyncAll(ctx context.Context) error {
	if err := s.checkChainReset(ctx); err != nil {
		return fmt.Errorf("check chain reset: %w", err)
	}
	s.warnOnHeightRegression(ctx)
	s.syncBlocks(ctx)
	s.backfillBlockTimes(ctx)
	s.backfillTransactions(ctx)
	s.backfillValopers(ctx)
	s.backfillTokenTransfers(ctx)
	s.syncUsers(ctx)
	if err := s.syncPackages(ctx); err != nil {
		return fmt.Errorf("sync packages: %w", err)
	}
	if err := s.syncCalls(ctx); err != nil {
		return fmt.Errorf("sync calls: %w", err)
	}
	if err := s.syncMsgRuns(ctx); err != nil {
		return fmt.Errorf("sync msg runs: %w", err)
	}
	// Last: both fold rows the passes above have just written.
	if err := s.syncTransferEdges(); err != nil {
		return fmt.Errorf("sync transfer edges: %w", err)
	}
	if err := s.syncCallerEdges(); err != nil {
		return fmt.Errorf("sync caller edges: %w", err)
	}
	return nil
}

// syncTransferEdges folds newly-synced bank_sends rows into transfer_edges.
//
// Unlike every other pass this reads local SQLite rather than walking the
// indexer: bank_sends is already populated by syncCalls, so there is no fetch
// to make, only a local GROUP BY. No ctx for the same reason — there is no
// round trip to cancel.
func (s *Syncer) syncTransferEdges() error {
	last, _, err := s.db.TransferEdgesLastHeight(s.networkID)
	if err != nil {
		return fmt.Errorf("transfer edges cursor: %w", err)
	}
	rows, err := s.db.RollupBankSendsSince(s.networkID, last)
	if err != nil {
		return fmt.Errorf("rollup bank sends: %w", err)
	}
	if len(rows) == 0 {
		return nil
	}
	if err := s.db.UpsertTransferEdges(s.networkID, rows); err != nil {
		return fmt.Errorf("upsert transfer edges: %w", err)
	}
	log.Printf("[%s] syncTransferEdges: rolled up %d edges", s.networkID, len(rows))
	return nil
}

// syncCallerEdges folds newly-synced calls rows into caller_edges. Same
// local-read shape as syncTransferEdges.
func (s *Syncer) syncCallerEdges() error {
	last, _, err := s.db.CallerEdgesLastHeight(s.networkID)
	if err != nil {
		return fmt.Errorf("caller edges cursor: %w", err)
	}
	rows, err := s.db.RollupCallsSince(s.networkID, last)
	if err != nil {
		return fmt.Errorf("rollup calls: %w", err)
	}
	if len(rows) == 0 {
		return nil
	}
	if err := s.db.UpsertCallerEdges(s.networkID, rows); err != nil {
		return fmt.Errorf("upsert caller edges: %w", err)
	}
	log.Printf("[%s] syncCallerEdges: rolled up %d edges", s.networkID, len(rows))
	return nil
}

func (s *Syncer) upsertTx(tx indexer.Transaction, blockTime string) {
	gasFee := 0
	if tx.GasFee != nil {
		gasFee = tx.GasFee.Amount
	}
	s.db.UpsertTransaction(s.networkID, tx.Hash, tx.BlockHeight, blockTime, tx.GasUsed, tx.GasWanted, gasFee, tx.Success)
}

// fetchBlockTimes fetches block times from the indexer for all unique heights in txs.
func (s *Syncer) fetchBlockTimes(ctx context.Context, txs []indexer.Transaction) map[int]string {
	seen := make(map[int]bool)
	for _, tx := range txs {
		seen[tx.BlockHeight] = true
	}
	if len(seen) == 0 {
		return nil
	}
	heights := make([]int, 0, len(seen))
	for h := range seen {
		heights = append(heights, h)
	}
	m, err := s.client.GetBlocksByHeights(ctx, heights)
	if err != nil {
		log.Printf("[%s] fetchBlockTimes: %v", s.networkID, err)
		return nil
	}
	return m
}

// txPageFetcher fetches one page of transactions above a cursor, reporting
// whether more remain. See Client.transactionsFromHeight.
type txPageFetcher func(context.Context, *int) ([]indexer.Transaction, bool, error)

// walkTransactions feeds every transaction above cursor to process, one indexer
// page at a time.
//
// The indexer truncates a response at its element cap and says so, which makes
// the cap the page size: each page is the contiguous next stretch above the
// cursor, and the last row's height is where the following page starts. Threading
// the cursor through this loop rather than re-deriving it from stored rows each
// pass is what guarantees progress — a page whose rows land in none of the tables
// a cursor is derived from would otherwise be fetched forever.
func walkTransactions(
	ctx context.Context,
	cursor *int,
	fetch txPageFetcher,
	process func([]indexer.Transaction),
) error {
	for {
		txs, truncated, err := fetch(ctx, cursor)
		if err != nil {
			return err
		}
		if len(txs) == 0 {
			return nil
		}

		process(txs)

		if !truncated {
			return nil
		}
		next := txs[len(txs)-1].BlockHeight
		cursor = &next
	}
}

func (s *Syncer) syncPackages(ctx context.Context) error {
	lastHeight, err := s.getLastBlockHeight(ctx, "packages")
	if err != nil {
		return err
	}

	count := 0
	err = walkTransactions(ctx, lastHeight, s.client.GetAllPackages, func(txs []indexer.Transaction) {
		times := s.fetchBlockTimes(ctx, txs)
		for _, tx := range txs {
			bt := times[tx.BlockHeight]
			s.upsertTx(tx, bt)
			for msgIndex, msg := range tx.Messages {
				if msg.Value.Typename == "MsgAddPackage" && msg.Value.Package != nil {
					if err := s.analyzer.ProcessPackage(
						s.networkID,
						msg.Value.Package,
						msg.Value.Creator,
						tx.Hash,
						tx.BlockHeight,
						msgIndex,
						bt,
						tx.Success,
					); err != nil {
						log.Printf("[%s] process package %s: %v", s.networkID, msg.Value.Package.Path, err)
						continue
					}
					count++
				}
			}
		}
	})
	log.Printf("[%s] synced %d packages", s.networkID, count)
	return err
}

// backfillBatch bounds how many block heights are repaired per sync pass. The
// work is spread across passes rather than done in one burst so a large gap
// cannot stall startup or hammer a public indexer.
const backfillBatch = 200

// Transaction repair is one indexer request per block, so it moves in smaller steps.
const (
	backfillTxBatch     = 100
	backfillConcurrency = 10
)

// backfillBlockTimes fills in block_time for rows written before that column
// existed.
//
// Incremental sync only moves forward from the cursor, so historical rows would
// otherwise never get a timestamp. That matters twice over: rows without a
// timestamp cannot be ordered against another chain's rows in a merged view, and
// list endpoints have to ask the indexer for block times at request time instead
// of reading what is already stored.
//
// Best-effort by design — failures are logged and retried on the next pass
// rather than failing the sync.
func (s *Syncer) backfillBlockTimes(ctx context.Context) {
	heights, err := s.db.HeightsMissingBlockTime(s.networkID, backfillBatch)
	if err != nil {
		log.Printf("[%s] backfill: %v", s.networkID, err)
		return
	}
	if len(heights) == 0 {
		return
	}

	times, err := s.client.GetBlockTimesForHeights(ctx, heights)
	if err != nil {
		log.Printf("[%s] backfill: fetching %d block times: %v", s.networkID, len(heights), err)
		return
	}
	if len(times) == 0 {
		return
	}

	updated, err := s.db.SetBlockTimes(s.networkID, times)
	if err != nil {
		log.Printf("[%s] backfill: %v", s.networkID, err)
		return
	}
	log.Printf("[%s] backfilled block_time on %d rows across %d blocks", s.networkID, updated, len(times))
}

// backfillTransactions fills in transaction rows for history that predates the
// transactions table.
//
// Event tables record what happened, but gas and fee figures live only on the
// transaction row. Without this, all-time gas totals computed from local storage
// silently under-report — on a live instance, 37 transactions out of 2738.
//
// Gas cannot be reconstructed from what is already stored, so the rows have to
// come back from the indexer. Bounded per pass, newest first, same as the
// block_time repair.
func (s *Syncer) backfillTransactions(ctx context.Context) {
	heights, err := s.db.HeightsMissingTransactions(s.networkID, backfillTxBatch)
	if err != nil {
		log.Printf("[%s] transaction backfill: %v", s.networkID, err)
		return
	}
	if len(heights) == 0 {
		return
	}

	// One request per block, run concurrently: sequential fetches made a large
	// gap take the better part of an hour to close.
	type blockTxs struct {
		txs []indexer.Transaction
		err error
	}
	results := make([]blockTxs, len(heights))
	var wg sync.WaitGroup
	sem := make(chan struct{}, backfillConcurrency)
	for i, h := range heights {
		wg.Add(1)
		go func(i, h int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			txs, err := s.client.GetTransactionsByBlock(ctx, h)
			results[i] = blockTxs{txs: txs, err: err}
		}(i, h)
	}
	wg.Wait()

	var all []indexer.Transaction
	for _, r := range results {
		if r.err != nil {
			// An unhealthy indexer: keep what we have and retry the rest next pass.
			log.Printf("[%s] transaction backfill: %v", s.networkID, r.err)
			break
		}
		all = append(all, r.txs...)
	}
	if len(all) == 0 {
		return
	}

	blockTimes := s.fetchBlockTimes(ctx, all)
	rows := make([]store.TxRow, 0, len(all))
	for _, tx := range all {
		gasFee := 0
		if tx.GasFee != nil {
			gasFee = tx.GasFee.Amount
		}
		rows = append(rows, store.TxRow{
			Hash:        tx.Hash,
			BlockHeight: tx.BlockHeight,
			BlockTime:   blockTimes[tx.BlockHeight],
			GasUsed:     tx.GasUsed,
			GasWanted:   tx.GasWanted,
			GasFee:      gasFee,
			Success:     tx.Success,
		})
	}
	if err := s.db.UpsertTransactions(s.networkID, rows); err != nil {
		log.Printf("[%s] transaction backfill: %v", s.networkID, err)
		return
	}
	log.Printf("[%s] backfilled %d transactions across %d blocks", s.networkID, len(rows), len(heights))
}

// chainFingerprint identifies a specific chain instance by its first block.
// The chain ID alone is not enough: a reset network keeps its chain ID and comes
// back with a different block 1, which is exactly what portal-loop and staging
// style networks do.
func (s *Syncer) chainFingerprint(ctx context.Context) (string, error) {
	block, err := s.client.GetBlock(ctx, 1)
	if err != nil {
		return "", err
	}
	if block == nil {
		return "", errors.New("block 1 not found")
	}
	if block.Hash == "" {
		return "", errors.New("block 1 has no hash")
	}
	return block.ChainID + ":" + block.Hash, nil
}

// checkChainReset wipes locally stored data when the indexer is no longer serving
// the chain that data came from.
//
// Sync cursors are derived from the highest stored block height, so after a reset
// to a lower height the cursor sits above the new tip permanently: every pass asks
// for blocks that do not exist yet, stores nothing, and reports success. The
// network silently freezes while continuing to serve pre-reset rows whose heights
// and tx hashes no longer refer to anything on that chain.
//
// Detection is by fingerprint rather than by height comparison on purpose. A
// lagging indexer replica also reports a tip below what we have stored, and
// wiping a large chain because a replica was behind would be far worse than the
// bug being fixed.
func (s *Syncer) checkChainReset(ctx context.Context) error {
	key := fingerprintKeyPrefix + s.networkID

	current, err := s.chainFingerprint(ctx)
	if err != nil {
		// Never wipe on uncertainty: an unreachable indexer or a pruned block 1
		// is not evidence of a reset.
		log.Printf("[%s] chain fingerprint unavailable, skipping reset check: %v", s.networkID, err)
		return nil
	}

	stored, err := s.db.GetSyncState(key)
	if err != nil {
		return fmt.Errorf("read chain fingerprint: %w", err)
	}

	switch stored {
	case "":
		// First sync for this network, or a database predating this check.
		return s.db.SetSyncState(key, current)
	case current:
		return nil
	}

	log.Printf("[%s] CHAIN RESET DETECTED: stored chain %q is no longer served (indexer now serves %q); discarding local data for this network",
		s.networkID, stored, current)

	deleted, err := s.db.DeleteNetworkData(s.networkID)
	if err != nil {
		return fmt.Errorf("discard data after chain reset: %w", err)
	}
	// The proposer memo now points at deleted rows. Keeping it would write every
	// subsequent block with a dangling proposer_id, and GetBlockProposers joins
	// on that id, so those blocks would silently vanish from the aggregate.
	s.proposerIDs = nil
	log.Printf("[%s] removed %d rows, re-syncing from the new genesis", s.networkID, deleted)

	return s.db.SetSyncState(key, current)
}

// warnOnHeightRegression reports the lagging-replica case: the same chain, but an
// indexer tip below what is already stored, so sync cannot advance until it
// catches up. Not an error, and deliberately not a reason to discard data.
func (s *Syncer) warnOnHeightRegression(ctx context.Context) {
	remote, err := s.client.LatestBlockHeight(ctx)
	if err != nil {
		return
	}
	stored, err := s.db.MaxBlockHeight(s.networkID)
	if err != nil || stored == 0 {
		return
	}
	if remote < stored {
		log.Printf("[%s] indexer tip %d is below stored height %d on the same chain; sync will not advance until the indexer catches up",
			s.networkID, remote, stored)
	}
}

// Page size measured against the live indexer: 5,000 blocks in ~500ms / 684KB.
// The per-pass budget bounds one SyncAll pass to ~100k blocks (~10s) so a full
// 3.3M-block backfill spreads over ~33 passes instead of stalling package,
// call and msg-run syncing behind a single 5-6 minute run.
const (
	defaultBlockPageSize     = 5000
	defaultBlockPagesPerPass = 20
)

// blocksBackfillDepthKey namespaces the -block-history-days value the backfill
// stopped at, when it stopped because of the cap. See markBackfillDone and
// shouldResumeBackfill.
func blocksBackfillDepthKey(network string) string {
	return "blocks_backfill_depth:" + network
}

// blockHistoryCutoff is the oldest block time worth backfilling, and whether a
// cutoff applies at all. See main.go's -block-history-days.
func (s *Syncer) blockHistoryCutoff() (time.Time, bool) {
	if s.blockHistoryDays <= 0 {
		return time.Time{}, false
	}
	return time.Now().UTC().AddDate(0, 0, -s.blockHistoryDays), true
}

// syncBlocks keeps the blocks table current and backfills history.
//
// Two cursors, both derived from the table itself: head sync walks forward from
// MAX(height) to the tip, backfill walks backward from MIN(height). Because the
// table always holds a contiguous height range, neither cursor can be fooled by
// a gap — nothing may insert blocks outside that range.
//
// Backward rather than forward: filling oldest-first would leave the dashboard's
// default 90d window empty until the backfill nearly finished.
func (s *Syncer) syncBlocks(ctx context.Context) {
	// A negative depth declines block persistence outright: no seeding, no head
	// sync, no backfill. The block charts then render empty, which is the
	// operator's stated choice.
	if s.blockHistoryDays < 0 {
		return
	}

	tip, err := s.client.LatestBlockHeight(ctx)
	if err != nil {
		log.Printf("[%s] syncBlocks: tip: %v", s.networkID, err)
		return
	}

	minH, maxH, ok, err := s.db.BlockHeightBounds(s.networkID)
	if err != nil {
		log.Printf("[%s] syncBlocks: bounds: %v", s.networkID, err)
		return
	}

	// One budget shared across seeding, head sync, and backfill: the point of
	// bounding a pass is a cap on total blocks fetched, not just on the
	// backfill loop, so every phase below draws from the same pagesLeft.
	pagesLeft := s.blockPagesPerPass

	if !ok {
		// Seed at the tip so recent windows populate immediately.
		from := tip - s.blockPageSize + 1
		if from < 1 {
			from = 1
		}
		if !s.fetchBlockPage(ctx, from, tip) {
			return
		}
		pagesLeft--
		minH, maxH, ok, err = s.db.BlockHeightBounds(s.networkID)
		if err != nil || !ok {
			return
		}
	}

	// Head sync: catch up to the tip.
	if maxH < tip && pagesLeft > 0 {
		to := maxH + s.blockPageSize
		if to > tip {
			to = tip
		}
		s.fetchBlockPage(ctx, maxH+1, to)
		pagesLeft--
	}

	// Backfill: walk down until genesis or an empty page, bounded per pass.
	if done, _ := s.db.GetSyncState(store.BlocksBackfillDoneKey(s.networkID)); done == "1" {
		if !s.shouldResumeBackfill() {
			return
		}
		// The operator raised -block-history-days (including to 0, unlimited)
		// past the depth this backfill stopped at. Clear the flag and fall
		// through into the same pass instead of waiting for the next one, so
		// resuming doesn't cost an extra 30s cycle.
		log.Printf("[%s] syncBlocks: -block-history-days raised past the recorded cap, resuming backfill", s.networkID)
		if err := s.db.SetSyncState(store.BlocksBackfillDoneKey(s.networkID), ""); err != nil {
			log.Printf("[%s] syncBlocks: clear backfill done flag: %v", s.networkID, err)
			return
		}
	}
	cutoff, capped := s.blockHistoryCutoff()
	if capped {
		// Checked before the first page as well as after each one, so a pass
		// that starts already past the cutoff (e.g. the operator lowered the
		// depth between restarts) terminates instead of fetching one more page.
		if oldest, ok, err := s.db.OldestBlockTime(s.networkID); err == nil && ok && oldest.Before(cutoff) {
			s.markBackfillDone("history cap reached", minH, s.blockHistoryDays)
			return
		}
	}
	reachedFloor := false
	reachedCap := false
	for i := 0; i < pagesLeft && minH > 1; i++ {
		to := minH - 1
		from := to - s.blockPageSize + 1
		if from < 1 {
			from = 1
		}
		if !s.fetchBlockPage(ctx, from, to) {
			return
		}
		newMin, _, ok, err := s.db.BlockHeightBounds(s.networkID)
		if err != nil || !ok {
			return
		}
		if newMin >= minH {
			// The page returned nothing new. That alone doesn't distinguish "the
			// indexer prunes below here" from "a replica still catching up (or a
			// load balancer fronting a partially-populated node) served an empty
			// range this once" — both come back as an empty getBlocks: [] with
			// HTTP 200. Marking done on the wrong one is worse than not marking
			// it at all: the coverage endpoint would then report complete: true
			// while history is actually missing, silently and permanently, since
			// nothing ever revisits a range once backfill has passed it.
			//
			// Confirm with a direct probe at the boundary height. A retry here
			// costs one query and, on a genuinely pruned floor, would come back
			// empty again anyway.
			block, perr := s.client.GetBlock(ctx, minH-1)
			if perr != nil {
				log.Printf("[%s] syncBlocks: floor probe at height %d failed: %v; retrying next pass", s.networkID, minH-1, perr)
				return
			}
			if block != nil {
				log.Printf("[%s] syncBlocks: empty page %d-%d but probe found height %d; treating as transient, retrying next pass", s.networkID, from, to, minH-1)
				return
			}
			reachedFloor = true
			break
		}
		minH = newMin
		if capped {
			oldest, ok, err := s.db.OldestBlockTime(s.networkID)
			if err == nil && ok && oldest.Before(cutoff) {
				reachedCap = true
				break
			}
		}
	}
	// A several-hundred-megabyte backfill that logs only failures gives an
	// operator no way to tell "still working" from "silently stopped" short of
	// reading sync_state out of SQLite. One line per pass is cheap.
	log.Printf("[%s] syncBlocks: backfill at height %d (tip %d)", s.networkID, minH, tip)
	switch {
	case reachedCap:
		s.markBackfillDone(fmt.Sprintf("history cap of %dd reached", s.blockHistoryDays), minH, s.blockHistoryDays)
	case reachedFloor:
		s.markBackfillDone("pruned floor confirmed by probe", minH, 0)
	case minH <= 1:
		s.markBackfillDone("reached genesis", minH, 0)
	}
}

// markBackfillDone marks the block backfill as finished and records the depth
// it stopped at.
//
// depth is the configured -block-history-days value when the stop reason was
// the history cap, and 0 for every other reason (genesis reached, or a pruned
// floor confirmed by probe): those two have nothing deeper to fetch no matter
// what -block-history-days is set to, so recording 0 clears any cap depth left
// over from an earlier, shallower run and keeps shouldResumeBackfill from
// trying to resume a backfill that cannot go any further.
func (s *Syncer) markBackfillDone(reason string, floor, depth int) {
	if err := s.db.SetSyncState(store.BlocksBackfillDoneKey(s.networkID), "1"); err != nil {
		log.Printf("[%s] syncBlocks: mark done: %v", s.networkID, err)
		return
	}
	depthVal := ""
	if depth > 0 {
		depthVal = strconv.Itoa(depth)
	}
	if err := s.db.SetSyncState(blocksBackfillDepthKey(s.networkID), depthVal); err != nil {
		log.Printf("[%s] syncBlocks: record backfill depth: %v", s.networkID, err)
	}
	log.Printf("[%s] syncBlocks: backfill done (%s), floor height %d", s.networkID, reason, floor)
}

// shouldResumeBackfill reports whether a backfill previously marked done
// should resume because the operator raised -block-history-days past the
// depth it stopped at (including raising it to 0, meaning unlimited).
//
// Only a cap-terminated backfill ever records a depth (see markBackfillDone):
// one that stopped at genesis or a confirmed pruned floor has nothing deeper
// to fetch regardless of how the flag changes, so a missing or zero recorded
// depth means "do not resume" — there is nothing to resume.
func (s *Syncer) shouldResumeBackfill() bool {
	recorded, err := s.db.GetSyncState(blocksBackfillDepthKey(s.networkID))
	if err != nil || recorded == "" {
		return false
	}
	recordedDepth, err := strconv.Atoi(recorded)
	if err != nil || recordedDepth <= 0 {
		return false
	}
	if s.blockHistoryDays == 0 {
		return true // unlimited is deeper than any finite recorded cap
	}
	return s.blockHistoryDays > recordedDepth
}

// proposerID returns the interned id for an address, memoised in the syncer so
// a 5,000-block page costs one query per *distinct* proposer instead of two per
// block. gno.land runs a handful of validators, so the map stays tiny.
func (s *Syncer) proposerID(address string) (int64, error) {
	if s.proposerIDs == nil {
		s.proposerIDs = make(map[string]int64)
	}
	if id, ok := s.proposerIDs[address]; ok {
		return id, nil
	}
	id, err := s.db.InternProposer(s.networkID, address)
	if err != nil {
		return 0, err
	}
	s.proposerIDs[address] = id
	return id, nil
}

// fetchBlockPage stores one height range. Returns false when the page failed,
// so the caller stops and retries next pass rather than spinning.
//
// The whole page is written in one UpsertBlocks call: per-row writes would hold
// and release the write lock 5,000 times, and the comment on UpsertTransactions
// records that read requests already queue behind a per-row backfill of a
// hundred rows.
//
// A page is all-or-nothing: if any row's proposer lookup fails, the page is
// abandoned rather than written with that row missing. Writing the rest would
// leave a silent hole in the stored range that no later pass ever revisits —
// head sync only extends above MAX(height) and backfill only extends below
// MIN(height), so a gap in the middle is permanent and undetectable. Retrying
// the whole page next pass costs nothing (UpsertBlocks is idempotent) and
// keeps the contiguous-range invariant the two cursors depend on.
func (s *Syncer) fetchBlockPage(ctx context.Context, from, to int) bool {
	blocks, err := s.client.GetBlocksInRange(ctx, from, to)
	if err != nil {
		log.Printf("[%s] syncBlocks: range %d-%d: %v", s.networkID, from, to, err)
		return false
	}
	if len(blocks) == 0 {
		return true
	}

	rows := make([]store.BlockRow, 0, len(blocks))
	for _, b := range blocks {
		var pid int64
		if b.ProposerAddressRaw != "" {
			pid, err = s.proposerID(b.ProposerAddressRaw)
			if err != nil {
				log.Printf("[%s] syncBlocks: intern proposer for range %d-%d: %v", s.networkID, from, to, err)
				return false
			}
		}
		rows = append(rows, store.BlockRow{Height: b.Height, Time: b.Time, ProposerID: pid, NumTxs: b.NumTxs})
	}
	if err := s.db.UpsertBlocks(s.networkID, rows); err != nil {
		log.Printf("[%s] syncBlocks: upsert range %d-%d: %v", s.networkID, from, to, err)
		return false
	}
	return true
}

func (s *Syncer) getLastBlockHeight(ctx context.Context, tableName string) (*int, error) {
	return s.db.LastBlockHeight(ctx, tableName, s.networkID)
}

func (s *Syncer) getLastRecentTransactionBlockHeight(ctx context.Context) (*int, error) {
	return s.db.LastCallOrSendHeight(ctx, s.networkID)
}

// valopersMarker identifies the validator registration realm.
//
// Matched as a substring rather than a fixed path so a move between realms
// (gnops → gov, or a versioned path) does not silently stop recording. The word
// is specific enough that a false match is unlikely.
const valopersMarker = "valopers"

// looksLikeAddress reports whether s is a gno bech32 account address.
func looksLikeAddress(s string) bool {
	return strings.HasPrefix(s, "g1") && len(s) == 40
}

// valoperSubject reads the validator address and moniker out of a valopers call.
//
// The argument layout differs per function, and taking args[0] as the moniker —
// which is what the frontend used to do — is right for exactly one of them:
//
//	Register(moniker, description, serverType, address, pubkey)
//	UpdateMoniker(address, newMoniker)
//	UpdateDescription(address, description)
//	UpdateKeepRunning / UpdateServerType / UpdateSigningKey(address, value)
//	DeleteFromAuthList(address, address)
//
// So for everything except Register and UpdateMoniker, args[0] is an address,
// and the old code recorded addresses as names. It also keyed on the caller,
// which is wrong twice over: Register carries the validator address in its
// arguments, and an admin can update an entry that is not their own.
//
// moniker is empty when the call does not set a name — most of them do not.
func valoperSubject(msg indexer.TxMessage) (address, moniker string) {
	args := msg.Value.Args
	switch msg.Value.Func {
	case "Register":
		if len(args) > 0 {
			moniker = args[0]
		}
		// The address is an argument, but fall back to the caller for a shorter
		// call than the current ABI.
		if len(args) > 3 && looksLikeAddress(args[3]) {
			address = args[3]
		} else {
			address = msg.Value.Caller
		}
	case "UpdateMoniker":
		if len(args) > 1 {
			moniker = args[1]
		}
		fallthrough
	default:
		if address == "" {
			if len(args) > 0 && looksLikeAddress(args[0]) {
				address = args[0]
			} else {
				address = msg.Value.Caller
			}
		}
	}
	return address, moniker
}

// recordValoper stores a validator registration when a call is one.
//
// Best effort: this feeds a display nicety, and losing one must not interrupt a
// sync pass.
func (s *Syncer) recordValoper(tx indexer.Transaction, msg indexer.TxMessage, blockTime string) {
	if !strings.Contains(msg.Value.PkgPath, valopersMarker) {
		return
	}
	address, moniker := valoperSubject(msg)
	if address == "" {
		return
	}
	if err := s.db.InsertValoperRegistration(
		s.networkID, tx.Hash, tx.BlockHeight, blockTime,
		msg.Value.Caller, msg.Value.Func, address, moniker, tx.Success,
	); err != nil {
		log.Printf("[%s] record valoper: %v", s.networkID, err)
	}
}

// backfillValoperBatch bounds how many historical registrations are repaired per
// pass. Each is one keyed request, and there are only ever a few hundred in
// total, so this closes quickly.
const backfillValoperBatch = 50

// backfillValopers recovers monikers for valopers calls stored before
// registrations were recorded.
//
// The call rows have everything except the moniker, which lives only in the
// call's arguments. Those are recoverable one transaction at a time by hash,
// which is a keyed lookup — unlike filtering all of history by pkg_path, which
// costs ~30s on a busy chain however little it returns.
func (s *Syncer) backfillValopers(ctx context.Context) {
	hashes, err := s.db.ValoperCallsMissingRegistration(s.networkID, backfillValoperBatch)
	if err != nil {
		log.Printf("[%s] valoper backfill: %v", s.networkID, err)
		return
	}
	if len(hashes) == 0 {
		return
	}

	recorded := 0
	for _, h := range hashes {
		tx, err := s.client.GetTransactionByHash(ctx, h)
		if err != nil {
			// Keep what we have and retry the rest next pass.
			log.Printf("[%s] valoper backfill: %v", s.networkID, err)
			break
		}
		if tx == nil {
			continue
		}
		for _, msg := range tx.Messages {
			if msg.Value.Typename != "MsgCall" {
				continue
			}
			if !strings.Contains(msg.Value.PkgPath, valopersMarker) {
				continue
			}
			s.recordValoper(*tx, msg, tx.BlockTime)
			recorded++
		}
	}
	if recorded > 0 {
		log.Printf("[%s] backfilled %d valoper registrations", s.networkID, recorded)
	}
}

// recordStorageEvents persists a transaction's storage deposits and unlocks,
// returning how many it stored.
//
// Piggybacked on the call walk rather than given its own pass: txFieldsLight
// already selects these events, so they arrive with every transaction the
// syncer fetches and were simply being dropped. A separate pass would re-fetch
// the same transactions to read a field already in hand.
//
// Failures are logged and skipped rather than aborting the walk. A storage row
// is derived detail — losing one costs a number on a page, where abandoning the
// pass costs every call and send behind it.
// backfillTokenTransfers walks the history the token ledger never saw.
//
// recordTokenTransfers rides the sync walk, and its comment says that is why it
// "needs no separate backfill pass". That holds for a database built from
// genesis by a binary that already had the feature. Sync resumes from the
// highest stored height, so on a database that already existed the ledger can
// only ever fill from the moment the feature shipped, and TokenSummaries then
// presents that window as an exact supply: a holder who last moved tokens
// before the cutoff is invisible, and a token minted before it has a supply of
// whatever moved after.
//
// Same shape as the other repairs: a bounded batch per pass, a cursor that
// survives restarts, and a stop condition that is reached rather than guessed.
// The events are already selected by the sync query, so this is a re-walk and
// not a new field.
func (s *Syncer) backfillTokenTransfers(ctx context.Context) {
	from, to, more, err := s.db.TokenBackfillRange(s.networkID, backfillTxBatch)
	if err != nil {
		log.Printf("[%s] token transfer backfill: %v", s.networkID, err)
		return
	}
	if !more {
		return
	}

	type blockTxs struct {
		txs []indexer.Transaction
		err error
	}
	heights := make([]int, 0, to-from)
	for h := from; h < to; h++ {
		heights = append(heights, h)
	}
	results := make([]blockTxs, len(heights))
	var wg sync.WaitGroup
	sem := make(chan struct{}, backfillConcurrency)
	for i, h := range heights {
		wg.Add(1)
		go func(i, h int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			txs, err := s.client.GetTransactionsByBlock(ctx, h)
			results[i] = blockTxs{txs: txs, err: err}
		}(i, h)
	}
	wg.Wait()

	// The cursor only advances over heights that actually answered. An
	// unhealthy indexer must cost a retry, never a silent hole in the ledger
	// that nothing will ever come back for.
	// Collect first, then resolve block times in one pass, because that is
	// where the canonical walk gets them: tx.BlockTime is not populated by
	// GetTransactionsByBlock, and a row written with an empty block_time would
	// land in the ledger and break the very window the page states.
	var answered []indexer.Transaction
	done := from
	for i, r := range results {
		if r.err != nil {
			log.Printf("[%s] token transfer backfill at %d: %v", s.networkID, heights[i], r.err)
			break
		}
		answered = append(answered, r.txs...)
		done = heights[i] + 1
	}
	stored := 0
	if len(answered) > 0 {
		times := s.fetchBlockTimes(ctx, answered)
		for _, tx := range answered {
			stored += s.recordTokenTransfers(tx, times[tx.BlockHeight])
		}
	}
	if done == from {
		return // nothing answered; leave the cursor alone and retry next pass
	}
	if err := s.db.SetTokenBackfillCursor(s.networkID, done); err != nil {
		log.Printf("[%s] token transfer backfill cursor: %v", s.networkID, err)
		return
	}
	log.Printf("[%s] token transfer backfill: %d..%d, %d transfer(s) recovered",
		s.networkID, from, done-1, stored)
}

// recordTokenTransfers stores the GRC20 Transfer events in a transaction.
//
// Rides the walk syncCalls already does: the events are in the payload being
// iterated, with `attrs` already selected, so this costs no extra query and
// needs no separate backfill pass.
//
// Only Transfer is stored. Approval says what an address is *allowed* to move,
// which changes no balance and would inflate every count it appeared in.
//
// ⚠️ The token is the `token` attribute, never ev.PkgPath. Every GRC20 event on
// the chain reports the library path (gno.land/p/nt/grc20/v0) there, so keying
// on it would collapse every asset on the chain into one.
func (s *Syncer) recordTokenTransfers(tx indexer.Transaction, blockTime string) int {
	if tx.Response == nil || !tx.Success {
		// A failed transaction moved nothing. Its events are still reported,
		// and counting them would invent supply.
		return 0
	}
	stored := 0
	for i, ev := range tx.Response.Events {
		if ev.Typename != "GnoEvent" || ev.Type != "Transfer" {
			continue
		}
		var t store.TokenTransfer
		for _, attr := range ev.Attrs {
			switch attr.Key {
			case "token":
				t.Token = attr.Value
			case "from":
				t.From = attr.Value
			case "to":
				t.To = attr.Value
			case "value":
				t.Value = store.ParseTokenValue(attr.Value)
			}
		}
		if t.Token == "" {
			// A Transfer without a token attribute is not one this ledger can
			// attribute, and a row keyed on "" would pool every such event into
			// a token that does not exist.
			continue
		}
		t.BlockHeight, t.BlockTime = tx.BlockHeight, blockTime
		if err := s.db.InsertTokenTransfer(s.networkID, tx.Hash, i, t); err != nil {
			log.Printf("[%s] store token transfer: %v", s.networkID, err)
			continue
		}
		stored++
	}
	return stored
}

func (s *Syncer) recordStorageEvents(tx indexer.Transaction, blockTime string) int {
	if tx.Response == nil {
		return 0
	}
	stored := 0
	for i, ev := range tx.Response.Events {
		var kind string
		var fee int
		switch ev.Typename {
		case "StorageDepositEvent":
			kind = "deposit"
			if ev.FeeDelta != nil {
				fee = ev.FeeDelta.Amount
			}
		case "StorageUnlockEvent":
			kind = "unlock"
			// Signed, so that summing the column answers "what did storage
			// cost" without every reader having to know which kinds subtract.
			if ev.FeeRefund != nil {
				fee = -ev.FeeRefund.Amount
			}
		default:
			continue
		}
		// bytes_delta is stored exactly as the chain emits it, which means
		// negative for an unlock: keeper.go sets BytesDelta to the realm's
		// signed storage diff, and gnovm/stdlibs/chain/emit_event.go says so on
		// the field. Negating it here would make SUM(bytes_delta) add freed
		// bytes to used ones, and every aggregate over this table assumes the
		// sign is the chain's (GetStorageDeltaTimeSeries splits on > 0 and < 0).
		// TestStorageBytesAndFeeAgree pins the invariant that catches a flip.
		bytesDelta := ev.BytesDelta
		// The event index is over all events, not over storage events only:
		// it has to stay stable across passes, and filtering first would
		// renumber rows whenever the event list changed shape.
		if err := s.db.InsertStorageEvent(s.networkID, tx.Hash, i, ev.PkgPath,
			tx.BlockHeight, blockTime, kind, bytesDelta, fee); err != nil {
			log.Printf("[%s] store storage event: %v", s.networkID, err)
			continue
		}
		stored++
	}
	return stored
}

func (s *Syncer) syncCalls(ctx context.Context) error {
	lastHeight, err := s.getLastRecentTransactionBlockHeight(ctx)
	if err != nil {
		return fmt.Errorf("last synced call height: %w", err)
	}

	callCount, sendCount, storageCount, transferCount := 0, 0, 0, 0
	err = walkTransactions(ctx, lastHeight, s.client.GetTransactionsFromHeight, func(txs []indexer.Transaction) {
		times := s.fetchBlockTimes(ctx, txs)
		for _, tx := range txs {
			bt := times[tx.BlockHeight]
			s.upsertTx(tx, bt)
			storageCount += s.recordStorageEvents(tx, bt)
			transferCount += s.recordTokenTransfers(tx, bt)
			for i, msg := range tx.Messages {
				switch msg.Value.Typename {
				case "MsgCall":
					if err := s.analyzer.ProcessCall(
						s.networkID,
						tx.Hash, tx.BlockHeight, i,
						bt,
						msg.Value.Caller,
						msg.Value.PkgPath,
						msg.Value.Func,
						tx.Success,
					); err != nil {
						log.Printf("[%s] process call: %v", s.networkID, err)
						continue
					}
					callCount++
					// Args are available here and nowhere else — they are not
					// stored on the call row. Capture the moniker while we have it.
					s.recordValoper(tx, msg, bt)
				case "BankMsgSend":
					if err := s.db.InsertBankSend(
						s.networkID,
						tx.Hash, tx.BlockHeight,
						bt,
						msg.Value.FromAddress,
						msg.Value.ToAddress,
						msg.Value.Amount,
						tx.Success,
					); err != nil {
						log.Printf("[%s] process send: %v", s.networkID, err)
						continue
					}
					sendCount++
				}
			}
		}
	})
	log.Printf("[%s] synced %d calls, %d sends, %d storage events, %d token transfers",
		s.networkID, callCount, sendCount, storageCount, transferCount)
	if err != nil {
		return fmt.Errorf("walk transactions: %w", err)
	}
	return nil
}

func (s *Syncer) syncMsgRuns(ctx context.Context) error {
	lastHeight, err := s.getLastBlockHeight(ctx, "msg_runs")
	if err != nil {
		return err
	}

	count := 0
	err = walkTransactions(ctx, lastHeight, s.client.GetMsgRunTransactions, func(txs []indexer.Transaction) {
		times := s.fetchBlockTimes(ctx, txs)
		for _, tx := range txs {
			bt := times[tx.BlockHeight]
			s.upsertTx(tx, bt)
			for _, msg := range tx.Messages {
				if msg.Value.Typename == "MsgRun" && msg.Value.Package != nil {
					if err := s.analyzer.ProcessMsgRun(
						s.networkID,
						tx.Hash, tx.BlockHeight,
						bt,
						msg.Value.Caller,
						msg.Value.Package.Files,
						tx.Success,
					); err != nil {
						log.Printf("[%s] process msgrun: %v", s.networkID, err)
						continue
					}
					count++
				}
			}
		}
	})
	log.Printf("[%s] synced %d msg_runs", s.networkID, count)
	return err
}
