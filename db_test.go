package main

import (
	"database/sql"
	"path/filepath"
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
