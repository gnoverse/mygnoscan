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
