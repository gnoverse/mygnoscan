package main

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	_ "modernc.org/sqlite"
	"regexp"
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

// WaitBackground blocks until the startup work started in NewDB has finished.
//
// Only ANALYZE runs there today, and it takes the write lock for the better part
// of a second on a large database. Anything else that writes during startup has
// to wait for it or risk SQLITE_BUSY — the one-time dependency re-extraction lost
// a package that way, which cost it the whole pass, because the version marker is
// only written once every package succeeds.
func (d *DB) WaitBackground() {
	d.background.Wait()
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

		-- Gas rollups: the gas page's two expensive aggregates, precomputed.
		--
		-- Both scale with the chain and had reached 14s on sapphire, against a
		-- 30s server write timeout — the same trajectory that broke the address
		-- page twice. Neither can be indexed away: attributing gas per realm
		-- means touching every call, and the totals are a sum over every
		-- transaction.
		--
		-- Refreshed wholesale on a timer rather than maintained per insert.
		-- Incremental maintenance would have to be exactly right across
		-- re-syncs, backfills and chain resets, any of which would otherwise
		-- double-count; a periodic recompute is idempotent by construction.
		CREATE TABLE IF NOT EXISTS gas_realm_rollup (
			network   TEXT NOT NULL,
			path      TEXT NOT NULL,
			gas_used  INTEGER NOT NULL,
			gas_fee   INTEGER NOT NULL,
			tx_count  INTEGER NOT NULL,
			PRIMARY KEY (network, path)
		);

		CREATE INDEX IF NOT EXISTS idx_gas_rollup_gas
			ON gas_realm_rollup(network, gas_used DESC);

		CREATE TABLE IF NOT EXISTS gas_totals_rollup (
			network       TEXT PRIMARY KEY,
			tx_count      INTEGER NOT NULL,
			gas_used      INTEGER NOT NULL,
			gas_wanted    INTEGER NOT NULL,
			gas_fee       INTEGER NOT NULL,
			success_count INTEGER NOT NULL
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
	q += ` AND ` + d.networkFilter("network", network)
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
	// ComputedAt is when the rollups behind these figures were built, empty if
	// they were computed live. Surfaced so a reader can tell fresh from stale.
	ComputedAt string `json:"computed_at,omitempty"`

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

	// Prefer the rollup. Both aggregates below scale with the chain and had
	// reached 14s on sapphire; precomputed they are a lookup. ComputedAt tells
	// the reader how fresh the figures are — showing stale numbers without
	// saying so is the failure mode this avoids.
	rollupReady, computedAt := d.gasRollupReady()

	// Always filter, even for "all networks": an empty WHERE cannot use the
	// (network, gas_used) index, which is what makes the top-gas sort scan the
	// whole table. Scoping to the configured set keeps the index usable and
	// keeps retired networks out of the totals.
	where := " WHERE " + d.networkFilter("network", network)
	args := []any{}

	out := &GasStats{ComputedAt: computedAt}

	totalsQuery := `
		SELECT COUNT(*),
		       COALESCE(SUM(gas_used), 0),
		       COALESCE(SUM(gas_wanted), 0),
		       COALESCE(SUM(gas_fee), 0),
		       COALESCE(SUM(CASE WHEN success THEN 1 ELSE 0 END), 0)
		FROM transactions` + where
	if rollupReady {
		totalsQuery = `
		SELECT COALESCE(SUM(tx_count),0), COALESCE(SUM(gas_used),0), COALESCE(SUM(gas_wanted),0),
		       COALESCE(SUM(gas_fee),0), COALESCE(SUM(success_count),0)
		FROM gas_totals_rollup` + where
	}
	err := d.db.QueryRow(totalsQuery, args...).
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

	realmQuery := `
		SELECT path, SUM(gas_used), SUM(gas_fee), COUNT(*) FROM (
			SELECT c.pkg_path AS path, t.gas_used, t.gas_fee, t.tx_hash
			  FROM calls c JOIN transactions t
			    ON t.network = c.network AND t.tx_hash = c.tx_hash` + realmWhere + `
			UNION ALL
			SELECT p.path AS path, t.gas_used, t.gas_fee, t.tx_hash
			  FROM packages p JOIN transactions t
			    ON t.network = p.network AND t.tx_hash = p.tx_hash` + realmWhere + `
			UNION ALL
			SELECT 'MsgRun by ' || m.caller AS path, t.gas_used, t.gas_fee, t.tx_hash
			  FROM msg_runs m JOIN transactions t
			    ON t.network = m.network AND t.tx_hash = m.tx_hash` + realmWhere + `
		) GROUP BY path ORDER BY SUM(gas_used) DESC LIMIT ?`
	if rollupReady {
		// The rollup is per (network, path); a path on two chains stays two
		// rows there, so the read re-groups when several networks are in scope.
		realmQuery = `
		SELECT path, SUM(gas_used), SUM(gas_fee), SUM(tx_count)
		FROM gas_realm_rollup WHERE ` + d.networkFilter("network", network) + `
		GROUP BY path ORDER BY SUM(gas_used) DESC LIMIT ?`
	}
	rows, err := d.db.Query(realmQuery, append(realmArgs, topN)...)
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
	Files   []FileInfo `json:"files"`
	Imports []string   `json:"imports"`
	// Dependents carries each importer's creator, not just its path.
	//
	// A flat list of paths reads as adoption when it may be one project's
	// version churn: r/gnoswap/router has 25 dependents, 21 of which are one
	// deployer's sequential gnomemepad releases. The question a reader has is
	// "how many independent parties depend on this", and answering it from paths
	// alone meant cross-referencing creators by hand across pages.
	Dependents []Dependent  `json:"dependents"`
	Callers    []CallInfo   `json:"recent_calls"`
	MsgRunRefs []MsgRunInfo `json:"msgrun_refs"`
	CallCount  int          `json:"call_count"`
}

// Dependent is one realm that imports another.
type Dependent struct {
	Path    string `json:"path"`
	Creator string `json:"creator,omitempty"`
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
	// Scoped to the configured networks, so this agrees with the same count on
	// the home page. It did not: the stat tile read 321 realms while the
	// directory header read 508, because this branch counted retired chains and
	// that one did not. topaz alone accounts for the 187 between them.
	q := `SELECT COUNT(*) FROM packages WHERE is_realm = ? AND ` +
		d.networkFilter("network", network)
	var count int
	err := d.db.QueryRow(q, realmOnly).Scan(&count)
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
		FROM packages p WHERE p.is_realm = ? AND ` + d.networkFilter("p.network", network)
	args := []any{realmOnly}
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

	q := `SELECT network, path, name, creator, block_height, tx_hash, is_realm, num_files
	      FROM packages WHERE path = ? AND ` + d.networkFilter("network", network)
	args := []any{path}

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

	// Dependents (who imports this), with each one's creator.
	//
	// LEFT JOIN rather than JOIN: an edge can point at a package this instance
	// has not synced the deploy for, and dropping those would understate the
	// count. Such a row simply has no creator.
	depRows, err := d.db.Query(`
		SELECT dep.package_path, COALESCE(pkg.creator, '')
		FROM dependencies dep
		LEFT JOIN packages pkg ON pkg.network = dep.network AND pkg.path = dep.package_path
		WHERE dep.import_path = ? AND dep.network = ?
		ORDER BY dep.package_path ASC`, path, p.Network)
	if err != nil {
		return nil, err
	}
	defer depRows.Close()
	for depRows.Next() {
		var dep Dependent
		if err := depRows.Scan(&dep.Path, &dep.Creator); err != nil {
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
		rows, err = d.db.Query(`SELECT import_path FROM dependencies WHERE package_path = ? AND `+
			d.networkFilter("network", network), p)
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
		rows, err = d.db.Query(`SELECT package_path FROM dependencies WHERE import_path = ? AND `+
			d.networkFilter("network", network), p)
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
	// Per (caller, network): the same address on two chains is two actors, and
	// collapsing them undercounts. 19,054 blended against 19,117 on production.
	d.db.QueryRow(`SELECT COUNT(*) FROM (SELECT DISTINCT caller, network FROM calls` + nf + `)`).Scan(&s.UniqueCallers)
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

// accountSortClause maps a sort name to an ORDER BY over the aggregated columns.
//
// The sums are recomputed rather than referenced by alias: SQLite allows the
// alias here but the expression is what the index-free aggregate produces
// either way, and spelling it out keeps the mapping readable next to the query.
func accountSortClause(sortBy string) string {
	switch sortBy {
	case "calls":
		return "SUM(call_count) DESC"
	case "deploys":
		return "SUM(deploy_count) DESC"
	case "runs":
		return "SUM(run_count) DESC"
	case "sends":
		return "SUM(send_count) DESC"
	default: // total activity
		return "(SUM(call_count) + SUM(deploy_count) + SUM(run_count) + SUM(send_count)) DESC"
	}
}

// GetActiveAccounts returns the most active accounts, paged.
//
// limit and offset are honoured so this can back a real leaderboard: it used to
// return a fixed top 100 with no controls at all, which #10 flagged as the
// reason it could not serve as one.
func (d *DB) GetActiveAccounts(network, sortBy string, limit, offset int) ([]AccountInfo, error) {
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
		ORDER BY ` + accountSortClause(sortBy) + `
		LIMIT ? OFFSET ?
	`
	rows, err := d.db.Query(q, limit, offset)
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
	d.db.QueryRow(`SELECT COALESCE(SUM(LENGTH(body)), 0) FROM package_files WHERE ` +
		d.networkFilter("network", network)).Scan(&n)
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
	pkgFilter := d.networkFilter("network", network)
	d.db.QueryRow(`SELECT COUNT(*) FROM packages WHERE is_realm = 1 AND ` + pkgFilter).Scan(&a.TotalRealms)
	d.db.QueryRow(`SELECT COUNT(*) FROM packages WHERE is_realm = 0 AND ` + pkgFilter).Scan(&a.TotalPackages)
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
	d.db.QueryRow(`SELECT COALESCE(SUM(LENGTH(body)), 0) / 1024 FROM package_files WHERE ` +
		d.networkFilter("network", network)).Scan(&a.TotalSourceKB)

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

	// Scope to the configured networks, always.
	//
	// An empty filter is not "all networks" — it is "every network ever synced",
	// including retired ones whose rows are still here. topaz has been gone for
	// months and still holds 3,093 transactions, 2,945 calls and 272 packages in
	// production, and every unfiltered series above was quietly counting them.
	netFilter := " AND " + d.networkFilter("t.network", network)

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

	args := []any{startTime, startTime, startTime, startTime}

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

	// Scope to the configured networks, always.
	//
	// An empty filter is not "all networks" — it is "every network ever synced",
	// including retired ones whose rows are still here. topaz has been gone for
	// months and still holds 3,093 transactions, 2,945 calls and 272 packages in
	// production, and every unfiltered series above was quietly counting them.
	netFilter := " AND " + d.networkFilter("t.network", network)

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

	args := []any{startTime, startTime}

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

// perChainBucketCount builds a per-bucket count of distinct addresses, keyed by
// (address, network) rather than by address alone.
//
// The same address on two chains is two actors with two histories, so counting
// it once undercounts. Every address figure in the project is counted this way;
// doing it here too is what keeps a series' components consistent with the
// totals computed beside them, rather than producing a page whose own numbers
// contradict each other.
func perChainBucketCount(sqlFmt, typ, column, from, netFilter string) string {
	return fmt.Sprintf(
		"SELECT bucket, '%s' as typ, COUNT(*) as cnt FROM ("+
			" SELECT DISTINCT strftime('%s', t.block_time) as bucket, %s, t.network"+
			" FROM %s WHERE t.block_time >= ?%s"+
			") GROUP BY bucket",
		typ, sqlFmt, column, from, netFilter)
}

func (d *DB) GetCallerTimeSeries(network, granularity string, days int) ([]CallerTimePoint, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	sqlFmt, step, truncFn := timeseriesFormat(granularity)
	startTime := time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339)

	// Scope to the configured networks, always.
	//
	// An empty filter is not "all networks" — it is "every network ever synced",
	// including retired ones whose rows are still here. topaz has been gone for
	// months and still holds 3,093 transactions, 2,945 calls and 272 packages in
	// production, and every unfiltered series above was quietly counting them.
	netFilter := " AND " + d.networkFilter("t.network", network)

	subqs := []string{
		// Counted per (address, network), like every other address figure: the
		// same address on two chains is two actors. Counting the pair keeps this
		// consistent with total_active in the active-addresses series, which
		// would otherwise be computed one way and its components another.
		perChainBucketCount(sqlFmt, "callers", "t.caller", "calls t", netFilter),
		perChainBucketCount(sqlFmt, "deployers", "t.creator", "packages t", netFilter),
		perChainBucketCount(sqlFmt, "senders", "t.from_address", "bank_sends t", netFilter),
	}
	q := strings.Join(subqs, " UNION ALL ") + " ORDER BY bucket ASC"

	args := []any{startTime, startTime, startTime}

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

	extraFilters := " AND " + d.networkFilter("p.network", network)
	args := []any{startTime}
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

	// Scope to the configured networks, always.
	//
	// An empty filter is not "all networks" — it is "every network ever synced",
	// including retired ones whose rows are still here. topaz has been gone for
	// months and still holds 3,093 transactions, 2,945 calls and 272 packages in
	// production, and every unfiltered series above was quietly counting them.
	netFilter := " AND " + d.networkFilter("network", network)

	now := time.Now().UTC()
	since1h := now.Add(-1 * time.Hour).Format(time.RFC3339)
	since24h := now.Add(-24 * time.Hour).Format(time.RFC3339)
	since7d := now.Add(-7 * 24 * time.Hour).Format(time.RFC3339)

	d.db.QueryRow(`SELECT COUNT(*) FROM transactions WHERE block_time >= ?`+netFilter, since1h).Scan(&ov.TxLast1h)

	var total24h, success24h, gasUsed24h, gasWanted24h int
	d.db.QueryRow(`SELECT COUNT(*), SUM(CASE WHEN success THEN 1 ELSE 0 END), SUM(gas_used), SUM(gas_wanted) FROM transactions WHERE block_time >= ?`+netFilter, since24h).Scan(&total24h, &success24h, &gasUsed24h, &gasWanted24h)
	ov.TxLast24h = total24h
	if total24h > 0 {
		ov.SuccessRate24h = float64(success24h) / float64(total24h)
	}
	if gasWanted24h > 0 {
		ov.GasEfficiency24h = float64(gasUsed24h) / float64(gasWanted24h)
	}

	// Scoped to the configured networks and counted per chain.
	//
	// This one was wrong in two directions at once: the all-networks branch had
	// no network filter at all, so it counted retired topaz, and the distinct
	// count collapsed an address seen on two chains into one. On production:
	// 63,404 as written, 63,343 once topaz is excluded, 63,421 counted honestly.
	addrFilter := " AND " + d.networkFilter("network", network)
	addrQuery := `SELECT COUNT(*) FROM (SELECT DISTINCT addr, network FROM (
		SELECT caller as addr, network FROM calls WHERE block_time >= ?` + addrFilter + `
		UNION SELECT creator, network FROM packages WHERE block_time >= ?` + addrFilter + `
		UNION SELECT from_address, network FROM bank_sends WHERE block_time >= ?` + addrFilter + `
	))`
	d.db.QueryRow(addrQuery, since24h, since24h, since24h).Scan(&ov.ActiveAddresses24h)

	d.db.QueryRow(`SELECT COUNT(*) FROM packages WHERE block_time >= ?`+netFilter, since7d).Scan(&ov.NewPackages7d)

	return ov, nil
}

func (d *DB) GetHealthTimeSeries(network, granularity string, days int) ([]HealthTimePoint, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	sqlFmt, step, truncFn := timeseriesFormat(granularity)
	startTime := time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339)

	// Scope to the configured networks, always.
	//
	// An empty filter is not "all networks" — it is "every network ever synced",
	// including retired ones whose rows are still here. topaz has been gone for
	// months and still holds 3,093 transactions, 2,945 calls and 272 packages in
	// production, and every unfiltered series above was quietly counting them.
	netFilter := " AND " + d.networkFilter("t.network", network)

	q := fmt.Sprintf(
		"SELECT strftime('%s', t.block_time) as bucket,"+
			" COUNT(*) as total,"+
			" SUM(CASE WHEN t.success THEN 1 ELSE 0 END) as success,"+
			" SUM(CASE WHEN NOT t.success THEN 1 ELSE 0 END) as failed"+
			" FROM transactions t"+
			" WHERE t.block_time >= ?%s"+
			" GROUP BY bucket ORDER BY bucket ASC",
		sqlFmt, netFilter)

	rows, err := d.db.Query(q, startTime)
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
	q += " AND " + d.networkFilter("p.network", network)
	q += " ORDER BY p.path ASC"
	rows, err := d.db.Query(q, startTime)
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

	// Scope to the configured networks, always.
	//
	// An empty filter is not "all networks" — it is "every network ever synced",
	// including retired ones whose rows are still here. topaz has been gone for
	// months and still holds 3,093 transactions, 2,945 calls and 272 packages in
	// production, and every unfiltered series above was quietly counting them.
	netFilter := " AND " + d.networkFilter("t.network", network)

	// Individual counts: callers, deployers, senders — counted the same way as
	// the union total below, so the parts agree with the whole.
	subqs := []string{
		perChainBucketCount(sqlFmt, "callers", "t.caller", "calls t", netFilter),
		perChainBucketCount(sqlFmt, "deployers", "t.creator", "packages t", netFilter),
		perChainBucketCount(sqlFmt, "senders", "t.from_address", "bank_sends t", netFilter),
	}
	q := strings.Join(subqs, " UNION ALL ") + " ORDER BY bucket ASC"

	args := []any{startTime, startTime, startTime}

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

	// Union total active addresses per bucket, counted per chain.
	//
	// COUNT(DISTINCT addr) collapses an address seen on two chains into one, but
	// it is two actors with two histories — the same reasoning as everywhere
	// else. The inner query already carries the network, so counting the pair
	// costs nothing extra.
	unionNetFilter := " AND " + d.networkFilter("network", network)
	unionQ := fmt.Sprintf(
		"SELECT bucket, COUNT(*) as cnt FROM ("+
			" SELECT DISTINCT strftime('%s', block_time) as bucket, addr, network FROM ("+
			"  SELECT caller as addr, block_time, network FROM calls WHERE block_time >= ?%s"+
			"  UNION SELECT creator, block_time, network FROM packages WHERE block_time >= ?%s"+
			"  UNION SELECT from_address, block_time, network FROM bank_sends WHERE block_time >= ?%s"+
			" )) GROUP BY bucket ORDER BY bucket ASC",
		sqlFmt, unionNetFilter, unionNetFilter, unionNetFilter)

	urows, err := d.db.Query(unionQ, startTime, startTime, startTime)
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

// AddressLabel is a display name for an address, derived from on-chain data.
type AddressLabel struct {
	Label string `json:"label"`
	Kind  string `json:"kind"`
	Why   string `json:"why"`
}

// namespaceLabelMinPackages is how many packages a deployer must have published
// under a namespace before that namespace names it. Below this the evidence is
// one or two deploys, which is as easily a guest as an owner.
const namespaceLabelMinPackages = 3

// namespaceLabelDominance is the share of a deployer's own packages that must sit
// under the namespace. Someone who publishes widely and happens to have three
// packages under a prefix is not that prefix's owner.
const namespaceLabelDominance = 0.6

var namespacePath = regexp.MustCompile(`^gno\.land/[rp]/([^/]+)/`)

// DerivedAddressLabels names addresses from what they have deployed.
//
// The signal is namespace ownership: an address that is the sole deployer of
// gno.land/r/gnoswap/* is gnoswap. Derived rather than hand-maintained, so it
// stays correct as namespaces appear, and it independently reproduces the
// hand-written entries that already existed — which is the check that the rule
// is the right one.
//
// Two guards keep it from naming the wrong address:
//
//   - A namespace with more than one deployer names nobody. Seven of them exist
//     on the live chains (onbloc has three), and picking one would be a guess
//     presented as a fact.
//   - A namespace that is itself an address (gno.land/r/g1abc.../foo) is skipped.
//     The prefix is the deployer, so it carries no name.
func (d *DB) DerivedAddressLabels(network string) (map[string]AddressLabel, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	rows, err := d.db.Query(`SELECT path, creator FROM packages WHERE ` +
		d.networkFilter("network", network))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	perCreator := map[string]map[string]int{} // creator -> namespace -> packages
	total := map[string]int{}                 // creator -> packages with any namespace
	deployers := map[string]map[string]bool{} // namespace -> creators

	for rows.Next() {
		var path, creator string
		if err := rows.Scan(&path, &creator); err != nil {
			return nil, err
		}
		m := namespacePath.FindStringSubmatch(path)
		if m == nil || strings.HasPrefix(m[1], "g1") {
			continue
		}
		ns := m[1]
		if perCreator[creator] == nil {
			perCreator[creator] = map[string]int{}
		}
		perCreator[creator][ns]++
		total[creator]++
		if deployers[ns] == nil {
			deployers[ns] = map[string]bool{}
		}
		deployers[ns][creator] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	labels := map[string]AddressLabel{}
	for creator, counts := range perCreator {
		best, n := "", 0
		for ns, c := range counts {
			if c > n {
				best, n = ns, c
			}
		}
		if best == "" || n < namespaceLabelMinPackages {
			continue
		}
		if len(deployers[best]) != 1 {
			continue
		}
		if float64(n)/float64(total[creator]) < namespaceLabelDominance {
			continue
		}
		labels[creator] = AddressLabel{
			Label: "@" + best,
			Kind:  "namespace",
			Why:   fmt.Sprintf("sole deployer of gno.land/*/%s/* (%d packages)", best, n),
		}
	}
	return labels, nil
}

// --- Watchlist ------------------------------------------------------------

// WatchedRealm is one realm's activity summary, for a watchlist digest.
type WatchedRealm struct {
	Network    string `json:"network"`
	Path       string `json:"path"`
	Exists     bool   `json:"exists"`
	Calls      int    `json:"calls"`
	Calls24h   int    `json:"calls_24h"`
	Importers  int    `json:"importers"`
	LastHeight int    `json:"last_height"`
	LastTime   string `json:"last_time,omitempty"`
	// NewSince counts activity above the height the caller last saw. Height
	// rather than a timestamp because it is exact and monotonic per chain,
	// where a wall-clock comparison drifts against block time.
	//
	// Zero when the caller supplied no baseline. Reporting the entire history as
	// "new" in that case would be technically defensible and useless.
	NewSince int `json:"new_since"`
}

// WatchedAddress is one address's activity summary.
type WatchedAddress struct {
	Network    string `json:"network"`
	Address    string `json:"address"`
	Calls      int    `json:"calls"`
	Deploys    int    `json:"deploys"`
	Sends      int    `json:"sends"`
	Received   int    `json:"received"`
	Calls24h   int    `json:"calls_24h"`
	LastHeight int    `json:"last_height"`
	LastTime   string `json:"last_time,omitempty"`
	NewSince   int    `json:"new_since"`
}

// WatchRequest is one item a caller is watching, with the height it last saw.
type WatchRequest struct {
	ID    string
	Since int
}

// WatchRealms summarises activity for a set of watched realms.
//
// Answered entirely from stored rows: a watchlist is checked often and by
// definition covers things the caller already cares about, so it must not cost
// an indexer round-trip per item.
func (d *DB) WatchRealms(network string, items []WatchRequest) ([]WatchedRealm, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	if len(items) == 0 {
		return []WatchedRealm{}, nil
	}
	since24h := time.Now().UTC().Add(-24 * time.Hour).Format(time.RFC3339)
	netFilter := d.networkFilter("network", network)

	out := make([]WatchedRealm, 0, len(items))
	for _, item := range items {
		w := WatchedRealm{Path: item.ID}

		// The realm itself. A watched path can exist on several chains; report
		// the one the current view is scoped to, or the newest deploy otherwise.
		err := d.db.QueryRow(`SELECT network FROM packages WHERE path = ? AND `+netFilter+
			` ORDER BY block_height DESC LIMIT 1`, item.ID).Scan(&w.Network)
		if err == nil {
			w.Exists = true
		} else if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}

		d.db.QueryRow(`SELECT COUNT(*) FROM calls WHERE pkg_path = ? AND `+netFilter,
			item.ID).Scan(&w.Calls)
		d.db.QueryRow(`SELECT COUNT(*) FROM calls WHERE pkg_path = ? AND block_time >= ? AND `+netFilter,
			item.ID, since24h).Scan(&w.Calls24h)
		d.db.QueryRow(`SELECT COUNT(*) FROM dependencies WHERE import_path = ? AND `+netFilter,
			item.ID).Scan(&w.Importers)

		var height sql.NullInt64
		var when sql.NullString
		d.db.QueryRow(`SELECT block_height, block_time FROM calls WHERE pkg_path = ? AND `+netFilter+
			` ORDER BY block_height DESC LIMIT 1`, item.ID).Scan(&height, &when)
		w.LastHeight, w.LastTime = int(height.Int64), when.String

		if item.Since > 0 {
			d.db.QueryRow(`SELECT COUNT(*) FROM calls WHERE pkg_path = ? AND block_height > ? AND `+netFilter,
				item.ID, item.Since).Scan(&w.NewSince)
		}
		out = append(out, w)
	}
	return out, nil
}

// WatchAddresses summarises activity for a set of watched addresses.
func (d *DB) WatchAddresses(network string, items []WatchRequest) ([]WatchedAddress, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	if len(items) == 0 {
		return []WatchedAddress{}, nil
	}
	since24h := time.Now().UTC().Add(-24 * time.Hour).Format(time.RFC3339)
	nf := d.networkFilter("network", network)

	out := make([]WatchedAddress, 0, len(items))
	for _, item := range items {
		w := WatchedAddress{Address: item.ID}

		d.db.QueryRow(`SELECT COUNT(*) FROM calls WHERE caller = ? AND `+nf, item.ID).Scan(&w.Calls)
		d.db.QueryRow(`SELECT COUNT(*) FROM packages WHERE creator = ? AND `+nf, item.ID).Scan(&w.Deploys)
		d.db.QueryRow(`SELECT COUNT(*) FROM bank_sends WHERE from_address = ? AND `+nf, item.ID).Scan(&w.Sends)
		d.db.QueryRow(`SELECT COUNT(*) FROM bank_sends WHERE to_address = ? AND `+nf, item.ID).Scan(&w.Received)
		d.db.QueryRow(`SELECT COUNT(*) FROM calls WHERE caller = ? AND block_time >= ? AND `+nf,
			item.ID, since24h).Scan(&w.Calls24h)

		// Newest activity across every table the address can appear in, so a
		// deploy-only or send-only account still reports a last-seen height.
		var height sql.NullInt64
		var when sql.NullString
		d.db.QueryRow(`SELECT block_height, block_time, network FROM (
			SELECT block_height, block_time, network FROM calls WHERE caller = ? AND `+nf+`
			UNION ALL SELECT block_height, block_time, network FROM packages WHERE creator = ? AND `+nf+`
			UNION ALL SELECT block_height, block_time, network FROM bank_sends WHERE from_address = ? AND `+nf+`
			UNION ALL SELECT block_height, block_time, network FROM bank_sends WHERE to_address = ? AND `+nf+`
		) ORDER BY block_height DESC LIMIT 1`,
			item.ID, item.ID, item.ID, item.ID).Scan(&height, &when, &w.Network)
		w.LastHeight, w.LastTime = int(height.Int64), when.String

		if item.Since > 0 {
			d.db.QueryRow(`SELECT COUNT(*) FROM (
				SELECT block_height FROM calls WHERE caller = ? AND block_height > ? AND `+nf+`
				UNION ALL SELECT block_height FROM packages WHERE creator = ? AND block_height > ? AND `+nf+`
				UNION ALL SELECT block_height FROM bank_sends WHERE from_address = ? AND block_height > ? AND `+nf+`
				UNION ALL SELECT block_height FROM bank_sends WHERE to_address = ? AND block_height > ? AND `+nf+`
			)`, item.ID, item.Since, item.ID, item.Since, item.ID, item.Since, item.ID, item.Since).Scan(&w.NewSince)
		}
		out = append(out, w)
	}
	return out, nil
}

// --- Filtered transaction listing ------------------------------------------

// StoredTx is one transaction as the list view needs it: enough to render a row
// without asking the indexer.
type StoredTx struct {
	Network     string `json:"network"`
	Hash        string `json:"hash"`
	BlockHeight int    `json:"block_height"`
	BlockTime   string `json:"block_time,omitempty"`
	Type        string `json:"type"`
	Detail      string `json:"detail,omitempty"`
	Caller      string `json:"caller,omitempty"`
	Success     bool   `json:"success"`
	// Gas figures live only on the transaction row, so they are joined in where
	// the view needs them and left zero where it does not.
	GasUsed int `json:"gas_used,omitempty"`
	GasFee  int `json:"gas_fee,omitempty"`
}

// txSource maps a message type to the table that records it, and to the columns
// that describe one of its rows.
//
// `success` is the awkward one. Three of these tables carry it; `packages` does
// not — it is keyed by (network, path) and holds the current state of a package
// rather than one row per deploy, so there is no per-attempt outcome to store.
// For that table the flag comes from the transaction row instead.
type txSource struct {
	table   string
	caller  string
	detail  string
	success string
}

var txSources = map[string]txSource{
	"MsgCall":       {table: "calls", caller: "caller", detail: "pkg_path || '::' || func_name", success: "e.success"},
	"MsgAddPackage": {table: "packages", caller: "creator", detail: "path", success: "COALESCE(t.success, 1)"},
	"MsgRun":        {table: "msg_runs", caller: "caller", detail: "''", success: "e.success"},
	"BankMsgSend":   {table: "bank_sends", caller: "from_address", detail: "to_address || ' ' || amount", success: "e.success"},
}

// FilteredTransactions lists transactions of one message type from storage.
//
// Filtering this at the indexer does not work. It has no index for message type,
// so a query for deploys walks the chain until it finds enough — and deploys are
// rare next to calls. Measured on sapphire: a 50-row page of MsgAddPackage took
// 12 seconds and exceeded the client deadline, while the same page unfiltered
// took 0.5s.
//
// The syncer already writes one row per message into a per-type table, each
// indexed by (network, block_height). That is exactly this query, and it pages
// properly: a real offset over an ordered index rather than a window that has to
// be re-walked to reach page two.
func (d *DB) FilteredTransactions(network, msgType string, success *bool, limit, offset int) ([]StoredTx, int, error) {
	src, ok := txSources[msgType]
	if !ok {
		return nil, 0, fmt.Errorf("unknown message type: %s", msgType)
	}
	d.mu.RLock()
	defer d.mu.RUnlock()

	// packages needs the transaction row for its success flag; the others carry
	// their own. LEFT JOIN so a package whose transaction has not been backfilled
	// still lists, defaulting to success rather than vanishing.
	join := ""
	if src.success != "e.success" {
		join = " LEFT JOIN transactions t ON t.network = e.network AND t.tx_hash = e.tx_hash"
	}

	where := " WHERE " + d.networkFilter("e.network", network)
	if success != nil {
		if *success {
			where += " AND " + src.success + " = 1"
		} else {
			where += " AND " + src.success + " = 0"
		}
	}

	var total int
	if err := d.db.QueryRow(`SELECT COUNT(*) FROM ` + src.table + ` e` + join + where).Scan(&total); err != nil {
		return nil, 0, err
	}

	rows, err := d.db.Query(`
		SELECT e.network, e.tx_hash, e.block_height, COALESCE(e.block_time, ''),
		       COALESCE(e.`+src.caller+`, ''), `+src.detail+`, `+src.success+`
		FROM `+src.table+` e`+join+where+`
		ORDER BY e.block_height DESC, e.tx_hash ASC
		LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	out := []StoredTx{}
	for rows.Next() {
		t := StoredTx{Type: msgType}
		if err := rows.Scan(&t.Network, &t.Hash, &t.BlockHeight, &t.BlockTime,
			&t.Caller, &t.Detail, &t.Success); err != nil {
			return nil, 0, err
		}
		out = append(out, t)
	}
	return out, total, rows.Err()
}

// govDAOPathPrefix is the realm the governance view is about.
//
// A prefix, not a substring: paths are `gno.land/r/gov/dao` and its versioned
// subpackages, and an anchored pattern can walk the (network, pkg_path) index
// instead of scanning. It also cannot pick up gnoswap's `gov/staker` and
// `gov/governance`, which a bare "gov" would.
const govDAOPathPrefix = "gno.land/r/gov/dao"

// GovDAOCalls lists governance calls from storage.
//
// Asking the indexer for these does not work on a chain that has none. The
// filter is a substring match over a field it has no index for, so it widens its
// window until the deadline and then fails — measured at 12s and a 500 on
// sapphire, which has no governance activity at all.
//
// Worse, it returned a *wrong* row on pearl: the predicate
// `MsgCall: { pkg_path: { like: ... } }` matched a message that is not a MsgCall
// and carries no pkg_path, so the governance view listed an `auth/create_session`
// transaction. Reading the calls table cannot do that — a row is there only
// because the syncer decoded a MsgCall with that path.
func (d *DB) GovDAOCalls(network string, limit int) ([]StoredTx, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	rows, err := d.db.Query(`
		SELECT network, tx_hash, block_height, COALESCE(block_time, ''),
		       caller, pkg_path || '::' || func_name, success
		FROM calls
		WHERE pkg_path LIKE ? AND `+d.networkFilter("network", network)+`
		ORDER BY block_height DESC, tx_hash ASC
		LIMIT ?`, govDAOPathPrefix+"%", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []StoredTx{}
	for rows.Next() {
		t := StoredTx{Type: "MsgCall"}
		if err := rows.Scan(&t.Network, &t.Hash, &t.BlockHeight, &t.BlockTime,
			&t.Caller, &t.Detail, &t.Success); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// AddressTransactions lists an address's activity from storage.
//
// The indexer cannot serve this at chain scale. The query is five address
// predicates over fields it has no index for, so it scans: windowing it from the
// tip (#121) bought time and the chain outgrew it — the busiest account on
// sapphire went back to a 500 at 13.9s as its history grew.
//
// Every message the syncer decodes is already written to a per-type table keyed
// by the address involved, and those are indexed. This is the same question with
// an index behind it, and it pages properly.
func (d *DB) AddressTransactions(network, addr string, limit, offset int) ([]StoredTx, int, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	nf := d.networkFilter("network", network)

	// Each branch is bounded before the union.
	//
	// Unbounded, the union has to be fully materialised and sorted before the
	// outer LIMIT can pick anything — 580,000 rows for the busiest account on
	// sapphire, measured at 3.5s. The global newest N must lie within each
	// branch's own newest N, so taking that many per branch first is exact, not
	// an approximation, and cuts it to 1.5s.
	//
	// SQLite requires each bounded branch to be wrapped in a subselect: ORDER BY
	// is not allowed directly on a UNION ALL arm.
	//
	// Measured against adding (network, address, block_height) indexes to all
	// four tables, which made it slightly *worse* — 1.70s — so they are not here.
	take := limit + offset

	branch := func(sel, table, cond string) string {
		return fmt.Sprintf("SELECT * FROM (SELECT %s FROM %s WHERE %s AND %s"+
			" ORDER BY block_height DESC LIMIT %d)", sel, table, cond, nf, take)
	}

	// One branch per way an address can appear. bank_sends matches both
	// directions because being paid is activity too.
	union := strings.Join([]string{
		branch(`network, tx_hash, block_height, COALESCE(block_time,'') bt, 'MsgCall' typ,
		        caller who, pkg_path || '::' || func_name detail, success`, "calls", "caller = ?"),
		branch(`network, tx_hash, block_height, COALESCE(block_time,''), 'MsgAddPackage',
		        creator, path, 1`, "packages", "creator = ?"),
		branch(`network, tx_hash, block_height, COALESCE(block_time,''), 'MsgRun',
		        caller, '', success`, "msg_runs", "caller = ?"),
		branch(`network, tx_hash, block_height, COALESCE(block_time,''), 'BankMsgSend',
		        from_address, to_address || ' ' || amount, success`, "bank_sends",
			"(from_address = ? OR to_address = ?)"),
	}, " UNION ALL ")

	// The count is over unbounded branches — a total that stopped at the page
	// size would not be a total. It is cheap: 0.155s for the same account,
	// because counting needs no sort.
	countUnion := strings.Join([]string{
		"SELECT tx_hash FROM calls WHERE caller = ? AND " + nf,
		"SELECT tx_hash FROM packages WHERE creator = ? AND " + nf,
		"SELECT tx_hash FROM msg_runs WHERE caller = ? AND " + nf,
		"SELECT tx_hash FROM bank_sends WHERE (from_address = ? OR to_address = ?) AND " + nf,
	}, " UNION ALL ")

	args := []any{addr, addr, addr, addr, addr}

	var total int
	if err := d.db.QueryRow(`SELECT COUNT(*) FROM (`+countUnion+`)`, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	// Gas comes from the transaction row. LEFT JOIN so an event whose
	// transaction has not been backfilled still lists, with zero gas rather
	// than disappearing.
	rows, err := d.db.Query(`
		SELECT e.network, e.tx_hash, e.block_height, e.bt, e.typ, e.who, e.detail, e.success,
		       COALESCE(t.gas_used, 0), COALESCE(t.gas_fee, 0)
		FROM (`+union+`) e
		LEFT JOIN transactions t ON t.network = e.network AND t.tx_hash = e.tx_hash
		ORDER BY e.block_height DESC, e.tx_hash ASC LIMIT ? OFFSET ?`,
		append(args, limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	out := []StoredTx{}
	for rows.Next() {
		var t StoredTx
		if err := rows.Scan(&t.Network, &t.Hash, &t.BlockHeight, &t.BlockTime,
			&t.Type, &t.Caller, &t.Detail, &t.Success, &t.GasUsed, &t.GasFee); err != nil {
			return nil, 0, err
		}
		out = append(out, t)
	}
	return out, total, rows.Err()
}

// --- Gas rollups ------------------------------------------------------------

// gasRollupKey records when the rollups were last recomputed, so the page can
// say how fresh its numbers are rather than presenting stale ones as live.
const gasRollupKey = "gas_rollup_at"

// gasRollupInterval is how often the rollups are rebuilt. These are all-time
// aggregates, so minutes of staleness is invisible to a reader — and the
// recompute takes the write lock, so doing it per sync pass would mean holding
// it every 30 seconds for the sake of numbers nobody watches change.
const gasRollupInterval = 5 * time.Minute

// RefreshGasRollups recomputes both gas aggregates for every configured network.
//
// This is the expensive query the gas page used to run per request — 3.4s for
// the realm attribution and 1.3s for the totals on sapphire, and both grow with
// the chain. Running it on a timer instead turns a 14-second page into a lookup.
//
// Whole-table replacement inside one transaction: readers see either the old
// rollup or the new one, never a half-written mixture.
func (d *DB) RefreshGasRollups() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	// A busy write lock loses the whole refresh, and the failure is quiet: the
	// read path falls back to computing live, so the page merely stays slow.
	// Retry rather than wait for the next tick.
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 2 * time.Second)
		}
		if lastErr = d.refreshGasRollups(); lastErr == nil {
			return nil
		}
	}
	return lastErr
}

func (d *DB) refreshGasRollups() error {

	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	scope := d.networkFilter("t.network", "")

	if _, err := tx.Exec(`DELETE FROM gas_realm_rollup`); err != nil {
		return err
	}
	if _, err := tx.Exec(`
		INSERT INTO gas_realm_rollup (network, path, gas_used, gas_fee, tx_count)
		SELECT network, path, SUM(gas_used), SUM(gas_fee), COUNT(*) FROM (
			SELECT t.network, c.pkg_path AS path, t.gas_used, t.gas_fee
			  FROM calls c JOIN transactions t
			    ON t.network = c.network AND t.tx_hash = c.tx_hash AND ` + scope + `
			UNION ALL
			SELECT t.network, p.path, t.gas_used, t.gas_fee
			  FROM packages p JOIN transactions t
			    ON t.network = p.network AND t.tx_hash = p.tx_hash AND ` + scope + `
			UNION ALL
			SELECT t.network, 'MsgRun by ' || m.caller, t.gas_used, t.gas_fee
			  FROM msg_runs m JOIN transactions t
			    ON t.network = m.network AND t.tx_hash = m.tx_hash AND ` + scope + `
		) GROUP BY network, path`); err != nil {
		return err
	}

	if _, err := tx.Exec(`DELETE FROM gas_totals_rollup`); err != nil {
		return err
	}
	if _, err := tx.Exec(`
		INSERT INTO gas_totals_rollup (network, tx_count, gas_used, gas_wanted, gas_fee, success_count)
		SELECT network, COUNT(*), COALESCE(SUM(gas_used),0), COALESCE(SUM(gas_wanted),0),
		       COALESCE(SUM(gas_fee),0), COALESCE(SUM(CASE WHEN success THEN 1 ELSE 0 END),0)
		FROM transactions t WHERE ` + scope + `
		GROUP BY network`); err != nil {
		return err
	}

	if _, err := tx.Exec(
		`INSERT INTO sync_state (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		gasRollupKey, time.Now().UTC().Format(time.RFC3339)); err != nil {
		return err
	}
	return tx.Commit()
}

// gasRollupReady reports whether the rollups hold anything, and when they were
// built. Callers hold the read lock.
//
// An empty rollup means the timer has not fired yet — a fresh database, or the
// first start after this shipped. The gas page falls back to computing live in
// that case rather than showing zeros, which would read as "this chain has used
// no gas".
func (d *DB) gasRollupReady() (bool, string) {
	var n int
	if err := d.db.QueryRow(`SELECT COUNT(*) FROM gas_totals_rollup`).Scan(&n); err != nil || n == 0 {
		return false, ""
	}
	var at string
	d.db.QueryRow(`SELECT value FROM sync_state WHERE key = ?`, gasRollupKey).Scan(&at)
	return true, at
}
