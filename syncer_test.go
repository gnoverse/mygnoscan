package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeIndexer serves just enough of the tx-indexer GraphQL API to drive the
// syncer: block 1 (the reset fingerprint) and the latest height.
type fakeIndexer struct {
	mu sync.Mutex

	block1Hash    string
	chainID       string
	latestHeight  int
	block1Failing bool
}

func (f *fakeIndexer) set(hash string, height int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.block1Hash = hash
	f.latestHeight = height
}

func (f *fakeIndexer) failBlock1(failing bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.block1Failing = failing
}

func (f *fakeIndexer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	query := string(body)

	f.mu.Lock()
	defer f.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	switch {
	case strings.Contains(query, "latestBlockHeight"):
		fmt.Fprintf(w, `{"data":{"latestBlockHeight":%d}}`, f.latestHeight)

	case strings.Contains(query, "getBlocks"):
		if f.block1Failing {
			http.Error(w, "indexer unavailable", http.StatusInternalServerError)
			return
		}
		b, _ := json.Marshal(f.block1Hash)
		fmt.Fprintf(w, `{"data":{"getBlocks":[{"hash":%s,"height":1,"chain_id":%q,"time":"2026-01-01T00:00:00Z"}]}}`,
			b, f.chainID)

	default:
		// Any transaction query: no results, so sync is a no-op.
		fmt.Fprint(w, `{"data":{"getTransactions":[]}}`)
	}
}

// newTestSyncer wires a syncer against a fake indexer and a real temp database.
func newTestSyncer(t *testing.T, network string) (*Syncer, *fakeIndexer, *DB) {
	t.Helper()

	fake := &fakeIndexer{chainID: "testchain", block1Hash: "genesis-a", latestHeight: 1000}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	db, err := NewDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	return NewSyncer(NewIndexerClient(srv.URL), db, NewAnalyzer(db), network), fake, db
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
