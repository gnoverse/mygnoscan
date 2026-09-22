package store

import (
	"database/sql"
	"fmt"
	"log"
	"strings"
	"sync"

	"github.com/moul/mygnoscan/pkg/config"
	_ "modernc.org/sqlite"
)

type DB struct {
	db *sql.DB

	// mu guards `configured` below. It is deliberately *not* a general database
	// lock any more.
	//
	// It used to be one: every write took mu.Lock, every read took mu.RLock,
	// and the rollup refresh held the exclusive lock for its whole rebuild —
	// ~32 seconds out of every 300 on production, during which every read path
	// queued. Roughly one page load in nine was multi-second for no reason the
	// reader could see, and measurably so: 8.14s against 0.06–0.16s for the
	// identical endpoint outside the window.
	//
	// The database opens in WAL mode, which exists precisely so readers proceed
	// alongside a writer. The serialization was imposed above SQLite and undid
	// what WAL provides. See #143.
	mu sync.RWMutex

	// writeMu serializes writers against each other, which is the part that was
	// worth keeping. SQLite allows one writer at a time; without this, a long
	// rollup transaction and the syncer's inserts race for that slot and the
	// loser waits out busy_timeout or fails. Readers do not take it.
	writeMu sync.Mutex

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

func (d *DB) SetConfiguredNetworks(networks []config.NetworkConfig) {
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
		callSQL = ""
	}

	// Migrate: the old UNIQUE(network, tx_hash, pkg_path, func_name) silently
	// dropped every call past the first when one transaction bundled several
	// MsgCall messages to the same function — the shape of a "multicall". calls
	// and bank_sends share syncCalls' resume cursor (getLastRecentTransaction-
	// BlockHeight), so both are dropped together to force a full, correct
	// resync rather than leaving bank_sends' cursor ahead of the history calls
	// just lost.
	if callSQL != "" && !strings.Contains(callSQL, "msg_index") {
		db.Exec(`DROP TABLE IF EXISTS calls`)
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

	// Migrate: package_submissions didn't always exist. packages has always
	// been a current-state projection (one row per path, INSERT OR REPLACE),
	// so on a database that already has packages rows but no
	// package_submissions table, dropping packages is the only way to make
	// syncPackages re-walk history and backfill it — its resume cursor is
	// MAX(block_height) FROM packages itself (see getLastBlockHeight), which
	// already sits at the tip on an existing install and would otherwise
	// never look back. package_files and dependencies are untouched: both are
	// path-keyed and idempotently re-upserted by the same resync, so nothing
	// there needs dropping. See the comment on package_submissions itself.
	var pkgSubSQL string
	db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name='package_submissions'`).Scan(&pkgSubSQL)
	if pkgSubSQL == "" && pkgSQL != "" {
		db.Exec(`DROP TABLE IF EXISTS packages`)
	}

	// Migrate: add block_time to tables created before it existed. Must run
	// before initSchema, which builds indexes on that column.
	if err := migrateAddBlockTime(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate block_time: %w", err)
	}

	if err := migrateStorageUnlockSign(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate storage unlock sign: %w", err)
	}

	if err := initSchema(db); err != nil {
		db.Close()
		return nil, err
	}

	// After initSchema, which is what creates the column on a fresh database.
	if err := migrateBankSendUgnot(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate bank send ugnot: %w", err)
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
		-- packages answers "what is deployed at this path right now", and only
		-- that. It is keyed (network, path) and written INSERT OR REPLACE, so a
		-- redeploy overwrites rather than appends and the table holds one row
		-- per path no matter how many times that path was submitted. On mainnet
		-- as of 2026-09-22 that is 368 rows against 447 deploy messages.
		--
		-- So it is the wrong source for any question about history: how many
		-- deploys there were, when a creator first deployed, what a given
		-- transaction deployed, how deploy activity moved over time. Every one
		-- of those needs package_submissions, which is one row per message and
		-- never overwritten.
		--
		-- The tell is the join key. Joining packages on path is a current-state
		-- question and correct here; joining it on tx_hash is a history
		-- question wearing a current-state table, and it silently loses every
		-- transaction whose path was later redeployed.
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

		-- One row per MsgAddPackage ever seen, never overwritten — unlike
		-- packages above, which is a current-state projection (one row per
		-- path, INSERT OR REPLACE) and always was. That was fine while a path
		-- could be deployed exactly once; it no longer is. Under the "inert"
		-- code submission policy a parked package is invisible to every
		-- liveness probe (vm/qfile, vm/qrender), so anything that submits and
		-- then verifies by querying the path concludes the deploy failed and
		-- resubmits — routinely, once per retry, for as long as an approver
		-- is stuck. Each resubmission used to silently overwrite the last in
		-- packages, which made a creator's own AddPackage history undercount
		-- (187 shown against 256 on-chain, on the account that surfaced this),
		-- vanished the earlier submission from /txs filtered by MsgAddPackage,
		-- and from watch. TestAddressTransactionsIncludesEveryPackageResubmission
		-- and TestNewPackagesCountsFirstSubmissionNotLatest pin both halves.
		CREATE TABLE IF NOT EXISTS package_submissions (
			network TEXT NOT NULL DEFAULT 'gnoland1',
			tx_hash TEXT NOT NULL,
			msg_index INTEGER NOT NULL DEFAULT 0,
			path TEXT NOT NULL,
			name TEXT NOT NULL,
			creator TEXT NOT NULL,
			block_height INTEGER NOT NULL,
			block_time TEXT,
			is_realm BOOLEAN NOT NULL,
			num_files INTEGER NOT NULL,
			success BOOLEAN NOT NULL,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (network, tx_hash, msg_index)
		);

		-- Storage deposits and unlocks, one row per event.
		--
		-- These arrive on every transaction the sync walk already fetches
		-- (txFieldsLight selects them), and were being discarded. Reading them
		-- back needed a live per-realm indexer query, so anything wanting
		-- storage across many realms at once — a list column, a chain-wide
		-- total, a share-by-realm trend — was one indexer round-trip per realm.
		--
		-- fee is stored signed: positive for a deposit, negative for a refund,
		-- so summing the column answers "what did storage cost" without the
		-- reader having to know which kinds subtract. bytes_delta follows the
		-- same convention.
		CREATE TABLE IF NOT EXISTS storage_events (
			network      TEXT NOT NULL DEFAULT 'gnoland1',
			tx_hash      TEXT NOT NULL,
			event_index  INTEGER NOT NULL,
			pkg_path     TEXT NOT NULL,
			block_height INTEGER NOT NULL,
			block_time   TEXT,
			kind         TEXT NOT NULL,
			bytes_delta  INTEGER NOT NULL,
			fee          INTEGER NOT NULL,
			PRIMARY KEY (network, tx_hash, event_index)
		);

		-- Edge rollups for the network graphs, bucketed by day so a window query
		-- is a range scan rather than a scan of every send or call in history.
		--
		-- last_height is a per-edge cursor: a pass folds in rows above
		-- MAX(last_height) and adds to the existing totals, so re-running it
		-- cannot double-count. The day in the primary key is what keeps a
		-- window narrowable; collapsing parallel edges is left to read time.
		CREATE TABLE IF NOT EXISTS transfer_edges (
			network      TEXT NOT NULL DEFAULT 'gnoland1',
			from_address TEXT NOT NULL,
			to_address   TEXT NOT NULL,
			day          TEXT NOT NULL,
			total_value  INTEGER NOT NULL DEFAULT 0,
			tx_count     INTEGER NOT NULL DEFAULT 0,
			last_height  INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (network, from_address, to_address, day)
		);

		CREATE TABLE IF NOT EXISTS caller_edges (
			network     TEXT NOT NULL DEFAULT 'gnoland1',
			caller      TEXT NOT NULL,
			pkg_path    TEXT NOT NULL,
			day         TEXT NOT NULL,
			calls       INTEGER NOT NULL DEFAULT 0,
			last_height INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (network, caller, pkg_path, day)
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
			-- Position of this MsgCall within its transaction's message list.
			-- A tx can bundle several MsgCall messages to the same function (a
			-- "multicall"); without this, all but the first collapsed into one
			-- row and every call past the first went uncounted.
			msg_index INTEGER NOT NULL DEFAULT 0,
			block_height INTEGER NOT NULL,
			block_time TEXT,
			caller TEXT NOT NULL,
			pkg_path TEXT NOT NULL,
			func_name TEXT NOT NULL,
			success BOOLEAN NOT NULL,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(network, tx_hash, msg_index)
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
			-- amount is the coin string the message carried, verbatim, and it
			-- is a coin *list*: "100ugnot,5foo". ugnot_amount is it parsed, for
			-- summing. Both, for the same reason balances keeps both: a total
			-- has to add a number, and a page has to show what the chain said.
			--
			-- Summing the string instead needed a denom stripped out of it with
			-- REPLACE, and SQLite's CAST takes the leading numeric prefix and
			-- discards the rest without erroring, so "5foo,100ugnot" summed as
			-- 5 ugnot and "5foo" invented 5 ugnot that never existed.
			amount TEXT NOT NULL,
			ugnot_amount INTEGER,
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

		-- Per-realm storage totals, the same shape and lifecycle as
		-- gas_realm_rollup: rebuilt wholesale on the rollup tick, keyed so a
		-- list page can look a realm up rather than scan its events.
		CREATE TABLE IF NOT EXISTS storage_realm_rollup (
			network      TEXT NOT NULL,
			path         TEXT NOT NULL,
			bytes_net    INTEGER NOT NULL,
			fee_net      INTEGER NOT NULL,
			event_count  INTEGER NOT NULL,
			PRIMARY KEY (network, path)
		);

		CREATE INDEX IF NOT EXISTS idx_storage_rollup_fee
			ON storage_realm_rollup(network, fee_net DESC);

		-- Same idea as gas_realm_rollup, attributed to the caller instead of the
		-- realm. Built from DISTINCT (network, tx_hash, caller) pairs rather than
		-- one row per message: a transaction's gas is paid once, so a multicall
		-- bundling many messages to the same caller must not multiply it by the
		-- message count. See the caller branch of refreshRollups.
		CREATE TABLE IF NOT EXISTS gas_caller_rollup (
			network   TEXT NOT NULL,
			caller    TEXT NOT NULL,
			gas_used  INTEGER NOT NULL,
			gas_fee   INTEGER NOT NULL,
			tx_count  INTEGER NOT NULL,
			PRIMARY KEY (network, caller)
		);

		CREATE INDEX IF NOT EXISTS idx_gas_caller_rollup_gas
			ON gas_caller_rollup(network, gas_used DESC);

		CREATE TABLE IF NOT EXISTS gas_totals_rollup (
			network       TEXT PRIMARY KEY,
			tx_count      INTEGER NOT NULL,
			gas_used      INTEGER NOT NULL,
			gas_wanted    INTEGER NOT NULL,
			gas_fee       INTEGER NOT NULL,
			success_count INTEGER NOT NULL
		);

		-- Bank rollups, on the same timer and for the same reason as the gas
		-- ones: seven aggregate passes over bank_sends, three of them
		-- COUNT(DISTINCT) over the whole table, measured at 5.4s and growing.
		CREATE TABLE IF NOT EXISTS bank_totals_rollup (
			network          TEXT PRIMARY KEY,
			total_sends      INTEGER NOT NULL,
			unique_senders   INTEGER NOT NULL,
			unique_receivers INTEGER NOT NULL,
			unique_addresses INTEGER NOT NULL,
			total_volume     INTEGER NOT NULL
		);

		-- One row per (network, leaderboard, address). The kind column names
		-- which leaderboard, so the three rankings share a table rather than
		-- needing three that differ only in ORDER BY.
		CREATE TABLE IF NOT EXISTS bank_top_rollup (
			network TEXT NOT NULL,
			kind    TEXT NOT NULL,
			address TEXT NOT NULL,
			count   INTEGER NOT NULL,
			total   INTEGER NOT NULL,
			PRIMARY KEY (network, kind, address)
		);

		-- Active addresses, stored as distinct tuples rather than as counts.
		--
		-- /api/timeseries/active-addresses was the slowest endpoint on the site
		-- at 15.9s on production, and the one the dashboards page waits on. The
		-- question — how many distinct addresses were active in each bucket —
		-- means touching every call, deploy and send in the window and
		-- deduplicating them per bucket, which no index removes: covering
		-- indexes on (network, block_time, addr) bought 26% and left the plan
		-- still building a temp B-tree.
		--
		-- Counts per day would not work. An address active on three days of a
		-- week is one weekly active address, not three, so summing daily counts
		-- overcounts, and the error grows with the bucket width — worst on the
		-- 1y and all windows, where the number matters most. Storing the tuples
		-- keeps every granularity exact, because the stored grain is finer than
		-- anything served: hourly counts the tuples, wider buckets re-deduplicate.
		--
		-- Hourly is a measurement, not a guess. On the production database
		-- 1,274,004 raw rows reduce to 95,615 distinct (network, day, address)
		-- and 110,073 distinct (network, hour, address) — hourly is only 15%
		-- larger than daily, so one table serves every window and no separate
		-- live path is needed for 24h and 7d. Growth is ~1,650 rows/day.
		--
		-- WITHOUT ROWID: every column is part of the key, so a rowid would be a
		-- second b-tree storing the same data twice.
		CREATE TABLE IF NOT EXISTS active_addr_rollup (
			network TEXT NOT NULL,
			bucket  TEXT NOT NULL,   -- UTC hour, 'YYYY-MM-DDTHH'
			kind    TEXT NOT NULL,   -- callers | deployers | senders
			addr    TEXT NOT NULL,
			PRIMARY KEY (network, bucket, kind, addr)
		) WITHOUT ROWID;

		-- The symbol index: one row per declaration in every package's current
		-- source, so a search can answer "which package declares
		-- IterateByOffset" rather than only "which path contains that string".
		--
		-- Derived, not authoritative. package_files is the source and this is a
		-- projection of it, rebuilt for a package whenever that package's files
		-- change. Keyed on the package rather than on the submission, because
		-- package_files itself holds current bodies only: a redeploy overwrites
		-- them, so the bodies an older submission was compiled from are simply
		-- not here. An API diff between two deploys of the same path therefore
		-- still needs a spine of its own, and this table is not it.
		--
		-- recv is the receiver type for a method and empty for everything else.
		-- It is in the primary key because Get on two different types is two
		-- different symbols, and without it one silently replaces the other.
		CREATE TABLE IF NOT EXISTS symbols (
			network      TEXT NOT NULL,
			package_path TEXT NOT NULL,
			kind         TEXT NOT NULL,   -- const | var | type | func | method
			recv         TEXT NOT NULL DEFAULT '',
			name         TEXT NOT NULL,
			signature    TEXT NOT NULL DEFAULT '',
			doc          TEXT NOT NULL DEFAULT '',
			file         TEXT NOT NULL DEFAULT '',
			line         INTEGER NOT NULL DEFAULT 0,
			exported     BOOLEAN NOT NULL DEFAULT 0,
			PRIMARY KEY (network, package_path, kind, recv, name)
		);

		-- Name first, because every query this table exists for filters on it.
		-- Prefix matches use the index; the substring fallback does not, which
		-- is why the search asks for a prefix first and only widens if that
		-- came back short.
		CREATE INDEX IF NOT EXISTS symbols_name ON symbols(name);
		CREATE INDEX IF NOT EXISTS symbols_pkg ON symbols(network, package_path);

		-- What the index was built from, so a rebuild can be skipped.
		--
		-- source_key is tx_hash, block_height, file count and total bytes joined
		-- by pipes, and the shape is chosen so the whole "has anything changed"
		-- question is one
		-- SQL join that reads no source at all. package_files is the largest
		-- table on a busy chain; a periodic pass that hashed every body would
		-- be reading hundreds of megabytes every few minutes to discover that
		-- nothing moved.
		--
		-- Not the deploy height alone: a resync rewrites rows at the same
		-- height, and the file count and byte total are what notice a package
		-- that was half-synced when the last pass ran. A body cannot change
		-- without a new MsgAddPackage, so tx_hash carries the rest.
		CREATE TABLE IF NOT EXISTS symbol_index (
			network      TEXT NOT NULL,
			package_path TEXT NOT NULL,
			source_key   TEXT NOT NULL,
			symbol_count INTEGER NOT NULL DEFAULT 0,
			indexed_at   DATETIME DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (network, package_path)
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

		-- Account balances, cached.
		--
		-- Not synced and not derivable: gno has no balance in any indexed
		-- message, and deriving one from bank_sends would be wrong in a way
		-- readers could not see, because it ignores gas fees, storage deposits,
		-- genesis allocations and every transfer a realm makes through a banker
		-- rather than a BankMsgSend.
		--
		-- So each row is one live bank/balances read, swept in the
		-- background. amount is the raw coin string the chain returned, and
		-- ugnot is it parsed for sorting: keeping both means the rich list can
		-- ORDER BY without the display value ever having been through a lossy
		-- conversion. height is the chain tip when it was read, which is what
		-- dates the figure; a balance with no height was never successfully
		-- fetched.
		CREATE TABLE IF NOT EXISTS balances (
			network    TEXT    NOT NULL,
			address    TEXT    NOT NULL,
			amount     TEXT    NOT NULL DEFAULT '',
			ugnot      INTEGER NOT NULL DEFAULT 0,
			height     INTEGER NOT NULL DEFAULT 0,
			fetched_at TEXT    NOT NULL,
			PRIMARY KEY (network, address)
		) WITHOUT ROWID;

		-- GRC20 transfers, from the Transfer events the chain already emits.
		--
		-- A complete ledger: an empty from_addr is a mint, an empty to_addr is
		-- a burn, and replaying the column gives exact supply and every
		-- holder's balance without a single extra RPC call.
		--
		-- token is the full triple the events carry
		-- (gno.land/r/gnoland/wugnot.wugnot.0000000), not a bare realm path:
		-- one realm can expose several tokens. pkg_path is that realm, split
		-- out at insert so queries do not have to parse the key.
		--
		-- NOT keyed on the event's pkg_path, which is the grc20 library for
		-- every token on the chain and would collapse them all into one.
		CREATE TABLE IF NOT EXISTS token_transfers (
			network    TEXT NOT NULL,
			tx_hash    TEXT NOT NULL,
			event_idx  INTEGER NOT NULL,
			token      TEXT NOT NULL,
			pkg_path   TEXT NOT NULL,
			from_addr  TEXT NOT NULL DEFAULT '',
			to_addr    TEXT NOT NULL DEFAULT '',
			value      INTEGER NOT NULL DEFAULT 0,
			block_height INTEGER NOT NULL,
			block_time TEXT NOT NULL,
			PRIMARY KEY (network, tx_hash, event_idx)
		) WITHOUT ROWID;

		CREATE INDEX IF NOT EXISTS idx_token_transfers_token ON token_transfers(network, token, block_height DESC);
		CREATE INDEX IF NOT EXISTS idx_token_transfers_from ON token_transfers(network, token, from_addr);
		CREATE INDEX IF NOT EXISTS idx_token_transfers_to ON token_transfers(network, token, to_addr);

		-- "What does this address hold" reads along the other axis to the two
		-- above, which lead with the token. Without these, asking what one
		-- realm holds scans every transfer on the chain, because the address is
		-- the second column of a three-column index and the first is unbound.
		CREATE INDEX IF NOT EXISTS idx_token_transfers_holder_from ON token_transfers(network, from_addr);
		CREATE INDEX IF NOT EXISTS idx_token_transfers_holder_to ON token_transfers(network, to_addr);

		-- The rich list's only query: the top balances on one chain.
		CREATE INDEX IF NOT EXISTS idx_balances_rank ON balances(network, ugnot DESC);

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
		CREATE INDEX IF NOT EXISTS idx_storage_net_path  ON storage_events(network, pkg_path);
		CREATE INDEX IF NOT EXISTS idx_storage_net_time  ON storage_events(network, block_time);
		CREATE INDEX IF NOT EXISTS idx_transfer_edges_day  ON transfer_edges(network, day, total_value);
		CREATE INDEX IF NOT EXISTS idx_transfer_edges_from ON transfer_edges(network, from_address);
		CREATE INDEX IF NOT EXISTS idx_transfer_edges_to   ON transfer_edges(network, to_address);
		CREATE INDEX IF NOT EXISTS idx_caller_edges_day    ON caller_edges(network, day, calls);
		CREATE INDEX IF NOT EXISTS idx_caller_edges_pkg    ON caller_edges(network, pkg_path);
		CREATE INDEX IF NOT EXISTS idx_pkgsub_net_creator ON package_submissions(network, creator);
		CREATE INDEX IF NOT EXISTS idx_pkgsub_net_path    ON package_submissions(network, path);
		CREATE INDEX IF NOT EXISTS idx_pkgsub_block_time  ON package_submissions(network, block_time);

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

		-- ListPackages' "last call" column and sort (block_height DESC LIMIT 1
		-- per package). block_height trailing, descending, lets that read the
		-- newest row for a (network, pkg_path) straight off the index instead of
		-- sorting a page's worth of rows on every request.
		CREATE INDEX IF NOT EXISTS idx_calls_net_pkg_height ON calls(network, pkg_path, block_height DESC);
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

var backfillTables = []string{"packages", "package_submissions", "calls", "msg_runs", "bank_sends", "transactions"}

// HeightsMissingBlockTime returns block heights that have rows with no
// block_time, oldest first, capped at limit.
//
// Rows written before block_time existed never got one, and incremental sync
// only ever moves forward from the cursor, so nothing fills them in. Without a
// timestamp a row cannot be ordered against another chain's rows, and list
// endpoints have to ask the indexer at request time instead of reading storage.

// BlocksBackfillDoneKey namespaces the "this network's block backfill has
// finished" marker in sync_state.
//
// Lives with the store rather than with the syncer that writes it: the store's
// own queries read the marker to decide whether block-derived columns can be
// trusted yet, so the key is part of the schema's vocabulary rather than an
// implementation detail of the sync loop.
func BlocksBackfillDoneKey(network string) string {
	return "blocks_backfill_done:" + network
}

// migrateBankSendUgnot fills the ugnot column on rows written before it existed.
//
// NULL is the marker: a row that has never been parsed has no value, and the
// backfill only looks at those, so it is idempotent and resumable without a
// separate flag in sync_state. New rows arrive parsed from InsertBankSend.
//
// Parsing is done per distinct coin string rather than per row. Production
// holds 21,592 sends across 3,444 distinct amounts (2026-09-21), and the
// distinct values are what carry the information; the ratio only widens as a
// chain grows, because a send repeats round numbers.
func migrateBankSendUgnot(db *sql.DB) error {
	// Errors ignored: on a database that already has the column, and on a
	// fresh one where initSchema just created it, this is a duplicate column.
	db.Exec(`ALTER TABLE bank_sends ADD COLUMN ugnot_amount INTEGER`)

	rows, err := db.Query(`SELECT DISTINCT amount FROM bank_sends WHERE ugnot_amount IS NULL`)
	if err != nil {
		return err
	}
	var amounts []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			rows.Close()
			return err
		}
		amounts = append(amounts, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(amounts) == 0 {
		return nil
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`UPDATE bank_sends SET ugnot_amount = ? WHERE amount = ? AND ugnot_amount IS NULL`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, a := range amounts {
		if _, err := stmt.Exec(ParseUgnot(a), a); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	log.Printf("bank_sends: parsed ugnot for %d distinct amounts", len(amounts))
	return nil
}

// migrateStorageUnlockSign repairs storage_events rows written while the
// syncer negated an already-negative BytesDelta.
//
// The chain emits a signed diff, negative for an unlock. The syncer used to
// flip it, so every unlock landed positive and SUM(bytes_delta) added freed
// bytes to used ones instead of cancelling them. On mainnet that reported
// gno.land/r/gnoland/wugnot at 10,918,147 bytes where the chain says 1,180,507,
// and left the released series of every realm flat at zero.
//
// The same rows carry the fee, which was always stored with the chain's sign,
// so the two columns of one row disagreed and the fee one was right. That is
// also what makes this migration safe to run repeatedly: it only touches
// unlock rows whose bytes are positive, which cannot occur once the sign is
// correct, so a second run is a no-op.
func migrateStorageUnlockSign(db *sql.DB) error {
	var exists int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='storage_events'`,
	).Scan(&exists); err != nil || exists == 0 {
		return err
	}
	res, err := db.Exec(
		`UPDATE storage_events SET bytes_delta = -bytes_delta
		  WHERE kind = 'unlock' AND bytes_delta > 0`)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		log.Printf("migration: corrected the sign of %d storage unlock rows", n)
	}
	return nil
}
