package main

import (
	"database/sql"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

type DB struct {
	db *sql.DB
	mu sync.RWMutex
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

	return &DB{db: db}, nil
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

		CREATE TABLE IF NOT EXISTS proposers (
			id      INTEGER PRIMARY KEY,
			network TEXT NOT NULL,
			address TEXT NOT NULL,
			UNIQUE (network, address)
		);

		CREATE TABLE IF NOT EXISTS blocks (
			network     TEXT NOT NULL DEFAULT 'gnoland1',
			height      INTEGER NOT NULL,
			time        TEXT NOT NULL,
			proposer_id INTEGER,
			num_txs     INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (network, height)
		) WITHOUT ROWID;

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
		CREATE INDEX IF NOT EXISTS idx_blocks_time ON blocks(network, time);
	`)
	return err
}

func (d *DB) Close() error {
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
	"blocks",
	"proposers",
}

// DeleteNetworkData removes every row belonging to a network, in one transaction.
// Used when a chain reset makes the stored history refer to blocks that no longer
// exist. Returns the number of rows removed.
//
// The blocks backfill flag in sync_state goes with them. Leaving it set would
// mark an empty blocks table as fully backfilled, so the coverage endpoint would
// report complete history for a network that has none — and the backfill would
// never run again to fix it. It is bookkeeping rather than data, so it is not
// counted in the returned row total.
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
	if _, err := tx.Exec(
		`DELETE FROM sync_state WHERE key = ?`, blocksBackfillDoneKey(network),
	); err != nil {
		return 0, fmt.Errorf("clear blocks backfill flag: %w", err)
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

	where, args := "", []any{}
	if network != "" {
		where = " WHERE network = ?"
		args = append(args, network)
	}

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
	realmWhere, realmArgs := "", []any{}
	if network != "" {
		realmWhere = " AND t.network = ?"
		realmArgs = append(realmArgs, network, network, network)
	}
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
	txRows, err := d.db.Query(`
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
		FROM transactions t`+where+`
		ORDER BY t.gas_used DESC LIMIT ?`, append(args, topN)...)
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
	nf := ""
	if network != "" {
		nf = " WHERE network = '" + strings.ReplaceAll(network, "'", "''") + "'"
	}
	d.db.QueryRow(`SELECT COUNT(*) FROM calls` + nf).Scan(&s.TotalCalls)
	d.db.QueryRow(`SELECT COUNT(*) FROM packages` + nf).Scan(&s.TotalDeploys)
	if network != "" {
		d.db.QueryRow(`SELECT COUNT(*) FROM packages WHERE is_realm = 1 AND network = ?`, network).Scan(&s.TotalRealms)
	} else {
		d.db.QueryRow(`SELECT COUNT(*) FROM packages WHERE is_realm = 1`).Scan(&s.TotalRealms)
	}
	s.TotalPackages = s.TotalDeploys - s.TotalRealms
	d.db.QueryRow(`SELECT COUNT(*) FROM msg_runs` + nf).Scan(&s.TotalMsgRuns)
	d.db.QueryRow(`SELECT COUNT(*) FROM bank_sends` + nf).Scan(&s.TotalSends)
	s.TotalTxs = s.TotalCalls + s.TotalDeploys + s.TotalMsgRuns + s.TotalSends
	if network != "" {
		d.db.QueryRow(`SELECT COUNT(DISTINCT caller) FROM calls WHERE network = ?`, network).Scan(&s.UniqueCallers)
		d.db.QueryRow(`SELECT COALESCE(MAX(block_height), 0) FROM packages WHERE network = ?`, network).Scan(&s.LatestBlock)
	} else {
		d.db.QueryRow(`SELECT COUNT(DISTINCT caller) FROM calls`).Scan(&s.UniqueCallers)
		d.db.QueryRow(`SELECT COALESCE(MAX(block_height), 0) FROM packages`).Scan(&s.LatestBlock)
	}
	return &s, nil
}

// Search searches across packages and callers.
func (d *DB) Search(network, q string) ([]PackageInfo, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	qStr := `
		SELECT network, path, name, creator, block_height, tx_hash, is_realm, num_files
		FROM packages
		WHERE (path LIKE ? OR name LIKE ? OR creator LIKE ?)`
	args := []any{"%" + q + "%", "%" + q + "%", "%" + q + "%"}
	if network != "" {
		qStr += ` AND network = ?`
		args = append(args, network)
	}
	qStr += ` ORDER BY block_height DESC LIMIT 20`

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
	Name      string `json:"name"`
	Creator   string `json:"creator"`
	CallCount int    `json:"call_count"`
}

func (d *DB) GetTokenPackages(network string) ([]TokenInfo, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	q := `
		SELECT DISTINCT p.path, p.name, p.creator, COALESCE(c.cnt, 0)
		FROM packages p
		JOIN dependencies dep ON dep.package_path = p.path AND dep.network = p.network
		LEFT JOIN (SELECT pkg_path, network, COUNT(*) as cnt FROM calls GROUP BY pkg_path, network) c ON c.pkg_path = p.path AND c.network = p.network
		WHERE dep.import_path LIKE '%grc20%'`
	args := []any{}
	if network != "" {
		q += ` AND p.network = ?`
		args = append(args, network)
	}
	q += ` ORDER BY p.block_height DESC`
	rows, err := d.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tokens []TokenInfo
	for rows.Next() {
		var t TokenInfo
		if err := rows.Scan(&t.Path, &t.Name, &t.Creator, &t.CallCount); err != nil {
			return nil, err
		}
		tokens = append(tokens, t)
	}
	return tokens, rows.Err()
}

type AccountInfo struct {
	Address     string `json:"address"`
	CallCount   int    `json:"call_count"`
	DeployCount int    `json:"deploy_count"`
	MsgRunCount int    `json:"msgrun_count"`
	SendCount   int    `json:"send_count"`
}

func (d *DB) GetActiveAccounts(network string) ([]AccountInfo, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	nFilter := ""
	if network != "" {
		// SQLite-safe string interpolation for CTEs
		safe := strings.ReplaceAll(network, "'", "''")
		nFilter = " WHERE network = '" + safe + "'"
	}

	q := `
		SELECT address, SUM(call_count), SUM(deploy_count), SUM(run_count), SUM(send_count)
		FROM (
			SELECT caller as address, COUNT(*) as call_count, 0 as deploy_count, 0 as run_count, 0 as send_count FROM calls` + nFilter + ` GROUP BY caller
			UNION ALL
			SELECT creator as address, 0, COUNT(*), 0, 0 FROM packages` + nFilter + ` GROUP BY creator
			UNION ALL
			SELECT caller as address, 0, 0, COUNT(*), 0 FROM msg_runs` + nFilter + ` GROUP BY caller
			UNION ALL
			SELECT from_address as address, 0, 0, 0, COUNT(*) FROM bank_sends` + nFilter + ` GROUP BY from_address
			UNION ALL
			SELECT to_address as address, 0, 0, 0, COUNT(*) FROM bank_sends` + nFilter + ` GROUP BY to_address
		)
		GROUP BY address
		ORDER BY (SUM(call_count) + SUM(deploy_count) + SUM(run_count) + SUM(send_count)) DESC
		LIMIT 100
	`
	rows, err := d.db.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var accounts []AccountInfo
	for rows.Next() {
		var a AccountInfo
		if err := rows.Scan(&a.Address, &a.CallCount, &a.DeployCount, &a.MsgRunCount, &a.SendCount); err != nil {
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
}

const amountExpr = `COALESCE(SUM(CAST(REPLACE(REPLACE(amount, 'ugnot', ''), '"', '') AS INTEGER)), 0)`

func (d *DB) GetBankStats(network string) (*BankStats, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	nFilter := ""
	if network != "" {
		safe := strings.ReplaceAll(network, "'", "''")
		nFilter = " WHERE network = '" + safe + "'"
	}

	var s BankStats
	d.db.QueryRow(`SELECT COUNT(*) FROM bank_sends` + nFilter).Scan(&s.TotalSends)
	d.db.QueryRow(`SELECT COUNT(DISTINCT from_address) FROM bank_sends` + nFilter).Scan(&s.UniqueSenders)
	d.db.QueryRow(`SELECT COUNT(DISTINCT to_address) FROM bank_sends` + nFilter).Scan(&s.UniqueReceivers)
	d.db.QueryRow(`SELECT ` + amountExpr + ` FROM bank_sends` + nFilter).Scan(&s.TotalVolume)

	andFilter := ""
	if network != "" {
		safe := strings.ReplaceAll(network, "'", "''")
		andFilter = " AND network = '" + safe + "'"
	}
	d.db.QueryRow(`SELECT COUNT(DISTINCT addr) FROM (SELECT from_address as addr FROM bank_sends` + nFilter + ` UNION SELECT to_address FROM bank_sends` + nFilter + `)`).Scan(&s.UniqueAddresses)

	s.TopSenders = d.queryAddrStats(`SELECT from_address, COUNT(*), ` + amountExpr + ` FROM bank_sends` + nFilter + ` GROUP BY from_address ORDER BY COUNT(*) DESC LIMIT 10`)
	s.TopReceiversVol = d.queryAddrStats(`SELECT to_address, COUNT(*), ` + amountExpr + ` FROM bank_sends` + nFilter + ` GROUP BY to_address ORDER BY ` + amountExpr + ` DESC LIMIT 10`)
	s.TopReceiversCnt = d.queryAddrStats(`SELECT to_address, COUNT(*), ` + amountExpr + ` FROM bank_sends` + nFilter + ` GROUP BY to_address ORDER BY COUNT(*) DESC LIMIT 10`)

	_ = andFilter
	return &s, nil
}

type RealmActivity struct {
	Path       string `json:"path"`
	Calls      int    `json:"calls"`
	Callers    int    `json:"callers"`
	Dependents int    `json:"dependents"`
	IsRealm    bool   `json:"is_realm"`
}

type CallerActivity struct {
	Address string `json:"address"`
	Calls   int    `json:"calls"`
	Realms  int    `json:"realms"`
}

type ImportRank struct {
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

	nFilter := ""
	pFilter := ""
	if network != "" {
		safe := strings.ReplaceAll(network, "'", "''")
		nFilter = " WHERE network = '" + safe + "'"
		pFilter = " AND p.network = '" + safe + "'"
	}

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

	addrUnionFilter := ""
	if network != "" {
		safe := strings.ReplaceAll(network, "'", "''")
		addrUnionFilter = " WHERE network = '" + safe + "'"
	}
	d.db.QueryRow(`SELECT COUNT(DISTINCT addr) FROM (
		SELECT caller as addr FROM calls` + addrUnionFilter + ` UNION SELECT creator FROM packages` + addrUnionFilter + `
		UNION SELECT caller FROM msg_runs` + addrUnionFilter + ` UNION SELECT from_address FROM bank_sends` + addrUnionFilter + `
		UNION SELECT to_address FROM bank_sends` + addrUnionFilter + `
	)`).Scan(&a.TotalAddresses)
	if network != "" {
		d.db.QueryRow(`SELECT COALESCE(SUM(LENGTH(body)), 0) / 1024 FROM package_files WHERE network = ?`, network).Scan(&a.TotalSourceKB)
	} else {
		d.db.QueryRow(`SELECT COALESCE(SUM(LENGTH(body)), 0) / 1024 FROM package_files`).Scan(&a.TotalSourceKB)
	}

	callJoinFilter := ""
	depJoinFilter := ""
	if network != "" {
		safe := strings.ReplaceAll(network, "'", "''")
		callJoinFilter = " AND c_inner.network = '" + safe + "'"
		depJoinFilter = " AND dep_inner.network = '" + safe + "'"
	}

	// Top realms by calls
	rows, _ := d.db.Query(`
		SELECT p.path, COALESCE(c.cnt, 0), COALESCE(c.callers, 0), COALESCE(dep.cnt, 0), p.is_realm
		FROM packages p
		LEFT JOIN (SELECT pkg_path, COUNT(*) as cnt, COUNT(DISTINCT caller) as callers FROM calls GROUP BY pkg_path) c ON c.pkg_path = p.path
		LEFT JOIN (SELECT import_path, COUNT(*) as cnt FROM dependencies GROUP BY import_path) dep ON dep.import_path = p.path
		WHERE p.is_realm = 1` + pFilter + `
		ORDER BY COALESCE(c.cnt, 0) DESC LIMIT 15
	`)
	if rows != nil {
		defer rows.Close()
		for rows.Next() {
			var r RealmActivity
			rows.Scan(&r.Path, &r.Calls, &r.Callers, &r.Dependents, &r.IsRealm)
			a.TopRealms = append(a.TopRealms, r)
		}
	}

	// Top packages by imports (dependents)
	rows2, _ := d.db.Query(`
		SELECT p.path, COALESCE(c.cnt, 0), 0, COALESCE(dep.cnt, 0), p.is_realm
		FROM packages p
		LEFT JOIN (SELECT pkg_path, COUNT(*) as cnt FROM calls GROUP BY pkg_path) c ON c.pkg_path = p.path
		LEFT JOIN (SELECT import_path, COUNT(*) as cnt FROM dependencies GROUP BY import_path) dep ON dep.import_path = p.path
		WHERE p.is_realm = 0` + pFilter + `
		ORDER BY COALESCE(dep.cnt, 0) DESC LIMIT 15
	`)
	if rows2 != nil {
		defer rows2.Close()
		for rows2.Next() {
			var r RealmActivity
			rows2.Scan(&r.Path, &r.Calls, &r.Callers, &r.Dependents, &r.IsRealm)
			a.TopPackages = append(a.TopPackages, r)
		}
	}

	// Top callers
	callersQ := `SELECT caller, COUNT(*) as c, COUNT(DISTINCT pkg_path) as realms FROM calls` + nFilter + ` GROUP BY caller ORDER BY c DESC LIMIT 15`
	rows3, _ := d.db.Query(callersQ)
	if rows3 != nil {
		defer rows3.Close()
		for rows3.Next() {
			var c CallerActivity
			rows3.Scan(&c.Address, &c.Calls, &c.Realms)
			a.TopCallers = append(a.TopCallers, c)
		}
	}

	// Top imports
	importsQ := `SELECT import_path, COUNT(*) as c FROM dependencies WHERE import_path LIKE 'gno.land/%'`
	if network != "" {
		importsQ += ` AND network = '` + strings.ReplaceAll(network, "'", "''") + `'`
	}
	importsQ += ` GROUP BY import_path ORDER BY c DESC LIMIT 15`
	rows4, _ := d.db.Query(importsQ)
	if rows4 != nil {
		defer rows4.Close()
		for rows4.Next() {
			var i ImportRank
			rows4.Scan(&i.Path, &i.Imports)
			a.TopImports = append(a.TopImports, i)
		}
	}

	// Top deployers
	deployQ := `SELECT creator, COUNT(*) as c, 0 FROM packages` + nFilter + ` GROUP BY creator ORDER BY c DESC LIMIT 15`
	rows5, _ := d.db.Query(deployQ)
	if rows5 != nil {
		defer rows5.Close()
		for rows5.Next() {
			var c CallerActivity
			rows5.Scan(&c.Address, &c.Calls, &c.Realms)
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

	_ = callJoinFilter
	_ = depJoinFilter

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
		if err := rows.Scan(&s.Address, &s.Count, &s.Total); err != nil {
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
// for hourly/daily/weekly/monthly granularity.
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
	case "monthly":
		// The loop in fillBuckets truncates cur.Add(step) with truncFn, which
		// re-truncates to the 1st of the month — so the step only needs to
		// push cur into a later month, not span a fixed number of days. The
		// 1st of any month plus 31 days always lands in a later month (Jan 1
		// -> Feb 1; the shortest case, Feb 1 -> Mar 4), so exactly 31*24h
		// suffices even though it equals rather than exceeds the longest
		// month. This guards a loop inside a request handler against never
		// advancing, so keep the invariant intact if this changes.
		return "%Y-%m", 31 * 24 * time.Hour, func(t time.Time) time.Time {
			return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
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
	case "monthly":
		return t.UTC().Format("2006-01")
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
}

type SanityOverview struct {
	Network            string  `json:"network"`
	ChainHeight        int     `json:"chain_height"`
	LastBlockTime      string  `json:"last_block_time"`
	SecondsSinceBlock  int     `json:"seconds_since_block"`
	IsAlive            bool    `json:"is_alive"`
	TxLast1h           int     `json:"tx_last_1h"`
	TxLast24h          int     `json:"tx_last_24h"`
	SuccessRate24h     float64 `json:"success_rate_24h"`
	GasEfficiency24h   float64 `json:"gas_efficiency_24h"`
	ActiveAddresses24h int     `json:"active_addresses_24h"`
	NewPackages7d      int     `json:"new_packages_7d"`
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

	netFilter := ""
	if network != "" {
		netFilter = " AND t.network = ?"
	}

	q := fmt.Sprintf(
		"SELECT strftime('%s', t.block_time) as bucket,"+
			" SUM(t.gas_used) as total_gas_used,"+
			" SUM(t.gas_wanted) as total_gas_wanted,"+
			" SUM(t.gas_fee) as total_fees,"+
			" COUNT(*) as tx_count"+
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

	type row struct {
		bucket         string
		totalGasUsed   int
		totalGasWanted int
		totalFees      int
		txCount        int
	}
	buckets := make(map[string]*row)
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.bucket, &r.totalGasUsed, &r.totalGasWanted, &r.totalFees, &r.txCount); err != nil {
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
	var out []GasTimePoint
	for cur := start; !cur.After(end); cur = truncFn(cur.Add(step)) {
		k := bucketKey(cur, granularity)
		if r, ok := buckets[k]; ok {
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

	// Union total active addresses per bucket. Sources calls / packages /
	// msg_runs / bank_sends — the same four tables activityMsgTables lists for
	// the activity heatmap — so the two endpoints report the same total for the
	// same network and window. Keep this list in sync with activityMsgTables by
	// hand; nothing enforces the agreement automatically.
	var unionNetFilter string
	var unionArgs []any
	if network != "" {
		unionNetFilter = " AND network = ?"
		unionArgs = []any{startTime, network, startTime, network, startTime, network, startTime, network}
	} else {
		unionArgs = []any{startTime, startTime, startTime, startTime}
	}
	unionQ := fmt.Sprintf(
		"SELECT strftime('%s', block_time) as bucket, COUNT(DISTINCT addr) as cnt FROM ("+
			" SELECT caller as addr, block_time, network FROM calls WHERE block_time >= ?%s"+
			" UNION SELECT creator, block_time, network FROM packages WHERE block_time >= ?%s"+
			" UNION SELECT caller, block_time, network FROM msg_runs WHERE block_time >= ?%s"+
			" UNION SELECT from_address, block_time, network FROM bank_sends WHERE block_time >= ?%s"+
			") GROUP BY bucket ORDER BY bucket ASC",
		sqlFmt, unionNetFilter, unionNetFilter, unionNetFilter, unionNetFilter)

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

// blockTimeSources are the tables recording a chain timestamp, with the column
// that holds it. NetworkDataStart takes the minimum across all of them.
var blockTimeSources = []struct{ table, col string }{
	{"calls", "block_time"},
	{"packages", "block_time"},
	{"msg_runs", "block_time"},
	{"bank_sends", "block_time"},
	{"transactions", "block_time"},
	{"blocks", "time"},
}

// NetworkDataStart returns the earliest chain time this network has data for,
// across every table that records one. ok is false when nothing is indexed.
//
// The "all" window needs this. Without it the window must guess a range and a
// bucket size, and any fixed guess is wrong for a chain younger than the
// bucket — which is every gno chain that currently exists.
//
// The minimum spans tables rather than reading one: a network's earliest datum
// can be a package deploy while its latest is a call, so a single-table MIN
// would report a start later than the real one.
func (d *DB) NetworkDataStart(network string) (time.Time, bool, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	parts := make([]string, 0, len(blockTimeSources))
	args := make([]any, 0, len(blockTimeSources))
	for _, s := range blockTimeSources {
		// An empty network means every network, matching networkParam and the
		// conditional filters the other aggregate readers use.
		filter := ""
		if network != "" {
			filter = fmt.Sprintf(" AND %s.network = ?", s.table)
			args = append(args, network)
		}
		// Table and column names come from the constant above, never from input.
		parts = append(parts, fmt.Sprintf(
			"SELECT MIN(%[1]s.%[2]s) AS t FROM %[1]s WHERE %[1]s.%[2]s IS NOT NULL AND %[1]s.%[2]s != ''%[3]s",
			s.table, s.col, filter))
	}

	var earliest sql.NullString
	q := "SELECT MIN(t) FROM (" + strings.Join(parts, " UNION ALL ") + ")"
	if err := d.db.QueryRow(q, args...).Scan(&earliest); err != nil {
		return time.Time{}, false, err
	}
	if !earliest.Valid || earliest.String == "" {
		return time.Time{}, false, nil
	}
	ts, err := time.Parse(time.RFC3339, earliest.String)
	if err != nil {
		// Errors go up, not into logs. AGENTS.md singles out query-path readers
		// that swallow an error and return a zero value as "a known bug, not a
		// style to follow" — logging and returning ok=false was exactly that.
		// The caller already treats a non-nil error the same as "no span", so
		// propagating costs nothing and keeps the failure visible.
		return time.Time{}, false, fmt.Errorf("unparseable block_time %q: %w", earliest.String, err)
	}
	return ts, true, nil
}

// --- blocks ---

// InternProposer maps a proposer address to a small integer id, creating the
// row on first sight. Storing the id rather than the 40-byte address on every
// block row saves roughly 119MB across a 3.3M-block chain.
//
// The unique key is (network, address), so the same validator address on two
// chains gets two ids and an id can never span networks.
func (d *DB) InternProposer(network, address string) (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if _, err := d.db.Exec(
		`INSERT INTO proposers (network, address) VALUES (?, ?) ON CONFLICT DO NOTHING`,
		network, address,
	); err != nil {
		return 0, err
	}
	var id int64
	err := d.db.QueryRow(
		`SELECT id FROM proposers WHERE network = ? AND address = ?`, network, address,
	).Scan(&id)
	return id, err
}

type BlockRow struct {
	Height     int
	Time       string
	ProposerID int64
	NumTxs     int
}

// UpsertBlocks writes many blocks under a single lock and a single SQLite
// transaction, mirroring UpsertTransactions.
//
// This is not an optimisation. The comment on UpsertTransactions records that
// writing rows individually made read requests queue behind a backfill of a
// hundred rows; a block page is 5,000 rows and the full backfill is 3.3M, so
// per-row writes here would stall the API for the entire backfill.
//
// Idempotent on (network, height), so a re-synced page is a no-op.
func (d *DB) UpsertBlocks(network string, rows []BlockRow) error {
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
		INSERT INTO blocks (network, height, time, proposer_id, num_txs)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (network, height) DO UPDATE SET
			time = excluded.time,
			proposer_id = excluded.proposer_id,
			num_txs = excluded.num_txs`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, r := range rows {
		if _, err := stmt.Exec(network, r.Height, r.Time, r.ProposerID, r.NumTxs); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// UpsertBlock stores a single block. Convenience wrapper over UpsertBlocks for
// tests and one-off writes.
func (d *DB) UpsertBlock(network string, height int, blockTime string, proposerID int64, numTxs int) error {
	return d.UpsertBlocks(network, []BlockRow{{
		Height: height, Time: blockTime, ProposerID: proposerID, NumTxs: numTxs,
	}})
}

// BlockHeightBounds returns the lowest and highest stored height for a network.
// The syncer derives both its cursors from these rather than from separate
// state; ok is false when the network has no blocks yet.
func (d *DB) BlockHeightBounds(network string) (minH, maxH int, ok bool, err error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var lo, hi sql.NullInt64
	err = d.db.QueryRow(
		`SELECT MIN(height), MAX(height) FROM blocks WHERE network = ?`, network,
	).Scan(&lo, &hi)
	if err != nil {
		return 0, 0, false, err
	}
	if !lo.Valid || !hi.Valid {
		return 0, 0, false, nil
	}
	return int(lo.Int64), int(hi.Int64), true, nil
}

type BlockTimePoint struct {
	Time   string `json:"time"`
	Blocks int    `json:"blocks"`
	Txs    int    `json:"txs"`
}

type BlockTimeBin struct {
	Bin    string `json:"bin"`
	Blocks int    `json:"blocks"`
}

type ProposerCount struct {
	Address string `json:"address"`
	Blocks  int    `json:"blocks"`
}

type BlockCoverage struct {
	MinTime  string `json:"min_time"`
	MaxTime  string `json:"max_time"`
	Complete bool   `json:"complete"`
}

// blockTimeBinExpr bins a block-time delta (seconds) into the ranges below.
// Edges come from measurement against gnoland1: median 4.34s, observed
// 3.69-10.11s, so the resolution is concentrated where the mass actually is.
// Lower edges are inclusive.
const blockTimeBinExpr = `CASE
	WHEN d <  4.0 THEN '<4.0'
	WHEN d <  4.5 THEN '4.0-4.5'
	WHEN d <  5.0 THEN '4.5-5.0'
	WHEN d <  5.5 THEN '5.0-5.5'
	WHEN d <  6.0 THEN '5.5-6.0'
	WHEN d <  7.0 THEN '6.0-7.0'
	WHEN d <  8.0 THEN '7.0-8.0'
	WHEN d < 10.0 THEN '8.0-10.0'
	ELSE '>=10.0'
END`

// BlockTimeBinOrder is the display order of the histogram's bins.
var BlockTimeBinOrder = []string{
	"<4.0", "4.0-4.5", "4.5-5.0", "5.0-5.5", "5.5-6.0", "6.0-7.0", "7.0-8.0", "8.0-10.0", ">=10.0",
}

// GetBlockTimeSeries returns blocks and transactions per bucket.
func (d *DB) GetBlockTimeSeries(network, granularity string, days int) ([]BlockTimePoint, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	sqlFmt, step, truncFn := timeseriesFormat(granularity)
	start := time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339)

	q := fmt.Sprintf(
		`SELECT strftime('%s', time) AS bucket, COUNT(*), COALESCE(SUM(num_txs), 0)
		 FROM blocks WHERE network = ? AND time >= ?
		 GROUP BY bucket ORDER BY bucket ASC`, sqlFmt)

	rows, err := d.db.Query(q, network, start)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	buckets := make(map[string]*BlockTimePoint)
	for rows.Next() {
		var p BlockTimePoint
		if err := rows.Scan(&p.Time, &p.Blocks, &p.Txs); err != nil {
			return nil, err
		}
		cp := p
		buckets[p.Time] = &cp
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return fillBuckets(buckets, days, granularity, step, truncFn,
		func(k string) BlockTimePoint { return BlockTimePoint{Time: k} },
		func(p *BlockTimePoint) {}), nil
}

// GetBlockTimeHistogram bins the interval between consecutive blocks.
//
// Deltas are computed at query time with LAG rather than stored per block: it
// needs no extra column and, crucially, no handling for the page boundaries the
// syncer fetches across, where an ingest-time computation would have no
// predecessor to subtract from. The window's first block has a NULL delta and
// is excluded.
func (d *DB) GetBlockTimeHistogram(network string, days int) ([]BlockTimeBin, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	start := time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339)

	// The delta is rounded to milliseconds: julianday's double-precision julian
	// day number is large enough (~2.46M) that subtracting two close values
	// loses a few microseconds to floating-point cancellation, which is enough
	// to flip a delta landing exactly on a bin edge (e.g. 4.500000 computed as
	// 4.499996). Real block-time gaps are never meaningfully precise below a
	// millisecond, so rounding away that noise costs nothing.
	q := fmt.Sprintf(`
		WITH deltas AS (
			SELECT ROUND((julianday(time) - julianday(LAG(time) OVER (ORDER BY height))) * 86400.0, 3) AS d
			FROM blocks WHERE network = ? AND time >= ?
		)
		SELECT %s AS bin, COUNT(*) FROM deltas WHERE d IS NOT NULL GROUP BY bin`, blockTimeBinExpr)

	rows, err := d.db.Query(q, network, start)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	counts := make(map[string]int)
	for rows.Next() {
		var bin string
		var n int
		if err := rows.Scan(&bin, &n); err != nil {
			return nil, err
		}
		counts[bin] = n
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]BlockTimeBin, 0, len(BlockTimeBinOrder))
	for _, bin := range BlockTimeBinOrder {
		out = append(out, BlockTimeBin{Bin: bin, Blocks: counts[bin]})
	}
	return out, nil
}

// GetBlockProposers counts blocks proposed per validator in the window.
// Addresses only — moniker resolution lives in the frontend's _valMonikers.
func (d *DB) GetBlockProposers(network string, days, topN int) ([]ProposerCount, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	if topN <= 0 {
		topN = 25
	}
	start := time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339)

	// p.network is filtered as well as b.network: proposer ids are already
	// network-scoped by construction, but relying on that would make this join
	// silently wrong if the intern key ever changed.
	rows, err := d.db.Query(`
		SELECT p.address, COUNT(*) AS n
		FROM blocks b JOIN proposers p ON p.id = b.proposer_id
		WHERE b.network = ? AND p.network = ? AND b.time >= ?
		GROUP BY p.address ORDER BY n DESC LIMIT ?`,
		network, network, start, topN)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ProposerCount
	for rows.Next() {
		var c ProposerCount
		if err := rows.Scan(&c.Address, &c.Blocks); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// OldestBlockTime is the chain time of the oldest stored block for a network.
// ok is false when the network has no blocks yet.
//
// The backfill's history cap needs this: the cap is expressed in days but the
// backfill cursor is a height, and there is no fixed blocks-per-day rate to
// convert between them.
func (d *DB) OldestBlockTime(network string) (time.Time, bool, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var oldest sql.NullString
	err := d.db.QueryRow(`SELECT MIN(time) FROM blocks WHERE network = ?`, network).Scan(&oldest)
	if err != nil {
		return time.Time{}, false, err
	}
	if !oldest.Valid || oldest.String == "" {
		return time.Time{}, false, nil
	}
	ts, err := time.Parse(time.RFC3339, oldest.String)
	if err != nil {
		return time.Time{}, false, nil
	}
	return ts, true, nil
}

// GetBlockCoverage reports the stored block range and whether backfill finished.
//
// Complete comes from the syncer's flag, not from MIN(height) <= 1: an indexer
// that prunes early history never yields height 1, and an inferred version would
// report incomplete forever.
func (d *DB) GetBlockCoverage(network string) (BlockCoverage, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var cov BlockCoverage
	var lo, hi sql.NullString
	err := d.db.QueryRow(
		`SELECT MIN(time), MAX(time) FROM blocks WHERE network = ?`, network,
	).Scan(&lo, &hi)
	if err != nil {
		return cov, err
	}
	cov.MinTime, cov.MaxTime = lo.String, hi.String

	var done string
	err = d.db.QueryRow(
		`SELECT value FROM sync_state WHERE key = ?`, blocksBackfillDoneKey(network),
	).Scan(&done)
	if err != nil && err != sql.ErrNoRows {
		return cov, err
	}
	cov.Complete = done == "1"
	return cov, nil
}

// --- batch 2b: activity rhythm, acquisition, distributions ---
//
// Nothing here adds a table. Every query below reads `block_time`, already
// denormalized onto calls / packages / msg_runs / bank_sends / transactions.
//
// Vocabulary, settled here per the design doc's §9 open question: these four
// tables are per-*message*, so anything counting rows from them is counting
// messages, not transactions. Only `transactions` counts transactions, and the
// gas histogram below is the one reader of it. Axis labels say which.
//
// "Active address", also per §9: an address that *authored* a message — a
// caller, a deployer, or a bank-send sender. Bank-send **receivers do not
// count** (receiving is passive; an airdrop would otherwise manufacture
// thousands of "active users"), and **failed messages do count** (a failed call
// still proves key custody and still burned gas). Batch 1's
// GetActiveAddressTimeSeries deliberately sources the same four tables so the
// two endpoints report the same total; that agreement is maintained by hand
// across both call sites, not automatic, so keep them in sync if this list
// ever changes.

// activityMsgTables are the per-message tables, with the column naming the
// address that authored the message. Used by the activity heatmap and by
// first-seen derivation, so both cover exactly the same notion of "activity".
var activityMsgTables = []struct{ table, addrCol string }{
	{"calls", "caller"},
	{"packages", "creator"},
	{"msg_runs", "caller"},
	{"bank_sends", "from_address"},
}

// ActivityCell is one cell of the hour x day-of-week grid.
//
// Dow is 0=Monday..6=Sunday, not SQLite's %w (0=Sunday). Rotating server-side
// keeps the weekend adjacent at the end of the axis, which is the whole point
// of the chart, and keeps the frontend from re-deriving a convention.
type ActivityCell struct {
	Hour     int `json:"hour"`
	Dow      int `json:"dow"`
	Messages int `json:"messages"`
}

// GetActivityHeatmap counts messages per (hour-of-day, day-of-week) in UTC.
//
// Mode B: the window filters which messages are counted, but the output is
// always the full 24x7 grid, zero-filled. Empty cells are a real zero — "no
// messages at 03:00 on a Sunday" is the finding, not missing data — so per the
// design doc's §10.1 table this is a count series and empty means 0.
//
// The window is snapped down to a whole number of weeks (floor 7 days) before
// filtering: a window that is not a multiple of 7 gives some weekday columns
// one more occurrence than others (a 90-day window is 12.857 weeks), which
// systematically inflates whichever columns fall on the long side of the
// split — in a chart whose entire point is comparing those columns against
// each other.
func (d *DB) GetActivityHeatmap(network string, days int) ([]ActivityCell, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	weeks := days / 7
	if weeks < 1 {
		weeks = 1
	}
	start := time.Now().UTC().AddDate(0, 0, -weeks*7).Format(time.RFC3339)

	netFilter := ""
	if network != "" {
		netFilter = " AND t.network = ?"
	}
	parts := make([]string, 0, len(activityMsgTables))
	args := make([]any, 0, 2*len(activityMsgTables))
	for _, s := range activityMsgTables {
		// Table names come from the constant above, never from input.
		parts = append(parts, fmt.Sprintf(
			"SELECT strftime('%%H', t.block_time) AS h, strftime('%%w', t.block_time) AS w, COUNT(*) AS c"+
				" FROM %s t WHERE t.block_time >= ?%s GROUP BY h, w", s.table, netFilter))
		args = append(args, start)
		if network != "" {
			args = append(args, network)
		}
	}
	q := "SELECT h, w, SUM(c) FROM (" + strings.Join(parts, " UNION ALL ") + ") GROUP BY h, w"

	rows, err := d.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	counts := make(map[[2]int]int)
	for rows.Next() {
		// block_time is nullable TEXT and the window predicate above is a string
		// comparison, so a garbage value (e.g. "not-a-timestamp") can pass it and
		// make strftime() yield NULL. Scanning into sql.NullString rather than
		// string lets that row's contribution be skipped as a data-quality issue
		// instead of failing the whole heatmap.
		var hs, ws sql.NullString
		var c int
		if err := rows.Scan(&hs, &ws, &c); err != nil {
			return nil, err
		}
		if !hs.Valid || !ws.Valid {
			continue // an unparseable block_time cannot be placed on the grid
		}
		h, err := strconv.Atoi(hs.String)
		if err != nil {
			continue // an unparseable block_time cannot be placed on the grid
		}
		w, err := strconv.Atoi(ws.String)
		if err != nil {
			continue
		}
		counts[[2]int{h, (w + 6) % 7}] += c
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]ActivityCell, 0, 24*7)
	for dow := range 7 {
		for hour := range 24 {
			out = append(out, ActivityCell{Hour: hour, Dow: dow, Messages: counts[[2]int{hour, dow}]})
		}
	}
	return out, nil
}

// NewAddressPoint counts addresses seen on-chain for the first time in a bucket.
type NewAddressPoint struct {
	Time         string `json:"time"`
	NewAddresses int    `json:"new_addresses"`
}

// GetNewAddressTimeSeries buckets addresses by their first-ever appearance.
//
// First-seen is computed over *all* history and only then filtered to the
// window: deriving it from rows inside the window instead would relabel every
// long-standing address as "new" the moment the window moved, which is the
// difference between acquisition and plain activity.
//
// Empty bucket is 0 — a count series per §10.1.
func (d *DB) GetNewAddressTimeSeries(network, granularity string, days int) ([]NewAddressPoint, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	sqlFmt, step, truncFn := timeseriesFormat(granularity)
	start := time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339)

	netFilter := ""
	if network != "" {
		netFilter = " AND t.network = ?"
	}
	parts := make([]string, 0, len(activityMsgTables))
	args := make([]any, 0, len(activityMsgTables)+1)
	for _, s := range activityMsgTables {
		// Column and table names come from the constant above, never from input.
		parts = append(parts, fmt.Sprintf(
			"SELECT t.%s AS addr, t.block_time AS bt FROM %s t"+
				" WHERE t.block_time IS NOT NULL AND t.block_time != ''%s",
			s.addrCol, s.table, netFilter))
		if network != "" {
			args = append(args, network)
		}
	}
	args = append(args, start)
	q := fmt.Sprintf(
		"SELECT strftime('%s', first_seen) AS bucket, COUNT(*) FROM ("+
			" SELECT addr, MIN(bt) AS first_seen FROM (%s) GROUP BY addr"+
			") WHERE first_seen >= ? GROUP BY bucket ORDER BY bucket ASC",
		sqlFmt, strings.Join(parts, " UNION ALL "))

	rows, err := d.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	buckets := make(map[string]*NewAddressPoint)
	for rows.Next() {
		// The WHERE first_seen >= ? predicate above is a string comparison against
		// nullable TEXT, so a garbage block_time (e.g. "not-a-timestamp") can pass
		// it and make strftime() yield a NULL bucket. Scanning into sql.NullString
		// lets that row be skipped as a data-quality issue rather than failing the
		// whole series.
		var bucket sql.NullString
		var n int
		if err := rows.Scan(&bucket, &n); err != nil {
			return nil, err
		}
		if !bucket.Valid {
			continue
		}
		p := NewAddressPoint{Time: bucket.String, NewAddresses: n}
		buckets[p.Time] = &p
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return fillBuckets(buckets, days, granularity, step, truncFn,
		func(k string) NewAddressPoint { return NewAddressPoint{Time: k} },
		func(p *NewAddressPoint) {}), nil
}

// RollingActivePoint holds one day's active-address counts over three trailing
// windows. Ratios (DAU/MAU stickiness) are left to the caller.
type RollingActivePoint struct {
	Time string `json:"time"`
	DAU  int    `json:"dau"`
	WAU  int    `json:"wau"`
	MAU  int    `json:"mau"`
}

const (
	wauDays = 7
	mauDays = 30
	// rollingMinDays keeps a 24h window from collapsing the chart to one point.
	// DAU/WAU/MAU is a shape, and a shape needs more than a single column.
	rollingMinDays = 7
	// rollingMaxDays caps the rolling series independently of the general
	// 365-day timeseries cap: it is always a daily series (the handler drops
	// granularity), so days and output points are the same number, and nothing
	// upstream bounds it — parseTimeseriesParams exempts "monthly" from its cap,
	// and window=all on an empty database falls back to a fixed multi-year
	// (allWindowDays, monthly) mapping that reaches this endpoint too.
	rollingMaxDays = 365
)

// GetRollingActiveTimeSeries returns DAU, WAU and MAU per day.
//
// Always daily, whatever granularity the caller asked for: the three series are
// defined as trailing 1/7/30-*day* windows, so bucketing them hourly or monthly
// would make the labels lie. The handler drops granularity for this reason.
//
// The trailing windows are computed in Go over distinct (day, address) pairs
// rather than in SQL: a self-join over a 30-day trailing range would re-scan the
// union four times per day of output, whereas one pass produces every window.
// Rows are read from mauDays-1 days *before* the requested start so the first
// output day has a full trailing window rather than a truncated one.
//
// The window slides day by day rather than being rebuilt from scratch per
// output point: each day's addresses are folded into a reference count as the
// day enters the window, and unfolded (decrement, delete at zero) as it leaves.
// A rebuild-per-point approach was previously used here on the theory that a
// sliding window "cannot cheaply remove an address that also appears in a day
// still inside the window" — that reasoning does not hold: a ref count handles
// exactly that case, since the address stays present until every day
// contributing to it has left the window. The rebuild approach measured at
// 1.59s/2.78s for a 90/365-day window against 800k rows; the ref-count version
// is a single pass over the loaded days.
//
// Empty bucket is 0 — a count series per §10.1.
func (d *DB) GetRollingActiveTimeSeries(network string, days int) ([]RollingActivePoint, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	if days < rollingMinDays {
		days = rollingMinDays
	}
	now := time.Now().UTC()
	firstDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -days)
	loadFrom := firstDay.AddDate(0, 0, -(mauDays - 1)).Format(time.RFC3339)

	netFilter := ""
	if network != "" {
		netFilter = " AND t.network = ?"
	}
	parts := make([]string, 0, len(activityMsgTables))
	args := make([]any, 0, 2*len(activityMsgTables))
	for _, s := range activityMsgTables {
		// Column and table names come from the constant above, never from input.
		parts = append(parts, fmt.Sprintf(
			"SELECT strftime('%%Y-%%m-%%d', t.block_time) AS day, t.%s AS addr FROM %s t"+
				" WHERE t.block_time >= ?%s", s.addrCol, s.table, netFilter))
		args = append(args, loadFrom)
		if network != "" {
			args = append(args, network)
		}
	}
	q := "SELECT DISTINCT day, addr FROM (" + strings.Join(parts, " UNION ALL ") + ")"

	rows, err := d.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	perDay := make(map[string]map[string]struct{})
	for rows.Next() {
		// day comes from strftime() over nullable TEXT block_time; a garbage value
		// (e.g. "not-a-timestamp") makes it NULL rather than failing the query.
		// sql.NullString lets that row be skipped as a data-quality issue instead
		// of erroring the whole series.
		var day sql.NullString
		var addr string
		if err := rows.Scan(&day, &addr); err != nil {
			return nil, err
		}
		if !day.Valid {
			continue
		}
		set, ok := perDay[day.String]
		if !ok {
			set = make(map[string]struct{})
			perDay[day.String] = set
		}
		set[addr] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	loadFromDay := firstDay.AddDate(0, 0, -(mauDays - 1))
	wCounts := make(map[string]int)
	mCounts := make(map[string]int)

	// advance folds one more day into both trailing windows and evicts whichever
	// day just fell outside each window's span, keeping wCounts/mCounts always
	// equal to a reference count over exactly the trailing wauDays/mauDays days
	// ending at day.
	advance := func(day time.Time) {
		for addr := range perDay[day.Format("2006-01-02")] {
			wCounts[addr]++
			mCounts[addr]++
		}
		if wLeave := day.AddDate(0, 0, -wauDays); !wLeave.Before(loadFromDay) {
			for addr := range perDay[wLeave.Format("2006-01-02")] {
				wCounts[addr]--
				if wCounts[addr] == 0 {
					delete(wCounts, addr)
				}
			}
		}
		if mLeave := day.AddDate(0, 0, -mauDays); !mLeave.Before(loadFromDay) {
			for addr := range perDay[mLeave.Format("2006-01-02")] {
				mCounts[addr]--
				if mCounts[addr] == 0 {
					delete(mCounts, addr)
				}
			}
		}
	}

	// Pre-roll the window across the mauDays-1 days before the first output day,
	// so by the time output starts both windows already hold their full trailing
	// span instead of a truncated one.
	for d := loadFromDay; d.Before(firstDay); d = d.AddDate(0, 0, 1) {
		advance(d)
	}

	out := make([]RollingActivePoint, 0, days+1)
	for i := 0; i <= days; i++ {
		day := firstDay.AddDate(0, 0, i)
		advance(day)
		out = append(out, RollingActivePoint{
			Time: day.Format("2006-01-02"),
			DAU:  len(perDay[day.Format("2006-01-02")]),
			WAU:  len(wCounts),
			MAU:  len(mCounts),
		})
	}
	return out, nil
}

// GasBin is one bucket of the gas-used-per-transaction distribution.
type GasBin struct {
	Bin string `json:"bin"`
	Txs int    `json:"txs"`
}

// gasPerTxBinExpr bins a transaction's gas_used.
//
// Unlike blockTimeBinExpr, whose edges were measured against one chain, gas has
// no target value to cluster around and spans four orders of magnitude between
// chains — the local sapphire data runs 6.2e5 to 1.2e9 with a 6.5e7 median,
// while a bare mainnet transfer is orders of magnitude cheaper. So the edges are
// half-decade log steps, which keep *some* resolution wherever a chain's mass
// happens to sit instead of being right for one chain and degenerate on others.
// Lower edges are inclusive.
const gasPerTxBinExpr = `CASE
	WHEN g <       100000 THEN '<100k'
	WHEN g <       500000 THEN '100k-500k'
	WHEN g <      1000000 THEN '500k-1M'
	WHEN g <      5000000 THEN '1M-5M'
	WHEN g <     10000000 THEN '5M-10M'
	WHEN g <     50000000 THEN '10M-50M'
	WHEN g <    100000000 THEN '50M-100M'
	WHEN g <    500000000 THEN '100M-500M'
	ELSE '>=500M'
END`

// GasPerTxBinOrder is the display order of the gas histogram's bins.
var GasPerTxBinOrder = []string{
	"<100k", "100k-500k", "500k-1M", "1M-5M", "5M-10M", "10M-50M", "50M-100M", "100M-500M", ">=500M",
}

// GetGasPerTxHistogram bins gas_used across transactions in the window.
//
// This is the one reader here counting *transactions* rather than messages: gas
// is charged per transaction, so binning per message would count one fee several
// times. Rows with gas_used = 0 are excluded — that is the default for a row
// whose gas was never backfilled, not a transaction that genuinely burned none.
//
// Mode B: the window filters which transactions are counted; the bin set is
// fixed. Empty bin is 0, a count series per §10.1, matching what batch 2a's
// block-time histogram already does.
func (d *DB) GetGasPerTxHistogram(network string, days int) ([]GasBin, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	start := time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339)

	netFilter := ""
	args := []any{start}
	if network != "" {
		netFilter = " AND network = ?"
		args = append(args, network)
	}
	q := fmt.Sprintf(
		"WITH g AS (SELECT gas_used AS g FROM transactions WHERE block_time >= ? AND gas_used > 0%s)"+
			" SELECT %s AS bin, COUNT(*) FROM g GROUP BY bin", netFilter, gasPerTxBinExpr)

	rows, err := d.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	counts := make(map[string]int)
	for rows.Next() {
		var bin string
		var n int
		if err := rows.Scan(&bin, &n); err != nil {
			return nil, err
		}
		counts[bin] = n
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]GasBin, 0, len(GasPerTxBinOrder))
	for _, bin := range GasPerTxBinOrder {
		out = append(out, GasBin{Bin: bin, Txs: counts[bin]})
	}
	return out, nil
}

// FuncCallCell is one cell of a realm's function x day call grid.
type FuncCallCell struct {
	Func  string `json:"func"`
	Day   string `json:"day"`
	Calls int    `json:"calls"`
}

// funcHeatmapMaxFuncs caps the rows of the function heatmap. A realm with 200
// exported functions would otherwise render rows one pixel tall.
const funcHeatmapMaxFuncs = 20

// realmsWithCallsMaxLimit caps ?limit= on the realm selector. It feeds a
// dropdown, not a paginated list, so there is no legitimate reason to ask for
// more than this many rows; without a cap an unvalidated limit is unbounded.
const realmsWithCallsMaxLimit = 100

// GetRealmsWithCalls lists the realms called in the window, busiest first.
// Feeds the function heatmap's realm selector.
func (d *DB) GetRealmsWithCalls(network string, days, limit int) ([]string, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	if limit <= 0 {
		limit = 30
	} else if limit > realmsWithCallsMaxLimit {
		limit = realmsWithCallsMaxLimit
	}
	// Midnight of day-(days-1), the same start instant GetFunctionCallHeatmap
	// uses. GetFunctionCallHeatmap's grid begins there, so a realm whose only
	// calls fall between that instant and time.Now().AddDate(0, 0, -days) would
	// otherwise appear in this selector and yield an empty heatmap.
	now := time.Now().UTC()
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).
		AddDate(0, 0, -(days - 1)).Format(time.RFC3339)

	netFilter := ""
	args := []any{start}
	if network != "" {
		netFilter = " AND network = ?"
		args = append(args, network)
	}
	args = append(args, limit)

	rows, err := d.db.Query(fmt.Sprintf(
		"SELECT pkg_path, COUNT(*) AS n FROM calls WHERE block_time >= ?%s"+
			" GROUP BY pkg_path ORDER BY n DESC, pkg_path ASC LIMIT ?", netFilter), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var p string
		var n int
		if err := rows.Scan(&p, &n); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetFunctionCallHeatmap returns calls per (function, day) for one realm.
//
// The grid is zero-filled server-side over the full day range and the selected
// functions, so the caller never has to re-derive which days the window covers.
// Cells come back function-major, functions ordered busiest-first.
//
// Empty cell is 0 — "this function was not called that day" is a real zero, not
// missing data (§10.1).
func (d *DB) GetFunctionCallHeatmap(network, pkgPath string, days int) ([]FuncCallCell, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	if pkgPath == "" {
		return nil, nil
	}
	now := time.Now().UTC()
	firstDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -(days - 1))

	netFilter := ""
	args := []any{pkgPath, firstDay.Format(time.RFC3339)}
	if network != "" {
		netFilter = " AND network = ?"
		args = append(args, network)
	}
	q := "SELECT func_name, strftime('%Y-%m-%d', block_time) AS day, COUNT(*)" +
		" FROM calls WHERE pkg_path = ? AND block_time >= ?" + netFilter +
		" GROUP BY func_name, day"

	rows, err := d.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type key struct{ fn, day string }
	cells := make(map[key]int)
	totals := make(map[string]int)
	for rows.Next() {
		var fn string
		// day comes from strftime() over nullable TEXT block_time; a garbage
		// value (e.g. "not-a-timestamp") makes it NULL rather than failing the
		// query. sql.NullString lets that row be skipped as a data-quality issue
		// instead of erroring the whole heatmap.
		var day sql.NullString
		var n int
		if err := rows.Scan(&fn, &day, &n); err != nil {
			return nil, err
		}
		if !day.Valid {
			continue
		}
		cells[key{fn, day.String}] = n
		totals[fn] += n
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(totals) == 0 {
		return nil, nil
	}

	funcs := make([]string, 0, len(totals))
	for fn := range totals {
		funcs = append(funcs, fn)
	}
	sort.Slice(funcs, func(i, j int) bool {
		if totals[funcs[i]] != totals[funcs[j]] {
			return totals[funcs[i]] > totals[funcs[j]]
		}
		return funcs[i] < funcs[j]
	})
	if len(funcs) > funcHeatmapMaxFuncs {
		funcs = funcs[:funcHeatmapMaxFuncs]
	}

	out := make([]FuncCallCell, 0, len(funcs)*days)
	for _, fn := range funcs {
		for i := range days {
			day := firstDay.AddDate(0, 0, i).Format("2006-01-02")
			out = append(out, FuncCallCell{Func: fn, Day: day, Calls: cells[key{fn, day}]})
		}
	}
	return out, nil
}
