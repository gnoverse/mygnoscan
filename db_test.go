package main

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestDB opens a real SQLite file in a temp dir. The driver is pure Go, so
// this works everywhere including CI, and it exercises the actual schema rather
// than a mock.
func newTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := NewDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

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

// A network removed from the config keeps its rows. Without scoping, every
// all-networks total silently counts a chain that no longer exists — on
// production this inflated the transaction count by 4,101 and the realm count by
// 187 the day topaz was retired.
func TestStatsAreScopedToConfiguredNetworks(t *testing.T) {
	db := newTestDB(t)

	seed := func(network string, calls, realms int) {
		t.Helper()
		for i := 0; i < calls; i++ {
			if err := db.InsertCall(network, fmt.Sprintf("%s-tx-%d", network, i), 100+i,
				"2026-01-01T00:00:00Z", fmt.Sprintf("g1caller%d", i), "gno.land/r/demo/x", "Fn", true); err != nil {
				t.Fatalf("seed call: %v", err)
			}
		}
		for i := 0; i < realms; i++ {
			if err := db.UpsertPackage(network, fmt.Sprintf("gno.land/r/%s/pkg%d", network, i), "pkg",
				"g1creator", fmt.Sprintf("%s-dep-%d", network, i), 200+i, "2026-01-01T00:00:00Z", true, 1); err != nil {
				t.Fatalf("seed package: %v", err)
			}
		}
	}

	seed("live", 5, 2)
	seed("retired", 3, 1)

	tests := []struct {
		name       string
		configured []string
		network    string
		wantCalls  int
		wantRealms int
	}{
		{
			name:       "all networks counts only the configured one",
			configured: []string{"live"},
			network:    "",
			wantCalls:  5,
			wantRealms: 2,
		},
		{
			name:       "a named network is unaffected by the config list",
			configured: []string{"live"},
			network:    "live",
			wantCalls:  5,
			wantRealms: 2,
		},
		{
			name:       "re-adding the network brings its rows back",
			configured: []string{"live", "retired"},
			network:    "",
			wantCalls:  8,
			wantRealms: 3,
		},
		{
			name:       "no configuration counts everything, as before",
			configured: nil,
			network:    "",
			wantCalls:  8,
			wantRealms: 3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := make([]NetworkConfig, 0, len(tt.configured))
			for _, id := range tt.configured {
				cfg = append(cfg, NetworkConfig{ID: id})
			}
			db.SetConfiguredNetworks(cfg)

			s, err := db.GetStats(tt.network)
			if err != nil {
				t.Fatalf("GetStats: %v", err)
			}
			if s.TotalCalls != tt.wantCalls {
				t.Errorf("TotalCalls = %d, want %d", s.TotalCalls, tt.wantCalls)
			}
			if s.TotalRealms != tt.wantRealms {
				t.Errorf("TotalRealms = %d, want %d", s.TotalRealms, tt.wantRealms)
			}
		})
	}
}

// The filter interpolates identifiers, so a quote in a network id must not be
// able to close the literal.
func TestNetworkFilterQuoting(t *testing.T) {
	db := newTestDB(t)
	db.SetConfiguredNetworks([]NetworkConfig{{ID: "o'brien"}, {ID: "plain"}})

	if got, want := db.networkFilter("network", ""), `network IN ('o''brien','plain')`; got != want {
		t.Errorf("configured set: got %q, want %q", got, want)
	}
	if got, want := db.networkFilter("network", "o'brien"), `network = 'o''brien'`; got != want {
		t.Errorf("named network: got %q, want %q", got, want)
	}
	db.SetConfiguredNetworks(nil)
	if got, want := db.networkFilter("network", ""), "1=1"; got != want {
		t.Errorf("no config: got %q, want %q", got, want)
	}
}

// The validators view needs the moniker, which is the registration call's first
// argument — and args are not stored on the call row. Before this table the view
// filtered all of history by pkg_path on every request, which costs ~30s on a
// busy chain whatever it returns.
func TestValoperRegistrations(t *testing.T) {
	db := newTestDB(t)
	db.SetConfiguredNetworks([]NetworkConfig{{ID: "live"}})

	if err := db.InsertValoperRegistration("live", "tx-1", 100, "2026-01-01T00:00:00Z",
		"g1aaa", "Register", "g1aaa", "alice", true); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := db.InsertValoperRegistration("live", "tx-2", 200, "2026-01-02T00:00:00Z",
		"g1bbb", "Register", "g1bbb", "bob", false); err != nil {
		t.Fatalf("insert: %v", err)
	}
	// A different network must not leak into the configured view.
	if err := db.InsertValoperRegistration("other", "tx-3", 300, "",
		"g1ccc", "Register", "g1ccc", "carol", true); err != nil {
		t.Fatalf("insert: %v", err)
	}

	regs, err := db.ValoperRegistrations("")
	if err != nil {
		t.Fatalf("ValoperRegistrations: %v", err)
	}
	if len(regs) != 2 {
		t.Fatalf("got %d registrations, want 2 (an unconfigured network leaked in)", len(regs))
	}
	// Newest first, so the view shows recent registrations without sorting.
	if regs[0].BlockHeight != 200 || regs[1].BlockHeight != 100 {
		t.Errorf("order = %d,%d, want 200,100", regs[0].BlockHeight, regs[1].BlockHeight)
	}
	if regs[0].Moniker != "bob" || regs[0].Success {
		t.Errorf("row = %+v, want moniker bob and success false", regs[0])
	}

	// Re-registering replaces rather than duplicating.
	if err := db.InsertValoperRegistration("live", "tx-1", 100, "2026-01-01T00:00:00Z",
		"g1aaa", "Register", "g1aaa", "alice-renamed", true); err != nil {
		t.Fatalf("re-insert: %v", err)
	}
	regs, _ = db.ValoperRegistrations("live")
	if len(regs) != 2 {
		t.Fatalf("got %d registrations after re-insert, want 2", len(regs))
	}
	for _, r := range regs {
		if r.Caller == "g1aaa" && r.Moniker != "alice-renamed" {
			t.Errorf("moniker = %q, want alice-renamed", r.Moniker)
		}
	}
}

// The backfill finds valopers calls already stored that have no moniker yet, so
// history can be repaired one keyed lookup at a time.
func TestValoperCallsMissingRegistration(t *testing.T) {
	db := newTestDB(t)
	db.SetConfiguredNetworks([]NetworkConfig{{ID: "live"}})

	mustCall := func(hash, pkgPath string, height int) {
		t.Helper()
		if err := db.InsertCall("live", hash, height, "2026-01-01T00:00:00Z",
			"g1aaa", pkgPath, "Register", true); err != nil {
			t.Fatalf("InsertCall: %v", err)
		}
	}
	mustCall("tx-valoper-1", "gno.land/r/gnops/valopers", 100)
	mustCall("tx-valoper-2", "gno.land/r/gov/valopers/v2", 200)
	mustCall("tx-unrelated", "gno.land/r/demo/boards", 300)

	missing, err := db.ValoperCallsMissingRegistration("live", 10)
	if err != nil {
		t.Fatalf("ValoperCallsMissingRegistration: %v", err)
	}
	if len(missing) != 2 {
		t.Fatalf("got %v, want the two valopers calls only", missing)
	}

	// Once recorded, a call drops out of the work list.
	if err := db.InsertValoperRegistration("live", "tx-valoper-1", 100, "",
		"g1aaa", "Register", "g1aaa", "alice", true); err != nil {
		t.Fatalf("insert: %v", err)
	}
	missing, _ = db.ValoperCallsMissingRegistration("live", 10)
	if len(missing) != 1 || missing[0] != "tx-valoper-2" {
		t.Errorf("got %v, want only tx-valoper-2", missing)
	}
}

// The gas view's "most expensive transactions" query sorts by gas_used within a
// network. Without an index for that the sort scans the whole table and the
// eight correlated subqueries in its select list run far more than the twenty
// times the LIMIT needs — 16s on a chain with 314k transactions, against a 30s
// server write timeout.
//
// Asserts the plan, not a duration: a timing test would be flaky, and what
// actually regresses is the planner losing the index.
func TestTopGasQueryUsesAnIndex(t *testing.T) {
	db := newTestDB(t)

	const q = `
		WITH top AS (
			SELECT network, tx_hash, gas_used FROM transactions
			WHERE network = ? ORDER BY gas_used DESC LIMIT 20
		)
		SELECT t.tx_hash, t.gas_used,
		  COALESCE(
		    (SELECT 'MsgCall' FROM calls c WHERE c.network = t.network AND c.tx_hash = t.tx_hash LIMIT 1),
		    (SELECT 'MsgAddPackage' FROM packages p WHERE p.network = t.network AND p.tx_hash = t.tx_hash LIMIT 1),
		    (SELECT 'MsgRun' FROM msg_runs m WHERE m.network = t.network AND m.tx_hash = t.tx_hash LIMIT 1),
		    (SELECT 'BankMsgSend' FROM bank_sends b WHERE b.network = t.network AND b.tx_hash = t.tx_hash LIMIT 1),
		    '')
		FROM top t`

	rows, err := db.db.Query("EXPLAIN QUERY PLAN "+q, "sapphire")
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()

	var plan []string
	for rows.Next() {
		cols, err := rows.Columns()
		if err != nil {
			t.Fatalf("columns: %v", err)
		}
		vals := make([]any, len(cols))
		for i := range vals {
			vals[i] = new(sql.NullString)
		}
		if err := rows.Scan(vals...); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if s, ok := vals[len(vals)-1].(*sql.NullString); ok && s.Valid {
			plan = append(plan, s.String)
		}
	}

	joined := strings.Join(plan, " | ")

	// The ranking must come off the index rather than a sort.
	if !strings.Contains(joined, "idx_txs_network_gas") {
		t.Errorf("ranking does not use idx_txs_network_gas.\nplan: %s", joined)
	}
	if strings.Contains(joined, "USE TEMP B-TREE FOR ORDER BY") {
		t.Errorf("ranking still sorts in a temp b-tree.\nplan: %s", joined)
	}

	// Each type probe must be a keyed lookup. Without these the planner falls
	// back to the block_time indexes, which lead with network only, and every
	// probe scans that network's whole table — 2.7s instead of instant.
	for _, idx := range []string{
		"idx_calls_network_hash",
		"idx_packages_network_hash",
		"idx_msg_runs_network_hash",
		"idx_bank_sends_network_hash",
	} {
		if !strings.Contains(joined, idx) {
			t.Errorf("type probe does not use %s.\nplan: %s", idx, joined)
		}
	}
}

// In all-networks mode the gas series carries a per-network breakdown. Fees are
// denominated per chain and summing them across chains produces a figure that
// describes nothing, so the split is what lets the view aggregate honestly.
func TestGasTimeSeriesSplitsByNetwork(t *testing.T) {
	db := newTestDB(t)
	db.SetConfiguredNetworks([]NetworkConfig{{ID: "alpha"}, {ID: "beta"}})

	day := time.Now().UTC().Format("2006-01-02") + "T12:00:00Z"
	seed := func(network, hash string, gasUsed, fee int, ok bool) {
		t.Helper()
		if err := db.UpsertTransaction(network, hash, 100, day, gasUsed, gasUsed*2, fee, ok); err != nil {
			t.Fatalf("UpsertTransaction: %v", err)
		}
	}
	seed("alpha", "a1", 100, 10, true)
	seed("alpha", "a2", 200, 20, false)
	seed("beta", "b1", 300, 30, true)

	points, err := db.GetGasTimeSeries("", "daily", 2)
	if err != nil {
		t.Fatalf("GetGasTimeSeries: %v", err)
	}

	var bucket *GasTimePoint
	for i := range points {
		if points[i].TxCount > 0 {
			bucket = &points[i]
		}
	}
	if bucket == nil {
		t.Fatal("no non-empty bucket")
	}

	if len(bucket.ByNetwork) != 2 {
		t.Fatalf("split covers %d networks, want 2: %+v", len(bucket.ByNetwork), bucket.ByNetwork)
	}
	// The split must reconcile with the total, or the stacked bars would not add
	// up to the number printed above them.
	var txs, fees, used int
	for _, s := range bucket.ByNetwork {
		txs += s.TxCount
		fees += s.TotalFees
		used += s.TotalGasUsed
	}
	if txs != bucket.TxCount || fees != bucket.TotalFees || used != bucket.TotalGasUsed {
		t.Errorf("split does not sum to the total: txs %d/%d fees %d/%d gas %d/%d",
			txs, bucket.TxCount, fees, bucket.TotalFees, used, bucket.TotalGasUsed)
	}
	if a := bucket.ByNetwork["alpha"]; a.TxCount != 2 || a.SuccessCount != 1 || a.FailCount != 1 {
		t.Errorf("alpha = %+v, want 2 txs / 1 ok / 1 failed", a)
	}

	// With one network selected the split is the total, so sending it would be
	// noise — and the frontend keys "is this multi-network?" off its absence.
	single, err := db.GetGasTimeSeries("alpha", "daily", 2)
	if err != nil {
		t.Fatalf("GetGasTimeSeries(alpha): %v", err)
	}
	for _, p := range single {
		if p.ByNetwork != nil {
			t.Errorf("single-network series carries a split: %+v", p.ByNetwork)
			break
		}
	}
}
