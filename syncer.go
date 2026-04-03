package main

import (
	"context"
	"log"
)

type Syncer struct {
	client   *IndexerClient
	db       *DB
	analyzer *Analyzer
}

func NewSyncer(client *IndexerClient, db *DB, analyzer *Analyzer) *Syncer {
	return &Syncer{client: client, db: db, analyzer: analyzer}
}

// SyncAll fetches all data from the indexer and processes it.
func (s *Syncer) SyncAll(ctx context.Context) error {
	// Sync packages (MsgAddPackage)
	if err := s.syncPackages(ctx); err != nil {
		return err
	}

	// Sync calls (MsgCall)
	if err := s.syncCalls(ctx); err != nil {
		return err
	}

	// Sync MsgRun
	if err := s.syncMsgRuns(ctx); err != nil {
		return err
	}

	return nil
}

func (s *Syncer) syncPackages(ctx context.Context) error {
	txs, err := s.client.GetAllPackages(ctx)
	if err != nil {
		return err
	}

	count := 0
	for _, tx := range txs {
		for _, msg := range tx.Messages {
			if msg.Value.Typename == "MsgAddPackage" && msg.Value.Package != nil {
				if err := s.analyzer.ProcessPackage(
					msg.Value.Package,
					msg.Value.Creator,
					tx.Hash,
					tx.BlockHeight,
					tx.Success,
				); err != nil {
					log.Printf("process package %s: %v", msg.Value.Package.Path, err)
					continue
				}
				count++
			}
		}
	}
	log.Printf("synced %d packages", count)
	return nil
}

func (s *Syncer) syncCalls(ctx context.Context) error {
	txs, err := s.client.GetRecentTransactions(ctx)
	if err != nil {
		return err
	}

	count := 0
	for _, tx := range txs {
		for _, msg := range tx.Messages {
			switch msg.Value.Typename {
			case "MsgCall":
				if err := s.analyzer.ProcessCall(
					tx.Hash, tx.BlockHeight,
					msg.Value.Caller,
					msg.Value.PkgPath,
					msg.Value.Func,
					tx.Success,
				); err != nil {
					log.Printf("process call: %v", err)
					continue
				}
				count++
			}
		}
	}
	log.Printf("synced %d calls", count)
	return nil
}

func (s *Syncer) syncMsgRuns(ctx context.Context) error {
	txs, err := s.client.GetMsgRunTransactions(ctx)
	if err != nil {
		return err
	}

	count := 0
	for _, tx := range txs {
		for _, msg := range tx.Messages {
			if msg.Value.Typename == "MsgRun" && msg.Value.Package != nil {
				if err := s.analyzer.ProcessMsgRun(
					tx.Hash, tx.BlockHeight,
					msg.Value.Caller,
					msg.Value.Package.Files,
					tx.Success,
				); err != nil {
					log.Printf("process msgrun: %v", err)
					continue
				}
				count++
			}
		}
	}
	log.Printf("synced %d msg_runs", count)
	return nil
}
