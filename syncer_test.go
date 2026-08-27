package main

import (
	"context"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

// newTestSyncer wires a syncer against a fake indexer and a real temp database.
func newTestSyncer(t *testing.T, network string) (*Syncer, *fakeIndexer, *DB) {
	t.Helper()

	fake, client := newFakeIndexer(t)
	fake.chainID = "testchain"
	fake.set("genesis-a", 1000)

	db, err := NewDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	return NewSyncer(client, db, NewAnalyzer(db), network), fake, db
}

// seedNetwork writes one row into every network-scoped table.
func seedNetwork(t *testing.T, db *DB, network string, height int) {
	t.Helper()

	if err := db.UpsertPackage(network, "gno.land/r/demo/foo", "foo", "g1creator", "TXHASH", height, "", true, 1); err != nil {
		t.Fatalf("upsert package: %v", err)
	}
	if err := db.UpsertPackageFile(network, "gno.land/r/demo/foo", "foo.gno", "package foo"); err != nil {
		t.Fatalf("upsert package file: %v", err)
	}
	if err := db.SetDependencies(network, "gno.land/r/demo/foo", []string{"gno.land/p/demo/avl"}); err != nil {
		t.Fatalf("set dependencies: %v", err)
	}
	if err := db.InsertCall(network, "TXHASH", height, "", "g1caller", "gno.land/r/demo/foo", "Bar", true); err != nil {
		t.Fatalf("insert call: %v", err)
	}
	if err := db.InsertMsgRun(network, "TXHASH", height, "", "g1caller", "package main", true); err != nil {
		t.Fatalf("insert msg run: %v", err)
	}
	if err := db.InsertBankSend(network, "TXHASH", height, "", "g1from", "g1to", "1ugnot", true); err != nil {
		t.Fatalf("insert bank send: %v", err)
	}
	if err := db.UpsertTransaction(network, "TXHASH", height, "", 100, 200, 1, true); err != nil {
		t.Fatalf("upsert transaction: %v", err)
	}
}

func countNetworkRows(t *testing.T, db *DB, network string) int {
	t.Helper()

	total := 0
	for _, table := range networkScopedTables {
		var n int
		q := fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE network = ?`, table)
		if err := db.db.QueryRow(q, network).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		total += n
	}
	return total
}

func TestCheckChainResetDiscardsDataFromAPreviousChain(t *testing.T) {
	ctx := context.Background()
	syncer, fake, db := newTestSyncer(t, "staging")

	// First pass records the fingerprint of the chain we are syncing.
	if err := syncer.checkChainReset(ctx); err != nil {
		t.Fatalf("first reset check: %v", err)
	}
	seedNetwork(t, db, "staging", 900)
	seedNetwork(t, db, "topaz", 900) // another network, must survive

	if got := countNetworkRows(t, db, "staging"); got != len(networkScopedTables) {
		t.Fatalf("seeded rows = %d, want %d", got, len(networkScopedTables))
	}

	// Same chain: nothing is discarded.
	if err := syncer.checkChainReset(ctx); err != nil {
		t.Fatalf("second reset check: %v", err)
	}
	if got := countNetworkRows(t, db, "staging"); got != len(networkScopedTables) {
		t.Fatalf("rows after no-op check = %d, want %d", got, len(networkScopedTables))
	}

	// The chain is reset: same chain ID, new genesis, height back near zero.
	fake.set("genesis-b", 5)
	if err := syncer.checkChainReset(ctx); err != nil {
		t.Fatalf("reset check after reset: %v", err)
	}

	if got := countNetworkRows(t, db, "staging"); got != 0 {
		t.Errorf("stale rows left after reset: %d", got)
	}
	if got := countNetworkRows(t, db, "topaz"); got != len(networkScopedTables) {
		t.Errorf("other network lost %d rows", len(networkScopedTables)-got)
	}

	// The new fingerprint is now the stored one, so this is not re-detected.
	stored, err := db.GetSyncState(fingerprintKeyPrefix + "staging")
	if err != nil {
		t.Fatalf("read fingerprint: %v", err)
	}
	if want := "testchain:genesis-b"; stored != want {
		t.Errorf("stored fingerprint = %q, want %q", stored, want)
	}
}

func TestCheckChainResetKeepsDataWhenIndexerIsUnreachable(t *testing.T) {
	ctx := context.Background()
	syncer, fake, db := newTestSyncer(t, "staging")

	if err := syncer.checkChainReset(ctx); err != nil {
		t.Fatalf("first reset check: %v", err)
	}
	seedNetwork(t, db, "staging", 900)

	// An indexer that cannot answer is not evidence of a reset. This is the
	// case that must never wipe: it would turn a transient outage into data loss.
	fake.failBlock1(true)
	if err := syncer.checkChainReset(ctx); err != nil {
		t.Fatalf("reset check with failing indexer: %v", err)
	}
	if got := countNetworkRows(t, db, "staging"); got != len(networkScopedTables) {
		t.Errorf("rows = %d after indexer failure, want %d", got, len(networkScopedTables))
	}
}

func TestCheckChainResetIgnoresALaggingIndexer(t *testing.T) {
	ctx := context.Background()
	syncer, fake, db := newTestSyncer(t, "betanet")

	if err := syncer.checkChainReset(ctx); err != nil {
		t.Fatalf("first reset check: %v", err)
	}
	seedNetwork(t, db, "betanet", 3_000_000)

	// Same genesis, tip far below what we stored: a replica that is behind.
	// Sync will not advance, but the data is still valid and must be kept.
	fake.set("genesis-a", 12)
	if err := syncer.checkChainReset(ctx); err != nil {
		t.Fatalf("reset check with lagging indexer: %v", err)
	}
	if got := countNetworkRows(t, db, "betanet"); got != len(networkScopedTables) {
		t.Errorf("rows = %d after lagging indexer, want %d", got, len(networkScopedTables))
	}

	syncer.warnOnHeightRegression(ctx) // logs only; must not panic or delete
	if got := countNetworkRows(t, db, "betanet"); got != len(networkScopedTables) {
		t.Errorf("rows = %d after regression warning, want %d", got, len(networkScopedTables))
	}
}

func TestDeleteNetworkDataIsScopedToOneNetwork(t *testing.T) {
	db, err := NewDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	seedNetwork(t, db, "topaz", 100)
	seedNetwork(t, db, "staging", 200)

	deleted, err := db.DeleteNetworkData("topaz")
	if err != nil {
		t.Fatalf("delete network data: %v", err)
	}
	if want := int64(len(networkScopedTables)); deleted != want {
		t.Errorf("deleted %d rows, want %d", deleted, want)
	}
	if got := countNetworkRows(t, db, "topaz"); got != 0 {
		t.Errorf("topaz rows remaining: %d", got)
	}
	if got := countNetworkRows(t, db, "staging"); got != len(networkScopedTables) {
		t.Errorf("staging rows = %d, want %d", got, len(networkScopedTables))
	}
}

func TestMaxBlockHeight(t *testing.T) {
	db, err := NewDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	// No data yet: 0 rather than an error, so callers can treat it as "unknown".
	got, err := db.MaxBlockHeight("topaz")
	if err != nil {
		t.Fatalf("max block height on empty db: %v", err)
	}
	if got != 0 {
		t.Errorf("height = %d on empty db, want 0", got)
	}

	if err := db.UpsertTransaction("topaz", "A", 10, "", 0, 0, 0, true); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := db.UpsertTransaction("topaz", "B", 42, "", 0, 0, 0, true); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := db.UpsertTransaction("staging", "C", 99, "", 0, 0, 0, true); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, err = db.MaxBlockHeight("topaz")
	if err != nil {
		t.Fatalf("max block height: %v", err)
	}
	if got != 42 {
		t.Errorf("height = %d, want 42 (highest for topaz, ignoring other networks)", got)
	}
}

func TestWalkTransactionsCoversEveryRowAcrossPages(t *testing.T) {
	// The chain is two and a half pages deep and its blocks hold 3 transactions
	// each, so the indexer's cap falls inside a block on every page. Every row has
	// to arrive exactly once: a gap here is silent, permanent data loss, because
	// the sync cursors only ever move forward.
	fake := &truncatingTxIndexer{tip: 8500, txsPerBlock: 3}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	client := NewIndexerClient(srv.URL)

	seen := make(map[string]int)
	pages := 0
	err := walkTransactions(context.Background(), nil, client.GetTransactionsFromHeight,
		func(txs []Transaction) {
			pages++
			for _, tx := range txs {
				seen[tx.Hash]++
			}
		})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if pages < 3 {
		t.Errorf("walked %d pages for %d rows; expected the cap to force several",
			pages, fake.tip*fake.txsPerBlock)
	}
	want := fake.tip * fake.txsPerBlock
	if len(seen) != want {
		t.Errorf("saw %d distinct transactions, want %d", len(seen), want)
	}
	for h := 1; h <= fake.tip; h++ {
		for i := 0; i < fake.txsPerBlock; i++ {
			hash := fmt.Sprintf("tx-%d-%d", h, i)
			switch seen[hash] {
			case 1:
			case 0:
				t.Fatalf("%s never arrived — the walk skipped it", hash)
			default:
				t.Fatalf("%s arrived %d times", hash, seen[hash])
			}
		}
	}
}

func TestWalkTransactionsStopsWhenCaughtUp(t *testing.T) {
	fake := &truncatingTxIndexer{tip: 100, txsPerBlock: 1}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	cursor := 100
	calls := 0
	err := walkTransactions(context.Background(), &cursor,
		NewIndexerClient(srv.URL).GetTransactionsFromHeight,
		func(txs []Transaction) { calls++ })
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 0 {
		t.Errorf("processed %d pages while caught up, want 0", calls)
	}
}

// valoperSubject decides which address a valopers call is about and whether it
// sets a name. The argument layout differs per function, so every case here uses
// arguments taken from real sapphire transactions.
func TestValoperSubject(t *testing.T) {
	const (
		caller  = "g1qynsu9dwj9lq0m5fkje7jh6qy3md80ztqnshhm"
		subject = "g1rw8av9eghy9h8vajktzk9d0wn2sd5d9ftwkhuf"
	)

	tests := []struct {
		name        string
		fn          string
		args        []string
		wantAddress string
		wantMoniker string
	}{
		{
			// Register(moniker, description, serverType, address, pubkey) — the one
			// function where args[0] really is the name, and the address is args[3]
			// rather than the caller.
			name:        "Register takes the moniker first and the address fourth",
			fn:          "Register",
			args:        []string{"onbloc-val-01", "a long description", "cloud", subject, "gpub1..."},
			wantAddress: subject,
			wantMoniker: "onbloc-val-01",
		},
		{
			name:        "UpdateMoniker takes the address first and the name second",
			fn:          "UpdateMoniker",
			args:        []string{subject, "delete"},
			wantAddress: subject,
			wantMoniker: "delete",
		},
		{
			// The old code recorded this description's address as a moniker.
			name:        "UpdateDescription sets no name",
			fn:          "UpdateDescription",
			args:        []string{subject, "Infra & Insights"},
			wantAddress: subject,
			wantMoniker: "",
		},
		{
			name:        "UpdateKeepRunning sets no name",
			fn:          "UpdateKeepRunning",
			args:        []string{subject, "true"},
			wantAddress: subject,
			wantMoniker: "",
		},
		{
			name:        "UpdateSigningKey sets no name",
			fn:          "UpdateSigningKey",
			args:        []string{subject, "gpub1..."},
			wantAddress: subject,
			wantMoniker: "",
		},
		{
			name:        "a non-address first argument falls back to the caller",
			fn:          "UpdateServerType",
			args:        []string{"cloud"},
			wantAddress: caller,
			wantMoniker: "",
		},
		{
			name:        "no arguments falls back to the caller",
			fn:          "SomethingElse",
			args:        nil,
			wantAddress: caller,
			wantMoniker: "",
		},
		{
			// A Register shorter than the current ABI must still attribute the row.
			name:        "Register without an address argument falls back to the caller",
			fn:          "Register",
			args:        []string{"solo"},
			wantAddress: caller,
			wantMoniker: "solo",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := TxMessage{Value: MessageValue{
				Typename: "MsgCall",
				Caller:   caller,
				PkgPath:  "gno.land/r/gnops/valopers",
				Func:     tt.fn,
				Args:     tt.args,
			}}
			address, moniker := valoperSubject(msg)
			if address != tt.wantAddress {
				t.Errorf("address = %q, want %q", address, tt.wantAddress)
			}
			if moniker != tt.wantMoniker {
				t.Errorf("moniker = %q, want %q", moniker, tt.wantMoniker)
			}
		})
	}
}
