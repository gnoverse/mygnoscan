package main

import (
	"database/sql"
	"path/filepath"
	"testing"
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
