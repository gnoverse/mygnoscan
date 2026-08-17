package main

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// oldSchemaNoBlockTime is the schema as it existed after the network column was
// added but before the time-series work introduced block_time. Databases in this
// shape exist in the wild, and opening one used to fail at startup.
const oldSchemaNoBlockTime = `
CREATE TABLE packages (
	network TEXT NOT NULL DEFAULT 'gnoland1',
	path TEXT NOT NULL,
	name TEXT NOT NULL,
	creator TEXT NOT NULL,
	block_height INTEGER NOT NULL,
	tx_hash TEXT NOT NULL,
	is_realm BOOLEAN NOT NULL,
	num_files INTEGER NOT NULL,
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
	PRIMARY KEY (network, path)
);
CREATE TABLE package_files (
	network TEXT NOT NULL DEFAULT 'gnoland1',
	package_path TEXT NOT NULL,
	file_name TEXT NOT NULL,
	body TEXT NOT NULL,
	PRIMARY KEY (network, package_path, file_name)
);
CREATE TABLE dependencies (
	network TEXT NOT NULL DEFAULT 'gnoland1',
	package_path TEXT NOT NULL,
	import_path TEXT NOT NULL,
	PRIMARY KEY (network, package_path, import_path)
);
CREATE TABLE calls (
	tx_hash TEXT NOT NULL,
	block_height INTEGER NOT NULL,
	caller TEXT NOT NULL,
	pkg_path TEXT NOT NULL,
	func_name TEXT NOT NULL,
	success BOOLEAN NOT NULL,
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
	network TEXT NOT NULL DEFAULT 'gnoland1',
	UNIQUE(network, tx_hash, pkg_path, func_name)
);
CREATE TABLE msg_runs (
	tx_hash TEXT NOT NULL,
	block_height INTEGER NOT NULL,
	caller TEXT NOT NULL,
	source TEXT NOT NULL,
	success BOOLEAN NOT NULL,
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
	network TEXT NOT NULL DEFAULT 'gnoland1',
	UNIQUE(network, tx_hash, caller)
);
CREATE TABLE bank_sends (
	tx_hash TEXT NOT NULL,
	block_height INTEGER NOT NULL,
	from_address TEXT NOT NULL,
	to_address TEXT NOT NULL,
	amount TEXT NOT NULL,
	success BOOLEAN NOT NULL,
	network TEXT NOT NULL DEFAULT 'gnoland1',
	UNIQUE(network, tx_hash, from_address, to_address)
);
`

func TestNewDBMigratesDatabaseWithoutBlockTime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")

	// Build a database in the pre-block_time shape and put a row in it, so the
	// migration is proven to preserve data and not just to succeed.
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	if _, err := raw.Exec(oldSchemaNoBlockTime); err != nil {
		t.Fatalf("create old schema: %v", err)
	}
	if _, err := raw.Exec(
		`INSERT INTO packages (network, path, name, creator, block_height, tx_hash, is_realm, num_files)
		 VALUES ('gnoland1', 'gno.land/r/demo/foo', 'foo', 'g1creator', 4242, 'TXHASH', 1, 1)`,
	); err != nil {
		t.Fatalf("seed package: %v", err)
	}
	if _, err := raw.Exec(
		`INSERT INTO calls (tx_hash, block_height, caller, pkg_path, func_name, success, network)
		 VALUES ('TXHASH', 4242, 'g1caller', 'gno.land/r/demo/foo', 'Bar', 1, 'gnoland1')`,
	); err != nil {
		t.Fatalf("seed call: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	// This used to fail with "no such column: block_time", because initSchema
	// builds indexes on a column that CREATE TABLE IF NOT EXISTS cannot add.
	db, err := NewDB(path)
	if err != nil {
		t.Fatalf("open old database: %v", err)
	}
	defer db.Close()

	for _, table := range blockTimeTables {
		has, err := columnExists(db.db, table, "block_time")
		if err != nil {
			t.Fatalf("inspect %s: %v", table, err)
		}
		if !has {
			t.Errorf("%s is still missing block_time", table)
		}
	}

	// Existing rows survive, and the sync cursor with them: it is derived from
	// the highest stored block height, so losing rows would trigger a re-sync.
	var height int
	if err := db.db.QueryRow(
		`SELECT block_height FROM packages WHERE path = 'gno.land/r/demo/foo'`,
	).Scan(&height); err != nil {
		t.Fatalf("read migrated package: %v", err)
	}
	if height != 4242 {
		t.Errorf("block_height = %d after migration, want 4242", height)
	}

	var callers int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM calls`).Scan(&callers); err != nil {
		t.Fatalf("count calls: %v", err)
	}
	if callers != 1 {
		t.Errorf("calls = %d after migration, want 1", callers)
	}

	// The migration must be idempotent: every start runs it again.
	if err := migrateAddBlockTime(db.db); err != nil {
		t.Errorf("second migration pass: %v", err)
	}
}

func TestNewDBOnEmptyDatabase(t *testing.T) {
	db, err := NewDB(filepath.Join(t.TempDir(), "fresh.db"))
	if err != nil {
		t.Fatalf("open fresh database: %v", err)
	}
	defer db.Close()

	for _, table := range networkScopedTables {
		exists, err := tableExists(db.db, table)
		if err != nil {
			t.Fatalf("inspect %s: %v", table, err)
		}
		if !exists {
			t.Errorf("%s was not created", table)
		}
	}
	for _, table := range blockTimeTables {
		has, err := columnExists(db.db, table, "block_time")
		if err != nil {
			t.Fatalf("inspect %s: %v", table, err)
		}
		if !has {
			t.Errorf("%s is missing block_time on a fresh database", table)
		}
	}
}

func TestNewDBIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reopen.db")

	db, err := NewDB(path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if err := db.UpsertPackage("topaz", "gno.land/r/demo/foo", "foo", "g1c", "TX", 7, "", true, 1); err != nil {
		t.Fatalf("seed: %v", err)
	}
	db.Close()

	// Reopening runs the whole migration path again over real data, which is
	// what happens on every restart and every deploy.
	db2, err := NewDB(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()

	height, err := db2.MaxBlockHeight("topaz")
	if err != nil {
		t.Fatalf("max block height: %v", err)
	}
	if height != 0 {
		// UpsertPackage does not write to transactions, so 0 is expected here;
		// the point is that reopening did not destroy anything.
		t.Logf("max block height from transactions: %d", height)
	}

	var count int
	if err := db2.db.QueryRow(`SELECT COUNT(*) FROM packages WHERE network = 'topaz'`).Scan(&count); err != nil {
		t.Fatalf("count packages: %v", err)
	}
	if count != 1 {
		t.Errorf("packages = %d after reopen, want 1", count)
	}
}

func TestBackfillBlockTimes(t *testing.T) {
	db, err := NewDB(filepath.Join(t.TempDir(), "backfill.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	// Rows as an older build would have left them: no block_time anywhere.
	seedNetwork(t, db, "gnoland1", 100)
	seedNetwork(t, db, "topaz", 100)
	for _, table := range backfillTables {
		if _, err := db.db.Exec(
			`UPDATE ` + table + ` SET block_time = '' WHERE network = 'gnoland1'`); err != nil {
			t.Fatalf("clear block_time on %s: %v", table, err)
		}
	}
	// topaz stands in for an already-healthy network: every row has a time.
	for _, table := range backfillTables {
		if _, err := db.db.Exec(
			`UPDATE ` + table + ` SET block_time = 'ALREADY-SET' WHERE network = 'topaz'`); err != nil {
			t.Fatalf("seed topaz %s: %v", table, err)
		}
	}

	heights, err := db.HeightsMissingBlockTime("gnoland1", 200)
	if err != nil {
		t.Fatalf("find heights: %v", err)
	}
	if len(heights) != 1 || heights[0] != 100 {
		t.Fatalf("heights = %v, want [100]", heights)
	}

	// Other networks are not reported as needing repair.
	if h, err := db.HeightsMissingBlockTime("topaz", 200); err != nil || len(h) != 0 {
		t.Errorf("topaz heights = %v (err %v), want none", h, err)
	}

	updated, err := db.SetBlockTimes("gnoland1", map[int]string{100: "2026-01-01T00:00:00Z"})
	if err != nil {
		t.Fatalf("set block times: %v", err)
	}
	if updated != int64(len(backfillTables)) {
		t.Errorf("updated %d rows, want %d (one per table)", updated, len(backfillTables))
	}

	for _, table := range backfillTables {
		var got string
		if err := db.db.QueryRow(
			`SELECT block_time FROM ` + table + ` WHERE network = 'gnoland1'`).Scan(&got); err != nil {
			t.Fatalf("read %s: %v", table, err)
		}
		if got != "2026-01-01T00:00:00Z" {
			t.Errorf("%s block_time = %q, want the backfilled value", table, got)
		}
	}

	// Nothing left to do, and the repair is idempotent.
	if h, err := db.HeightsMissingBlockTime("gnoland1", 200); err != nil || len(h) != 0 {
		t.Errorf("after backfill heights = %v (err %v), want none", h, err)
	}

	// An existing timestamp is never overwritten — this repairs history, it
	// does not rewrite it.
	if _, err := db.SetBlockTimes("topaz", map[int]string{100: "2099-01-01T00:00:00Z"}); err != nil {
		t.Fatalf("set block times on topaz: %v", err)
	}
	var topaz string
	if err := db.db.QueryRow(
		`SELECT block_time FROM calls WHERE network = 'topaz'`).Scan(&topaz); err != nil {
		t.Fatalf("read topaz: %v", err)
	}
	if topaz != "ALREADY-SET" {
		t.Errorf("topaz block_time = %q, want it left untouched", topaz)
	}
}

func TestBackfillCoversStorageEvents(t *testing.T) {
	// storage_events joined backfillTables after a review found that a
	// transient fetchBlockTimes failure stamps rows with BlockTime: "" that
	// every reader's `block_time >= ?` filter then hides forever, and the
	// standing backfill pass never saw the table because it wasn't listed.
	db, err := NewDB(filepath.Join(t.TempDir(), "backfill-storage.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	if err := db.UpsertStorageEvents("gnoland1", []StorageEventRow{{
		TxHash: "TX1", EventIndex: 0, BlockHeight: 100, BlockTime: "",
		PkgPath: "gno.land/r/demo/foo", Kind: "deposit", BytesDelta: 10,
	}}); err != nil {
		t.Fatalf("seed storage event: %v", err)
	}

	heights, err := db.HeightsMissingBlockTime("gnoland1", 200)
	if err != nil {
		t.Fatalf("find heights: %v", err)
	}
	if len(heights) != 1 || heights[0] != 100 {
		t.Fatalf("heights = %v, want [100]", heights)
	}

	updated, err := db.SetBlockTimes("gnoland1", map[int]string{100: "2026-01-01T00:00:00Z"})
	if err != nil {
		t.Fatalf("set block times: %v", err)
	}
	if updated != 1 {
		t.Errorf("updated %d rows, want 1", updated)
	}

	var got string
	if err := db.db.QueryRow(
		`SELECT block_time FROM storage_events WHERE network = 'gnoland1'`).Scan(&got); err != nil {
		t.Fatalf("read storage_events: %v", err)
	}
	if got != "2026-01-01T00:00:00Z" {
		t.Errorf("block_time = %q, want the backfilled value", got)
	}

	if h, err := db.HeightsMissingBlockTime("gnoland1", 200); err != nil || len(h) != 0 {
		t.Errorf("after backfill heights = %v (err %v), want none", h, err)
	}
}

func TestHeightsMissingTransactions(t *testing.T) {
	db, err := NewDB(filepath.Join(t.TempDir(), "txgap.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	// An event recorded with no transaction row behind it — the shape history
	// synced by a build predating the transactions table is left in.
	if err := db.InsertBankSend("gnoland1", "ORPHAN", 500, "", "g1a", "g1b", "1ugnot", true); err != nil {
		t.Fatalf("seed orphan: %v", err)
	}
	// And one that is properly paired.
	if err := db.InsertCall("gnoland1", "PAIRED", 600, "", "g1c", "gno.land/r/x", "F", true); err != nil {
		t.Fatalf("seed call: %v", err)
	}
	if err := db.UpsertTransaction("gnoland1", "PAIRED", 600, "", 10, 20, 1, true); err != nil {
		t.Fatalf("seed transaction: %v", err)
	}
	// Another network must not appear in the result.
	if err := db.InsertBankSend("topaz", "OTHER", 700, "", "g1a", "g1b", "1ugnot", true); err != nil {
		t.Fatalf("seed topaz: %v", err)
	}

	heights, err := db.HeightsMissingTransactions("gnoland1", 100)
	if err != nil {
		t.Fatalf("find heights: %v", err)
	}
	if len(heights) != 1 || heights[0] != 500 {
		t.Fatalf("heights = %v, want [500] (only the unpaired event)", heights)
	}

	// Once the transaction row lands, the gap closes.
	if err := db.UpsertTransaction("gnoland1", "ORPHAN", 500, "", 5, 6, 7, true); err != nil {
		t.Fatalf("backfill transaction: %v", err)
	}
	heights, err = db.HeightsMissingTransactions("gnoland1", 100)
	if err != nil {
		t.Fatalf("find heights after backfill: %v", err)
	}
	if len(heights) != 0 {
		t.Errorf("heights = %v after backfill, want none", heights)
	}
}

func TestGetGasStatsUsesStoredTransactions(t *testing.T) {
	db, err := NewDB(filepath.Join(t.TempDir(), "gas.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	if err := db.InsertCall("topaz", "T1", 10, "", "g1c", "gno.land/r/demo/hot", "Run", true); err != nil {
		t.Fatalf("seed call: %v", err)
	}
	if err := db.UpsertTransaction("topaz", "T1", 10, "", 1000, 2000, 30, true); err != nil {
		t.Fatalf("seed tx: %v", err)
	}
	if err := db.InsertCall("topaz", "T2", 11, "", "g1c", "gno.land/r/demo/hot", "Run", true); err != nil {
		t.Fatalf("seed call: %v", err)
	}
	if err := db.UpsertTransaction("topaz", "T2", 11, "", 500, 900, 10, false); err != nil {
		t.Fatalf("seed tx: %v", err)
	}
	// A different network's gas must not leak into the totals.
	if err := db.UpsertTransaction("gnoland1", "OTHER", 12, "", 99999, 99999, 99999, true); err != nil {
		t.Fatalf("seed other network: %v", err)
	}

	stats, err := db.GetGasStats("topaz", 20)
	if err != nil {
		t.Fatalf("gas stats: %v", err)
	}
	if stats.TotalTxs != 2 {
		t.Errorf("total txs = %d, want 2", stats.TotalTxs)
	}
	if stats.TotalGasUsed != 1500 {
		t.Errorf("gas used = %d, want 1500", stats.TotalGasUsed)
	}
	if stats.TotalFees != 40 {
		t.Errorf("fees = %d, want 40", stats.TotalFees)
	}
	if stats.SuccessCount != 1 || stats.FailCount != 1 {
		t.Errorf("success/fail = %d/%d, want 1/1", stats.SuccessCount, stats.FailCount)
	}
	if len(stats.TopRealms) != 1 || stats.TopRealms[0].Path != "gno.land/r/demo/hot" {
		t.Fatalf("top realms = %+v, want the one called realm", stats.TopRealms)
	}
	if stats.TopRealms[0].Gas != 1500 || stats.TopRealms[0].TxCount != 2 {
		t.Errorf("realm gas/txs = %d/%d, want 1500/2", stats.TopRealms[0].Gas, stats.TopRealms[0].TxCount)
	}
	if len(stats.TopTxs) == 0 || stats.TopTxs[0].Hash != "T1" {
		t.Errorf("top txs = %+v, want the most expensive first", stats.TopTxs)
	}
	if stats.TopTxs[0].Type != "MsgCall" {
		t.Errorf("type = %q, want MsgCall resolved from the call row", stats.TopTxs[0].Type)
	}
}

func TestUpsertTransactionsIsBatchedAndIdempotent(t *testing.T) {
	db, err := NewDB(filepath.Join(t.TempDir(), "batch.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	rows := []TxRow{
		{Hash: "A", BlockHeight: 1, BlockTime: "t1", GasUsed: 10, GasWanted: 20, GasFee: 1, Success: true},
		{Hash: "B", BlockHeight: 2, BlockTime: "t2", GasUsed: 30, GasWanted: 40, GasFee: 2, Success: false},
	}
	if err := db.UpsertTransactions("topaz", rows); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	var count, gas int
	if err := db.db.QueryRow(
		`SELECT COUNT(*), COALESCE(SUM(gas_used),0) FROM transactions WHERE network='topaz'`).
		Scan(&count, &gas); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if count != 2 || gas != 40 {
		t.Errorf("count/gas = %d/%d, want 2/40", count, gas)
	}

	// Re-running a backfill pass must not duplicate or corrupt rows.
	if err := db.UpsertTransactions("topaz", rows); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if err := db.db.QueryRow(
		`SELECT COUNT(*) FROM transactions WHERE network='topaz'`).Scan(&count); err != nil {
		t.Fatalf("recount: %v", err)
	}
	if count != 2 {
		t.Errorf("count = %d after repeat, want 2", count)
	}

	if err := db.UpsertTransactions("topaz", nil); err != nil {
		t.Errorf("empty batch should be a no-op, got %v", err)
	}
}

func TestBlockTimesForHeights(t *testing.T) {
	db, err := NewDB(filepath.Join(t.TempDir(), "times.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	if err := db.UpsertTransactions("topaz", []TxRow{
		{Hash: "A", BlockHeight: 10, BlockTime: "2026-01-01T00:00:00Z"},
		{Hash: "B", BlockHeight: 11, BlockTime: ""}, // synced before times were known
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := db.UpsertTransactions("gnoland1", []TxRow{
		{Hash: "C", BlockHeight: 10, BlockTime: "OTHER-NETWORK"},
	}); err != nil {
		t.Fatalf("seed other: %v", err)
	}

	got, err := db.BlockTimesForHeights("topaz", []int{10, 11, 12})
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if got[10] != "2026-01-01T00:00:00Z" {
		t.Errorf("height 10 = %q, want the stored time", got[10])
	}
	// Heights with no usable time are absent, so the caller knows to ask the
	// indexer for exactly those rather than for everything.
	if _, ok := got[11]; ok {
		t.Errorf("height 11 should be absent (empty stored time), got %q", got[11])
	}
	if _, ok := got[12]; ok {
		t.Errorf("height 12 should be absent (not stored), got %q", got[12])
	}

	if _, err := db.BlockTimesForHeights("topaz", nil); err != nil {
		t.Errorf("empty height list should be a no-op, got %v", err)
	}
}

func TestBackfillSkipsGenesisRows(t *testing.T) {
	db, err := NewDB(filepath.Join(t.TempDir(), "genesis.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	// Genesis-loaded packages: height 0, no transaction, no block, ever.
	if err := db.UpsertPackage("gnoland1", "gno.land/r/gen", "gen", "g1c", "", 0, "", true, 1); err != nil {
		t.Fatalf("seed genesis package: %v", err)
	}
	// A real deployment that genuinely needs repair.
	if err := db.UpsertPackage("gnoland1", "gno.land/r/real", "real", "g1c", "TX", 42, "", true, 1); err != nil {
		t.Fatalf("seed package: %v", err)
	}

	// Both backfills must ignore height 0, or they re-query an unanswerable
	// height on every sync pass for the life of the process.
	txHeights, err := db.HeightsMissingTransactions("gnoland1", 100)
	if err != nil {
		t.Fatalf("transaction gaps: %v", err)
	}
	for _, h := range txHeights {
		if h == 0 {
			t.Error("transaction backfill returned genesis height 0")
		}
	}
	if len(txHeights) != 1 || txHeights[0] != 42 {
		t.Errorf("transaction gaps = %v, want [42]", txHeights)
	}

	timeHeights, err := db.HeightsMissingBlockTime("gnoland1", 100)
	if err != nil {
		t.Fatalf("block time gaps: %v", err)
	}
	for _, h := range timeHeights {
		if h == 0 {
			t.Error("block time backfill returned genesis height 0")
		}
	}
}

func TestTimeseriesFormatMonthly(t *testing.T) {
	sqlFmt, step, truncFn := timeseriesFormat("monthly")

	if sqlFmt != "%Y-%m" {
		t.Errorf("sqlFmt = %q, want %q", sqlFmt, "%Y-%m")
	}
	// Must be at least the longest month, or the fillBuckets loop below stalls.
	if step < 31*24*time.Hour {
		t.Errorf("step = %v, want >= 31 days", step)
	}

	got := truncFn(time.Date(2026, 3, 17, 9, 30, 45, 0, time.UTC))
	want := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("truncFn = %v, want %v", got, want)
	}
}

func TestBucketKeyMonthly(t *testing.T) {
	got := bucketKey(time.Date(2026, 3, 17, 9, 0, 0, 0, time.UTC), "monthly")
	if got != "2026-03" {
		t.Errorf("bucketKey = %q, want %q", got, "2026-03")
	}
}

// The fillBuckets loop advances with cur = truncFn(cur.Add(step)). If a monthly
// step ever truncates back into the month it started in, the loop never
// terminates and the request hangs. Walk two years, including a leap February.
func TestMonthlyStepAlwaysAdvances(t *testing.T) {
	_, step, truncFn := timeseriesFormat("monthly")

	cur := truncFn(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))
	for i := 0; i < 24; i++ {
		next := truncFn(cur.Add(step))
		if !next.After(cur) {
			t.Fatalf("monthly step did not advance from %v (got %v)", cur, next)
		}
		cur = next
	}
}

// newTestDB opens a real temp-file database. The driver is pure Go, so this
// works everywhere including CI, and the repo prefers a real database over a
// mock in tests.
func newTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := NewDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestInternProposer(t *testing.T) {
	db := newTestDB(t)

	a1, err := db.InternProposer("gnoland1", "g1aaa")
	if err != nil {
		t.Fatalf("intern: %v", err)
	}
	a2, err := db.InternProposer("gnoland1", "g1aaa")
	if err != nil {
		t.Fatalf("intern repeat: %v", err)
	}
	if a1 != a2 {
		t.Errorf("same address interned to different ids: %d vs %d", a1, a2)
	}

	// The same validator address on two chains must never share an id, or a
	// per-network aggregate would silently mix chains.
	b1, err := db.InternProposer("test12", "g1aaa")
	if err != nil {
		t.Fatalf("intern other network: %v", err)
	}
	if b1 == a1 {
		t.Errorf("same address on two networks shares id %d", a1)
	}
}

func TestUpsertBlockIsIdempotent(t *testing.T) {
	db := newTestDB(t)
	pid, _ := db.InternProposer("gnoland1", "g1aaa")

	for i := 0; i < 3; i++ {
		if err := db.UpsertBlock("gnoland1", 100, "2026-08-13T13:00:00Z", pid, 2); err != nil {
			t.Fatalf("upsert %d: %v", i, err)
		}
	}

	var n int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM blocks WHERE network = ?`, "gnoland1").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("row count = %d, want 1 after repeated upsert", n)
	}
}

func TestBlockHeightBounds(t *testing.T) {
	db := newTestDB(t)
	pid, _ := db.InternProposer("gnoland1", "g1aaa")

	if _, _, ok, err := db.BlockHeightBounds("gnoland1"); err != nil || ok {
		t.Fatalf("empty table: ok = %v, err = %v; want ok=false, err=nil", ok, err)
	}

	for _, h := range []int{50, 51, 52} {
		if err := db.UpsertBlock("gnoland1", h, "2026-08-13T13:00:00Z", pid, 0); err != nil {
			t.Fatalf("upsert %d: %v", h, err)
		}
	}
	// A different network's rows must not move this network's cursors.
	opid, _ := db.InternProposer("test12", "g1bbb")
	if err := db.UpsertBlock("test12", 9999, "2026-08-13T13:00:00Z", opid, 0); err != nil {
		t.Fatalf("upsert other network: %v", err)
	}

	minH, maxH, ok, err := db.BlockHeightBounds("gnoland1")
	if err != nil || !ok {
		t.Fatalf("ok = %v, err = %v; want ok=true, err=nil", ok, err)
	}
	if minH != 50 || maxH != 52 {
		t.Errorf("bounds = (%d, %d), want (50, 52)", minH, maxH)
	}
}

// seedBlocks stores blocks at fixed 1-second-multiples from a base time so
// delta bins are exactly predictable.
func seedBlocks(t *testing.T, db *DB, network string, proposer string, base time.Time, offsets []float64) {
	t.Helper()
	pid, err := db.InternProposer(network, proposer)
	if err != nil {
		t.Fatalf("intern: %v", err)
	}
	for i, off := range offsets {
		ts := base.Add(time.Duration(off * float64(time.Second))).UTC().Format("2006-01-02T15:04:05.000000000Z")
		if err := db.UpsertBlock(network, 1000+i, ts, pid, i); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}
}

func TestGetBlockTimeHistogram(t *testing.T) {
	db := newTestDB(t)
	base := time.Now().UTC().Add(-2 * time.Hour)
	// Cumulative offsets producing deltas of exactly:
	//   4.2 (bin 4.0-4.5), 4.5 (bin 4.5-5.0, lower edge is inclusive),
	//   6.5 (bin 6.0-7.0), 12.0 (bin >=10.0)
	seedBlocks(t, db, "gnoland1", "g1aaa", base, []float64{0, 4.2, 8.7, 15.2, 27.2})

	bins, err := db.GetBlockTimeHistogram("gnoland1", 1)
	if err != nil {
		t.Fatalf("histogram: %v", err)
	}

	got := map[string]int{}
	total := 0
	for _, b := range bins {
		got[b.Bin] = b.Blocks
		total += b.Blocks
	}
	// 5 blocks produce 4 deltas; the first block has a NULL delta and must be
	// excluded rather than counted as a zero-second interval.
	if total != 4 {
		t.Errorf("total binned = %d, want 4 (5 blocks - 1 with no predecessor)", total)
	}
	for bin, want := range map[string]int{"4.0-4.5": 1, "4.5-5.0": 1, "6.0-7.0": 1, ">=10.0": 1} {
		if got[bin] != want {
			t.Errorf("bin %q = %d, want %d (all bins: %v)", bin, got[bin], want, got)
		}
	}
}

func TestGetBlockProposersIsNetworkScoped(t *testing.T) {
	// Two networks holding blocks at the SAME heights. AGENTS.md calls this the
	// failure mode that goes wrong silently.
	db := newTestDB(t)
	base := time.Now().UTC().Add(-2 * time.Hour)
	seedBlocks(t, db, "gnoland1", "g1aaa", base, []float64{0, 5, 10})
	seedBlocks(t, db, "test12", "g1bbb", base, []float64{0, 5, 10})

	got, err := db.GetBlockProposers("gnoland1", 1, 10)
	if err != nil {
		t.Fatalf("proposers: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d proposers, want 1 (only gnoland1's)", len(got))
	}
	if got[0].Address != "g1aaa" {
		t.Errorf("address = %q, want g1aaa", got[0].Address)
	}
	if got[0].Blocks != 3 {
		t.Errorf("blocks = %d, want 3 — other network's rows leaked in", got[0].Blocks)
	}
}

func TestGetBlockCoverage(t *testing.T) {
	db := newTestDB(t)

	cov, err := db.GetBlockCoverage("gnoland1")
	if err != nil {
		t.Fatalf("empty coverage: %v", err)
	}
	if cov.Complete || cov.MinTime != "" {
		t.Errorf("empty table: %+v, want zero value and Complete=false", cov)
	}

	base := time.Now().UTC().Add(-2 * time.Hour)
	seedBlocks(t, db, "gnoland1", "g1aaa", base, []float64{0, 5})
	if err := db.SetSyncState(blocksBackfillDoneKey("gnoland1"), "1"); err != nil {
		t.Fatalf("set state: %v", err)
	}

	cov, err = db.GetBlockCoverage("gnoland1")
	if err != nil {
		t.Fatalf("coverage: %v", err)
	}
	if !cov.Complete {
		t.Error("Complete = false after the done flag was set")
	}
	if cov.MinTime == "" || cov.MaxTime == "" {
		t.Errorf("times not populated: %+v", cov)
	}
}

// --- batch 2b ---

// rfc3339 formats a test timestamp the way the syncer stores them.
func rfc3339(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func TestGetActivityHeatmapShape(t *testing.T) {
	db := newTestDB(t)
	// 2026-08-10 is a Monday; 14:00 UTC on it must land at (hour 14, dow 0).
	monday := time.Date(2026, 8, 10, 14, 0, 0, 0, time.UTC)
	// Anchor relative to now so the day-window filter cannot exclude the rows,
	// while keeping the weekday fixed: step back whole weeks from a known Monday
	// until the timestamp is in the past but inside a 30-day window.
	ts := monday
	for ts.After(time.Now().UTC()) {
		ts = ts.AddDate(0, 0, -7)
	}
	for ts.Before(time.Now().UTC().AddDate(0, 0, -20)) {
		ts = ts.AddDate(0, 0, 7)
	}
	if ts.After(time.Now().UTC()) {
		t.Skip("no Monday 14:00 UTC inside the test window right now")
	}
	wantDow := (int(ts.Weekday()) + 6) % 7

	for i := range 3 {
		if err := db.InsertCall("gnoland1", fmt.Sprintf("h%d", i), 1+i, rfc3339(ts), "g1a", "gno.land/r/x", "F", true); err != nil {
			t.Fatalf("insert call: %v", err)
		}
	}
	if err := db.InsertBankSend("gnoland1", "s1", 9, rfc3339(ts), "g1b", "g1c", "1ugnot", true); err != nil {
		t.Fatalf("insert send: %v", err)
	}
	// Another network's rows must not appear in a network-scoped read.
	if err := db.InsertCall("test12", "o1", 1, rfc3339(ts), "g1z", "gno.land/r/x", "F", true); err != nil {
		t.Fatalf("insert other: %v", err)
	}

	cells, err := db.GetActivityHeatmap("gnoland1", 30)
	if err != nil {
		t.Fatalf("heatmap: %v", err)
	}
	if len(cells) != 24*7 {
		t.Fatalf("got %d cells, want the full 24x7 grid", len(cells))
	}
	total := 0
	for _, c := range cells {
		if c.Hour < 0 || c.Hour > 23 || c.Dow < 0 || c.Dow > 6 {
			t.Fatalf("cell out of range: %+v", c)
		}
		total += c.Messages
		if c.Hour == 14 && c.Dow == wantDow && c.Messages != 4 {
			t.Errorf("cell (14, %d) = %d messages, want 4 (3 calls + 1 send)", wantDow, c.Messages)
		}
	}
	if total != 4 {
		t.Errorf("grid total = %d, want 4 — another network's rows leaked in", total)
	}
}

func TestGetNewAddressTimeSeriesCountsFirstSeenOnly(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().UTC()
	old := now.AddDate(0, 0, -20)
	recent := now.AddDate(0, 0, -2)

	// g1old first appears outside the 7-day window, then again inside it: it is
	// acquisition for the old bucket only, and must not be counted twice.
	mustCall(t, db, "gnoland1", "t1", 1, old, "g1old", "gno.land/r/x", "F")
	mustCall(t, db, "gnoland1", "t2", 2, recent, "g1old", "gno.land/r/x", "F")
	mustCall(t, db, "gnoland1", "t3", 3, recent, "g1new", "gno.land/r/x", "F")

	pts, err := db.GetNewAddressTimeSeries("gnoland1", "daily", 7)
	if err != nil {
		t.Fatalf("new addresses: %v", err)
	}
	total := 0
	for _, p := range pts {
		total += p.NewAddresses
	}
	if total != 1 {
		t.Errorf("new addresses over 7d = %d, want 1 (g1new only; g1old was first seen 20d ago)", total)
	}

	// Widening the window past g1old's first appearance must pick it up.
	pts, err = db.GetNewAddressTimeSeries("gnoland1", "daily", 30)
	if err != nil {
		t.Fatalf("new addresses 30d: %v", err)
	}
	total = 0
	for _, p := range pts {
		total += p.NewAddresses
	}
	if total != 2 {
		t.Errorf("new addresses over 30d = %d, want 2", total)
	}
}

func TestGetRollingActiveTimeSeries(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().UTC()

	// One address active 20 days ago, one active today. On today's point DAU
	// and WAU see only the recent one; MAU's 30-day window sees both.
	mustCall(t, db, "gnoland1", "a1", 1, now.AddDate(0, 0, -20), "g1old", "gno.land/r/x", "F")
	mustCall(t, db, "gnoland1", "a2", 2, now.Add(-1*time.Hour), "g1now", "gno.land/r/x", "F")

	pts, err := db.GetRollingActiveTimeSeries("gnoland1", 10)
	if err != nil {
		t.Fatalf("rolling: %v", err)
	}
	if len(pts) != 11 {
		t.Fatalf("got %d points, want 11 (days+1)", len(pts))
	}
	last := pts[len(pts)-1]
	if last.Time != now.Format("2006-01-02") {
		t.Errorf("last point = %q, want today (%s)", last.Time, now.Format("2006-01-02"))
	}
	if last.DAU != 1 {
		t.Errorf("DAU = %d, want 1", last.DAU)
	}
	if last.WAU != 1 {
		t.Errorf("WAU = %d, want 1 — the 20-day-old address is outside the 7-day window", last.WAU)
	}
	if last.MAU != 2 {
		t.Errorf("MAU = %d, want 2 — the 30-day window must reach back before the requested range", last.MAU)
	}
	for _, p := range pts {
		if p.DAU > p.WAU || p.WAU > p.MAU {
			t.Errorf("%s: DAU %d <= WAU %d <= MAU %d violated", p.Time, p.DAU, p.WAU, p.MAU)
		}
	}
}

func TestGetRollingActiveTimeSeriesFloorsShortWindows(t *testing.T) {
	db := newTestDB(t)
	pts, err := db.GetRollingActiveTimeSeries("gnoland1", 1)
	if err != nil {
		t.Fatalf("rolling: %v", err)
	}
	if len(pts) != rollingMinDays+1 {
		t.Errorf("got %d points for a 1-day request, want %d — a single column is not a shape", len(pts), rollingMinDays+1)
	}
}

func TestGetGasPerTxHistogram(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().UTC()

	cases := []struct {
		hash    string
		gasUsed int
		bin     string
	}{
		{"g1", 50_000, "<100k"},
		{"g2", 100_000, "100k-500k"}, // lower edge is inclusive
		{"g3", 750_000, "500k-1M"},
		{"g4", 65_000_000, "50M-100M"},
		{"g5", 900_000_000, ">=500M"},
	}
	for i, c := range cases {
		if err := db.UpsertTransaction("gnoland1", c.hash, i+1, rfc3339(now.Add(-time.Hour)), c.gasUsed, c.gasUsed, 1, true); err != nil {
			t.Fatalf("upsert tx: %v", err)
		}
	}
	// gas_used = 0 is the never-backfilled default, not a free transaction.
	if err := db.UpsertTransaction("gnoland1", "zero", 99, rfc3339(now.Add(-time.Hour)), 0, 0, 0, true); err != nil {
		t.Fatalf("upsert zero tx: %v", err)
	}

	bins, err := db.GetGasPerTxHistogram("gnoland1", 7)
	if err != nil {
		t.Fatalf("histogram: %v", err)
	}
	if len(bins) != len(GasPerTxBinOrder) {
		t.Fatalf("got %d bins, want the full fixed set of %d", len(bins), len(GasPerTxBinOrder))
	}
	got := map[string]int{}
	total := 0
	for i, b := range bins {
		if b.Bin != GasPerTxBinOrder[i] {
			t.Errorf("bin %d = %q, want %q — order is the x-axis", i, b.Bin, GasPerTxBinOrder[i])
		}
		got[b.Bin] = b.Txs
		total += b.Txs
	}
	if total != len(cases) {
		t.Errorf("total = %d, want %d — the gas_used=0 row must be excluded", total, len(cases))
	}
	for _, c := range cases {
		if got[c.bin] != 1 {
			t.Errorf("gas %d landed outside bin %q (all bins: %v)", c.gasUsed, c.bin, got)
		}
	}
}

func TestGetFunctionCallHeatmapIsAZeroFilledGrid(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().UTC()

	mustCall(t, db, "gnoland1", "f1", 1, now.Add(-2*time.Hour), "g1a", "gno.land/r/hot", "Busy")
	mustCall(t, db, "gnoland1", "f2", 2, now.Add(-3*time.Hour), "g1b", "gno.land/r/hot", "Busy")
	mustCall(t, db, "gnoland1", "f3", 3, now.Add(-4*time.Hour), "g1c", "gno.land/r/hot", "Quiet")
	// A different realm, and the same path on another network: neither may show.
	mustCall(t, db, "gnoland1", "f4", 4, now.Add(-2*time.Hour), "g1d", "gno.land/r/other", "Elsewhere")
	mustCall(t, db, "test12", "f5", 5, now.Add(-2*time.Hour), "g1e", "gno.land/r/hot", "OtherChain")

	cells, err := db.GetFunctionCallHeatmap("gnoland1", "gno.land/r/hot", 14)
	if err != nil {
		t.Fatalf("heatmap: %v", err)
	}
	if len(cells) != 2*14 {
		t.Fatalf("got %d cells, want 2 functions x 14 days zero-filled", len(cells))
	}
	if cells[0].Func != "Busy" {
		t.Errorf("first function = %q, want Busy (busiest first)", cells[0].Func)
	}
	byFunc := map[string]int{}
	days := map[string]bool{}
	for _, c := range cells {
		byFunc[c.Func] += c.Calls
		days[c.Day] = true
	}
	if len(days) != 14 {
		t.Errorf("got %d distinct days, want 14", len(days))
	}
	if byFunc["Busy"] != 2 || byFunc["Quiet"] != 1 {
		t.Errorf("call totals = %v, want Busy 2 / Quiet 1 — another realm or network leaked in", byFunc)
	}
	if _, ok := byFunc["OtherChain"]; ok {
		t.Error("a same-path realm on another network leaked into the grid")
	}

	// An unknown realm is empty, not an error, so the card says "no data".
	empty, err := db.GetFunctionCallHeatmap("gnoland1", "gno.land/r/nope", 14)
	if err != nil || len(empty) != 0 {
		t.Errorf("unknown realm: %d cells, err %v; want 0 and nil", len(empty), err)
	}
}

func TestGetRealmsWithCallsIsOrderedAndScoped(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().UTC()

	for i := range 3 {
		mustCall(t, db, "gnoland1", fmt.Sprintf("r%d", i), i+1, now.Add(-time.Hour), "g1a", "gno.land/r/busy", "F")
	}
	mustCall(t, db, "gnoland1", "rq", 10, now.Add(-time.Hour), "g1a", "gno.land/r/quiet", "F")
	mustCall(t, db, "test12", "rx", 11, now.Add(-time.Hour), "g1a", "gno.land/r/elsewhere", "F")

	got, err := db.GetRealmsWithCalls("gnoland1", 14, 0)
	if err != nil {
		t.Fatalf("realms: %v", err)
	}
	want := []string{"gno.land/r/busy", "gno.land/r/quiet"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("realm %d = %q, want %q (busiest first, other networks excluded)", i, got[i], want[i])
		}
	}
}

// --- Fix 1: one malformed block_time row must not 500 the whole chart ---
//
// block_time is nullable TEXT and the window predicate is a string comparison,
// so "not-a-timestamp" >= "2026-...": 'n' > '2' passes it. strftime() then
// yields NULL for that row, which used to fail rows.Scan outright. These tests
// insert exactly that value — the same one
// TestNetworkDataStartPropagatesUnparseableTimestamp uses — alongside good
// rows and assert the call succeeds and the good data still comes through.

func TestGetActivityHeatmapSkipsUnparseableBlockTime(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().UTC()

	mustCall(t, db, "gnoland1", "good", 1, now.Add(-time.Hour), "g1a", "gno.land/r/x", "F")
	if err := db.InsertCall("gnoland1", "bad", 2, "not-a-timestamp", "g1b", "gno.land/r/x", "F", true); err != nil {
		t.Fatalf("insert bad row: %v", err)
	}

	cells, err := db.GetActivityHeatmap("gnoland1", 7)
	if err != nil {
		t.Fatalf("heatmap errored on one bad row: %v", err)
	}
	total := 0
	for _, c := range cells {
		total += c.Messages
	}
	if total != 1 {
		t.Errorf("total = %d, want 1 (the good row); the bad row should be skipped, not counted", total)
	}
}

func TestGetNewAddressTimeSeriesSkipsUnparseableBlockTime(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().UTC()

	mustCall(t, db, "gnoland1", "good", 1, now.Add(-time.Hour), "g1good", "gno.land/r/x", "F")
	if err := db.InsertCall("gnoland1", "bad", 2, "not-a-timestamp", "g1bad", "gno.land/r/x", "F", true); err != nil {
		t.Fatalf("insert bad row: %v", err)
	}

	pts, err := db.GetNewAddressTimeSeries("gnoland1", "daily", 7)
	if err != nil {
		t.Fatalf("new addresses errored on one bad row: %v", err)
	}
	total := 0
	for _, p := range pts {
		total += p.NewAddresses
	}
	if total != 1 {
		t.Errorf("total = %d, want 1 (g1good only); the bad row should be skipped, not counted or errored", total)
	}
}

func TestGetRollingActiveTimeSeriesSkipsUnparseableBlockTime(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().UTC()

	mustCall(t, db, "gnoland1", "good", 1, now, "g1good", "gno.land/r/x", "F")
	if err := db.InsertCall("gnoland1", "bad", 2, "not-a-timestamp", "g1bad", "gno.land/r/x", "F", true); err != nil {
		t.Fatalf("insert bad row: %v", err)
	}

	pts, err := db.GetRollingActiveTimeSeries("gnoland1", 7)
	if err != nil {
		t.Fatalf("rolling errored on one bad row: %v", err)
	}
	last := pts[len(pts)-1]
	if last.DAU != 1 {
		t.Errorf("DAU = %d, want 1 (g1good only); the bad row should be skipped, not counted", last.DAU)
	}
}

func TestGetFunctionCallHeatmapSkipsUnparseableBlockTime(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().UTC()

	mustCall(t, db, "gnoland1", "good", 1, now.Add(-time.Hour), "g1a", "gno.land/r/x", "Good")
	if err := db.InsertCall("gnoland1", "bad", 2, "not-a-timestamp", "g1b", "gno.land/r/x", "Bad", true); err != nil {
		t.Fatalf("insert bad row: %v", err)
	}

	cells, err := db.GetFunctionCallHeatmap("gnoland1", "gno.land/r/x", 14)
	if err != nil {
		t.Fatalf("function heatmap errored on one bad row: %v", err)
	}
	byFunc := map[string]int{}
	for _, c := range cells {
		byFunc[c.Func] += c.Calls
	}
	if byFunc["Good"] != 1 {
		t.Errorf("Good calls = %d, want 1", byFunc["Good"])
	}
	if _, ok := byFunc["Bad"]; ok {
		t.Errorf("the bad row's function should not appear at all: %v", byFunc)
	}
}

// --- Fix 5: the heatmap window must be a whole number of weeks ---

func TestGetActivityHeatmapSnapsWindowToWholeWeeks(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().UTC()

	// One call on each of the last 90 days. A raw 90-day window spans 12.857
	// weeks, so some weekday columns pick up 13 occurrences of that weekday
	// while others only pick up 12 — a systematic inflation. Snapped down to
	// a whole number of weeks (90/7 = 12 weeks = 84 days), every weekday
	// column sees exactly 12 occurrences, so all seven totals must match.
	//
	// Each timestamp is nudged a minute earlier than the exact day boundary.
	// GetActivityHeatmap computes its own "now" a moment after this test
	// captured its own, so an unnudged timestamp sitting exactly on the
	// 84-day cutoff could land on either side of the boundary depending on
	// that gap — a one-minute margin keeps every row's window membership
	// deterministic while leaving the daily bucketing untouched.
	for i := 0; i < 90; i++ {
		ts := now.AddDate(0, 0, -i).Add(-time.Minute)
		if err := db.InsertCall("gnoland1", fmt.Sprintf("d%d", i), i+1, rfc3339(ts), "g1a", "gno.land/r/x", "F", true); err != nil {
			t.Fatalf("insert call %d: %v", i, err)
		}
	}

	cells, err := db.GetActivityHeatmap("gnoland1", 90)
	if err != nil {
		t.Fatalf("heatmap: %v", err)
	}
	perDow := make(map[int]int)
	for _, c := range cells {
		perDow[c.Dow] += c.Messages
	}
	total := 0
	for _, n := range perDow {
		total += n
	}
	if total == 0 {
		t.Fatal("no messages counted at all")
	}
	var want int
	allEqual := true
	for dow := 0; dow < 7; dow++ {
		n := perDow[dow]
		if dow == 0 {
			want = n
		} else if n != want {
			allEqual = false
		}
	}
	if !allEqual {
		t.Errorf("weekday totals are not equal — window is not a whole number of weeks: Mon=%d Tue=%d Wed=%d Thu=%d Fri=%d Sat=%d Sun=%d",
			perDow[0], perDow[1], perDow[2], perDow[3], perDow[4], perDow[5], perDow[6])
	}
}

// --- Fix 4: the active-address definition must agree across both endpoints ---

func TestActiveAddressAndActivityHeatmapAgreeOnMsgRuns(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().UTC()

	if err := db.InsertMsgRun("gnoland1", "r1", 1, rfc3339(now.Add(-time.Hour)), "g1runner", "source", true); err != nil {
		t.Fatalf("insert msg run: %v", err)
	}

	pts, err := db.GetActiveAddressTimeSeries("gnoland1", "daily", 7)
	if err != nil {
		t.Fatalf("active addresses: %v", err)
	}
	total := 0
	for _, p := range pts {
		total += p.TotalActive
	}
	if total != 1 {
		t.Errorf("GetActiveAddressTimeSeries total_active = %d, want 1 — msg_runs.caller must count as an active address", total)
	}

	cells, err := db.GetActivityHeatmap("gnoland1", 7)
	if err != nil {
		t.Fatalf("heatmap: %v", err)
	}
	heatmapTotal := 0
	for _, c := range cells {
		heatmapTotal += c.Messages
	}
	if heatmapTotal != total {
		t.Errorf("heatmap total = %d, active-address total = %d — the two endpoints disagree on the same fixture and window", heatmapTotal, total)
	}
}

// --- Fix 7: ?limit= on the realm selector must be capped ---

func TestGetRealmsWithCallsCapsLimit(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().UTC()

	for i := 0; i < realmsWithCallsMaxLimit+20; i++ {
		mustCall(t, db, "gnoland1", fmt.Sprintf("c%d", i), i+1, now.Add(-time.Hour), "g1a", fmt.Sprintf("gno.land/r/x%d", i), "F")
	}

	got, err := db.GetRealmsWithCalls("gnoland1", 14, realmsWithCallsMaxLimit+20)
	if err != nil {
		t.Fatalf("realms: %v", err)
	}
	if len(got) != realmsWithCallsMaxLimit {
		t.Errorf("got %d realms, want the cap of %d", len(got), realmsWithCallsMaxLimit)
	}
}

// --- Fix 6: network == "" (networkParam's "all networks") must union, not empty ---

func TestGetActivityHeatmapAllNetworks(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().UTC()

	mustCall(t, db, "gnoland1", "a1", 1, now.Add(-time.Hour), "g1a", "gno.land/r/x", "F")
	mustCall(t, db, "test12", "a2", 2, now.Add(-time.Hour), "g1b", "gno.land/r/x", "F")

	cells, err := db.GetActivityHeatmap("", 7)
	if err != nil {
		t.Fatalf("heatmap: %v", err)
	}
	total := 0
	for _, c := range cells {
		total += c.Messages
	}
	if total != 2 {
		t.Errorf("total = %d, want 2 (the union of both networks)", total)
	}
}

func TestGetNewAddressTimeSeriesAllNetworks(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().UTC()

	mustCall(t, db, "gnoland1", "a1", 1, now.Add(-time.Hour), "g1a", "gno.land/r/x", "F")
	mustCall(t, db, "test12", "a2", 2, now.Add(-time.Hour), "g1b", "gno.land/r/x", "F")

	pts, err := db.GetNewAddressTimeSeries("", "daily", 7)
	if err != nil {
		t.Fatalf("new addresses: %v", err)
	}
	total := 0
	for _, p := range pts {
		total += p.NewAddresses
	}
	if total != 2 {
		t.Errorf("total = %d, want 2 (one new address per network)", total)
	}
}

func TestGetRollingActiveTimeSeriesAllNetworks(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().UTC()

	mustCall(t, db, "gnoland1", "a1", 1, now, "g1a", "gno.land/r/x", "F")
	mustCall(t, db, "test12", "a2", 2, now, "g1b", "gno.land/r/x", "F")

	pts, err := db.GetRollingActiveTimeSeries("", 7)
	if err != nil {
		t.Fatalf("rolling: %v", err)
	}
	last := pts[len(pts)-1]
	if last.DAU != 2 {
		t.Errorf("DAU = %d, want 2 (the union of both networks)", last.DAU)
	}
}

func TestGetGasPerTxHistogramAllNetworks(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().UTC()

	if err := db.UpsertTransaction("gnoland1", "t1", 1, rfc3339(now.Add(-time.Hour)), 50_000, 50_000, 1, true); err != nil {
		t.Fatalf("upsert tx 1: %v", err)
	}
	if err := db.UpsertTransaction("test12", "t2", 2, rfc3339(now.Add(-time.Hour)), 50_000, 50_000, 1, true); err != nil {
		t.Fatalf("upsert tx 2: %v", err)
	}

	bins, err := db.GetGasPerTxHistogram("", 7)
	if err != nil {
		t.Fatalf("histogram: %v", err)
	}
	total := 0
	for _, b := range bins {
		total += b.Txs
	}
	if total != 2 {
		t.Errorf("total = %d, want 2 (the union of both networks)", total)
	}
}

func TestGetRealmsWithCallsAllNetworks(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().UTC()

	mustCall(t, db, "gnoland1", "a1", 1, now.Add(-time.Hour), "g1a", "gno.land/r/x", "F")
	mustCall(t, db, "test12", "a2", 2, now.Add(-time.Hour), "g1b", "gno.land/r/y", "F")

	got, err := db.GetRealmsWithCalls("", 14, 0)
	if err != nil {
		t.Fatalf("realms: %v", err)
	}
	want := map[string]bool{"gno.land/r/x": true, "gno.land/r/y": true}
	if len(got) != len(want) {
		t.Fatalf("got %v, want both realms across both networks", got)
	}
	for _, p := range got {
		if !want[p] {
			t.Errorf("unexpected realm %q", p)
		}
	}
}

func TestGetFunctionCallHeatmapAllNetworks(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().UTC()

	mustCall(t, db, "gnoland1", "a1", 1, now.Add(-time.Hour), "g1a", "gno.land/r/x", "F1")
	mustCall(t, db, "test12", "a2", 2, now.Add(-time.Hour), "g1b", "gno.land/r/x", "F2")

	cells, err := db.GetFunctionCallHeatmap("", "gno.land/r/x", 14)
	if err != nil {
		t.Fatalf("heatmap: %v", err)
	}
	byFunc := map[string]int{}
	for _, c := range cells {
		byFunc[c.Func] += c.Calls
	}
	if byFunc["F1"] != 1 || byFunc["F2"] != 1 {
		t.Errorf("call totals = %v, want F1: 1, F2: 1 (the union of both networks)", byFunc)
	}
}

func mustCall(t *testing.T, db *DB, network, hash string, height int, ts time.Time, caller, pkgPath, fn string) {
	t.Helper()
	if err := db.InsertCall(network, hash, height, rfc3339(ts), caller, pkgPath, fn, true); err != nil {
		t.Fatalf("insert call: %v", err)
	}
}

func TestOldestBlockTime(t *testing.T) {
	db := newTestDB(t)

	if _, ok, err := db.OldestBlockTime("gnoland1"); err != nil || ok {
		t.Fatalf("empty table: ok = %v, err = %v; want false, nil", ok, err)
	}

	base := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Second)
	seedBlocks(t, db, "gnoland1", "g1aaa", base, []float64{0, 60, 120})
	seedBlocks(t, db, "test12", "g1bbb", base.Add(-48*time.Hour), []float64{0})

	got, ok, err := db.OldestBlockTime("gnoland1")
	if err != nil || !ok {
		t.Fatalf("ok = %v, err = %v", ok, err)
	}
	if !got.Equal(base) {
		t.Errorf("oldest = %s, want %s — another network's older block leaked in", got, base)
	}
}

func TestNetworkDataStart(t *testing.T) {
	db := newTestDB(t)

	if _, ok, err := db.NetworkDataStart("gnoland1"); err != nil || ok {
		t.Fatalf("empty database: ok = %v, err = %v; want ok=false, err=nil", ok, err)
	}

	oldest := "2026-08-07T12:00:00Z"
	newer := "2026-08-14T12:00:00Z"
	if err := db.InsertCall("gnoland1", "TX1", 10, newer, "g1a", "gno.land/r/demo/foo", "Bar", true); err != nil {
		t.Fatalf("insert call: %v", err)
	}
	// The earliest datum lives in a different table than the latest, so a
	// single-table MIN would miss it.
	if err := db.UpsertPackage("gnoland1", "gno.land/r/demo/foo", "foo", "g1c", "TX0", 5, oldest, true, 1); err != nil {
		t.Fatalf("upsert package: %v", err)
	}
	// Another network's earlier data must not move this network's start.
	if err := db.InsertCall("test12", "TX2", 1, "2020-01-01T00:00:00Z", "g1b", "gno.land/r/demo/bar", "Baz", true); err != nil {
		t.Fatalf("insert other network call: %v", err)
	}

	got, ok, err := db.NetworkDataStart("gnoland1")
	if err != nil || !ok {
		t.Fatalf("ok = %v, err = %v; want ok=true, err=nil", ok, err)
	}
	want, _ := time.Parse(time.RFC3339, oldest)
	if !got.Equal(want) {
		t.Errorf("start = %v, want %v (a different network's rows leaked in)", got, want)
	}
}

// TestNetworkDataStartAllNetworks covers network == "", which networkParam
// uses for both a missing ?network and ?network=all, and which is the
// frontend's default state. The SQL builder takes a different shape on this
// path — it omits the per-subquery network filter entirely — so it needs its
// own coverage rather than relying on the scoped case above.
func TestNetworkDataStartAllNetworks(t *testing.T) {
	db := newTestDB(t)

	earlier := "2020-01-01T00:00:00Z"
	later := "2026-08-14T12:00:00Z"
	if err := db.InsertCall("gnoland1", "TX1", 10, later, "g1a", "gno.land/r/demo/foo", "Bar", true); err != nil {
		t.Fatalf("insert call on gnoland1: %v", err)
	}
	if err := db.InsertCall("test12", "TX2", 1, earlier, "g1b", "gno.land/r/demo/bar", "Baz", true); err != nil {
		t.Fatalf("insert call on test12: %v", err)
	}

	got, ok, err := db.NetworkDataStart("")
	if err != nil || !ok {
		t.Fatalf("ok = %v, err = %v; want ok=true, err=nil", ok, err)
	}
	want, _ := time.Parse(time.RFC3339, earlier)
	if !got.Equal(want) {
		t.Errorf("start = %v, want %v (the minimum across every configured network)", got, want)
	}
}

// TestNetworkDataStartLogsUnparseableTimestamp is Fix 6's coverage: a row
// that fails to parse must not silently produce ok=false with no signal.
// There's no exported hook to observe the log line from here, so this test
// exists mainly to pin down the documented behaviour (ok=false, err=nil) and
// as a place future contributors can extend if a hook is added later.
func TestNetworkDataStartPropagatesUnparseableTimestamp(t *testing.T) {
	// AGENTS.md: query-path readers return errors rather than swallowing them
	// and reporting a zero value. A chain whose timestamps stopped parsing must
	// surface that, not silently render a fixed multi-year window forever.
	db := newTestDB(t)

	if err := db.InsertCall("gnoland1", "TX1", 1, "not-a-timestamp", "g1a", "gno.land/r/demo/foo", "Bar", true); err != nil {
		t.Fatalf("insert call: %v", err)
	}

	got, ok, err := db.NetworkDataStart("gnoland1")
	if err == nil {
		t.Fatalf("got = %v, ok = %v, err = nil; want a non-nil error", got, ok)
	}
	if ok {
		t.Errorf("ok = true, want false alongside the error")
	}
	if !strings.Contains(err.Error(), "not-a-timestamp") {
		t.Errorf("error %q does not name the offending value", err)
	}
}

func TestUpsertStorageEventsKeepsSamePkgPathEvents(t *testing.T) {
	// 13 of 201 real mainnet transactions emit two or more storage events
	// sharing BOTH kind and pkg_path. A key of (network, tx_hash, pkg_path,
	// kind) would collapse them and under-count bytes; event_index is what
	// keeps them distinct.
	db := newTestDB(t)

	rows := []StorageEventRow{
		{TxHash: "TX1", EventIndex: 0, BlockHeight: 10, BlockTime: "2026-08-10T00:00:00Z",
			PkgPath: "gno.land/r/demo/foo", Kind: "deposit", BytesDelta: 100, FeeAmount: 10000, FeeDenom: "ugnot"},
		{TxHash: "TX1", EventIndex: 1, BlockHeight: 10, BlockTime: "2026-08-10T00:00:00Z",
			PkgPath: "gno.land/r/demo/foo", Kind: "deposit", BytesDelta: 250, FeeAmount: 25000, FeeDenom: "ugnot"},
	}
	if err := db.UpsertStorageEvents("gnoland1", rows); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	var n, total int
	if err := db.db.QueryRow(
		`SELECT COUNT(*), COALESCE(SUM(bytes_delta), 0) FROM storage_events WHERE network = ?`, "gnoland1",
	).Scan(&n, &total); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Errorf("row count = %d, want 2 — the second event was collapsed", n)
	}
	if total != 350 {
		t.Errorf("summed bytes_delta = %d, want 350", total)
	}
}

func TestUpsertStorageEventsIsIdempotent(t *testing.T) {
	// One observed transaction emitted 58 storage events; re-syncing a page
	// must be a no-op.
	db := newTestDB(t)

	rows := make([]StorageEventRow, 0, 58)
	for i := 0; i < 58; i++ {
		rows = append(rows, StorageEventRow{
			TxHash: "TXBIG", EventIndex: i, BlockHeight: 20, BlockTime: "2026-08-11T00:00:00Z",
			PkgPath: "gno.land/r/demo/bar", Kind: "deposit", BytesDelta: 10, FeeAmount: 1000, FeeDenom: "ugnot",
		})
	}
	for i := 0; i < 3; i++ {
		if err := db.UpsertStorageEvents("gnoland1", rows); err != nil {
			t.Fatalf("upsert %d: %v", i, err)
		}
	}

	var n int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM storage_events WHERE network = ?`, "gnoland1").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 58 {
		t.Errorf("row count = %d, want 58 after three identical upserts", n)
	}
}

func TestStorageEventsLastHeight(t *testing.T) {
	db := newTestDB(t)

	if _, ok, err := db.StorageEventsLastHeight("gnoland1"); err != nil || ok {
		t.Fatalf("empty: ok = %v, err = %v; want ok=false, err=nil", ok, err)
	}

	if err := db.UpsertStorageEvents("gnoland1", []StorageEventRow{
		{TxHash: "TX1", EventIndex: 0, BlockHeight: 5, BlockTime: "2026-08-10T00:00:00Z",
			PkgPath: "gno.land/r/demo/foo", Kind: "deposit", BytesDelta: 1},
		{TxHash: "TX2", EventIndex: 0, BlockHeight: 42, BlockTime: "2026-08-11T00:00:00Z",
			PkgPath: "gno.land/r/demo/foo", Kind: "deposit", BytesDelta: 1},
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	// Another network's higher height must not move this network's cursor.
	if err := db.UpsertStorageEvents("test12", []StorageEventRow{
		{TxHash: "TX3", EventIndex: 0, BlockHeight: 9999, BlockTime: "2026-08-11T00:00:00Z",
			PkgPath: "gno.land/r/demo/foo", Kind: "deposit", BytesDelta: 1},
	}); err != nil {
		t.Fatalf("upsert other network: %v", err)
	}

	h, ok, err := db.StorageEventsLastHeight("gnoland1")
	if err != nil || !ok {
		t.Fatalf("ok = %v, err = %v; want ok=true, err=nil", ok, err)
	}
	if h != 42 {
		t.Errorf("last height = %d, want 42 — another network's rows leaked in", h)
	}
}

// seedStorage writes one event, defaulting the stamp to now so window filters
// include it.
func seedStorage(t *testing.T, db *DB, network, tx string, idx int, pkg, kind string, bytes int, at time.Time) {
	t.Helper()
	err := db.UpsertStorageEvents(network, []StorageEventRow{{
		TxHash: tx, EventIndex: idx, BlockHeight: 100 + idx,
		BlockTime: at.UTC().Format(time.RFC3339), PkgPath: pkg, Kind: kind,
		BytesDelta: bytes, FeeAmount: bytes * 100, FeeDenom: "ugnot",
	}})
	if err != nil {
		t.Fatalf("seed storage: %v", err)
	}
}

func TestGetStorageTimeSeriesNetsDepositsAgainstUnlocks(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().UTC().Add(-2 * time.Hour)

	seedStorage(t, db, "gnoland1", "TX1", 0, "gno.land/r/demo/foo", "deposit", 1000, now)
	seedStorage(t, db, "gnoland1", "TX1", 1, "gno.land/r/demo/foo", "unlock", -400, now)

	pts, err := db.GetStorageTimeSeries("gnoland1", "", "daily", 7)
	if err != nil {
		t.Fatalf("series: %v", err)
	}
	var dep, rel, net int
	for _, p := range pts {
		dep += p.Deposited
		rel += p.Released
		net += p.Net
	}
	if dep != 1000 {
		t.Errorf("deposited = %d, want 1000", dep)
	}
	// Released stays negative, as the chain emits it.
	if rel != -400 {
		t.Errorf("released = %d, want -400 (kept negative)", rel)
	}
	if net != 600 {
		t.Errorf("net = %d, want 600", net)
	}
}

func TestGetStorageTimeSeriesAllowsNegativeNet(t *testing.T) {
	// A window catching an unlock whose deposit predates it nets negative, and
	// that must survive rather than being floored at zero — it is the signal
	// that events are being summed against history we do not have.
	db := newTestDB(t)
	now := time.Now().UTC().Add(-2 * time.Hour)
	seedStorage(t, db, "gnoland1", "TX1", 0, "gno.land/r/demo/foo", "unlock", -8192, now)

	pts, err := db.GetStorageTimeSeries("gnoland1", "", "daily", 7)
	if err != nil {
		t.Fatalf("series: %v", err)
	}
	net := 0
	for _, p := range pts {
		net += p.Net
	}
	if net != -8192 {
		t.Errorf("net = %d, want -8192 (not floored)", net)
	}
}

func TestGetStorageTimeSeriesSkipsMalformedBlockTime(t *testing.T) {
	// block_time is nullable TEXT compared as a string, so 'not-a-timestamp'
	// passes the window predicate and strftime yields NULL. Batch 2b shipped a
	// 500 from exactly this; the row must be skipped instead.
	db := newTestDB(t)
	now := time.Now().UTC().Add(-2 * time.Hour)
	seedStorage(t, db, "gnoland1", "TX1", 0, "gno.land/r/demo/foo", "deposit", 500, now)
	if err := db.UpsertStorageEvents("gnoland1", []StorageEventRow{{
		TxHash: "TXBAD", EventIndex: 0, BlockHeight: 999, BlockTime: "not-a-timestamp",
		PkgPath: "gno.land/r/demo/foo", Kind: "deposit", BytesDelta: 7,
	}}); err != nil {
		t.Fatalf("seed bad row: %v", err)
	}

	pts, err := db.GetStorageTimeSeries("gnoland1", "", "daily", 7)
	if err != nil {
		t.Fatalf("series returned an error instead of skipping the bad row: %v", err)
	}
	dep := 0
	for _, p := range pts {
		dep += p.Deposited
	}
	if dep != 500 {
		t.Errorf("deposited = %d, want 500 (the good row only)", dep)
	}
}

func TestGetStorageConsumersIsNetworkScoped(t *testing.T) {
	// Realm paths collide across chains, so merging two networks under one
	// label would be actively wrong.
	db := newTestDB(t)
	now := time.Now().UTC().Add(-2 * time.Hour)
	seedStorage(t, db, "gnoland1", "TX1", 0, "gno.land/r/gnoland/blog", "deposit", 1000, now)
	seedStorage(t, db, "test12", "TX2", 0, "gno.land/r/gnoland/blog", "deposit", 9999, now)

	got, err := db.GetStorageConsumers("gnoland1", 7, 10)
	if err != nil {
		t.Fatalf("consumers: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d consumers, want 1", len(got))
	}
	if got[0].PkgPath != "gno.land/r/gnoland/blog" || got[0].Net != 1000 {
		t.Errorf("consumer = %+v, want blog with net 1000 — the other network leaked in", got[0])
	}
}

func TestGetStorageConsumersSurvivesLimitOnAbsNet(t *testing.T) {
	// Both storage charts' copy claims the net-delta chart surfaces realms the
	// treemap can't — big negative-net releasers. `ORDER BY net DESC` buried
	// them at the bottom of the ordering, so topN truncated them away first.
	// Ordering by ABS(net) keeps the biggest movers in either direction.
	db := newTestDB(t)
	now := time.Now().UTC().Add(-2 * time.Hour)

	// Two small positive movers plus one large negative mover; topN=2 must
	// keep the negative one instead of the two small deposits.
	seedStorage(t, db, "gnoland1", "TX1", 0, "gno.land/r/demo/small1", "deposit", 50, now)
	seedStorage(t, db, "gnoland1", "TX2", 0, "gno.land/r/demo/small2", "deposit", 40, now)
	seedStorage(t, db, "gnoland1", "TX3", 0, "gno.land/r/demo/bignegative", "unlock", -900, now)

	got, err := db.GetStorageConsumers("gnoland1", 7, 2)
	if err != nil {
		t.Fatalf("consumers: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d consumers, want 2 (topN)", len(got))
	}
	var sawNegative bool
	for _, c := range got {
		if c.PkgPath == "gno.land/r/demo/bignegative" {
			sawNegative = true
			if c.Net != -900 {
				t.Errorf("bignegative net = %d, want -900", c.Net)
			}
		}
	}
	if !sawNegative {
		t.Errorf("consumers = %+v, want the large negative-net realm to survive the limit", got)
	}
}

func TestGetRealmsWithStorageComesFromEvents(t *testing.T) {
	// It used to list realms with package_files; it must now list realms that
	// actually have storage events.
	db := newTestDB(t)
	now := time.Now().UTC().Add(-2 * time.Hour)
	seedStorage(t, db, "gnoland1", "TX1", 0, "gno.land/r/demo/withevents", "deposit", 10, now)
	if err := db.UpsertPackage("gnoland1", "gno.land/r/demo/noevents", "noevents", "g1c", "TX9", 1,
		now.Format(time.RFC3339), true, 1); err != nil {
		t.Fatalf("upsert package: %v", err)
	}

	realms, err := db.GetRealmsWithStorage("gnoland1", 7)
	if err != nil {
		t.Fatalf("realms: %v", err)
	}
	if len(realms) != 1 || realms[0] != "gno.land/r/demo/withevents" {
		t.Errorf("realms = %v, want just the one with storage events", realms)
	}
}

func TestUpsertTransferEdgesAccumulatesAcrossRuns(t *testing.T) {
	// The same (from, to, day) bucket can be revisited when a later sync pass
	// finds more bank_sends rows for a day it already partially rolled up.
	// Re-upserting must add, not overwrite.
	db := newTestDB(t)

	if err := db.UpsertTransferEdges("gnoland1", []TransferEdgeRow{
		{FromAddress: "g1a", ToAddress: "g1b", Day: "2026-08-10", TotalValue: 100, TxCount: 1, LastHeight: 10},
	}); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if err := db.UpsertTransferEdges("gnoland1", []TransferEdgeRow{
		{FromAddress: "g1a", ToAddress: "g1b", Day: "2026-08-10", TotalValue: 50, TxCount: 1, LastHeight: 20},
	}); err != nil {
		t.Fatalf("second upsert: %v", err)
	}

	var total, count, height int
	err := db.db.QueryRow(
		`SELECT total_value, tx_count, last_height FROM transfer_edges WHERE network = ? AND from_address = ? AND to_address = ? AND day = ?`,
		"gnoland1", "g1a", "g1b", "2026-08-10",
	).Scan(&total, &count, &height)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if total != 150 {
		t.Errorf("total_value = %d, want 150 (accumulated, not overwritten)", total)
	}
	if count != 2 {
		t.Errorf("tx_count = %d, want 2", count)
	}
	if height != 20 {
		t.Errorf("last_height = %d, want 20 (the max of the two runs)", height)
	}
}

func TestTransferEdgesLastHeightIsNetworkScoped(t *testing.T) {
	db := newTestDB(t)

	if _, ok, err := db.TransferEdgesLastHeight("gnoland1"); err != nil || ok {
		t.Fatalf("empty: ok = %v, err = %v; want ok=false, err=nil", ok, err)
	}

	if err := db.UpsertTransferEdges("gnoland1", []TransferEdgeRow{
		{FromAddress: "g1a", ToAddress: "g1b", Day: "2026-08-10", TotalValue: 1, TxCount: 1, LastHeight: 42},
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	// Another network's higher height must not move this network's cursor.
	if err := db.UpsertTransferEdges("test12", []TransferEdgeRow{
		{FromAddress: "g1x", ToAddress: "g1y", Day: "2026-08-10", TotalValue: 1, TxCount: 1, LastHeight: 9999},
	}); err != nil {
		t.Fatalf("upsert other network: %v", err)
	}

	h, ok, err := db.TransferEdgesLastHeight("gnoland1")
	if err != nil || !ok {
		t.Fatalf("ok = %v, err = %v; want ok=true, err=nil", ok, err)
	}
	if h != 42 {
		t.Errorf("last height = %d, want 42 — another network's rows leaked in", h)
	}
}

func TestUpsertCallerEdgesAccumulatesAcrossRuns(t *testing.T) {
	db := newTestDB(t)

	if err := db.UpsertCallerEdges("gnoland1", []CallerEdgeRow{
		{Caller: "g1a", PkgPath: "gno.land/r/demo/foo", Day: "2026-08-10", Calls: 3, LastHeight: 10},
	}); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if err := db.UpsertCallerEdges("gnoland1", []CallerEdgeRow{
		{Caller: "g1a", PkgPath: "gno.land/r/demo/foo", Day: "2026-08-10", Calls: 2, LastHeight: 20},
	}); err != nil {
		t.Fatalf("second upsert: %v", err)
	}

	var calls, height int
	err := db.db.QueryRow(
		`SELECT calls, last_height FROM caller_edges WHERE network = ? AND caller = ? AND pkg_path = ? AND day = ?`,
		"gnoland1", "g1a", "gno.land/r/demo/foo", "2026-08-10",
	).Scan(&calls, &height)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if calls != 5 {
		t.Errorf("calls = %d, want 5 (accumulated, not overwritten)", calls)
	}
	if height != 20 {
		t.Errorf("last_height = %d, want 20", height)
	}
}

func TestCallerEdgesLastHeightIsNetworkScoped(t *testing.T) {
	db := newTestDB(t)

	if _, ok, err := db.CallerEdgesLastHeight("gnoland1"); err != nil || ok {
		t.Fatalf("empty: ok = %v, err = %v; want ok=false, err=nil", ok, err)
	}
	if err := db.UpsertCallerEdges("gnoland1", []CallerEdgeRow{
		{Caller: "g1a", PkgPath: "gno.land/r/demo/foo", Day: "2026-08-10", Calls: 1, LastHeight: 7},
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := db.UpsertCallerEdges("test12", []CallerEdgeRow{
		{Caller: "g1x", PkgPath: "gno.land/r/demo/bar", Day: "2026-08-10", Calls: 1, LastHeight: 9999},
	}); err != nil {
		t.Fatalf("upsert other network: %v", err)
	}

	h, ok, err := db.CallerEdgesLastHeight("gnoland1")
	if err != nil || !ok {
		t.Fatalf("ok = %v, err = %v; want ok=true, err=nil", ok, err)
	}
	if h != 7 {
		t.Errorf("last height = %d, want 7 — another network's rows leaked in", h)
	}
}

func TestRollupBankSendsSinceCollapsesByDayAndFiltersFailures(t *testing.T) {
	db := newTestDB(t)

	// Two sends same day/pair -> one bucket. One send a different day -> a
	// second bucket. One failed send -> excluded entirely (no value moved).
	if err := db.InsertBankSend("gnoland1", "TX1", 10, "2026-08-10T00:00:00Z", "g1a", "g1b", "100ugnot", true); err != nil {
		t.Fatalf("send 1: %v", err)
	}
	if err := db.InsertBankSend("gnoland1", "TX2", 11, "2026-08-10T05:00:00Z", "g1a", "g1b", "50ugnot", true); err != nil {
		t.Fatalf("send 2: %v", err)
	}
	if err := db.InsertBankSend("gnoland1", "TX3", 12, "2026-08-11T00:00:00Z", "g1a", "g1b", "25ugnot", true); err != nil {
		t.Fatalf("send 3: %v", err)
	}
	if err := db.InsertBankSend("gnoland1", "TX4", 13, "2026-08-11T01:00:00Z", "g1a", "g1b", "999ugnot", false); err != nil {
		t.Fatalf("failed send: %v", err)
	}

	rows, err := db.RollupBankSendsSince("gnoland1", 0)
	if err != nil {
		t.Fatalf("rollup: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rollup rows, want 2 (one per day)", len(rows))
	}
	byDay := map[string]TransferEdgeRow{}
	for _, r := range rows {
		byDay[r.Day] = r
	}
	if r := byDay["2026-08-10"]; r.TotalValue != 150 || r.TxCount != 2 || r.LastHeight != 11 {
		t.Errorf("2026-08-10 row = %+v, want total=150 count=2 height=11", r)
	}
	if r := byDay["2026-08-11"]; r.TotalValue != 25 || r.TxCount != 1 || r.LastHeight != 12 {
		t.Errorf("2026-08-11 row = %+v, want total=25 count=1 height=12 (failed send excluded)", r)
	}
}

func TestRollupBankSendsSinceRespectsCursor(t *testing.T) {
	db := newTestDB(t)
	if err := db.InsertBankSend("gnoland1", "TX1", 10, "2026-08-10T00:00:00Z", "g1a", "g1b", "100ugnot", true); err != nil {
		t.Fatalf("send: %v", err)
	}
	if err := db.InsertBankSend("gnoland1", "TX2", 20, "2026-08-11T00:00:00Z", "g1a", "g1b", "200ugnot", true); err != nil {
		t.Fatalf("send: %v", err)
	}

	rows, err := db.RollupBankSendsSince("gnoland1", 10)
	if err != nil {
		t.Fatalf("rollup: %v", err)
	}
	if len(rows) != 1 || rows[0].TotalValue != 200 {
		t.Errorf("rows = %+v, want just the height-20 send (height > 10)", rows)
	}
}

func TestRollupBankSendsSinceSkipsMalformedBlockTime(t *testing.T) {
	// block_time is nullable TEXT compared as a string; a malformed value
	// yields a NULL day from date(), which must be skipped rather than
	// crashing the pass or corrupting a real day's bucket.
	db := newTestDB(t)
	if err := db.InsertBankSend("gnoland1", "TX1", 10, "2026-08-10T00:00:00Z", "g1a", "g1b", "100ugnot", true); err != nil {
		t.Fatalf("good send: %v", err)
	}
	if err := db.InsertBankSend("gnoland1", "TX2", 11, "not-a-timestamp", "g1a", "g1b", "999ugnot", true); err != nil {
		t.Fatalf("bad send: %v", err)
	}

	rows, err := db.RollupBankSendsSince("gnoland1", 0)
	if err != nil {
		t.Fatalf("rollup returned an error instead of skipping the bad row: %v", err)
	}
	if len(rows) != 1 || rows[0].TotalValue != 100 {
		t.Errorf("rows = %+v, want just the good row", rows)
	}
}

func TestRollupCallsSinceCollapsesByDay(t *testing.T) {
	db := newTestDB(t)
	if err := db.InsertCall("gnoland1", "TX1", 10, "2026-08-10T00:00:00Z", "g1a", "gno.land/r/demo/foo", "Post", true); err != nil {
		t.Fatalf("call 1: %v", err)
	}
	if err := db.InsertCall("gnoland1", "TX2", 11, "2026-08-10T05:00:00Z", "g1a", "gno.land/r/demo/foo", "Edit", true); err != nil {
		t.Fatalf("call 2: %v", err)
	}
	if err := db.InsertCall("gnoland1", "TX3", 12, "2026-08-10T06:00:00Z", "g1a", "gno.land/r/demo/foo", "Post", false); err != nil {
		t.Fatalf("failed call: %v", err)
	}

	rows, err := db.RollupCallsSince("gnoland1", 0)
	if err != nil {
		t.Fatalf("rollup: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0].Calls != 2 || rows[0].LastHeight != 11 {
		t.Errorf("row = %+v, want calls=2 height=11 (failed call excluded)", rows[0])
	}
}

func seedTransferEdge(t *testing.T, db *DB, network, from, to, day string, value int64, txCount int) {
	t.Helper()
	if err := db.UpsertTransferEdges(network, []TransferEdgeRow{
		{FromAddress: from, ToAddress: to, Day: day, TotalValue: value, TxCount: txCount, LastHeight: 1},
	}); err != nil {
		t.Fatalf("seed transfer edge: %v", err)
	}
}

func TestGetTransferGraphTopNKeepsOnlyEdgesBetweenTopAddresses(t *testing.T) {
	db := newTestDB(t)
	today := time.Now().UTC().Format("2006-01-02")

	// g1a and g1b are the two biggest by volume; g1c is small and trades only
	// with g1a. With topN=2, the g1a<->g1c edge must be dropped because g1c
	// did not make the top-N set, even though g1a did.
	seedTransferEdge(t, db, "gnoland1", "g1a", "g1b", today, 1000, 1)
	seedTransferEdge(t, db, "gnoland1", "g1b", "g1a", today, 900, 1)
	seedTransferEdge(t, db, "gnoland1", "g1a", "g1c", today, 5, 1)

	g, err := db.GetTransferGraph("gnoland1", 7, 2, 0, "")
	if err != nil {
		t.Fatalf("graph: %v", err)
	}
	if len(g.Nodes) != 2 {
		t.Fatalf("got %d nodes, want 2 (top-2 by volume)", len(g.Nodes))
	}
	if len(g.Edges) != 2 {
		t.Fatalf("got %d edges, want 2 (both directions between g1a/g1b only)", len(g.Edges))
	}
	for _, e := range g.Edges {
		if e.To == "g1c" || e.From == "g1c" {
			t.Errorf("edge %+v touches g1c, which is not in the top-N set", e)
		}
	}
}

func TestGetTransferGraphMinValueFiltersDustEdges(t *testing.T) {
	db := newTestDB(t)
	today := time.Now().UTC().Format("2006-01-02")
	seedTransferEdge(t, db, "gnoland1", "g1a", "g1b", today, 1000, 1)
	seedTransferEdge(t, db, "gnoland1", "g1a", "g1c", today, 5, 1)

	g, err := db.GetTransferGraph("gnoland1", 7, 100, 100, "")
	if err != nil {
		t.Fatalf("graph: %v", err)
	}
	if len(g.Edges) != 1 || g.Edges[0].Value != 1000 {
		t.Errorf("edges = %+v, want just the 1000-value edge (min_value=100 drops the 5-value one)", g.Edges)
	}
}

func TestGetTransferGraphEgoModeReturnsOneHopNeighborhoodOnly(t *testing.T) {
	db := newTestDB(t)
	today := time.Now().UTC().Format("2006-01-02")
	seedTransferEdge(t, db, "gnoland1", "g1a", "g1b", today, 100, 1) // direct neighbor
	seedTransferEdge(t, db, "gnoland1", "g1b", "g1c", today, 999, 1) // 2 hops from g1a — must not appear

	g, err := db.GetTransferGraph("gnoland1", 7, 0, 0, "g1a")
	if err != nil {
		t.Fatalf("graph: %v", err)
	}
	if len(g.Edges) != 1 {
		t.Fatalf("got %d edges, want 1 (only g1a's direct edge)", len(g.Edges))
	}
	if g.Edges[0].From != "g1a" && g.Edges[0].To != "g1a" {
		t.Errorf("edge %+v does not touch the ego address", g.Edges[0])
	}
	for _, e := range g.Edges {
		if e.From == "g1c" || e.To == "g1c" {
			t.Errorf("2-hop neighbor g1c leaked into a 1-hop ego view: %+v", e)
		}
	}
}

func TestGetTransferGraphIsNetworkScoped(t *testing.T) {
	db := newTestDB(t)
	today := time.Now().UTC().Format("2006-01-02")
	seedTransferEdge(t, db, "gnoland1", "g1a", "g1b", today, 100, 1)
	seedTransferEdge(t, db, "test12", "g1a", "g1b", today, 99999, 1)

	g, err := db.GetTransferGraph("gnoland1", 7, 10, 0, "")
	if err != nil {
		t.Fatalf("graph: %v", err)
	}
	if len(g.Edges) != 1 || g.Edges[0].Value != 100 {
		t.Errorf("edges = %+v, want just gnoland1's edge — test12 leaked in", g.Edges)
	}
}

func seedCallerEdge(t *testing.T, db *DB, network, caller, pkgPath, day string, calls int) {
	t.Helper()
	if err := db.UpsertCallerEdges(network, []CallerEdgeRow{
		{Caller: caller, PkgPath: pkgPath, Day: day, Calls: calls, LastHeight: 1},
	}); err != nil {
		t.Fatalf("seed caller edge: %v", err)
	}
}

func TestGetCallerGraphTopNAndNodeTypes(t *testing.T) {
	db := newTestDB(t)
	today := time.Now().UTC().Format("2006-01-02")
	seedCallerEdge(t, db, "gnoland1", "g1a", "gno.land/r/demo/foo", today, 50)
	seedCallerEdge(t, db, "gnoland1", "g1b", "gno.land/r/demo/foo", today, 1)

	g, err := db.GetCallerGraph("gnoland1", 7, 1, 0)
	if err != nil {
		t.Fatalf("graph: %v", err)
	}
	if len(g.Edges) != 1 || g.Edges[0].Caller != "g1a" {
		t.Fatalf("edges = %+v, want just g1a's edge (top-1 caller)", g.Edges)
	}
	var sawCaller, sawRealm bool
	for _, n := range g.Nodes {
		if n.Type == "caller" {
			sawCaller = true
		}
		if n.Type == "realm" {
			sawRealm = true
		}
	}
	if !sawCaller || !sawRealm {
		t.Errorf("nodes = %+v, want at least one caller node and one realm node", g.Nodes)
	}
}
