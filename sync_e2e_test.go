package main

import (
	"context"
	"fmt"
	"testing"
)

// A full sync against a fake chain, end to end.
//
// The syncer had no test at all: every one of its paths was only ever exercised
// against a live indexer. It is also where correctness originates — a bug here
// writes wrong rows into storage, and every page downstream then renders them
// faithfully.
func newE2ESyncer(t *testing.T) (*Syncer, *fakeIndexer, *DB) {
	t.Helper()

	fake, client := newFakeIndexer(t)
	fake.chainID = "e2e-1"

	db := newTestDB(t)
	db.SetConfiguredNetworks([]NetworkConfig{{ID: "e2e"}})

	return NewSyncer(client, db, NewAnalyzer(db), "e2e"), fake, db
}

// seedMixedChain writes one of every message type the syncer handles, each in
// its own block so block times are unambiguous.
func seedMixedChain(f *fakeIndexer) {
	when := func(h int) string { return fmt.Sprintf("2026-08-01T00:%02d:00Z", h) }

	// Block 1 is the reset fingerprint and carries no transaction of its own.
	f.mu.Lock()
	f.blocks = append(f.blocks, Block{
		Hash: "genesis", Height: 1, ChainID: f.chainID, Time: when(1),
	})
	for h := 10; h <= 13; h++ {
		f.blocks = append(f.blocks, Block{
			Hash: fmt.Sprintf("block-%d", h), Height: h, ChainID: f.chainID,
			Time: when(h), NumTxs: 1, TotalTxs: h,
		})
	}
	f.mu.Unlock()

	f.add(
		fakePackage(10, when(10), "g1deployer", "gno.land/r/demo/boards"),
		fakeCall(11, when(11), "g1alice", "gno.land/r/demo/boards", "Post"),
		fakeSend(12, when(12), "g1alice", "g1bob", "1000ugnot"),
		fakeMsgRun(13, when(13), "g1carol"),
	)
}

func fakeMsgRun(height int, when, caller string) Transaction {
	return Transaction{
		Hash: fmt.Sprintf("tx-run-%d", height), Success: true, BlockHeight: height, BlockTime: when,
		Messages: []TxMessage{{
			TypeURL: "run", Route: "vm",
			Value: MessageValue{Typename: "MsgRun", Caller: caller, Package: &MemPackage{
				Name: "main", Path: "gno.land/e/" + caller + "/run",
				Files: []MemFile{{Name: "main.gno", Body: "package main\n\nimport \"gno.land/p/demo/avl\"\n\nfunc main() {}\n"}},
			}},
		}},
	}
}

func TestSyncAllWritesEveryMessageType(t *testing.T) {
	syncer, fake, db := newE2ESyncer(t)
	seedMixedChain(fake)

	if err := syncer.SyncAll(context.Background()); err != nil {
		t.Fatalf("SyncAll: %v", err)
	}

	counts := map[string]int{}
	for _, table := range []string{"packages", "calls", "bank_sends", "msg_runs", "transactions"} {
		var n int
		if err := db.db.QueryRow(`SELECT COUNT(*) FROM ` + table + ` WHERE network = 'e2e'`).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		counts[table] = n
	}

	for table, want := range map[string]int{
		"packages":   1,
		"calls":      1,
		"bank_sends": 1,
		"msg_runs":   1,
	} {
		if counts[table] != want {
			t.Errorf("%s has %d rows, want %d", table, counts[table], want)
		}
	}
	// Every message also produces a transaction row: gas and fees live only
	// there, so a missing one silently under-reports every gas total.
	if counts["transactions"] < 4 {
		t.Errorf("transactions has %d rows, want at least one per message", counts["transactions"])
	}
}

// Rows must carry the block time as they are written.
//
// Without it a row cannot be ordered against another chain's in a merged view,
// and list endpoints have to ask the indexer for times at request time instead
// of reading what is already stored.
func TestSyncStampsBlockTimes(t *testing.T) {
	syncer, fake, db := newE2ESyncer(t)
	seedMixedChain(fake)

	if err := syncer.SyncAll(context.Background()); err != nil {
		t.Fatalf("SyncAll: %v", err)
	}

	for _, table := range []string{"packages", "calls", "bank_sends", "msg_runs", "transactions"} {
		var missing int
		err := db.db.QueryRow(
			`SELECT COUNT(*) FROM ` + table + ` WHERE network = 'e2e' AND (block_time IS NULL OR block_time = '')`).Scan(&missing)
		if err != nil {
			t.Fatalf("query %s: %v", table, err)
		}
		if missing > 0 {
			t.Errorf("%s has %d row(s) with no block_time", table, missing)
		}
	}
}

// Syncing twice must not duplicate rows, and must not re-fetch what it already
// has: the cursor is what keeps a sync incremental.
func TestSyncIsIdempotentAndIncremental(t *testing.T) {
	syncer, fake, db := newE2ESyncer(t)
	seedMixedChain(fake)

	if err := syncer.SyncAll(context.Background()); err != nil {
		t.Fatalf("first SyncAll: %v", err)
	}
	first := countAll(t, db)

	queriesAfterFirst := len(fake.askedQueries())
	if err := syncer.SyncAll(context.Background()); err != nil {
		t.Fatalf("second SyncAll: %v", err)
	}
	second := countAll(t, db)

	for table, n := range first {
		if second[table] != n {
			t.Errorf("%s went from %d to %d rows on a second sync of unchanged data", table, n, second[table])
		}
	}
	if len(fake.askedQueries()) <= queriesAfterFirst {
		t.Error("the second sync issued no queries at all; it should still check for new blocks")
	}
}

// New blocks arriving after a sync are picked up by the next one, and only
// those: the cursor must advance rather than restart from genesis.
func TestSyncPicksUpNewBlocks(t *testing.T) {
	syncer, fake, db := newE2ESyncer(t)
	seedMixedChain(fake)

	if err := syncer.SyncAll(context.Background()); err != nil {
		t.Fatalf("first SyncAll: %v", err)
	}
	before := countAll(t, db)

	when := "2026-08-01T01:00:00Z"
	fake.mu.Lock()
	fake.blocks = append(fake.blocks, Block{Hash: "block-20", Height: 20, ChainID: fake.chainID, Time: when})
	fake.mu.Unlock()
	fake.add(fakeCall(20, when, "g1dave", "gno.land/r/demo/boards", "Post"))

	if err := syncer.SyncAll(context.Background()); err != nil {
		t.Fatalf("second SyncAll: %v", err)
	}
	after := countAll(t, db)

	if after["calls"] != before["calls"]+1 {
		t.Errorf("calls went from %d to %d, want exactly one more", before["calls"], after["calls"])
	}
	if after["packages"] != before["packages"] {
		t.Errorf("packages changed from %d to %d on a pass that added only a call",
			before["packages"], after["packages"])
	}
}

// A failing indexer must leave what is already stored alone. A sync that
// truncated on error would turn a transient outage into data loss.
func TestSyncFailureLeavesStoredDataIntact(t *testing.T) {
	syncer, fake, db := newE2ESyncer(t)
	seedMixedChain(fake)

	if err := syncer.SyncAll(context.Background()); err != nil {
		t.Fatalf("first SyncAll: %v", err)
	}
	before := countAll(t, db)

	fake.status = 500
	if err := syncer.SyncAll(context.Background()); err == nil {
		t.Error("a sync against a dead indexer reported success")
	}

	if after := countAll(t, db); after["calls"] != before["calls"] || after["packages"] != before["packages"] {
		t.Errorf("stored rows changed during a failed sync: %v then %v", before, after)
	}
}

// Dependencies are extracted from package source at sync time, so an import in
// a deployed file has to become an edge in the graph.
func TestSyncExtractsDependencies(t *testing.T) {
	syncer, fake, db := newE2ESyncer(t)
	seedMixedChain(fake)

	if err := syncer.SyncAll(context.Background()); err != nil {
		t.Fatalf("SyncAll: %v", err)
	}

	var edges int
	err := db.db.QueryRow(`SELECT COUNT(*) FROM dependencies WHERE network = 'e2e'
		AND import_path = 'gno.land/p/demo/avl'`).Scan(&edges)
	if err != nil {
		t.Fatalf("query dependencies: %v", err)
	}
	if edges == 0 {
		t.Error("the avl import in the deployed source produced no dependency edge")
	}
}

func countAll(t *testing.T, db *DB) map[string]int {
	t.Helper()

	out := map[string]int{}
	for _, table := range []string{"packages", "calls", "bank_sends", "msg_runs", "transactions", "dependencies"} {
		var n int
		if err := db.db.QueryRow(`SELECT COUNT(*) FROM ` + table + ` WHERE network = 'e2e'`).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		out[table] = n
	}
	return out
}
