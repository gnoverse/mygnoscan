package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeIndexer serves just enough of the tx-indexer GraphQL API to drive the
// syncer: block 1 (the reset fingerprint) and the latest height.
type fakeIndexer struct {
	mu sync.Mutex

	block1Hash    string
	chainID       string
	latestHeight  int
	block1Failing bool

	blockLo, blockHi int // the height range this fake "has"; 0,0 means none
	rangeCalls       int

	forceEmptyRangeOnce bool // next range (gt:/lt:) query returns [] regardless of blockLo/blockHi
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

// setBlocks makes the fake serve blocks in [lo, hi] and report hi as the tip.
func (f *fakeIndexer) setBlocks(lo, hi int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.blockLo, f.blockHi, f.latestHeight = lo, hi, hi
}

// forceEmptyRange makes the very next range (gt:/lt:) query return no rows,
// regardless of blockLo/blockHi — simulating a replica still catching up, or
// a load balancer fronting a partially-populated node, serving an empty page
// for a range that genuinely has data. It does not affect the single-height
// (eq:) probe query, which a different backend may answer correctly.
func (f *fakeIndexer) forceEmptyRange() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forceEmptyRangeOnce = true
}

// serveBlockRange renders a getBlocks range response clamped to [blockLo,
// blockHi]. Heights outside that range yield an empty array — how a pruned
// indexer behaves, and what the backfill's termination check keys on.
func (f *fakeIndexer) serveBlockRange(w http.ResponseWriter, query string) {
	from := intAfter(query, "gt:") + 1
	to := intAfter(query, "lt:") - 1
	if from < f.blockLo {
		from = f.blockLo
	}
	if to > f.blockHi {
		to = f.blockHi
	}
	var parts []string
	for h := from; h <= to; h++ {
		// 5s spacing keeps times deterministic; these tests assert on counts
		// and cursors, not on deltas.
		ts := time.Unix(int64(1767225600+h*5), 0).UTC().Format(time.RFC3339)
		parts = append(parts, fmt.Sprintf(
			`{"hash":"h%d","height":%d,"chain_id":%q,"time":%q,"num_txs":0,"proposer_address_raw":"g1aaa"}`,
			h, h, f.chainID, ts))
	}
	fmt.Fprintf(w, `{"data":{"getBlocks":[%s]}}`, strings.Join(parts, ","))
}

// intAfter reads the first integer following key in s, or 0.
func intAfter(s, key string) int {
	i := strings.Index(s, key)
	if i < 0 {
		return 0
	}
	rest := strings.TrimSpace(s[i+len(key):])
	end := 0
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	n, _ := strconv.Atoi(rest[:end])
	return n
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
		// GetBlock emits `eq:` (the fingerprint probe and the backfill's floor
		// probe); GetBlocksInRange emits `gt:`/`lt:`. Only the latter is a
		// backfill page.
		if strings.Contains(query, "gt:") {
			f.rangeCalls++
			if f.forceEmptyRangeOnce {
				f.forceEmptyRangeOnce = false
				fmt.Fprint(w, `{"data":{"getBlocks":[]}}`)
				return
			}
			f.serveBlockRange(w, query)
			return
		}
		// eq: query. Height 1 always answers with the fingerprint block,
		// regardless of blockLo/blockHi, since checkChainReset relies on it
		// independently of whatever range the backfill has set up.
		h := intAfter(query, "eq:")
		if h == 1 {
			b, _ := json.Marshal(f.block1Hash)
			fmt.Fprintf(w, `{"data":{"getBlocks":[{"hash":%s,"height":1,"chain_id":%q,"time":"2026-01-01T00:00:00Z"}]}}`,
				b, f.chainID)
			return
		}
		if h >= f.blockLo && h <= f.blockHi {
			ts := time.Unix(int64(1767225600+h*5), 0).UTC().Format(time.RFC3339)
			fmt.Fprintf(w, `{"data":{"getBlocks":[{"hash":"h%d","height":%d,"chain_id":%q,"time":%q,"num_txs":0,"proposer_address_raw":"g1aaa"}]}}`,
				h, h, f.chainID, ts)
			return
		}
		fmt.Fprint(w, `{"data":{"getBlocks":[]}}`)

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
	proposerID, err := db.InternProposer(network, "g1proposer")
	if err != nil {
		t.Fatalf("intern proposer: %v", err)
	}
	if err := db.UpsertBlock(network, height, "", proposerID, 1); err != nil {
		t.Fatalf("upsert block: %v", err)
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

func TestCheckChainResetClearsProposerIDMemo(t *testing.T) {
	// A chain reset wipes the proposers table. If the syncer's in-memory
	// proposerIDs memo survived, every block written after the reset would
	// carry a proposer_id pointing at a row that no longer exists, and
	// GetBlockProposers's inner join would silently drop those blocks from
	// the aggregate.
	ctx := context.Background()
	syncer, fake, db := newTestSyncer(t, "staging")

	if err := syncer.checkChainReset(ctx); err != nil {
		t.Fatalf("first reset check: %v", err)
	}

	id, err := syncer.proposerID("g1validator")
	if err != nil {
		t.Fatalf("intern proposer before reset: %v", err)
	}
	if syncer.proposerIDs["g1validator"] != id {
		t.Fatalf("proposer memo not populated before reset")
	}

	// The chain resets: same chain ID, new genesis, height back near zero.
	fake.set("genesis-b", 5)
	if err := syncer.checkChainReset(ctx); err != nil {
		t.Fatalf("reset check after reset: %v", err)
	}

	if syncer.proposerIDs != nil {
		t.Errorf("proposer memo survived the reset: %v", syncer.proposerIDs)
	}

	// Re-interning after the reset must produce an id backed by a real row,
	// not a stale memoised one from the deleted table.
	newID, err := syncer.proposerID("g1validator")
	if err != nil {
		t.Fatalf("intern proposer after reset: %v", err)
	}
	var count int
	if err := db.db.QueryRow(
		`SELECT COUNT(*) FROM proposers WHERE network = ? AND id = ? AND address = ?`,
		"staging", newID, "g1validator",
	).Scan(&count); err != nil {
		t.Fatalf("query proposers: %v", err)
	}
	if count != 1 {
		t.Errorf("post-reset proposer id %d does not point at a real proposers row", newID)
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

func TestSyncBlocksBoundedPerPass(t *testing.T) {
	// A pass must stop at its page budget rather than draining the whole chain
	// inline, which would stall package/call/msg-run syncing behind it.
	s, fake, db := newTestSyncer(t, "gnoland1")
	fake.setBlocks(1, 1_000_000)

	s.syncBlocks(context.Background())

	minH, maxH, ok, err := db.BlockHeightBounds("gnoland1")
	if err != nil || !ok {
		t.Fatalf("bounds after one pass: ok=%v err=%v", ok, err)
	}
	stored := maxH - minH + 1
	budget := blockPageSize * blockPagesPerPass
	if stored > budget {
		t.Errorf("stored %d blocks in one pass, budget is %d", stored, budget)
	}
	if stored == 0 {
		t.Error("stored no blocks at all")
	}
	if fake.rangeCalls == 0 {
		t.Error("made no range queries")
	}
}

func TestSyncBlocksResumesBackward(t *testing.T) {
	// Successive passes must extend the range downward, not restart at the tip.
	s, fake, db := newTestSyncer(t, "gnoland1")
	fake.setBlocks(1, 1_000_000)

	s.syncBlocks(context.Background())
	min1, max1, _, _ := db.BlockHeightBounds("gnoland1")

	s.syncBlocks(context.Background())
	min2, max2, _, _ := db.BlockHeightBounds("gnoland1")

	if min2 >= min1 {
		t.Errorf("second pass did not extend backward: min went %d -> %d", min1, min2)
	}
	if max2 < max1 {
		t.Errorf("second pass lost head blocks: max went %d -> %d", max1, max2)
	}
}

func TestSyncBlocksTerminatesAtGenesis(t *testing.T) {
	// A small chain must reach the bottom and set the done flag rather than
	// retrying a range that will never return rows.
	s, fake, db := newTestSyncer(t, "gnoland1")
	fake.setBlocks(1, 20)

	for i := 0; i < 5; i++ {
		s.syncBlocks(context.Background())
	}

	done, err := db.GetSyncState(blocksBackfillDoneKey("gnoland1"))
	if err != nil {
		t.Fatalf("get sync state: %v", err)
	}
	if done != "1" {
		t.Errorf("backfill done flag = %q, want \"1\"", done)
	}
}

func TestSyncBlocksTerminatesOnPrunedIndexer(t *testing.T) {
	// A pruned indexer never serves below its floor (here 500, well above
	// genesis). The backfill must notice a page that returns nothing new,
	// mark itself done, and stop — otherwise every future 30s cycle would
	// refetch the identical empty range and break again, forever.
	s, fake, db := newTestSyncer(t, "gnoland1")
	fake.setBlocks(500, 20000)

	for i := 0; i < 4; i++ {
		s.syncBlocks(context.Background())
	}
	callsBefore := fake.rangeCalls

	s.syncBlocks(context.Background())
	callsAfter := fake.rangeCalls

	if callsAfter != callsBefore {
		t.Errorf("range calls kept growing after termination: %d -> %d", callsBefore, callsAfter)
	}

	done, err := db.GetSyncState(blocksBackfillDoneKey("gnoland1"))
	if err != nil {
		t.Fatalf("get sync state: %v", err)
	}
	if done != "1" {
		t.Errorf("backfill done flag = %q, want \"1\" once the pruned floor is hit", done)
	}

	minH, _, ok, err := db.BlockHeightBounds("gnoland1")
	if err != nil || !ok {
		t.Fatalf("bounds: ok=%v err=%v", ok, err)
	}
	if minH < 500 {
		t.Errorf("stored below the indexer's floor: minH=%d", minH)
	}
}

func TestSyncBlocksTransientEmptyPageDoesNotMarkDone(t *testing.T) {
	// A single empty range response must not be treated as proof of a pruned
	// floor: a replica still catching up, or a load balancer fronting a
	// partially-populated node, can return getBlocks: [] for a range that
	// genuinely has data. Only a confirming probe at the boundary height may
	// conclude the floor is real; until then, backfill must retry rather than
	// permanently self-certifying incomplete history as done.
	s, fake, db := newTestSyncer(t, "gnoland1")
	fake.setBlocks(1, 200_000) // large enough that one pass can't reach genesis

	s.syncBlocks(context.Background())
	minAfterFirst, _, ok, err := db.BlockHeightBounds("gnoland1")
	if err != nil || !ok {
		t.Fatalf("bounds after first pass: ok=%v err=%v", ok, err)
	}
	if minAfterFirst <= 1 {
		t.Fatalf("first pass reached genesis already; test needs more headroom (minH=%d)", minAfterFirst)
	}

	fake.forceEmptyRange()
	s.syncBlocks(context.Background())

	done, err := db.GetSyncState(blocksBackfillDoneKey("gnoland1"))
	if err != nil {
		t.Fatalf("get sync state: %v", err)
	}
	if done == "1" {
		t.Error("backfill marked done after a single transient empty page")
	}

	minAfterForced, _, ok, err := db.BlockHeightBounds("gnoland1")
	if err != nil || !ok {
		t.Fatalf("bounds after forced-empty pass: ok=%v err=%v", ok, err)
	}
	if minAfterForced != minAfterFirst {
		t.Errorf("min height moved on a page that returned nothing: %d -> %d", minAfterFirst, minAfterForced)
	}

	// Progress must resume once the transient condition clears.
	s.syncBlocks(context.Background())
	minAfterResume, _, ok, err := db.BlockHeightBounds("gnoland1")
	if err != nil || !ok {
		t.Fatalf("bounds after resume pass: ok=%v err=%v", ok, err)
	}
	if minAfterResume >= minAfterForced {
		t.Errorf("backfill did not resume after the transient empty page cleared: min stayed at %d", minAfterResume)
	}
}

func TestSyncBlocksAbandonsPageOnProposerInternFailure(t *testing.T) {
	// A page is all-or-nothing. If a proposer lookup fails partway through a
	// page, writing the surviving rows would leave a hole inside the stored
	// [MIN, MAX] range that no later pass ever revisits (head sync only
	// extends above MAX, backfill only extends below MIN) — a silent,
	// permanent gap. The whole page must be abandoned instead.
	ctx := context.Background()
	network := "gnoland1"
	dbPath := filepath.Join(t.TempDir(), "test.db")

	db, err := NewDB(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}

	fake := &fakeIndexer{chainID: "testchain", block1Hash: "genesis-a"}
	fake.setBlocks(1, 100)
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	s := NewSyncer(NewIndexerClient(srv.URL), db, NewAnalyzer(db), network)

	// Force InternProposer to fail: close the connection after the fake
	// indexer is set up but before the page is processed, so GetBlocksInRange
	// (pure HTTP, no db) still succeeds while the db write path fails.
	if err := db.db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	if s.fetchBlockPage(ctx, 1, 50) {
		t.Fatal("fetchBlockPage succeeded despite a broken proposer lookup")
	}

	// Reopen a fresh connection to the same file: nothing should have been
	// committed, since the page must be abandoned before any write.
	db2, err := NewDB(dbPath)
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	defer db2.Close()

	_, _, ok, err := db2.BlockHeightBounds(network)
	if err != nil {
		t.Fatalf("bounds: %v", err)
	}
	if ok {
		t.Error("page was partially written despite a proposer intern failure")
	}
}

func TestSyncBlocksHonoursHistoryCap(t *testing.T) {
	// The fake serves blocks timestamped in early 2026, so a one-day cap puts
	// every one of them past the cutoff. The backfill must stop and mark itself
	// done after the seed page rather than draining its whole page budget.
	s, fake, db := newTestSyncer(t, "gnoland1")
	s.blockHistoryDays = 1
	fake.setBlocks(1, 1_000_000)

	s.syncBlocks(context.Background())

	minH, maxH, ok, err := db.BlockHeightBounds("gnoland1")
	if err != nil || !ok {
		t.Fatalf("bounds: ok=%v err=%v", ok, err)
	}
	stored := maxH - minH + 1
	if stored > blockPageSize {
		t.Errorf("stored %d blocks under a 1-day cap, want no more than one seed page (%d)", stored, blockPageSize)
	}
	done, err := db.GetSyncState(blocksBackfillDoneKey("gnoland1"))
	if err != nil {
		t.Fatalf("get sync state: %v", err)
	}
	if done != "1" {
		t.Errorf("backfill done flag = %q, want \"1\" — a capped backfill that never terminates refetches forever", done)
	}

	// And it stays stopped: a later pass must not resume walking backward.
	before := fake.rangeCalls
	s.syncBlocks(context.Background())
	minAfter, _, _, _ := db.BlockHeightBounds("gnoland1")
	if minAfter < minH {
		t.Errorf("a capped backfill resumed: min went %d -> %d", minH, minAfter)
	}
	if fake.rangeCalls > before+1 {
		t.Errorf("made %d extra range queries after the cap was reached, want at most 1 (head sync)", fake.rangeCalls-before)
	}
}

func TestSyncBlocksHistoryCapBreaksPartwayThroughBackfill(t *testing.T) {
	// TestSyncBlocksHonoursHistoryCap only exercises the pre-loop cap check:
	// its 1-day cap puts the seed page itself past the cutoff, so the in-loop
	// break inside the per-page backfill loop (and its "history cap of %dd
	// reached" log line) never runs. Here the cutoff falls roughly halfway
	// through the fake's block range, so several passes must walk backward,
	// crossing the cutoff mid-loop on one of them.
	s, fake, db := newTestSyncer(t, "gnoland1")
	const tip = 1_000_000
	fake.setBlocks(1, tip)

	// The fake dates height h at epoch + h*5s (see fakeIndexer.serveBlockRange).
	// Aim the cutoff at the chain's midpoint, then derive -block-history-days
	// from the real wall clock so the cutoff actually lands there.
	epoch := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	targetHeight := tip / 2
	targetTime := epoch.Add(time.Duration(targetHeight) * 5 * time.Second)
	capDays := int(time.Since(targetTime).Hours() / 24)
	if capDays <= 0 {
		t.Fatalf("test environment clock is not far enough past the fake's simulated chain to build a mid-range cutoff (capDays=%d)", capDays)
	}
	s.blockHistoryDays = capDays

	// Derive the actual cutoff height from the same cutoff syncBlocks computes,
	// rather than from targetHeight directly, so AddDate's day-level rounding
	// doesn't skew the expected stop point.
	cutoff, ok := s.blockHistoryCutoff()
	if !ok {
		t.Fatalf("blockHistoryCutoff reported uncapped with blockHistoryDays=%d", capDays)
	}
	cutoffHeight := int(cutoff.Sub(epoch).Seconds() / 5)
	if cutoffHeight <= 1 || cutoffHeight >= tip {
		t.Fatalf("bad test setup: cutoff height %d is not strictly inside (1, %d)", cutoffHeight, tip)
	}

	for i := 0; i < 10; i++ {
		s.syncBlocks(context.Background())
		if done, _ := db.GetSyncState(blocksBackfillDoneKey("gnoland1")); done == "1" {
			break
		}
	}

	done, err := db.GetSyncState(blocksBackfillDoneKey("gnoland1"))
	if err != nil {
		t.Fatalf("get sync state: %v", err)
	}
	if done != "1" {
		t.Fatalf("backfill never marked done; test needs more iterations or a different cutoff")
	}

	minH, _, ok, err := db.BlockHeightBounds("gnoland1")
	if err != nil || !ok {
		t.Fatalf("bounds: ok=%v err=%v", ok, err)
	}
	if minH <= 1 {
		t.Fatalf("reached genesis instead of stopping at the cap (minH=%d)", minH)
	}
	// Page-level granularity means the stop height lands within one page of
	// the true cutoff height, not exactly on it.
	if minH > cutoffHeight {
		t.Errorf("did not walk back far enough: minH=%d, cutoff height=%d", minH, cutoffHeight)
	}
	if minH < cutoffHeight-blockPageSize {
		t.Errorf("walked back past the cutoff by more than a page: minH=%d, cutoff height=%d", minH, cutoffHeight)
	}
}

func TestSyncBlocksWideHistoryCapStillReachesGenesis(t *testing.T) {
	// A cap far wider than the available history must not stop the backfill
	// short — genesis wins, exactly as with no cap at all.
	s, fake, db := newTestSyncer(t, "gnoland1")
	s.blockHistoryDays = 100_000
	fake.setBlocks(1, 20)

	for i := 0; i < 5; i++ {
		s.syncBlocks(context.Background())
	}

	minH, _, ok, err := db.BlockHeightBounds("gnoland1")
	if err != nil || !ok {
		t.Fatalf("bounds: ok=%v err=%v", ok, err)
	}
	if minH != 1 {
		t.Errorf("did not reach genesis under a wide cap: minH=%d", minH)
	}
	done, err := db.GetSyncState(blocksBackfillDoneKey("gnoland1"))
	if err != nil {
		t.Fatalf("get sync state: %v", err)
	}
	if done != "1" {
		t.Errorf("backfill done flag = %q, want \"1\"", done)
	}
}

func TestSyncBlocksZeroHistoryDaysMeansUnlimited(t *testing.T) {
	// blockHistoryDays == 0 must behave exactly like the pre-flag code: no
	// cap, full backfill to genesis.
	s, fake, db := newTestSyncer(t, "gnoland1")
	s.blockHistoryDays = 0
	fake.setBlocks(1, 20)

	for i := 0; i < 5; i++ {
		s.syncBlocks(context.Background())
	}

	minH, _, ok, err := db.BlockHeightBounds("gnoland1")
	if err != nil || !ok {
		t.Fatalf("bounds: ok=%v err=%v", ok, err)
	}
	if minH != 1 {
		t.Errorf("blockHistoryDays=0 did not reach genesis: minH=%d", minH)
	}
	// No cap depth should ever be recorded under an unlimited backfill: a
	// leftover depth would make a later positive -block-history-days look like
	// a shallower cap that must be resumed from, when actually nothing is
	// capped at all.
	depth, err := db.GetSyncState(blocksBackfillDepthKey("gnoland1"))
	if err != nil {
		t.Fatalf("get sync state: %v", err)
	}
	if depth != "" {
		t.Errorf("recorded a backfill depth %q under an unlimited (0) cap", depth)
	}
}

func TestSyncBlocksResumesWhenHistoryDaysIsRaised(t *testing.T) {
	// Fix 3: the done flag is depth-blind by itself. If the operator raises
	// -block-history-days after a capped backfill already marked itself done,
	// the old flag must not permanently freeze history at the old, shallower
	// depth.
	s, fake, db := newTestSyncer(t, "gnoland1")
	s.blockHistoryDays = 1
	fake.setBlocks(1, 1_000_000)

	s.syncBlocks(context.Background())

	done, err := db.GetSyncState(blocksBackfillDoneKey("gnoland1"))
	if err != nil || done != "1" {
		t.Fatalf("precondition failed: backfill not marked done under the 1-day cap (done=%q err=%v)", done, err)
	}
	minCapped, _, ok, err := db.BlockHeightBounds("gnoland1")
	if err != nil || !ok {
		t.Fatalf("bounds after capped backfill: ok=%v err=%v", ok, err)
	}

	// Operator restarts with a much deeper (still capped) depth.
	s.blockHistoryDays = 5000
	beforeCalls := fake.rangeCalls
	s.syncBlocks(context.Background())

	if fake.rangeCalls <= beforeCalls {
		t.Error("no additional range queries were made after raising -block-history-days")
	}
	minAfter, _, ok, err := db.BlockHeightBounds("gnoland1")
	if err != nil || !ok {
		t.Fatalf("bounds after resume: ok=%v err=%v", ok, err)
	}
	if minAfter >= minCapped {
		t.Errorf("backfill did not resume walking backward after being raised: min stayed at %d (was %d)", minAfter, minCapped)
	}
}

func TestSyncBlocksCanBeDeclined(t *testing.T) {
	// A negative depth is the operator declining block persistence outright.
	s, fake, db := newTestSyncer(t, "gnoland1")
	s.blockHistoryDays = -1
	fake.setBlocks(1, 1_000_000)

	s.syncBlocks(context.Background())

	if _, _, ok, err := db.BlockHeightBounds("gnoland1"); err != nil || ok {
		t.Errorf("stored blocks despite -block-history-days<0: ok=%v err=%v", ok, err)
	}
	if fake.rangeCalls != 0 {
		t.Errorf("made %d indexer range queries with block sync declined, want 0", fake.rangeCalls)
	}
}
