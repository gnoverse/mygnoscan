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
	client    *IndexerClient
	db        *DB
	analyzer  *Analyzer
	networkID string
}

func NewSyncer(client *IndexerClient, db *DB, analyzer *Analyzer, networkID string) *Syncer {
	return &Syncer{client: client, db: db, analyzer: analyzer, networkID: networkID}
}

// fingerprintKeyPrefix namespaces the stored chain fingerprint per network.
const fingerprintKeyPrefix = "chain_fingerprint:"

// SyncAll fetches all data from the indexer and processes it.
func (s *Syncer) SyncAll(ctx context.Context) error {
	if err := s.checkChainReset(ctx); err != nil {
		return fmt.Errorf("check chain reset: %w", err)
	}
	s.warnOnHeightRegression(ctx)
	s.backfillBlockTimes(ctx)
	s.backfillTransactions(ctx)
	if err := s.syncPackages(ctx); err != nil {
		return fmt.Errorf("sync packages: %w", err)
	}
	if err := s.syncCalls(ctx); err != nil {
		return fmt.Errorf("sync calls: %w", err)
	}
	if err := s.syncMsgRuns(ctx); err != nil {
		return fmt.Errorf("sync msg runs: %w", err)
	}
	return nil
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
		return fmt.Errorf("last synced call height: %w", err)
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
