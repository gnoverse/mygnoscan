package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"sync"
)

type Syncer struct {
	client      *IndexerClient
	db          *DB
	analyzer    *Analyzer
	networkID   string
	proposerIDs map[string]int64 // address -> interned id, memoised across passes
}

func NewSyncer(client *IndexerClient, db *DB, analyzer *Analyzer, networkID string) *Syncer {
	return &Syncer{client: client, db: db, analyzer: analyzer, networkID: networkID}
}

// fingerprintKeyPrefix namespaces the stored chain fingerprint per network.
const fingerprintKeyPrefix = "chain_fingerprint:"

// SyncAll fetches all data from the indexer and processes it.
func (s *Syncer) SyncAll(ctx context.Context) error {
	if err := s.checkChainReset(ctx); err != nil {
		return fmt.Errorf("checkChainReset error: %w", err)
	}
	s.warnOnHeightRegression(ctx)
	s.syncBlocks(ctx)
	s.backfillBlockTimes(ctx)
	s.backfillTransactions(ctx)
	if err := s.syncPackages(ctx); err != nil {
		return fmt.Errorf("syncPackages error: %w", err)
	}
	if err := s.syncCalls(ctx); err != nil {
		return fmt.Errorf("sync calls error: %w", err)
	}
	return s.syncMsgRuns(ctx)
}

func (s *Syncer) upsertTx(tx Transaction, blockTime string) {
	gasFee := 0
	if tx.GasFee != nil {
		gasFee = tx.GasFee.Amount
	}
	s.db.UpsertTransaction(s.networkID, tx.Hash, tx.BlockHeight, blockTime, tx.GasUsed, tx.GasWanted, gasFee, tx.Success)
}

// fetchBlockTimes fetches block times from the indexer for all unique heights in txs.
func (s *Syncer) fetchBlockTimes(ctx context.Context, txs []Transaction) map[int]string {
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
// whether more remain. See IndexerClient.transactionsFromHeight.
type txPageFetcher func(context.Context, *int) ([]Transaction, bool, error)

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
	process func([]Transaction),
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
	err = walkTransactions(ctx, lastHeight, s.client.GetAllPackages, func(txs []Transaction) {
		times := s.fetchBlockTimes(ctx, txs)
		for _, tx := range txs {
			bt := times[tx.BlockHeight]
			s.upsertTx(tx, bt)
			for _, msg := range tx.Messages {
				if msg.Value.Typename == "MsgAddPackage" && msg.Value.Package != nil {
					if err := s.analyzer.ProcessPackage(
						s.networkID,
						msg.Value.Package,
						msg.Value.Creator,
						tx.Hash,
						tx.BlockHeight,
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
		txs []Transaction
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

	var all []Transaction
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
	rows := make([]TxRow, 0, len(all))
	for _, tx := range all {
		gasFee := 0
		if tx.GasFee != nil {
			gasFee = tx.GasFee.Amount
		}
		rows = append(rows, TxRow{
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
	blockPageSize     = 5000
	blockPagesPerPass = 20
)

func blocksBackfillDoneKey(network string) string {
	return "blocks_backfill_done:" + network
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
	pagesLeft := blockPagesPerPass

	if !ok {
		// Seed at the tip so recent windows populate immediately.
		from := tip - blockPageSize + 1
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
		to := maxH + blockPageSize
		if to > tip {
			to = tip
		}
		s.fetchBlockPage(ctx, maxH+1, to)
		pagesLeft--
	}

	// Backfill: walk down until genesis or an empty page, bounded per pass.
	if done, _ := s.db.GetSyncState(blocksBackfillDoneKey(s.networkID)); done == "1" {
		return
	}
	reachedFloor := false
	for i := 0; i < pagesLeft && minH > 1; i++ {
		to := minH - 1
		from := to - blockPageSize + 1
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
	}
	if minH <= 1 || reachedFloor {
		if err := s.db.SetSyncState(blocksBackfillDoneKey(s.networkID), "1"); err != nil {
			log.Printf("[%s] syncBlocks: mark done: %v", s.networkID, err)
			return
		}
		reason := "reached genesis"
		if reachedFloor {
			reason = "pruned floor confirmed by probe"
		}
		log.Printf("[%s] syncBlocks: backfill done (%s), floor height %d", s.networkID, reason, minH)
	}
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

	rows := make([]BlockRow, 0, len(blocks))
	for _, b := range blocks {
		var pid int64
		if b.ProposerAddressRaw != "" {
			pid, err = s.proposerID(b.ProposerAddressRaw)
			if err != nil {
				log.Printf("[%s] syncBlocks: intern proposer for range %d-%d: %v", s.networkID, from, to, err)
				return false
			}
		}
		rows = append(rows, BlockRow{Height: b.Height, Time: b.Time, ProposerID: pid, NumTxs: b.NumTxs})
	}
	if err := s.db.UpsertBlocks(s.networkID, rows); err != nil {
		log.Printf("[%s] syncBlocks: upsert range %d-%d: %v", s.networkID, from, to, err)
		return false
	}
	return true
}

func (s *Syncer) getLastBlockHeight(ctx context.Context, tableName string) (*int, error) {
	var lastHeight int
	err := s.db.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT block_height 
	FROM %s 
	WHERE network = $1
	ORDER BY block_height DESC 
	LIMIT 1`, tableName), s.networkID).Scan(&lastHeight)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("error getting last block height: %w", err)
	}
	return &lastHeight, nil
}

func (s *Syncer) getLastRecentTransactionBlockHeight(ctx context.Context) (*int, error) {
	var lastHeight int
	err := s.db.db.QueryRowContext(ctx, `SELECT block_height
		FROM calls
		WHERE network = $1 
		UNION
		SELECT block_height
		FROM bank_sends
		WHERE network = $1
		ORDER BY block_height DESC 
		LIMIT 1`, s.networkID).Scan(&lastHeight)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("error getting last block height: %w", err)
	}
	return &lastHeight, nil
}

func (s *Syncer) syncCalls(ctx context.Context) error {
	lastHeight, err := s.getLastRecentTransactionBlockHeight(ctx)
	if err != nil {
		return fmt.Errorf("getLastRecentTransactionBlockHeight error : %w", err)
	}

	callCount, sendCount := 0, 0
	err = walkTransactions(ctx, lastHeight, s.client.GetTransactionsFromHeight, func(txs []Transaction) {
		times := s.fetchBlockTimes(ctx, txs)
		for _, tx := range txs {
			bt := times[tx.BlockHeight]
			s.upsertTx(tx, bt)
			for _, msg := range tx.Messages {
				switch msg.Value.Typename {
				case "MsgCall":
					if err := s.analyzer.ProcessCall(
						s.networkID,
						tx.Hash, tx.BlockHeight,
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
	log.Printf("[%s] synced %d calls, %d sends", s.networkID, callCount, sendCount)
	if err != nil {
		return fmt.Errorf("getTransactionsFromHeight error : %w", err)
	}
	return nil
}

func (s *Syncer) syncMsgRuns(ctx context.Context) error {
	lastHeight, err := s.getLastBlockHeight(ctx, "msg_runs")
	if err != nil {
		return err
	}

	count := 0
	err = walkTransactions(ctx, lastHeight, s.client.GetMsgRunTransactions, func(txs []Transaction) {
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
