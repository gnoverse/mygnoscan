package main

import (
	"database/sql"
	"fmt"
	"log"
	_ "modernc.org/sqlite"
	"strings"
	"sync"
	"time"
)

type DB struct {
	db *sql.DB
	mu sync.RWMutex

	// background tracks work started by NewDB that outlives it. Close waits on
	// it: the ANALYZE below is still writing WAL files when a caller finishes,
	// and a test using t.TempDir() would fail its cleanup with "directory not
	// empty" — intermittently, roughly one run in eight.
	background sync.WaitGroup

	// configured is the set of networks the process is running, set once at
	// startup. Rows survive a network being retired from the config, so without
	// this the database — not the config — decides which networks exist.
	configured []string
}

// SetConfiguredNetworks scopes unfiltered reads to the networks currently in the
// config. Call once at startup, before serving.
func (d *DB) SetConfiguredNetworks(networks []NetworkConfig) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.configured = d.configured[:0]
	for _, n := range networks {
		d.configured = append(d.configured, n.ID)
	}
}

// networkFilter builds a SQL condition restricting rows to one network, or to
// the configured set when network is empty.
//
// Returns a bare condition, so callers supply their own WHERE or AND. When
// nothing is configured it yields `1=1`, which keeps every call site a plain
// concatenation rather than a branch.
//
// The identifiers are interpolated rather than bound. These fragments are
// spliced into CTEs at several points, where positional parameters would have to
// be threaded through in query order, and both sources are trusted: a named
// network has already been checked against the config by rejectUnknownNetwork,
// and the fallback list is the config file itself, never a request. The quote
// doubling is belt and braces.
func (d *DB) networkFilter(column, network string) string {
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
	if network != "" {
		return column + " = " + quote(network)
	}
	if len(d.configured) == 0 {
		return "1=1"
	}
	quoted := make([]string, 0, len(d.configured))
	for _, n := range d.configured {
		quoted = append(quoted, quote(n))
	}
	return column + " IN (" + strings.Join(quoted, ",") + ")"
}

func NewDB(path string) (*DB, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}

	// Migrate: drop tables if they lack UNIQUE constraints (old schema)
	var callSQL string
	db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name='calls'`).Scan(&callSQL)
	if callSQL != "" && !strings.Contains(callSQL, "UNIQUE") {
		db.Exec(`DROP TABLE IF EXISTS calls`)
		db.Exec(`DROP TABLE IF EXISTS msg_runs`)
		db.Exec(`DROP TABLE IF EXISTS bank_sends`)
	}

	// Migrate: add network column if missing (packages needs table rebuild for PK change)
	var pkgSQL string
	db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name='packages'`).Scan(&pkgSQL)
	if pkgSQL != "" && !strings.Contains(pkgSQL, "network") {
		if err := migrateAddNetworkColumn(db); err != nil {
			db.Close()
			return nil, fmt.Errorf("migrate network: %w", err)
		}
	}

	// Migrate: add block_time to tables created before it existed. Must run
	// before initSchema, which builds indexes on that column.
	if err := migrateAddBlockTime(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate block_time: %w", err)
	}

	if err := initSchema(db); err != nil {
		db.Close()
		return nil, err
	}

	d := &DB{db: db}

	// Refresh the planner's statistics in the background.
	//
	// Without sqlite_stat1 the planner guesses, and adding indexes can make it
	// guess worse: the gas-by-realm join picked (network, pkg_path) over the
	// (network, tx_hash) index it actually wanted and went from 2.51s to 3.73s.
	// With statistics it chooses correctly and lands at 2.09s — better than
	// before the new indexes existed.
	//
	// 0.87s on a 569MB database, and it grows with the chain, so it runs on every
	// start rather than once: stale statistics are how the planner drifts back to
	// the wrong choice. Off the startup path because it takes the write lock.
	d.background.Add(1)
	go func() {
		defer d.background.Done()

		if _, err := db.Exec(`ANALYZE`); err != nil {
			log.Printf("analyze: %v", err)
		}
	}()

	return d, nil
}

// blockTimeTables are the tables carrying a block_time column.
var blockTimeTables = []string{"packages", "calls", "msg_runs", "bank_sends", "transactions"}

// migrateAddBlockTime adds block_time to tables that predate it.
//
// CREATE TABLE IF NOT EXISTS cannot add a column to a table that already exists,
// and initSchema creates indexes on block_time, so without this a database
// written by a build older than the time-series work fails at startup with
// "no such column: block_time" and the process exits.
func migrateAddBlockTime(db *sql.DB) error {
	for _, table := range blockTimeTables {
		exists, err := tableExists(db, table)
		if err != nil {
			return err
		}
		if !exists {
			// initSchema will create it, block_time included.
			continue
		}
		has, err := columnExists(db, table, "block_time")
		if err != nil {
			return err
		}
		if has {
			continue
		}
		// Table names come from the constant above, never from input.
		if _, err := db.Exec(fmt.Sprintf(`ALTER TABLE %s ADD COLUMN block_time TEXT`, table)); err != nil {
			return fmt.Errorf("add block_time to %s: %w", table, err)
		}
	}
	return nil
}

func tableExists(db *sql.DB, name string) (bool, error) {
	var count int
	err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name,
	).Scan(&count)
	return count > 0, err
}

func columnExists(db *sql.DB, table, column string) (bool, error) {
	// PRAGMA does not accept bound parameters; table comes from a constant.
	rows, err := db.Query(fmt.Sprintf(`PRAGMA table_info(%s)`, table))
	if err != nil {
		return false, err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			cid, notnull, pk int
			name, ctype      string
			dflt             sql.NullString
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

func migrateAddNetworkColumn(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Rebuild packages with (network, path) PK
	if _, err := tx.Exec(`
		CREATE TABLE IF NOT EXISTS packages_new (
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
		)
	`); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO packages_new (network, path, name, creator, block_height, tx_hash, is_realm, num_files, created_at) SELECT 'gnoland1', path, name, creator, block_height, tx_hash, is_realm, num_files, created_at FROM packages`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DROP TABLE packages`); err != nil {
		return err
	}
	if _, err := tx.Exec(`ALTER TABLE packages_new RENAME TO packages`); err != nil {
		return err
	}

	// Rebuild package_files with (network, package_path, file_name) PK
	if _, err := tx.Exec(`
		CREATE TABLE IF NOT EXISTS package_files_new (
			network TEXT NOT NULL DEFAULT 'gnoland1',
			package_path TEXT NOT NULL,
			file_name TEXT NOT NULL,
			body TEXT NOT NULL,
			PRIMARY KEY (network, package_path, file_name)
		)
	`); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO package_files_new (network, package_path, file_name, body) SELECT 'gnoland1', package_path, file_name, body FROM package_files`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DROP TABLE package_files`); err != nil {
		return err
	}
	if _, err := tx.Exec(`ALTER TABLE package_files_new RENAME TO package_files`); err != nil {
		return err
	}

	// Rebuild dependencies with (network, package_path, import_path) PK
	if _, err := tx.Exec(`
		CREATE TABLE IF NOT EXISTS dependencies_new (
			network TEXT NOT NULL DEFAULT 'gnoland1',
			package_path TEXT NOT NULL,
			import_path TEXT NOT NULL,
			PRIMARY KEY (network, package_path, import_path)
		)
	`); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO dependencies_new (network, package_path, import_path) SELECT 'gnoland1', package_path, import_path FROM dependencies`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DROP TABLE dependencies`); err != nil {
		return err
	}
	if _, err := tx.Exec(`ALTER TABLE dependencies_new RENAME TO dependencies`); err != nil {
		return err
	}

	// For calls/msg_runs/bank_sends: just add column (ignore errors if already exists)
	tx.Exec(`ALTER TABLE calls ADD COLUMN network TEXT NOT NULL DEFAULT 'gnoland1'`)
	tx.Exec(`ALTER TABLE msg_runs ADD COLUMN network TEXT NOT NULL DEFAULT 'gnoland1'`)
	tx.Exec(`ALTER TABLE bank_sends ADD COLUMN network TEXT NOT NULL DEFAULT 'gnoland1'`)

	return tx.Commit()
}

func initSchema(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS packages (
			network TEXT NOT NULL DEFAULT 'gnoland1',
			path TEXT NOT NULL,
			name TEXT NOT NULL,
			creator TEXT NOT NULL,
			block_height INTEGER NOT NULL,
			block_time TEXT,
			tx_hash TEXT NOT NULL,
			is_realm BOOLEAN NOT NULL,
			num_files INTEGER NOT NULL,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (network, path)
		);

		CREATE TABLE IF NOT EXISTS package_files (
			network TEXT NOT NULL DEFAULT 'gnoland1',
			package_path TEXT NOT NULL,
			file_name TEXT NOT NULL,
			body TEXT NOT NULL,
			PRIMARY KEY (network, package_path, file_name)
		);

		CREATE TABLE IF NOT EXISTS dependencies (
			network TEXT NOT NULL DEFAULT 'gnoland1',
			package_path TEXT NOT NULL,
			import_path TEXT NOT NULL,
			PRIMARY KEY (network, package_path, import_path)
		);

		CREATE TABLE IF NOT EXISTS calls (
			network TEXT NOT NULL DEFAULT 'gnoland1',
			tx_hash TEXT NOT NULL,
			block_height INTEGER NOT NULL,
			block_time TEXT,
			caller TEXT NOT NULL,
			pkg_path TEXT NOT NULL,
			func_name TEXT NOT NULL,
			success BOOLEAN NOT NULL,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(network, tx_hash, pkg_path, func_name)
		);

		CREATE TABLE IF NOT EXISTS msg_runs (
			network TEXT NOT NULL DEFAULT 'gnoland1',
			tx_hash TEXT NOT NULL,
			block_height INTEGER NOT NULL,
			block_time TEXT,
			caller TEXT NOT NULL,
			source TEXT NOT NULL,
			success BOOLEAN NOT NULL,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(network, tx_hash, caller)
		);

		CREATE TABLE IF NOT EXISTS bank_sends (
			network TEXT NOT NULL DEFAULT 'gnoland1',
			tx_hash TEXT NOT NULL,
			block_height INTEGER NOT NULL,
			block_time TEXT,
			from_address TEXT NOT NULL,
			to_address TEXT NOT NULL,
			amount TEXT NOT NULL,
			success BOOLEAN NOT NULL,
			UNIQUE(network, tx_hash, from_address, to_address)
		);

		-- Validator registrations, the one thing the calls table cannot answer:
		-- the moniker is the call's first argument, and args are not stored.
		-- Keeping just this instead of every call's args is the difference between
		-- a few hundred rows and a column on half a million.
		CREATE TABLE IF NOT EXISTS valoper_registrations (
			network TEXT NOT NULL DEFAULT 'gnoland1',
			tx_hash TEXT NOT NULL,
			block_height INTEGER NOT NULL,
			block_time TEXT,
			caller TEXT NOT NULL,
			func_name TEXT NOT NULL,
			-- The validator the call is about, which is not always the caller: an
			-- admin can update someone else's entry, and Register carries the
			-- validator address in its arguments.
			address TEXT NOT NULL,
			-- Empty unless the call actually sets a name. Most valopers functions
			-- do not.
			moniker TEXT NOT NULL,
			success BOOLEAN NOT NULL,
			UNIQUE(network, tx_hash, caller)
		);

		CREATE INDEX IF NOT EXISTS idx_valoper_network_height
			ON valoper_registrations(network, block_height DESC);

		CREATE TABLE IF NOT EXISTS transactions (
			network      TEXT NOT NULL DEFAULT 'gnoland1',
			tx_hash      TEXT NOT NULL,
			block_height INTEGER NOT NULL,
			block_time   TEXT,
			gas_used     INTEGER NOT NULL DEFAULT 0,
			gas_wanted   INTEGER NOT NULL DEFAULT 0,
			gas_fee      INTEGER NOT NULL DEFAULT 0,
			success      BOOLEAN NOT NULL,
			PRIMARY KEY (network, tx_hash)
		);

		CREATE TABLE IF NOT EXISTS sync_state (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		);

		CREATE INDEX IF NOT EXISTS idx_txs_height ON transactions(network, block_height);
		CREATE INDEX IF NOT EXISTS idx_calls_pkg ON calls(pkg_path);
		CREATE INDEX IF NOT EXISTS idx_calls_caller ON calls(caller);
		CREATE INDEX IF NOT EXISTS idx_deps_import ON dependencies(import_path);
		CREATE INDEX IF NOT EXISTS idx_packages_creator ON packages(creator);
		CREATE INDEX IF NOT EXISTS idx_packages_realm ON packages(is_realm);
		CREATE INDEX IF NOT EXISTS idx_msg_runs_caller ON msg_runs(caller);
		CREATE INDEX IF NOT EXISTS idx_bank_from ON bank_sends(from_address);
		CREATE INDEX IF NOT EXISTS idx_bank_to ON bank_sends(to_address);
		CREATE INDEX IF NOT EXISTS idx_calls_height   ON calls(network, block_height);
		CREATE INDEX IF NOT EXISTS idx_pkgs_height    ON packages(network, block_height);
		CREATE INDEX IF NOT EXISTS idx_runs_height    ON msg_runs(network, block_height);
		CREATE INDEX IF NOT EXISTS idx_sends_height   ON bank_sends(network, block_height);
		CREATE INDEX IF NOT EXISTS idx_calls_block_time  ON calls(network, block_time);
		CREATE INDEX IF NOT EXISTS idx_pkgs_block_time   ON packages(network, block_time);
		CREATE INDEX IF NOT EXISTS idx_runs_block_time   ON msg_runs(network, block_time);
		CREATE INDEX IF NOT EXISTS idx_sends_block_time  ON bank_sends(network, block_time);
		CREATE INDEX IF NOT EXISTS idx_txs_block_time    ON transactions(network, block_time);

		-- The gas view's "most expensive transactions" sorts by gas_used within a
		-- network and keeps 20 rows. Without this the sort cannot be served from
		-- an index, so the eight correlated subqueries in its select list are
		-- evaluated across the whole table rather than the twenty rows that
		-- survive the LIMIT: 16s on a chain with 314k transactions, against a 30s
		-- server write timeout.
		CREATE INDEX IF NOT EXISTS idx_txs_network_gas ON transactions(network, gas_used DESC);

		-- Resolving a transaction's type and target means looking it up by
		-- (network, tx_hash) in each of these. Without a matching index the
		-- planner falls back to the block_time indexes, which lead with network
		-- only — so every lookup scans that network's whole table. On a chain with
		-- 533k calls that turned twenty lookups into seconds.
		CREATE INDEX IF NOT EXISTS idx_calls_network_hash      ON calls(network, tx_hash);
		CREATE INDEX IF NOT EXISTS idx_packages_network_hash   ON packages(network, tx_hash);
		CREATE INDEX IF NOT EXISTS idx_msg_runs_network_hash   ON msg_runs(network, tx_hash);
		CREATE INDEX IF NOT EXISTS idx_bank_sends_network_hash ON bank_sends(network, tx_hash);

		-- The analytics and accounts views aggregate by actor and by package
		-- inside a network. The single-column indexes that existed (caller,
		-- creator, from_address, to_address) cannot serve a query that also
		-- filters on network, so those aggregates fell back to table scans.
		-- Measured on production data: the five-way distinct-address union went
		-- 1.99s -> 0.42s and the realm join 1.23s -> 0.16s.
		CREATE INDEX IF NOT EXISTS idx_calls_net_caller  ON calls(network, caller);
		CREATE INDEX IF NOT EXISTS idx_calls_net_pkg     ON calls(network, pkg_path);
		CREATE INDEX IF NOT EXISTS idx_pkgs_net_creator  ON packages(network, creator);
		CREATE INDEX IF NOT EXISTS idx_runs_net_caller   ON msg_runs(network, caller);
		CREATE INDEX IF NOT EXISTS idx_sends_net_from    ON bank_sends(network, from_address);
		CREATE INDEX IF NOT EXISTS idx_sends_net_to      ON bank_sends(network, to_address);

		-- The realm/analytics joins group calls by package and count distinct
		-- callers within it, which this covers without touching the table.
		CREATE INDEX IF NOT EXISTS idx_calls_pkg_caller  ON calls(pkg_path, caller);

		-- The same, once the network joined the grouping key. Without the caller
		-- in the index SQLite builds a temp B-tree for COUNT(DISTINCT caller),
		-- which cost 1.60s over 685k rows to produce 159 groups; with it the
		-- callers arrive already ordered within each group and the count is a
		-- linear pass. Measured 1.29s -> 0.02s on the realms query.
		CREATE INDEX IF NOT EXISTS idx_calls_net_pkg_caller ON calls(network, pkg_path, caller);

		-- The gas-by-realm aggregate joins every call, deploy and run back to its
		-- transaction for the gas figures. That join touches one row per event —
		-- 682k on sapphire, to produce twenty — so what matters is that it never
		-- has to leave the index. Covering it: 3.79s -> 1.96s.
		CREATE INDEX IF NOT EXISTS idx_txs_net_hash_gas ON transactions(network, tx_hash, gas_used, gas_fee);

		-- And its mirror, for the top-callers ranking: group by (network,
		-- caller) counting distinct packages. Same shape, same reason, 0.69s ->
		-- 0.26s. Both are covering, so neither touches the table.
		CREATE INDEX IF NOT EXISTS idx_calls_net_caller_pkg ON calls(network, caller, pkg_path);
	`)
	return err
}

// Close waits for background work to finish before closing the handle, so no
// goroutine is left writing to a database — or a directory — the caller
// considers done with.
func (d *DB) Close() error {
	d.background.Wait()

	return d.db.Close()
}

// UpsertPackage inserts or updates a package.
func (d *DB) UpsertPackage(network, path, name, creator, txHash string, blockHeight int, blockTime string, isRealm bool, numFiles int) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.db.Exec(`
		INSERT OR REPLACE INTO packages (network, path, name, creator, tx_hash, block_height, block_time, is_realm, num_files)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, network, path, name, creator, txHash, blockHeight, blockTime, isRealm, numFiles)
	return err
}

// UpsertPackageFile inserts or updates a package file.
func (d *DB) UpsertPackageFile(network, pkgPath, fileName, body string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.db.Exec(`
		INSERT OR REPLACE INTO package_files (network, package_path, file_name, body)
		VALUES (?, ?, ?, ?)
	`, network, pkgPath, fileName, body)
	return err
}

// StoredPackageRef identifies a package whose source is held locally.
type StoredPackageRef struct {
	Network string
	Path    string
}

// StoredPackageRefs lists every package that has source in the database.
//
// Deliberately returns references rather than bodies, and takes no callback: a
// caller iterating packages will want to write as it goes, and d.mu is not
// reentrant — handing out source under the read lock would deadlock the first
// caller that tried. package_files is also the largest table on a busy chain, so
// not buffering every body is worth having anyway.
func (d *DB) StoredPackageRefs() ([]StoredPackageRef, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	rows, err := d.db.Query(`
		SELECT DISTINCT network, package_path
		FROM package_files
		ORDER BY network, package_path
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []StoredPackageRef
	for rows.Next() {
		var ref StoredPackageRef
		if err := rows.Scan(&ref.Network, &ref.Path); err != nil {
			return nil, err
		}
		out = append(out, ref)
	}
	return out, rows.Err()
}

// StoredPackageFiles returns one package's stored source.
func (d *DB) StoredPackageFiles(network, pkgPath string) ([]MemFile, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	rows, err := d.db.Query(`
		SELECT file_name, body FROM package_files
		WHERE network = ? AND package_path = ?
		ORDER BY file_name
	`, network, pkgPath)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []MemFile
	for rows.Next() {
		var f MemFile
		if err := rows.Scan(&f.Name, &f.Body); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// SetDependencies replaces all dependencies for a package.
func (d *DB) SetDependencies(network, pkgPath string, imports []string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM dependencies WHERE network = ? AND package_path = ?`, network, pkgPath); err != nil {
		return err
	}

	stmt, err := tx.Prepare(`INSERT OR IGNORE INTO dependencies (network, package_path, import_path) VALUES (?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, imp := range imports {
		if _, err := stmt.Exec(network, pkgPath, imp); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// InsertCall records a MsgCall.
func (d *DB) InsertCall(network, txHash string, blockHeight int, blockTime, caller, pkgPath, funcName string, success bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.db.Exec(`
		INSERT OR IGNORE INTO calls (network, tx_hash, block_height, block_time, caller, pkg_path, func_name, success)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, network, txHash, blockHeight, blockTime, caller, pkgPath, funcName, success)
	return err
}

// ValoperRegistration is one validator's registration call, flattened. The
// moniker is the call's first argument.
type ValoperRegistration struct {
	TxHash      string `json:"tx_hash"`
	BlockHeight int    `json:"block_height"`
	BlockTime   string `json:"block_time,omitempty"`
	Caller      string `json:"caller"`
	Func        string `json:"func"`
	Address     string `json:"address"`
	Moniker     string `json:"moniker"`
	Success     bool   `json:"success"`
	Network     string `json:"network,omitempty"`
}

// InsertValoperRegistration records a call to a valopers realm.
func (d *DB) InsertValoperRegistration(network, txHash string, blockHeight int, blockTime, caller, funcName, address, moniker string, success bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.db.Exec(`
		INSERT OR REPLACE INTO valoper_registrations
			(network, tx_hash, block_height, block_time, caller, func_name, address, moniker, success)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, network, txHash, blockHeight, blockTime, caller, funcName, address, moniker, success)
	return err
}

// ValoperRegistrations returns registrations newest first.
//
// Served from storage rather than the indexer: filtering all of history by
// pkg_path costs ~30s on a busy chain regardless of how little it returns,
// because the price is the scan.
func (d *DB) ValoperRegistrations(network string) ([]ValoperRegistration, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	rows, err := d.db.Query(`
		SELECT network, tx_hash, block_height, COALESCE(block_time, ''), caller, func_name, address, moniker, success
		FROM valoper_registrations
		WHERE ` + d.networkFilter("network", network) + `
		ORDER BY block_height DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []ValoperRegistration{}
	for rows.Next() {
		var v ValoperRegistration
		if err := rows.Scan(&v.Network, &v.TxHash, &v.BlockHeight, &v.BlockTime,
			&v.Caller, &v.Func, &v.Address, &v.Moniker, &v.Success); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ValoperCallsMissingRegistration lists valopers calls already stored that have
// no registration row, so the moniker can be backfilled from the indexer.
//
// The calls table has everything except the moniker, which only exists in the
// call's arguments — and those were never stored. Fetching by hash is a keyed
// lookup, so repairing history costs one cheap request per row rather than a
// full-history scan.
func (d *DB) ValoperCallsMissingRegistration(network string, limit int) ([]string, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	rows, err := d.db.Query(`
		SELECT DISTINCT c.tx_hash
		FROM calls c
		WHERE c.pkg_path LIKE '%valopers%'
		  AND `+d.networkFilter("c.network", network)+`
		  AND NOT EXISTS (
			SELECT 1 FROM valoper_registrations v
			WHERE v.network = c.network AND v.tx_hash = c.tx_hash
		  )
		ORDER BY c.block_height DESC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// InsertMsgRun records a MsgRun transaction with its source.
func (d *DB) InsertMsgRun(network, txHash string, blockHeight int, blockTime, caller, source string, success bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.db.Exec(`
		INSERT OR IGNORE INTO msg_runs (network, tx_hash, block_height, block_time, caller, source, success)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, network, txHash, blockHeight, blockTime, caller, source, success)
	return err
}

func (d *DB) InsertBankSend(network, txHash string, blockHeight int, blockTime, from, to, amount string, success bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.db.Exec(`INSERT OR IGNORE INTO bank_sends (network, tx_hash, block_height, block_time, from_address, to_address, amount, success) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		network, txHash, blockHeight, blockTime, from, to, amount, success)
	return err
}

// GetSyncState reads a sync state value.
func (d *DB) GetSyncState(key string) (string, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	var val string
	err := d.db.QueryRow(`SELECT value FROM sync_state WHERE key = ?`, key).Scan(&val)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return val, err
}

// SetSyncState writes a sync state value.
func (d *DB) SetSyncState(key, value string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.db.Exec(`
		INSERT OR REPLACE INTO sync_state (key, value) VALUES (?, ?)
	`, key, value)
	return err
}

// networkScopedTables lists every table whose rows belong to a single network.
var networkScopedTables = []string{
	"packages",
	"package_files",
	"dependencies",
	"calls",
	"msg_runs",
	"bank_sends",
	"transactions",
}

// DeleteNetworkData removes every row belonging to a network, in one transaction.
// Used when a chain reset makes the stored history refer to blocks that no longer
// exist. Returns the number of rows removed.
func (d *DB) DeleteNetworkData(network string) (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	tx, err := d.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	var total int64
	for _, table := range networkScopedTables {
		// Table names come from the constant above, never from input.
		res, err := tx.Exec(fmt.Sprintf(`DELETE FROM %s WHERE network = ?`, table), network)
		if err != nil {
			return 0, fmt.Errorf("delete from %s: %w", table, err)
		}
		if n, err := res.RowsAffected(); err == nil {
			total += n
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return total, nil
}

// blockTimeTables lists tables whose rows carry a block_time worth backfilling.
// package_files and dependencies have no height of their own.
var backfillTables = []string{"packages", "calls", "msg_runs", "bank_sends", "transactions"}

// HeightsMissingBlockTime returns block heights that have rows with no
// block_time, oldest first, capped at limit.
//
// Rows written before block_time existed never got one, and incremental sync
// only ever moves forward from the cursor, so nothing fills them in. Without a
// timestamp a row cannot be ordered against another chain's rows, and list
// endpoints have to ask the indexer at request time instead of reading storage.
func (d *DB) HeightsMissingBlockTime(network string, limit int) ([]int, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var parts []string
	for _, t := range backfillTables {
		// Table names come from the constant above, never from input.
		// Genesis rows have no block to read a time from; excluding them keeps
		// the repair from re-querying an unanswerable height on every pass.
		parts = append(parts, fmt.Sprintf(
			`SELECT DISTINCT block_height FROM %s WHERE network = ? AND block_height > 0 AND (block_time IS NULL OR block_time = '')`, t))
	}
	query := strings.Join(parts, " UNION ") + " ORDER BY block_height DESC LIMIT ?"

	args := make([]any, 0, len(backfillTables)+1)
	for range backfillTables {
		args = append(args, network)
	}
	args = append(args, limit)

	rows, err := d.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var heights []int
	for rows.Next() {
		var h int
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		heights = append(heights, h)
	}
	return heights, rows.Err()
}

// BlockTimesForHeights returns known block times for the given heights, read
// from stored transactions.
//
// Stamping list responses by asking the indexer for each block is the dominant
// cost of those endpoints: a public indexer answers in roughly a quarter second,
// so a page touching 50 distinct blocks spends over a second on timestamps alone.
// The syncer already records block_time on every transaction it writes, so for
// anything already synced this is a local lookup instead.
func (d *DB) BlockTimesForHeights(network string, heights []int) (map[int]string, error) {
	if len(heights) == 0 {
		return nil, nil
	}
	d.mu.RLock()
	defer d.mu.RUnlock()

	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(heights)), ",")
	args := make([]any, 0, len(heights)+1)
	q := `SELECT block_height, block_time FROM transactions
	      WHERE block_time IS NOT NULL AND block_time != ''`
	if network != "" {
		q += ` AND network = ?`
		args = append(args, network)
	}
	q += ` AND block_height IN (` + placeholders + `) GROUP BY block_height`
	for _, h := range heights {
		args = append(args, h)
	}

	rows, err := d.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[int]string, len(heights))
	for rows.Next() {
		var h int
		var t string
		if err := rows.Scan(&h, &t); err != nil {
			return nil, err
		}
		out[h] = t
	}
	return out, rows.Err()
}

// TxRow is a transaction to be stored.
type TxRow struct {
	Hash        string
	BlockHeight int
	BlockTime   string
	GasUsed     int
	GasWanted   int
	GasFee      int
	Success     bool
}

// UpsertTransactions writes many transactions under a single lock and a single
// SQLite transaction.
//
// Writing them one at a time takes and releases the write lock per row, and with
// an application-level RWMutex over the database that means read requests queue
// behind every individual insert — a backfill pass of 100 rows measurably slowed
// the API while it ran.
func (d *DB) UpsertTransactions(network string, rows []TxRow) error {
	if len(rows) == 0 {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`
		INSERT OR IGNORE INTO transactions
			(network, tx_hash, block_height, block_time, gas_used, gas_wanted, gas_fee, success)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, r := range rows {
		if _, err := stmt.Exec(network, r.Hash, r.BlockHeight, r.BlockTime,
			r.GasUsed, r.GasWanted, r.GasFee, r.Success); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// HeightsMissingTransactions returns block heights that have event rows with no
// corresponding entry in the transactions table, newest first, capped at limit.
//
// The transactions table was added after the event tables, and incremental sync
// only writes it going forward, so history synced by an older build has calls
// and transfers recorded with no transaction row carrying their gas.
func (d *DB) HeightsMissingTransactions(network string, limit int) ([]int, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var parts []string
	for _, t := range []string{"packages", "calls", "msg_runs", "bank_sends"} {
		// Height 0 is genesis: those packages were loaded with the chain rather
		// than deployed by a transaction, so no transaction row will ever exist
		// for them. Including them made the backfill retry the same query every
		// pass, forever, finding nothing.
		parts = append(parts, fmt.Sprintf(`
			SELECT DISTINCT e.block_height FROM %s e
			WHERE e.network = ? AND e.block_height > 0
			  AND NOT EXISTS (
			    SELECT 1 FROM transactions t
			    WHERE t.network = e.network AND t.tx_hash = e.tx_hash)`, t))
	}
	query := strings.Join(parts, " UNION ") + " ORDER BY block_height DESC LIMIT ?"

	args := make([]any, 0, 5)
	for i := 0; i < 4; i++ {
		args = append(args, network)
	}
	args = append(args, limit)

	rows, err := d.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var heights []int
	for rows.Next() {
		var h int
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		heights = append(heights, h)
	}
	return heights, rows.Err()
}

// SetBlockTimes fills in block_time for rows at the given heights that lack it.
// Existing values are left alone: this repairs history, it does not rewrite it.
func (d *DB) SetBlockTimes(network string, times map[int]string) (int64, error) {
	if len(times) == 0 {
		return 0, nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	tx, err := d.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	var updated int64
	for _, table := range backfillTables {
		stmt, err := tx.Prepare(fmt.Sprintf(
			`UPDATE %s SET block_time = ? WHERE network = ? AND block_height = ? AND (block_time IS NULL OR block_time = '')`, table))
		if err != nil {
			return 0, fmt.Errorf("prepare %s: %w", table, err)
		}
		for height, t := range times {
			if t == "" {
				continue
			}
			res, err := stmt.Exec(t, network, height)
			if err != nil {
				stmt.Close()
				return 0, fmt.Errorf("update %s: %w", table, err)
			}
			if n, err := res.RowsAffected(); err == nil {
				updated += n
			}
		}
		stmt.Close()
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return updated, nil
}

// GasRealm is per-realm gas consumption.
type GasRealm struct {
	Path    string `json:"path"`
	Gas     int    `json:"gas"`
	Fees    int    `json:"fees"`
	TxCount int    `json:"tx_count"`
}

// GasTx is a single expensive transaction.
type GasTx struct {
	Hash        string `json:"hash"`
	BlockHeight int    `json:"block_height"`
	GasUsed     int    `json:"gas_used"`
	GasWanted   int    `json:"gas_wanted"`
	Fee         int    `json:"fee"`
	Type        string `json:"type"`
	Detail      string `json:"detail"`
	Success     bool   `json:"success"`
}

// GasStats aggregates gas usage for a network.
type GasStats struct {
	TotalTxs       int
	TotalGasUsed   int
	TotalGasWanted int
	TotalFees      int
	SuccessCount   int
	FailCount      int
	TopRealms      []GasRealm
	TopTxs         []GasTx
}

// GetGasStats computes gas aggregates from stored transactions.
//
// Previously this was derived by downloading every transaction on the chain from
// the indexer on each request. The transactions table already carries gas_used,
// gas_wanted, gas_fee and success per network, so this is a handful of aggregates
// over local data instead.
func (d *DB) GetGasStats(network string, topN int) (*GasStats, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	// Always filter, even for "all networks": an empty WHERE cannot use the
	// (network, gas_used) index, which is what makes the top-gas sort scan the
	// whole table. Scoping to the configured set keeps the index usable and
	// keeps retired networks out of the totals.
	where := " WHERE " + d.networkFilter("network", network)
	args := []any{}

	out := &GasStats{}
	err := d.db.QueryRow(`
		SELECT COUNT(*),
		       COALESCE(SUM(gas_used), 0),
		       COALESCE(SUM(gas_wanted), 0),
		       COALESCE(SUM(gas_fee), 0),
		       COALESCE(SUM(CASE WHEN success THEN 1 ELSE 0 END), 0)
		FROM transactions`+where, args...).
		Scan(&out.TotalTxs, &out.TotalGasUsed, &out.TotalGasWanted, &out.TotalFees, &out.SuccessCount)
	if err != nil {
		return nil, fmt.Errorf("gas totals: %w", err)
	}
	out.FailCount = out.TotalTxs - out.SuccessCount

	// Attribute each transaction's gas to what it touched. A transaction is
	// joined to at most one realm here; calls and deployments are the two paths
	// that carry a package path, and MsgRun is grouped under its caller because
	// its ephemeral path is unique per run and would otherwise be one row each.
	realmWhere := " AND " + d.networkFilter("t.network", network)
	realmArgs := []any{}
	rows, err := d.db.Query(`
		SELECT path, SUM(gas_used), SUM(gas_fee), COUNT(*) FROM (
			SELECT c.pkg_path AS path, t.gas_used, t.gas_fee, t.tx_hash
			  FROM calls c JOIN transactions t
			    ON t.network = c.network AND t.tx_hash = c.tx_hash`+realmWhere+`
			UNION ALL
			SELECT p.path AS path, t.gas_used, t.gas_fee, t.tx_hash
			  FROM packages p JOIN transactions t
			    ON t.network = p.network AND t.tx_hash = p.tx_hash`+realmWhere+`
			UNION ALL
			SELECT 'MsgRun by ' || m.caller AS path, t.gas_used, t.gas_fee, t.tx_hash
			  FROM msg_runs m JOIN transactions t
			    ON t.network = m.network AND t.tx_hash = m.tx_hash`+realmWhere+`
		) GROUP BY path ORDER BY SUM(gas_used) DESC LIMIT ?`,
		append(realmArgs, topN)...)
	if err != nil {
		return nil, fmt.Errorf("gas by realm: %w", err)
	}
	for rows.Next() {
		var r GasRealm
		if err := rows.Scan(&r.Path, &r.Gas, &r.Fees, &r.TxCount); err != nil {
			rows.Close()
			return nil, err
		}
		out.TopRealms = append(out.TopRealms, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Most expensive transactions, with the type and target resolved from
	// whichever table recorded the message.
	//
	// The ranking is materialised first so the subqueries below are evaluated
	// exactly topN times rather than at the planner's discretion.
	//
	// This is insurance, not the fix: measured, the CTE alone changed nothing
	// (2.47s -> 2.43s). What made this query slow was the absence of a
	// (network, tx_hash) index on the four tables it probes — see the schema.
	// The CTE stays because it makes the intended shape explicit and bounds the
	// damage if the planner ever loses those indexes again.
	txRows, err := d.db.Query(`
		WITH top AS (
			SELECT network, tx_hash, block_height, gas_used, gas_wanted, gas_fee, success
			FROM transactions`+where+`
			ORDER BY gas_used DESC LIMIT ?
		)
		SELECT t.tx_hash, t.block_height, t.gas_used, t.gas_wanted, t.gas_fee, t.success,
		  COALESCE(
		    (SELECT 'MsgCall' FROM calls c WHERE c.network = t.network AND c.tx_hash = t.tx_hash LIMIT 1),
		    (SELECT 'MsgAddPackage' FROM packages p WHERE p.network = t.network AND p.tx_hash = t.tx_hash LIMIT 1),
		    (SELECT 'MsgRun' FROM msg_runs m WHERE m.network = t.network AND m.tx_hash = t.tx_hash LIMIT 1),
		    (SELECT 'BankMsgSend' FROM bank_sends b WHERE b.network = t.network AND b.tx_hash = t.tx_hash LIMIT 1),
		    ''),
		  COALESCE(
		    (SELECT c.pkg_path || '::' || c.func_name FROM calls c WHERE c.network = t.network AND c.tx_hash = t.tx_hash LIMIT 1),
		    (SELECT p.path FROM packages p WHERE p.network = t.network AND p.tx_hash = t.tx_hash LIMIT 1),
		    (SELECT 'MsgRun by ' || m.caller FROM msg_runs m WHERE m.network = t.network AND m.tx_hash = t.tx_hash LIMIT 1),
		    '')
		FROM top t
		ORDER BY t.gas_used DESC`, append(args, topN)...)
	if err != nil {
		return nil, fmt.Errorf("top gas transactions: %w", err)
	}
	defer txRows.Close()
	for txRows.Next() {
		var t GasTx
		if err := txRows.Scan(&t.Hash, &t.BlockHeight, &t.GasUsed, &t.GasWanted,
			&t.Fee, &t.Success, &t.Type, &t.Detail); err != nil {
			return nil, err
		}
		out.TopTxs = append(out.TopTxs, t)
	}
	return out, txRows.Err()
}

// MaxBlockHeight returns the highest block height stored for a network, or 0 if
// the network has no data yet.
func (d *DB) MaxBlockHeight(network string) (int, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	var height sql.NullInt64
	err := d.db.QueryRow(
		`SELECT MAX(block_height) FROM transactions WHERE network = ?`, network,
	).Scan(&height)
	if err != nil {
		return 0, err
	}
	return int(height.Int64), nil
}

type PackageInfo struct {
	Network     string `json:"network,omitempty"`
	Path        string `json:"path"`
	Name        string `json:"name"`
	Creator     string `json:"creator"`
	BlockHeight int    `json:"block_height"`
	BlockTime   string `json:"block_time,omitempty"`
	TxHash      string `json:"tx_hash"`
	IsRealm     bool   `json:"is_realm"`
	NumFiles    int    `json:"num_files"`
	Calls       int    `json:"calls"`
	Importers   int    `json:"importers"`
	Imports     int    `json:"imports"`
}

type PackageDetail struct {
	PackageInfo
	Files      []FileInfo   `json:"files"`
	Imports    []string     `json:"imports"`
	Dependents []string     `json:"dependents"`
	Callers    []CallInfo   `json:"recent_calls"`
	MsgRunRefs []MsgRunInfo `json:"msgrun_refs"`
	CallCount  int          `json:"call_count"`
}

type FileInfo struct {
	Name string `json:"name"`
	Body string `json:"body"`
}

type CallInfo struct {
	TxHash      string `json:"tx_hash"`
	BlockHeight int    `json:"block_height"`
	Caller      string `json:"caller"`
	FuncName    string `json:"func_name"`
	Success     bool   `json:"success"`
}

type MsgRunInfo struct {
	TxHash      string `json:"tx_hash"`
	BlockHeight int    `json:"block_height"`
	Caller      string `json:"caller"`
	Success     bool   `json:"success"`
}

func (d *DB) CountPackages(network string, realmOnly bool) (int, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	q := `SELECT COUNT(*) FROM packages WHERE is_realm = ?`
	args := []any{realmOnly}
	if network != "" {
		q += ` AND network = ?`
		args = append(args, network)
	}
	var count int
	err := d.db.QueryRow(q, args...).Scan(&count)
	return count, err
}

// ListPackages returns all packages, optionally filtered.
// packageSortClause maps a sort key to SQL. Whitelisted rather than
// interpolated: the value comes from a query parameter.
func packageSortClause(sortBy string) string {
	switch sortBy {
	case "calls":
		return "calls DESC, p.block_height DESC"
	case "importers":
		return "importers DESC, p.block_height DESC"
	case "imports":
		return "imports DESC, p.block_height DESC"
	case "name":
		return "p.path ASC"
	case "oldest":
		return "p.block_height ASC"
	default: // newest
		return "p.block_height DESC"
	}
}

func (d *DB) ListPackages(network string, realmOnly bool, limit, offset int, sortBy string) ([]PackageInfo, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	// Usage counts come from correlated subqueries rather than joins: a join on
	// path alone would mix networks together, and grouping four tables in one
	// query multiplies rows against each other.
	q := `SELECT p.network, p.path, p.name, p.creator, p.block_height, p.tx_hash, p.is_realm, p.num_files,
		(SELECT COUNT(*) FROM calls c
		   WHERE c.network = p.network AND c.pkg_path = p.path) AS calls,
		(SELECT COUNT(*) FROM dependencies d
		   WHERE d.network = p.network AND d.import_path = p.path) AS importers,
		(SELECT COUNT(*) FROM dependencies d
		   WHERE d.network = p.network AND d.package_path = p.path) AS imports
		FROM packages p WHERE p.is_realm = ?`
	args := []any{realmOnly}
	if network != "" {
		q += ` AND p.network = ?`
		args = append(args, network)
	}
	q += ` ORDER BY ` + packageSortClause(sortBy)
	if limit > 0 {
		q += fmt.Sprintf(` LIMIT %d OFFSET %d`, limit, offset)
	}

	rows, err := d.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var pkgs []PackageInfo
	for rows.Next() {
		var p PackageInfo
		if err := rows.Scan(&p.Network, &p.Path, &p.Name, &p.Creator, &p.BlockHeight, &p.TxHash,
			&p.IsRealm, &p.NumFiles, &p.Calls, &p.Importers, &p.Imports); err != nil {
			return nil, err
		}
		pkgs = append(pkgs, p)
	}
	return pkgs, rows.Err()
}

// GetPackageDetail returns full details for a package.
func (d *DB) GetPackageDetail(network, path string) (*PackageDetail, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	q := `SELECT network, path, name, creator, block_height, tx_hash, is_realm, num_files FROM packages WHERE path = ?`
	args := []any{path}
	if network != "" {
		q += ` AND network = ?`
		args = append(args, network)
	}

	var p PackageDetail
	err := d.db.QueryRow(q, args...).Scan(&p.Network, &p.Path, &p.Name, &p.Creator, &p.BlockHeight, &p.TxHash, &p.IsRealm, &p.NumFiles)
	if err != nil {
		return nil, err
	}

	// Files
	filesQ := `SELECT file_name, body FROM package_files WHERE package_path = ? AND network = ?`
	rows, err := d.db.Query(filesQ, path, p.Network)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var f FileInfo
		if err := rows.Scan(&f.Name, &f.Body); err != nil {
			return nil, err
		}
		p.Files = append(p.Files, f)
	}

	// Imports (dependencies)
	impRows, err := d.db.Query(`SELECT import_path FROM dependencies WHERE package_path = ? AND network = ?`, path, p.Network)
	if err != nil {
		return nil, err
	}
	defer impRows.Close()
	for impRows.Next() {
		var imp string
		if err := impRows.Scan(&imp); err != nil {
			return nil, err
		}
		p.Imports = append(p.Imports, imp)
	}

	// Dependents (who imports this)
	depRows, err := d.db.Query(`SELECT package_path FROM dependencies WHERE import_path = ? AND network = ?`, path, p.Network)
	if err != nil {
		return nil, err
	}
	defer depRows.Close()
	for depRows.Next() {
		var dep string
		if err := depRows.Scan(&dep); err != nil {
			return nil, err
		}
		p.Dependents = append(p.Dependents, dep)
	}

	// Recent calls
	callRows, err := d.db.Query(`
		SELECT tx_hash, block_height, caller, func_name, success
		FROM calls WHERE pkg_path = ? AND network = ?
		ORDER BY block_height DESC LIMIT 50
	`, path, p.Network)
	if err != nil {
		return nil, err
	}
	defer callRows.Close()
	for callRows.Next() {
		var c CallInfo
		if err := callRows.Scan(&c.TxHash, &c.BlockHeight, &c.Caller, &c.FuncName, &c.Success); err != nil {
			return nil, err
		}
		p.Callers = append(p.Callers, c)
	}

	// Call count
	d.db.QueryRow(`SELECT COUNT(*) FROM calls WHERE pkg_path = ? AND network = ?`, path, p.Network).Scan(&p.CallCount)

	// MsgRun references (where source contains import of this path)
	runRows, err := d.db.Query(`
		SELECT tx_hash, block_height, caller, success
		FROM msg_runs WHERE source LIKE ? AND network = ?
		ORDER BY block_height DESC LIMIT 50
	`, "%"+path+"%", p.Network)
	if err != nil {
		return nil, err
	}
	defer runRows.Close()
	for runRows.Next() {
		var r MsgRunInfo
		if err := runRows.Scan(&r.TxHash, &r.BlockHeight, &r.Caller, &r.Success); err != nil {
			return nil, err
		}
		p.MsgRunRefs = append(p.MsgRunRefs, r)
	}

	return &p, nil
}

// GetDependencyGraph returns the full dependency graph for a package (recursive).
func (d *DB) GetDependencyGraph(network, path string) (map[string][]string, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	graph := make(map[string][]string)
	visited := make(map[string]bool)

	var walk func(p string) error
	walk = func(p string) error {
		if visited[p] {
			return nil
		}
		visited[p] = true

		var rows *sql.Rows
		var err error
		if network != "" {
			rows, err = d.db.Query(`SELECT import_path FROM dependencies WHERE package_path = ? AND network = ?`, p, network)
		} else {
			rows, err = d.db.Query(`SELECT import_path FROM dependencies WHERE package_path = ?`, p)
		}
		if err != nil {
			return err
		}
		defer rows.Close()

		var deps []string
		for rows.Next() {
			var dep string
			if err := rows.Scan(&dep); err != nil {
				return err
			}
			deps = append(deps, dep)
		}
		graph[p] = deps

		for _, dep := range deps {
			if strings.HasPrefix(dep, "gno.land/") {
				if err := walk(dep); err != nil {
					return err
				}
			}
		}
		return nil
	}

	if err := walk(path); err != nil {
		return nil, err
	}
	return graph, nil
}

// GetReverseGraph returns all packages that depend on path (recursive).
func (d *DB) GetReverseGraph(network, path string) (map[string][]string, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	graph := make(map[string][]string)
	visited := make(map[string]bool)

	var walk func(p string) error
	walk = func(p string) error {
		if visited[p] {
			return nil
		}
		visited[p] = true

		var rows *sql.Rows
		var err error
		if network != "" {
			rows, err = d.db.Query(`SELECT package_path FROM dependencies WHERE import_path = ? AND network = ?`, p, network)
		} else {
			rows, err = d.db.Query(`SELECT package_path FROM dependencies WHERE import_path = ?`, p)
		}
		if err != nil {
			return err
		}
		defer rows.Close()

		var deps []string
		for rows.Next() {
			var dep string
			if err := rows.Scan(&dep); err != nil {
				return err
			}
			deps = append(deps, dep)
		}
		graph[p] = deps

		for _, dep := range deps {
			if err := walk(dep); err != nil {
				return err
			}
		}
		return nil
	}

	if err := walk(path); err != nil {
		return nil, err
	}
	return graph, nil
}

type Stats struct {
	TotalTxs      int `json:"total_txs"`
	TotalCalls    int `json:"total_calls"`
	TotalDeploys  int `json:"total_deploys"`
	TotalMsgRuns  int `json:"total_msg_runs"`
	TotalSends    int `json:"total_sends"`
	TotalRealms   int `json:"total_realms"`
	TotalPackages int `json:"total_packages"`
	UniqueCallers int `json:"unique_callers"`
	LatestBlock   int `json:"latest_block"`
}

// GetStats returns aggregate statistics.
func (d *DB) GetStats(network string) (*Stats, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var s Stats
	// One filter for every count, so a retired network cannot inflate some
	// totals and not others.
	nf := " WHERE " + d.networkFilter("network", network)
	d.db.QueryRow(`SELECT COUNT(*) FROM calls` + nf).Scan(&s.TotalCalls)
	d.db.QueryRow(`SELECT COUNT(*) FROM packages` + nf).Scan(&s.TotalDeploys)
	d.db.QueryRow(`SELECT COUNT(*) FROM packages` + nf + ` AND is_realm = 1`).Scan(&s.TotalRealms)
	s.TotalPackages = s.TotalDeploys - s.TotalRealms
	d.db.QueryRow(`SELECT COUNT(*) FROM msg_runs` + nf).Scan(&s.TotalMsgRuns)
	d.db.QueryRow(`SELECT COUNT(*) FROM bank_sends` + nf).Scan(&s.TotalSends)
	s.TotalTxs = s.TotalCalls + s.TotalDeploys + s.TotalMsgRuns + s.TotalSends
	d.db.QueryRow(`SELECT COUNT(DISTINCT caller) FROM calls` + nf).Scan(&s.UniqueCallers)
	d.db.QueryRow(`SELECT COALESCE(MAX(block_height), 0) FROM packages` + nf).Scan(&s.LatestBlock)
	return &s, nil
}

// Search searches across packages and callers.
func (d *DB) Search(network, q string) ([]PackageInfo, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	// The counts are selected, not left to the scan: this used to read eight
	// columns into eleven destinations, so every search returned
	// "expected 8 destination arguments in Scan, not 11" and the site's search
	// box was dead for any query.
	qStr := `
		SELECT p.network, p.path, p.name, p.creator, p.block_height, p.tx_hash,
		       p.is_realm, p.num_files,
		       (SELECT COUNT(*) FROM calls c WHERE c.network = p.network AND c.pkg_path = p.path),
		       (SELECT COUNT(*) FROM dependencies d WHERE d.network = p.network AND d.import_path = p.path),
		       (SELECT COUNT(*) FROM dependencies d WHERE d.network = p.network AND d.package_path = p.path)
		FROM packages p
		WHERE (p.path LIKE ? OR p.name LIKE ? OR p.creator LIKE ?)`
	args := []any{"%" + q + "%", "%" + q + "%", "%" + q + "%"}
	qStr += ` AND ` + d.networkFilter("p.network", network)
	qStr += ` ORDER BY p.block_height DESC LIMIT 20`

	rows, err := d.db.Query(qStr, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var pkgs []PackageInfo
	for rows.Next() {
		var p PackageInfo
		if err := rows.Scan(&p.Network, &p.Path, &p.Name, &p.Creator, &p.BlockHeight, &p.TxHash,
			&p.IsRealm, &p.NumFiles, &p.Calls, &p.Importers, &p.Imports); err != nil {
			return nil, err
		}
		pkgs = append(pkgs, p)
	}
	return pkgs, rows.Err()
}

type TokenInfo struct {
	Path      string `json:"path"`
	Network   string `json:"network,omitempty"`
	Name      string `json:"name"`
	Creator   string `json:"creator"`
	CallCount int    `json:"call_count"`
}

func (d *DB) GetTokenPackages(network string) ([]TokenInfo, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	// p.network comes along so the row can say which chain it is from. No two
	// chains currently share a package path, but nothing prevents it, and a
	// silent merge would be indistinguishable from a single deployment.
	q := `
		SELECT DISTINCT p.path, p.network, p.name, p.creator, COALESCE(c.cnt, 0)
		FROM packages p
		JOIN dependencies dep ON dep.package_path = p.path AND dep.network = p.network
		LEFT JOIN (SELECT pkg_path, network, COUNT(*) as cnt FROM calls GROUP BY network, pkg_path) c ON c.pkg_path = p.path AND c.network = p.network
		WHERE dep.import_path LIKE '%grc20%'`
	// Scoped like every other reader, so a retired network cannot reappear here.
	q += ` AND ` + d.networkFilter("p.network", network)
	args := []any{}
	q += ` ORDER BY p.block_height DESC`
	rows, err := d.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tokens := []TokenInfo{}
	for rows.Next() {
		var t TokenInfo
		if err := rows.Scan(&t.Path, &t.Network, &t.Name, &t.Creator, &t.CallCount); err != nil {
			return nil, err
		}
		tokens = append(tokens, t)
	}
	return tokens, rows.Err()
}

type AccountInfo struct {
	Address     string `json:"address"`
	Network     string `json:"network,omitempty"`
	CallCount   int    `json:"call_count"`
	DeployCount int    `json:"deploy_count"`
	MsgRunCount int    `json:"msgrun_count"`
	SendCount   int    `json:"send_count"`
}

func (d *DB) GetActiveAccounts(network string) ([]AccountInfo, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	nFilter := " WHERE " + d.networkFilter("network", network)

	// Grouped by (address, network), not by address alone.
	//
	// The same key can exist on several chains, and its activity there is
	// unrelated: an address busy on sapphire and quiet on gnoland1 is two
	// different actors as far as this table is concerned. Summing across chains
	// produced one row whose numbers belonged to neither — 47 addresses on the
	// production database were conflated this way.
	//
	// The consequence is that an address can appear once per chain it is active
	// on. That is the honest shape; the network column says which is which.
	q := `
		SELECT address, network, SUM(call_count), SUM(deploy_count), SUM(run_count), SUM(send_count)
		FROM (
			SELECT caller as address, network, COUNT(*) as call_count, 0 as deploy_count, 0 as run_count, 0 as send_count FROM calls` + nFilter + ` GROUP BY network, caller
			UNION ALL
			SELECT creator as address, network, 0, COUNT(*), 0, 0 FROM packages` + nFilter + ` GROUP BY network, creator
			UNION ALL
			SELECT caller as address, network, 0, 0, COUNT(*), 0 FROM msg_runs` + nFilter + ` GROUP BY network, caller
			UNION ALL
			SELECT from_address as address, network, 0, 0, 0, COUNT(*) FROM bank_sends` + nFilter + ` GROUP BY from_address, network
			UNION ALL
			SELECT to_address as address, network, 0, 0, 0, COUNT(*) FROM bank_sends` + nFilter + ` GROUP BY to_address, network
		)
		GROUP BY address, network
		ORDER BY (SUM(call_count) + SUM(deploy_count) + SUM(run_count) + SUM(send_count)) DESC
		LIMIT 100
	`
	rows, err := d.db.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	accounts := []AccountInfo{}
	for rows.Next() {
		var a AccountInfo
		if err := rows.Scan(&a.Address, &a.Network, &a.CallCount, &a.DeployCount, &a.MsgRunCount, &a.SendCount); err != nil {
			return nil, err
		}
		accounts = append(accounts, a)
	}
	return accounts, rows.Err()
}

func (d *DB) TotalSourceBytes(network string) int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	var n int
	if network != "" {
		d.db.QueryRow(`SELECT COALESCE(SUM(LENGTH(body)), 0) FROM package_files WHERE network = ?`, network).Scan(&n)
	} else {
		d.db.QueryRow(`SELECT COALESCE(SUM(LENGTH(body)), 0) FROM package_files`).Scan(&n)
	}
	return n
}

type AddrStat struct {
	// Network is the chain this row's figures belong to.
	//
	// An address is a different actor on each chain, and its ugnot is a
	// different asset. Ranking by address alone summed both: one top sender was
	// showing 900,400,000,000 ugnot that was really two chains' balances added
	// together, which is a figure describing nothing.
	Network string `json:"network,omitempty"`
	Address string `json:"address"`
	Count   int    `json:"count"`
	Total   int64  `json:"total"`
}

type BankStats struct {
	TotalSends      int        `json:"total_sends"`
	UniqueSenders   int        `json:"unique_senders"`
	UniqueReceivers int        `json:"unique_receivers"`
	UniqueAddresses int        `json:"unique_addresses"`
	TotalVolume     int64      `json:"total_volume"`
	TopSenders      []AddrStat `json:"top_senders"`
	TopReceiversVol []AddrStat `json:"top_receivers_volume"`
	TopReceiversCnt []AddrStat `json:"top_receivers_count"`

	// ByNetwork splits the totals per chain, populated only in all-networks
	// mode. Counts are meaningful summed; volume is not — gnoland1 ugnot and
	// sapphire ugnot are different assets, so the blended figure describes
	// nothing. The split is what lets the view aggregate honestly.
	ByNetwork map[string]BankSlice `json:"by_network,omitempty"`
}

// BankSlice is one network's share of the bank totals.
type BankSlice struct {
	TotalSends  int   `json:"total_sends"`
	TotalVolume int64 `json:"total_volume"`
}

const amountExpr = `COALESCE(SUM(CAST(REPLACE(REPLACE(amount, 'ugnot', ''), '"', '') AS INTEGER)), 0)`

func (d *DB) GetBankStats(network string) (*BankStats, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	nFilter := " WHERE " + d.networkFilter("network", network)

	var s BankStats
	d.db.QueryRow(`SELECT COUNT(*) FROM bank_sends` + nFilter).Scan(&s.TotalSends)
	d.db.QueryRow(`SELECT ` + amountExpr + ` FROM bank_sends` + nFilter).Scan(&s.TotalVolume)

	// One pass for the per-chain breakdown; the grouping key already leads the
	// indexes this reads.
	if network == "" {
		if rows, err := d.db.Query(`SELECT network, COUNT(*), ` + amountExpr +
			` FROM bank_sends` + nFilter + ` GROUP BY network`); err == nil {
			s.ByNetwork = map[string]BankSlice{}
			for rows.Next() {
				var net string
				var slice BankSlice
				if err := rows.Scan(&net, &slice.TotalSends, &slice.TotalVolume); err == nil {
					s.ByNetwork[net] = slice
				}
			}
			rows.Close()
		}
	}

	// Addresses are counted per chain, like everywhere else: the same string on
	// two chains is two actors. Blended, production reports 68,580 against
	// 68,672 counted honestly.
	d.db.QueryRow(`SELECT COUNT(*) FROM (SELECT DISTINCT addr, network FROM (
		SELECT from_address as addr, network FROM bank_sends` + nFilter + `
		UNION SELECT to_address, network FROM bank_sends` + nFilter + `))`).Scan(&s.UniqueAddresses)
	d.db.QueryRow(`SELECT COUNT(*) FROM (SELECT DISTINCT from_address, network FROM bank_sends` + nFilter + `)`).Scan(&s.UniqueSenders)
	d.db.QueryRow(`SELECT COUNT(*) FROM (SELECT DISTINCT to_address, network FROM bank_sends` + nFilter + `)`).Scan(&s.UniqueReceivers)

	// Every ranking leads its GROUP BY with the network, which both keys the row
	// correctly and matches the (network, address) indexes these read.
	s.TopSenders = d.queryAddrStats(`SELECT network, from_address, COUNT(*), ` + amountExpr + ` FROM bank_sends` + nFilter + ` GROUP BY network, from_address ORDER BY COUNT(*) DESC LIMIT 10`)
	s.TopReceiversVol = d.queryAddrStats(`SELECT network, to_address, COUNT(*), ` + amountExpr + ` FROM bank_sends` + nFilter + ` GROUP BY network, to_address ORDER BY ` + amountExpr + ` DESC LIMIT 10`)
	s.TopReceiversCnt = d.queryAddrStats(`SELECT network, to_address, COUNT(*), ` + amountExpr + ` FROM bank_sends` + nFilter + ` GROUP BY network, to_address ORDER BY COUNT(*) DESC LIMIT 10`)

	return &s, nil
}

// Every ranking row carries the chain it belongs to.
//
// 193 package paths now exist on more than one network — pearl launched with
// the same demo realms gnoland1 has — so a path alone no longer identifies a
// row, and neither does an address: the busiest caller on the site is active on
// two chains and the next-busiest on three. Ranking without the network merges
// separate actors into one row whose numbers belong to neither.
type RealmActivity struct {
	Network    string `json:"network,omitempty"`
	Path       string `json:"path"`
	Calls      int    `json:"calls"`
	Callers    int    `json:"callers"`
	Dependents int    `json:"dependents"`
	IsRealm    bool   `json:"is_realm"`
}

type CallerActivity struct {
	Network string `json:"network,omitempty"`
	Address string `json:"address"`
	Calls   int    `json:"calls"`
	Realms  int    `json:"realms"`
}

type ImportRank struct {
	Network string `json:"network,omitempty"`
	Path    string `json:"path"`
	Imports int    `json:"imports"`
}

type Analytics struct {
	// Summaries
	TotalRealms    int `json:"total_realms"`
	TotalPackages  int `json:"total_packages"`
	TotalCalls     int `json:"total_calls"`
	TotalDeploys   int `json:"total_deploys"`
	TotalMsgRuns   int `json:"total_msg_runs"`
	TotalSends     int `json:"total_sends"`
	TotalAddresses int `json:"total_addresses"`
	TotalSourceKB  int `json:"total_source_kb"`

	// Rankings
	TopRealms    []RealmActivity  `json:"top_realms"`
	TopPackages  []RealmActivity  `json:"top_packages"`
	TopCallers   []CallerActivity `json:"top_callers"`
	TopImports   []ImportRank     `json:"top_imports"`
	TopDeployers []CallerActivity `json:"top_deployers"`
	RecentRealms []PackageInfo    `json:"recent_realms"`
}

func (d *DB) GetAnalytics(network string) (*Analytics, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	nFilter := " WHERE " + d.networkFilter("network", network)
	pFilter := " AND " + d.networkFilter("p.network", network)

	var a Analytics
	if network != "" {
		d.db.QueryRow(`SELECT COUNT(*) FROM packages WHERE is_realm = 1 AND network = ?`, network).Scan(&a.TotalRealms)
		d.db.QueryRow(`SELECT COUNT(*) FROM packages WHERE is_realm = 0 AND network = ?`, network).Scan(&a.TotalPackages)
	} else {
		d.db.QueryRow(`SELECT COUNT(*) FROM packages WHERE is_realm = 1`).Scan(&a.TotalRealms)
		d.db.QueryRow(`SELECT COUNT(*) FROM packages WHERE is_realm = 0`).Scan(&a.TotalPackages)
	}
	d.db.QueryRow(`SELECT COUNT(*) FROM calls` + nFilter).Scan(&a.TotalCalls)
	d.db.QueryRow(`SELECT COUNT(*) FROM packages` + nFilter).Scan(&a.TotalDeploys)
	d.db.QueryRow(`SELECT COUNT(*) FROM msg_runs` + nFilter).Scan(&a.TotalMsgRuns)
	d.db.QueryRow(`SELECT COUNT(*) FROM bank_sends` + nFilter).Scan(&a.TotalSends)

	// Addresses are counted per chain. The same string on two chains is two
	// actors with two histories, and collapsing them undercounts: 68,506
	// blended against 68,806 counted honestly.
	addrUnionFilter := " WHERE " + d.networkFilter("network", network)
	d.db.QueryRow(`SELECT COUNT(*) FROM (SELECT DISTINCT addr, network FROM (
		SELECT caller as addr, network FROM calls` + addrUnionFilter + ` UNION SELECT creator, network FROM packages` + addrUnionFilter + `
		UNION SELECT caller, network FROM msg_runs` + addrUnionFilter + ` UNION SELECT from_address, network FROM bank_sends` + addrUnionFilter + `
		UNION SELECT to_address, network FROM bank_sends` + addrUnionFilter + `
	))`).Scan(&a.TotalAddresses)
	if network != "" {
		d.db.QueryRow(`SELECT COALESCE(SUM(LENGTH(body)), 0) / 1024 FROM package_files WHERE network = ?`, network).Scan(&a.TotalSourceKB)
	} else {
		d.db.QueryRow(`SELECT COALESCE(SUM(LENGTH(body)), 0) / 1024 FROM package_files`).Scan(&a.TotalSourceKB)
	}

	// The aggregate subqueries below join on (path, network), not on path alone.
	//
	// That join key is the correctness fix. 193 package paths now exist on more
	// than one chain — pearl launched carrying the same demo realms gnoland1
	// has — so matching on path alone attributes every chain's calls to every
	// copy of the realm. On production that put a realm with 22,789 calls at the
	// top of pearl's leaderboard when pearl had none of them, and ordered the
	// whole page by other chains' traffic.
	//
	// The filters are an optimization on top, not the fix: with the network in
	// the join key the results are already right without them. They shrink the
	// subquery to the selected chain's rows, which is worth a great deal when
	// one chain holds most of the traffic — pearl's page went from 0.59s to
	// 0.013s. Removing them changes timing, not answers.
	//
	// Every GROUP BY below leads with the network so it matches the existing
	// (network, col) indexes. Grouping is order-insensitive semantically but not
	// to the planner: with the network second, SQLite cannot walk the index in
	// group order and builds a temporary B-tree instead, which cost sapphire
	// 3.19s -> 5.57s until the terms were swapped.
	callFilter := " WHERE " + d.networkFilter("network", network)
	depFilter := " WHERE " + d.networkFilter("network", network)

	// Top realms by calls
	rows, _ := d.db.Query(`
		SELECT p.network, p.path, COALESCE(c.cnt, 0), COALESCE(c.callers, 0), COALESCE(dep.cnt, 0), p.is_realm
		FROM packages p
		LEFT JOIN (SELECT pkg_path, network, COUNT(*) as cnt, COUNT(DISTINCT caller) as callers FROM calls` + callFilter + ` GROUP BY network, pkg_path) c
			ON c.pkg_path = p.path AND c.network = p.network
		LEFT JOIN (SELECT import_path, network, COUNT(*) as cnt FROM dependencies` + depFilter + ` GROUP BY network, import_path) dep
			ON dep.import_path = p.path AND dep.network = p.network
		WHERE p.is_realm = 1` + pFilter + `
		ORDER BY COALESCE(c.cnt, 0) DESC LIMIT 15
	`)
	if rows != nil {
		defer rows.Close()
		for rows.Next() {
			var r RealmActivity
			rows.Scan(&r.Network, &r.Path, &r.Calls, &r.Callers, &r.Dependents, &r.IsRealm)
			a.TopRealms = append(a.TopRealms, r)
		}
	}

	// Top packages by imports (dependents)
	rows2, _ := d.db.Query(`
		SELECT p.network, p.path, COALESCE(c.cnt, 0), 0, COALESCE(dep.cnt, 0), p.is_realm
		FROM packages p
		LEFT JOIN (SELECT pkg_path, network, COUNT(*) as cnt FROM calls` + callFilter + ` GROUP BY network, pkg_path) c
			ON c.pkg_path = p.path AND c.network = p.network
		LEFT JOIN (SELECT import_path, network, COUNT(*) as cnt FROM dependencies` + depFilter + ` GROUP BY network, import_path) dep
			ON dep.import_path = p.path AND dep.network = p.network
		WHERE p.is_realm = 0` + pFilter + `
		ORDER BY COALESCE(dep.cnt, 0) DESC LIMIT 15
	`)
	if rows2 != nil {
		defer rows2.Close()
		for rows2.Next() {
			var r RealmActivity
			rows2.Scan(&r.Network, &r.Path, &r.Calls, &r.Callers, &r.Dependents, &r.IsRealm)
			a.TopPackages = append(a.TopPackages, r)
		}
	}

	// Top callers, per chain: the busiest caller on the site is active on two
	// networks and the next on three, so one row per address would be a sum
	// across separate actors.
	callersQ := `SELECT network, caller, COUNT(*) as c, COUNT(DISTINCT pkg_path) as realms FROM calls` + nFilter + ` GROUP BY network, caller ORDER BY c DESC LIMIT 15`
	rows3, _ := d.db.Query(callersQ)
	if rows3 != nil {
		defer rows3.Close()
		for rows3.Next() {
			var c CallerActivity
			rows3.Scan(&c.Network, &c.Address, &c.Calls, &c.Realms)
			a.TopCallers = append(a.TopCallers, c)
		}
	}

	// Top imports
	importsQ := `SELECT network, import_path, COUNT(*) as c FROM dependencies WHERE import_path LIKE 'gno.land/%'`
	importsQ += " AND " + d.networkFilter("network", network)
	importsQ += ` GROUP BY network, import_path ORDER BY c DESC LIMIT 15`
	rows4, _ := d.db.Query(importsQ)
	if rows4 != nil {
		defer rows4.Close()
		for rows4.Next() {
			var i ImportRank
			rows4.Scan(&i.Network, &i.Path, &i.Imports)
			a.TopImports = append(a.TopImports, i)
		}
	}

	// Top deployers
	deployQ := `SELECT network, creator, COUNT(*) as c, 0 FROM packages` + nFilter + ` GROUP BY network, creator ORDER BY c DESC LIMIT 15`
	rows5, _ := d.db.Query(deployQ)
	if rows5 != nil {
		defer rows5.Close()
		for rows5.Next() {
			var c CallerActivity
			rows5.Scan(&c.Network, &c.Address, &c.Calls, &c.Realms)
			a.TopDeployers = append(a.TopDeployers, c)
		}
	}

	// Recent realms
	recentQ := `SELECT network, path, name, creator, block_height, tx_hash, is_realm, num_files FROM packages WHERE is_realm = 1` + pFilter + ` ORDER BY block_height DESC LIMIT 10`
	rows6, _ := d.db.Query(recentQ)
	if rows6 != nil {
		defer rows6.Close()
		for rows6.Next() {
			var p PackageInfo
			rows6.Scan(&p.Network, &p.Path, &p.Name, &p.Creator, &p.BlockHeight, &p.TxHash, &p.IsRealm, &p.NumFiles)
			a.RecentRealms = append(a.RecentRealms, p)
		}
	}

	return &a, nil
}

func (d *DB) queryAddrStats(query string) []AddrStat {
	rows, err := d.db.Query(query)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var result []AddrStat
	for rows.Next() {
		var s AddrStat
		if err := rows.Scan(&s.Network, &s.Address, &s.Count, &s.Total); err != nil {
			continue
		}
		result = append(result, s)
	}
	return result
}

// --- Time-series types ---

type TxTimePoint struct {
	Time    string `json:"time"`
	Calls   int    `json:"calls"`
	Deploys int    `json:"deploys"`
	MsgRuns int    `json:"msg_runs"`
	Sends   int    `json:"sends"`
	Total   int    `json:"total"`
}

type PkgTimePoint struct {
	Time     string `json:"time"`
	Packages int    `json:"packages"`
	Realms   int    `json:"realms"`
	Total    int    `json:"total"`
}

type CallerTimePoint struct {
	Time            string `json:"time"`
	UniqueCallers   int    `json:"unique_callers"`
	UniqueDeployers int    `json:"unique_deployers"`
	UniqueSenders   int    `json:"unique_senders"`
}

// timeseriesFormat returns the SQLite strftime pattern, step duration, and truncation function
// for daily/hourly/weekly granularity.
func timeseriesFormat(granularity string) (sqlFmt string, step time.Duration, truncFn func(time.Time) time.Time) {
	switch granularity {
	case "hourly":
		return "%Y-%m-%dT%H", time.Hour, func(t time.Time) time.Time {
			return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, time.UTC)
		}
	case "weekly":
		return "%G-W%V", 7 * 24 * time.Hour, func(t time.Time) time.Time {
			d := int(t.Weekday())
			if d == 0 {
				d = 7
			}
			return time.Date(t.Year(), t.Month(), t.Day()-d+1, 0, 0, 0, 0, time.UTC)
		}
	default:
		return "%Y-%m-%d", 24 * time.Hour, func(t time.Time) time.Time {
			return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
		}
	}
}

func bucketKey(t time.Time, granularity string) string {
	switch granularity {
	case "hourly":
		return t.UTC().Format("2006-01-02T15")
	case "weekly":
		year, week := t.UTC().ISOWeek()
		return fmt.Sprintf("%d-W%02d", year, week)
	default:
		return t.UTC().Format("2006-01-02")
	}
}

func fillBuckets[T any](
	buckets map[string]*T,
	days int,
	granularity string,
	step time.Duration,
	truncFn func(time.Time) time.Time,
	empty func(string) T,
	finalize func(*T),
) []T {
	now := time.Now().UTC()
	start := truncFn(now.AddDate(0, 0, -days))
	end := truncFn(now)
	var out []T
	for cur := start; !cur.After(end); cur = truncFn(cur.Add(step)) {
		k := bucketKey(cur, granularity)
		if pt, ok := buckets[k]; ok {
			finalize(pt)
			out = append(out, *pt)
		} else {
			out = append(out, empty(k))
		}
	}
	return out
}

func (d *DB) GetTransactionTimeSeries(network, granularity string, days int) ([]TxTimePoint, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	sqlFmt, step, truncFn := timeseriesFormat(granularity)
	startTime := time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339)

	netFilter := ""
	if network != "" {
		netFilter = " AND t.network = ?"
	}

	subq := func(table, typ string) string {
		return fmt.Sprintf(
			"SELECT strftime('%s', t.block_time) as bucket, '%s' as typ, COUNT(*) as cnt"+
				" FROM %s t"+
				" WHERE t.block_time >= ?%s"+
				" GROUP BY bucket",
			sqlFmt, typ, table, netFilter)
	}

	q := subq("calls", "calls") +
		" UNION ALL " + subq("packages", "deploys") +
		" UNION ALL " + subq("msg_runs", "msg_runs") +
		" UNION ALL " + subq("bank_sends", "sends") +
		" ORDER BY bucket ASC"

	var args []any
	for range 4 {
		args = append(args, startTime)
		if network != "" {
			args = append(args, network)
		}
	}

	rows, err := d.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	buckets := make(map[string]*TxTimePoint)
	for rows.Next() {
		var bucket, typ string
		var cnt int
		if err := rows.Scan(&bucket, &typ, &cnt); err != nil {
			return nil, err
		}
		pt, ok := buckets[bucket]
		if !ok {
			pt = &TxTimePoint{Time: bucket}
			buckets[bucket] = pt
		}
		switch typ {
		case "calls":
			pt.Calls = cnt
		case "deploys":
			pt.Deploys = cnt
		case "msg_runs":
			pt.MsgRuns = cnt
		case "sends":
			pt.Sends = cnt
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return fillBuckets(buckets, days, granularity, step, truncFn,
		func(k string) TxTimePoint { return TxTimePoint{Time: k} },
		func(pt *TxTimePoint) { pt.Total = pt.Calls + pt.Deploys + pt.MsgRuns + pt.Sends },
	), nil
}

func (d *DB) GetPackageTimeSeries(network, granularity string, days int) ([]PkgTimePoint, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	sqlFmt, step, truncFn := timeseriesFormat(granularity)
	startTime := time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339)

	netFilter := ""
	if network != "" {
		netFilter = " AND t.network = ?"
	}

	subq := func(typ string, isRealm int) string {
		return fmt.Sprintf(
			"SELECT strftime('%s', t.block_time) as bucket, '%s' as typ, COUNT(*) as cnt"+
				" FROM packages t"+
				" WHERE t.block_time >= ? AND t.is_realm = %d%s"+
				" GROUP BY bucket",
			sqlFmt, typ, isRealm, netFilter)
	}

	q := subq("packages", 0) +
		" UNION ALL " + subq("realms", 1) +
		" ORDER BY bucket ASC"

	var args []any
	for range 2 {
		args = append(args, startTime)
		if network != "" {
			args = append(args, network)
		}
	}

	rows, err := d.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	buckets := make(map[string]*PkgTimePoint)
	for rows.Next() {
		var bucket, typ string
		var cnt int
		if err := rows.Scan(&bucket, &typ, &cnt); err != nil {
			return nil, err
		}
		pt, ok := buckets[bucket]
		if !ok {
			pt = &PkgTimePoint{Time: bucket}
			buckets[bucket] = pt
		}
		switch typ {
		case "packages":
			pt.Packages = cnt
		case "realms":
			pt.Realms = cnt
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return fillBuckets(buckets, days, granularity, step, truncFn,
		func(k string) PkgTimePoint { return PkgTimePoint{Time: k} },
		func(pt *PkgTimePoint) { pt.Total = pt.Packages + pt.Realms },
	), nil
}

func (d *DB) GetCallerTimeSeries(network, granularity string, days int) ([]CallerTimePoint, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	sqlFmt, step, truncFn := timeseriesFormat(granularity)
	startTime := time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339)

	netFilter := ""
	if network != "" {
		netFilter = " AND t.network = ?"
	}

	subqs := []string{
		fmt.Sprintf(
			"SELECT strftime('%s', t.block_time) as bucket, 'callers' as typ, COUNT(DISTINCT t.caller) as cnt"+
				" FROM calls t"+
				" WHERE t.block_time >= ?%s"+
				" GROUP BY bucket",
			sqlFmt, netFilter),
		fmt.Sprintf(
			"SELECT strftime('%s', t.block_time) as bucket, 'deployers' as typ, COUNT(DISTINCT t.creator) as cnt"+
				" FROM packages t"+
				" WHERE t.block_time >= ?%s"+
				" GROUP BY bucket",
			sqlFmt, netFilter),
		fmt.Sprintf(
			"SELECT strftime('%s', t.block_time) as bucket, 'senders' as typ, COUNT(DISTINCT t.from_address) as cnt"+
				" FROM bank_sends t"+
				" WHERE t.block_time >= ?%s"+
				" GROUP BY bucket",
			sqlFmt, netFilter),
	}
	q := strings.Join(subqs, " UNION ALL ") + " ORDER BY bucket ASC"

	var args []any
	for range 3 {
		args = append(args, startTime)
		if network != "" {
			args = append(args, network)
		}
	}

	rows, err := d.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	buckets := make(map[string]*CallerTimePoint)
	for rows.Next() {
		var bucket, typ string
		var cnt int
		if err := rows.Scan(&bucket, &typ, &cnt); err != nil {
			return nil, err
		}
		pt, ok := buckets[bucket]
		if !ok {
			pt = &CallerTimePoint{Time: bucket}
			buckets[bucket] = pt
		}
		switch typ {
		case "callers":
			pt.UniqueCallers = cnt
		case "deployers":
			pt.UniqueDeployers = cnt
		case "senders":
			pt.UniqueSenders = cnt
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return fillBuckets(buckets, days, granularity, step, truncFn,
		func(k string) CallerTimePoint { return CallerTimePoint{Time: k} },
		func(*CallerTimePoint) {},
	), nil
}

type StorageTimePoint struct {
	Time          string `json:"time"`
	BytesAdded    int    `json:"bytes_added"`
	FilesAdded    int    `json:"files_added"`
	PackagesAdded int    `json:"packages_added"`
}

func (d *DB) GetStorageTimeSeries(network, realmPath, granularity string, days int) ([]StorageTimePoint, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	sqlFmt, step, truncFn := timeseriesFormat(granularity)
	startTime := time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339)

	extraFilters := ""
	args := []any{startTime}
	if network != "" {
		extraFilters += " AND p.network = ?"
		args = append(args, network)
	}
	if realmPath != "" {
		extraFilters += " AND p.path = ?"
		args = append(args, realmPath)
	}

	q := fmt.Sprintf(
		"SELECT strftime('%s', p.block_time) as bucket,"+
			" SUM(LENGTH(pf.body)) as bytes_added,"+
			" COUNT(*) as files_added,"+
			" COUNT(DISTINCT p.path) as packages_added"+
			" FROM package_files pf"+
			" JOIN packages p ON p.network = pf.network AND p.path = pf.package_path"+
			" WHERE p.block_time >= ?%s"+
			" GROUP BY bucket ORDER BY bucket ASC",
		sqlFmt, extraFilters)

	rows, err := d.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type row struct {
		bucket        string
		bytesAdded    int
		filesAdded    int
		packagesAdded int
	}
	buckets := make(map[string]*row)
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.bucket, &r.bytesAdded, &r.filesAdded, &r.packagesAdded); err != nil {
			return nil, err
		}
		buckets[r.bucket] = &r
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	start := truncFn(now.AddDate(0, 0, -days))
	end := truncFn(now)
	var out []StorageTimePoint
	for cur := start; !cur.After(end); cur = truncFn(cur.Add(step)) {
		k := bucketKey(cur, granularity)
		if r, ok := buckets[k]; ok {
			out = append(out, StorageTimePoint{
				Time:          k,
				BytesAdded:    r.bytesAdded,
				FilesAdded:    r.filesAdded,
				PackagesAdded: r.packagesAdded,
			})
		} else {
			out = append(out, StorageTimePoint{Time: k})
		}
	}
	return out, nil
}

func (d *DB) UpsertTransaction(network, txHash string, blockHeight int, blockTime string, gasUsed, gasWanted, gasFee int, success bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.db.Exec(`
		INSERT OR IGNORE INTO transactions (network, tx_hash, block_height, block_time, gas_used, gas_wanted, gas_fee, success)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, network, txHash, blockHeight, blockTime, gasUsed, gasWanted, gasFee, success)
	return err
}

type GasTimePoint struct {
	Time           string  `json:"time"`
	TotalGasUsed   int     `json:"total_gas_used"`
	TotalGasWanted int     `json:"total_gas_wanted"`
	TotalFees      int     `json:"total_fees"`
	TxCount        int     `json:"tx_count"`
	AvgGasUsed     int     `json:"avg_gas_used"`
	GasEfficiency  float64 `json:"gas_efficiency"`
	AvgFee         int     `json:"avg_fee"`
	SuccessCount   int     `json:"success_count"`
	FailCount      int     `json:"fail_count"`

	// ByNetwork splits the same bucket per chain, and is only populated in
	// all-networks mode. Fees are denominated per chain and are not the same
	// asset across them, so a single summed figure describes nothing — the
	// split is what makes an aggregate honest. Counts remain summed above,
	// where adding them is meaningful.
	ByNetwork map[string]GasTimeSlice `json:"by_network,omitempty"`
}

// GasTimeSlice is one network's share of a bucket.
type GasTimeSlice struct {
	TotalGasUsed   int `json:"total_gas_used"`
	TotalGasWanted int `json:"total_gas_wanted"`
	TotalFees      int `json:"total_fees"`
	TxCount        int `json:"tx_count"`
	SuccessCount   int `json:"success_count"`
	FailCount      int `json:"fail_count"`
}

type SanityOverview struct {
	Network string `json:"network"`
	// ByNetwork carries one entry per chain in all-networks mode, and is absent
	// when a single network is selected.
	//
	// Liveness is the one figure that cannot be merged at all. It is not even
	// wrong to sum, the way a denominated amount is — there is simply no such
	// thing as the height, or the last block time, of four chains at once. This
	// page used to answer with clientFor(""), which returned an arbitrary entry
	// of a Go map, so it presented one randomly-chosen chain's liveness under a
	// global heading.
	ByNetwork          map[string]SanityLiveness `json:"by_network,omitempty"`
	ChainHeight        int                       `json:"chain_height"`
	LastBlockTime      string                    `json:"last_block_time"`
	SecondsSinceBlock  int                       `json:"seconds_since_block"`
	IsAlive            bool                      `json:"is_alive"`
	TxLast1h           int                       `json:"tx_last_1h"`
	TxLast24h          int                       `json:"tx_last_24h"`
	SuccessRate24h     float64                   `json:"success_rate_24h"`
	GasEfficiency24h   float64                   `json:"gas_efficiency_24h"`
	ActiveAddresses24h int                       `json:"active_addresses_24h"`
	NewPackages7d      int                       `json:"new_packages_7d"`
}

// SanityLiveness is the per-chain half of the overview: the figures that come
// from a live indexer rather than from stored rows.
type SanityLiveness struct {
	ChainHeight       int    `json:"chain_height"`
	LastBlockTime     string `json:"last_block_time,omitempty"`
	SecondsSinceBlock int    `json:"seconds_since_block"`
	IsAlive           bool   `json:"is_alive"`
	// Reachable distinguishes "this chain is not producing blocks" from "we
	// could not ask it", which look identical in the fields above.
	Reachable bool `json:"reachable"`
}

type HealthTimePoint struct {
	Time        string  `json:"time"`
	Total       int     `json:"total"`
	Success     int     `json:"success"`
	Failed      int     `json:"failed"`
	SuccessRate float64 `json:"success_rate"`
}

type ActiveAddressTimePoint struct {
	Time            string `json:"time"`
	TotalActive     int    `json:"total_active"`
	UniqueCallers   int    `json:"unique_callers"`
	UniqueDeployers int    `json:"unique_deployers"`
	UniqueSenders   int    `json:"unique_senders"`
}

func (d *DB) GetGasTimeSeries(network, granularity string, days int) ([]GasTimePoint, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	sqlFmt, step, truncFn := timeseriesFormat(granularity)
	startTime := time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339)

	// Always scoped, so a retired network cannot show up in the series and the
	// (network, ...) indexes stay usable for the all-networks case.
	netFilter := " AND " + d.networkFilter("t.network", network)

	// Group by network as well, then fold. One pass gives both the totals and
	// the per-chain split, and the extra grouping key is already the leading
	// column of the indexes this reads.
	q := fmt.Sprintf(
		"SELECT strftime('%s', t.block_time) as bucket, t.network,"+
			" SUM(t.gas_used) as total_gas_used,"+
			" SUM(t.gas_wanted) as total_gas_wanted,"+
			" SUM(t.gas_fee) as total_fees,"+
			" COUNT(*) as tx_count,"+
			" SUM(CASE WHEN t.success THEN 1 ELSE 0 END) as success_count"+
			" FROM transactions t"+
			" WHERE t.block_time >= ?%s"+
			" GROUP BY bucket, t.network ORDER BY bucket ASC",
		sqlFmt, netFilter)

	args := []any{startTime}

	rows, err := d.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type row struct {
		bucket         string
		totalGasUsed   int
		totalGasWanted int
		totalFees      int
		txCount        int
		successCount   int
		byNetwork      map[string]GasTimeSlice
	}
	buckets := make(map[string]*row)
	for rows.Next() {
		var bucket, net string
		var slice GasTimeSlice
		if err := rows.Scan(&bucket, &net, &slice.TotalGasUsed, &slice.TotalGasWanted,
			&slice.TotalFees, &slice.TxCount, &slice.SuccessCount); err != nil {
			return nil, err
		}
		slice.FailCount = slice.TxCount - slice.SuccessCount

		r := buckets[bucket]
		if r == nil {
			r = &row{bucket: bucket, byNetwork: map[string]GasTimeSlice{}}
			buckets[bucket] = r
		}
		r.totalGasUsed += slice.TotalGasUsed
		r.totalGasWanted += slice.TotalGasWanted
		r.totalFees += slice.TotalFees
		r.txCount += slice.TxCount
		r.successCount += slice.SuccessCount
		r.byNetwork[net] = slice
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	start := truncFn(now.AddDate(0, 0, -days))
	end := truncFn(now)
	var out []GasTimePoint
	for cur := start; !cur.After(end); cur = truncFn(cur.Add(step)) {
		k := bucketKey(cur, granularity)
		if r, ok := buckets[k]; ok {
			// Only worth sending when several chains are in play; with one
			// selected the split is the total.
			var split map[string]GasTimeSlice
			if network == "" {
				split = r.byNetwork
			}
			avg := 0
			avgFee := 0
			var eff float64
			if r.txCount > 0 {
				avg = r.totalGasUsed / r.txCount
				avgFee = r.totalFees / r.txCount
			}
			if r.totalGasWanted > 0 {
				eff = float64(r.totalGasUsed) / float64(r.totalGasWanted)
			}
			out = append(out, GasTimePoint{
				Time:           k,
				TotalGasUsed:   r.totalGasUsed,
				TotalGasWanted: r.totalGasWanted,
				TotalFees:      r.totalFees,
				TxCount:        r.txCount,
				AvgGasUsed:     avg,
				GasEfficiency:  eff,
				AvgFee:         avgFee,
				SuccessCount:   r.successCount,
				FailCount:      r.txCount - r.successCount,
				ByNetwork:      split,
			})
		} else {
			out = append(out, GasTimePoint{Time: k})
		}
	}
	return out, nil
}

func (d *DB) GetSanityOverview(network string) (*SanityOverview, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	ov := &SanityOverview{Network: network}

	netFilter := ""
	if network != "" {
		netFilter = " AND network = ?"
	}

	now := time.Now().UTC()
	since1h := now.Add(-1 * time.Hour).Format(time.RFC3339)
	since24h := now.Add(-24 * time.Hour).Format(time.RFC3339)
	since7d := now.Add(-7 * 24 * time.Hour).Format(time.RFC3339)

	args1h := []any{since1h}
	if network != "" {
		args1h = append(args1h, network)
	}
	d.db.QueryRow(`SELECT COUNT(*) FROM transactions WHERE block_time >= ?`+netFilter, args1h...).Scan(&ov.TxLast1h)

	args24h := []any{since24h}
	if network != "" {
		args24h = append(args24h, network)
	}
	var total24h, success24h, gasUsed24h, gasWanted24h int
	d.db.QueryRow(`SELECT COUNT(*), SUM(CASE WHEN success THEN 1 ELSE 0 END), SUM(gas_used), SUM(gas_wanted) FROM transactions WHERE block_time >= ?`+netFilter, args24h...).Scan(&total24h, &success24h, &gasUsed24h, &gasWanted24h)
	ov.TxLast24h = total24h
	if total24h > 0 {
		ov.SuccessRate24h = float64(success24h) / float64(total24h)
	}
	if gasWanted24h > 0 {
		ov.GasEfficiency24h = float64(gasUsed24h) / float64(gasWanted24h)
	}

	addrArgs := []any{since24h, since24h, since24h}
	addrQuery := `SELECT COUNT(DISTINCT addr) FROM (
		SELECT caller as addr FROM calls WHERE block_time >= ?
		UNION SELECT creator FROM packages WHERE block_time >= ?
		UNION SELECT from_address FROM bank_sends WHERE block_time >= ?
	)`
	if network != "" {
		addrArgs = []any{since24h, network, since24h, network, since24h, network}
		addrQuery = `SELECT COUNT(DISTINCT addr) FROM (
			SELECT caller as addr FROM calls WHERE block_time >= ? AND network = ?
			UNION SELECT creator FROM packages WHERE block_time >= ? AND network = ?
			UNION SELECT from_address FROM bank_sends WHERE block_time >= ? AND network = ?
		)`
	}
	d.db.QueryRow(addrQuery, addrArgs...).Scan(&ov.ActiveAddresses24h)

	args7d := []any{since7d}
	if network != "" {
		args7d = append(args7d, network)
	}
	d.db.QueryRow(`SELECT COUNT(*) FROM packages WHERE block_time >= ?`+netFilter, args7d...).Scan(&ov.NewPackages7d)

	return ov, nil
}

func (d *DB) GetHealthTimeSeries(network, granularity string, days int) ([]HealthTimePoint, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	sqlFmt, step, truncFn := timeseriesFormat(granularity)
	startTime := time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339)

	netFilter := ""
	if network != "" {
		netFilter = " AND t.network = ?"
	}

	q := fmt.Sprintf(
		"SELECT strftime('%s', t.block_time) as bucket,"+
			" COUNT(*) as total,"+
			" SUM(CASE WHEN t.success THEN 1 ELSE 0 END) as success,"+
			" SUM(CASE WHEN NOT t.success THEN 1 ELSE 0 END) as failed"+
			" FROM transactions t"+
			" WHERE t.block_time >= ?%s"+
			" GROUP BY bucket ORDER BY bucket ASC",
		sqlFmt, netFilter)

	args := []any{startTime}
	if network != "" {
		args = append(args, network)
	}

	rows, err := d.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type rowData struct {
		bucket  string
		total   int
		success int
		failed  int
	}
	buckets := make(map[string]*rowData)
	for rows.Next() {
		var r rowData
		if err := rows.Scan(&r.bucket, &r.total, &r.success, &r.failed); err != nil {
			return nil, err
		}
		buckets[r.bucket] = &r
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	start := truncFn(now.AddDate(0, 0, -days))
	end := truncFn(now)
	var out []HealthTimePoint
	for cur := start; !cur.After(end); cur = truncFn(cur.Add(step)) {
		k := bucketKey(cur, granularity)
		if r, ok := buckets[k]; ok {
			rate := -1.0
			if r.total > 0 {
				rate = float64(r.success) / float64(r.total)
			}
			out = append(out, HealthTimePoint{
				Time:        k,
				Total:       r.total,
				Success:     r.success,
				Failed:      r.failed,
				SuccessRate: rate,
			})
		} else {
			out = append(out, HealthTimePoint{Time: k, SuccessRate: -1})
		}
	}
	return out, nil
}

func (d *DB) GetRealmsWithStorage(network string, days int) ([]string, error) {
	startTime := time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339)
	q := `SELECT DISTINCT p.path
		FROM package_files pf
		JOIN packages p ON p.network = pf.network AND p.path = pf.package_path
		WHERE p.block_time >= ? AND p.is_realm = 1`
	args := []any{startTime}
	if network != "" {
		q += " AND p.network = ?"
		args = append(args, network)
	}
	q += " ORDER BY p.path ASC"
	rows, err := d.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var paths []string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, err
		}
		paths = append(paths, path)
	}
	return paths, rows.Err()
}

func (d *DB) GetActiveAddressTimeSeries(network, granularity string, days int) ([]ActiveAddressTimePoint, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	sqlFmt, step, truncFn := timeseriesFormat(granularity)
	startTime := time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339)

	netFilter := ""
	if network != "" {
		netFilter = " AND t.network = ?"
	}

	// Individual counts: callers, deployers, senders
	subqs := []string{
		fmt.Sprintf("SELECT strftime('%s', t.block_time) as bucket, 'callers' as typ, COUNT(DISTINCT t.caller) as cnt FROM calls t WHERE t.block_time >= ?%s GROUP BY bucket", sqlFmt, netFilter),
		fmt.Sprintf("SELECT strftime('%s', t.block_time) as bucket, 'deployers' as typ, COUNT(DISTINCT t.creator) as cnt FROM packages t WHERE t.block_time >= ?%s GROUP BY bucket", sqlFmt, netFilter),
		fmt.Sprintf("SELECT strftime('%s', t.block_time) as bucket, 'senders' as typ, COUNT(DISTINCT t.from_address) as cnt FROM bank_sends t WHERE t.block_time >= ?%s GROUP BY bucket", sqlFmt, netFilter),
	}
	q := strings.Join(subqs, " UNION ALL ") + " ORDER BY bucket ASC"

	var args []any
	for range 3 {
		args = append(args, startTime)
		if network != "" {
			args = append(args, network)
		}
	}

	rows, err := d.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	buckets := make(map[string]*ActiveAddressTimePoint)
	for rows.Next() {
		var bucket, typ string
		var cnt int
		if err := rows.Scan(&bucket, &typ, &cnt); err != nil {
			return nil, err
		}
		pt, ok := buckets[bucket]
		if !ok {
			pt = &ActiveAddressTimePoint{Time: bucket}
			buckets[bucket] = pt
		}
		switch typ {
		case "callers":
			pt.UniqueCallers = cnt
		case "deployers":
			pt.UniqueDeployers = cnt
		case "senders":
			pt.UniqueSenders = cnt
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Union total active addresses per bucket
	var unionNetFilter string
	var unionArgs []any
	if network != "" {
		unionNetFilter = " AND network = ?"
		unionArgs = []any{startTime, network, startTime, network, startTime, network}
	} else {
		unionArgs = []any{startTime, startTime, startTime}
	}
	unionQ := fmt.Sprintf(
		"SELECT strftime('%s', block_time) as bucket, COUNT(DISTINCT addr) as cnt FROM ("+
			" SELECT caller as addr, block_time, network FROM calls WHERE block_time >= ?%s"+
			" UNION SELECT creator, block_time, network FROM packages WHERE block_time >= ?%s"+
			" UNION SELECT from_address, block_time, network FROM bank_sends WHERE block_time >= ?%s"+
			") GROUP BY bucket ORDER BY bucket ASC",
		sqlFmt, unionNetFilter, unionNetFilter, unionNetFilter)

	urows, err := d.db.Query(unionQ, unionArgs...)
	if err != nil {
		return nil, err
	}
	defer urows.Close()
	for urows.Next() {
		var bucket string
		var cnt int
		if err := urows.Scan(&bucket, &cnt); err != nil {
			return nil, err
		}
		if pt, ok := buckets[bucket]; ok {
			pt.TotalActive = cnt
		} else {
			buckets[bucket] = &ActiveAddressTimePoint{Time: bucket, TotalActive: cnt}
		}
	}
	if err := urows.Err(); err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	start := truncFn(now.AddDate(0, 0, -days))
	end := truncFn(now)
	var out []ActiveAddressTimePoint
	for cur := start; !cur.After(end); cur = truncFn(cur.Add(step)) {
		k := bucketKey(cur, granularity)
		if pt, ok := buckets[k]; ok {
			out = append(out, *pt)
		} else {
			out = append(out, ActiveAddressTimePoint{Time: k})
		}
	}
	return out, nil
}
