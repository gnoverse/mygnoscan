package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
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

// SyncAll fetches all data from the indexer and processes it.
func (s *Syncer) SyncAll(ctx context.Context) error {
	if err := s.syncPackages(ctx); err != nil {
		return err
	}
	if err := s.syncCalls(ctx); err != nil {
		return err
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

func (s *Syncer) syncPackages(ctx context.Context) error {
	lastHeight, err := s.getLastBlockHeight(ctx, "packages")
	if err != nil {
		return err
	}

	txs, err := s.client.GetAllPackages(ctx, lastHeight)
	if err != nil {
		return err
	}

	times := s.fetchBlockTimes(ctx, txs)
	count := 0
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
	log.Printf("[%s] synced %d packages", s.networkID, count)
	return nil
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
		return err
	}

	txs, err := s.client.GetRecentTransactionsFromHeight(ctx, lastHeight)
	if err != nil {
		return err
	}

	times := s.fetchBlockTimes(ctx, txs)
	callCount, sendCount := 0, 0
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
	log.Printf("[%s] synced %d calls, %d sends", s.networkID, callCount, sendCount)
	return nil
}

func (s *Syncer) syncMsgRuns(ctx context.Context) error {
	lastHeight, err := s.getLastBlockHeight(ctx, "msg_runs")
	if err != nil {
		return err
	}

	txs, err := s.client.GetMsgRunTransactions(ctx, lastHeight)
	if err != nil {
		return err
	}

	times := s.fetchBlockTimes(ctx, txs)
	count := 0
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
	log.Printf("[%s] synced %d msg_runs", s.networkID, count)
	return nil
}
