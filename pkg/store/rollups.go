package store

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

func (d *DB) bankRollupReady() (bool, string) {
	var n int
	if err := d.db.QueryRow(`SELECT COUNT(*) FROM bank_totals_rollup`).Scan(&n); err != nil || n == 0 {
		return false, ""
	}
	var at string
	d.db.QueryRow(`SELECT value FROM sync_state WHERE key = ?`, rollupComputedAtKey).Scan(&at)
	return true, at
}

// Every ranking row carries the chain it belongs to.
//
// 193 package paths now exist on more than one network — pearl launched with
// the same demo realms gnoland1 has — so a path alone no longer identifies a
// row, and neither does an address: the busiest caller on the site is active on
// two chains and the next-busiest on three. Ranking without the network merges
// separate actors into one row whose numbers belong to neither.

const rollupComputedAtKey = "gas_rollup_at"

// RollupInterval is how often the rollups are rebuilt. These are all-time
// aggregates, so minutes of staleness is invisible to a reader — and the
// recompute takes the write lock, so doing it per sync pass would mean holding
// it every 30 seconds for the sake of numbers nobody watches change.

const RollupInterval = 5 * time.Minute

// bankTopRollupLimit is how many rows each bank leaderboard keeps. The read
// shows ten; the slack absorbs a network filter narrowing the set afterwards.

const bankTopRollupLimit = 400

// RefreshRollups recomputes both gas aggregates for every configured network.
//
// This is the expensive query the gas page used to run per request — 3.4s for
// the realm attribution and 1.3s for the totals on sapphire, and both grow with
// the chain. Running it on a timer instead turns a 14-second page into a lookup.
//
// Whole-table replacement inside one transaction: readers see either the old
// rollup or the new one, never a half-written mixture.

func (d *DB) RefreshRollups() error {
	// A busy write lock loses the whole refresh, and the failure is quiet: the
	// read path falls back to computing live, so the page merely stays slow.
	// Retry rather than wait for the next tick.
	//
	// The backoff sleeps outside every lock, and the write lock is taken per
	// attempt rather than across the loop. It used to be held for the whole
	// thing, so a contended refresh blocked writers for the rebuild *plus* six
	// seconds of waiting — the retry made the stall longer than the work.
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 2 * time.Second)
		}
		if lastErr = d.refreshRollups(); lastErr == nil {
			return nil
		}
	}
	return lastErr
}

func (d *DB) refreshRollups() error {
	// Held for the rebuild so the syncer's inserts do not race this transaction
	// for SQLite's single writer slot. Readers are unaffected: since #143 they
	// no longer share a lock with writers at all.
	d.writeMu.Lock()
	defer d.writeMu.Unlock()

	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// configured is read under the read lock and released immediately. The
	// scope is a plain string, so nothing below depends on still holding it.
	d.mu.RLock()
	scope := d.networkFilter("t.network", "")
	d.mu.RUnlock()

	if _, err := tx.Exec(`DELETE FROM gas_realm_rollup`); err != nil {
		return err
	}
	if _, err := tx.Exec(`
		INSERT INTO gas_realm_rollup (network, path, gas_used, gas_fee, tx_count)
		SELECT network, path, SUM(gas_used), SUM(gas_fee), COUNT(*) FROM (
			SELECT DISTINCT t.network, c.pkg_path AS path, t.tx_hash, t.gas_used, t.gas_fee
			  FROM calls c JOIN transactions t
			    ON t.network = c.network AND t.tx_hash = c.tx_hash AND ` + scope + `
			UNION
			SELECT DISTINCT t.network, p.path, t.tx_hash, t.gas_used, t.gas_fee
			  FROM packages p JOIN transactions t
			    ON t.network = p.network AND t.tx_hash = p.tx_hash AND ` + scope + `
			UNION
			SELECT DISTINCT t.network, 'MsgRun by ' || m.caller, t.tx_hash, t.gas_used, t.gas_fee
			  FROM msg_runs m JOIN transactions t
			    ON t.network = m.network AND t.tx_hash = m.tx_hash AND ` + scope + `
		) GROUP BY network, path`); err != nil {
		return err
	}

	if _, err := tx.Exec(`DELETE FROM storage_realm_rollup`); err != nil {
		return err
	}
	// bytes_delta and fee are already signed at insert time, so an unlock
	// subtracts by summing. A realm whose deposits and refunds cancel out ends
	// at zero, which is the truthful answer rather than an absence.
	if _, err := tx.Exec(`
		INSERT INTO storage_realm_rollup (network, path, bytes_net, fee_net, event_count)
		SELECT network, pkg_path, SUM(bytes_delta), SUM(fee), COUNT(*)
		  FROM storage_events t
		 WHERE ` + scope + `
		 GROUP BY network, pkg_path`); err != nil {
		return err
	}

	if _, err := tx.Exec(`DELETE FROM gas_caller_rollup`); err != nil {
		return err
	}
	if _, err := tx.Exec(`
		INSERT INTO gas_caller_rollup (network, caller, gas_used, gas_fee, tx_count)
		SELECT network, caller, SUM(gas_used), SUM(gas_fee), COUNT(*) FROM (
			SELECT DISTINCT t.network, c.caller AS caller, t.tx_hash, t.gas_used, t.gas_fee
			  FROM calls c JOIN transactions t
			    ON t.network = c.network AND t.tx_hash = c.tx_hash AND ` + scope + `
			UNION
			SELECT DISTINCT t.network, p.creator AS caller, t.tx_hash, t.gas_used, t.gas_fee
			  FROM packages p JOIN transactions t
			    ON t.network = p.network AND t.tx_hash = p.tx_hash AND ` + scope + `
			UNION
			SELECT DISTINCT t.network, m.caller AS caller, t.tx_hash, t.gas_used, t.gas_fee
			  FROM msg_runs m JOIN transactions t
			    ON t.network = m.network AND t.tx_hash = m.tx_hash AND ` + scope + `
			UNION
			SELECT DISTINCT t.network, b.from_address AS caller, t.tx_hash, t.gas_used, t.gas_fee
			  FROM bank_sends b JOIN transactions t
			    ON t.network = b.network AND t.tx_hash = b.tx_hash AND ` + scope + `
		) GROUP BY network, caller`); err != nil {
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

	// Bank aggregates share this transaction and this timer: same shape, same
	// reason, and one refresh means one write-lock window rather than two.
	if err := d.refreshBankRollups(tx); err != nil {
		return err
	}

	// The active-address tuples too, for the same reason. They are also the
	// reason the stamp written below has to be inside this transaction: the
	// series reads it back to decide which hours it can trust.
	if err := d.refreshActiveAddrRollup(tx); err != nil {
		return err
	}

	if _, err := tx.Exec(
		`INSERT INTO sync_state (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		rollupComputedAtKey, time.Now().UTC().Format(time.RFC3339)); err != nil {
		return err
	}
	return tx.Commit()
}

// RefreshBankRollups recomputes the bank aggregates for every configured
// network.
//
// GetBankStats runs seven passes over bank_sends, three of them COUNT(DISTINCT)
// over the whole table — 5.4s and growing. Same treatment as the gas rollups and
// on the same timer: whole-table replacement in one transaction, so a reader
// sees one consistent generation.

func (d *DB) refreshBankRollups(tx *sql.Tx) error {
	scope := d.networkFilter("network", "")

	if _, err := tx.Exec(`DELETE FROM bank_totals_rollup`); err != nil {
		return err
	}
	if _, err := tx.Exec(`
		INSERT INTO bank_totals_rollup
			(network, total_sends, unique_senders, unique_receivers, unique_addresses, total_volume)
		SELECT s.network, COUNT(*),
		       COUNT(DISTINCT s.from_address),
		       COUNT(DISTINCT s.to_address),
		       (SELECT COUNT(*) FROM (
		            SELECT DISTINCT addr FROM (
		                SELECT from_address addr FROM bank_sends b WHERE b.network = s.network
		                UNION ALL SELECT to_address FROM bank_sends b WHERE b.network = s.network))),
		       ` + amountExpr + `
		FROM bank_sends s WHERE ` + scope + `
		GROUP BY s.network`); err != nil {
		return err
	}

	if _, err := tx.Exec(`DELETE FROM bank_top_rollup`); err != nil {
		return err
	}
	// Bounded per network per leaderboard: the read only ever shows ten, and
	// storing every address would make the rollup as large as the table.
	//
	// One kind per *ordering*, not one per address column. Storing the top 400
	// receivers by count and re-sorting those by volume gives the wrong answer:
	// an address that received one enormous transfer is not in the top 400 by
	// count, so it never reaches the volume ranking. Caught by diffing against
	// live computation on production — every total matched and only that one
	// list differed, which is exactly how a truncation bug looks.
	for _, r := range []struct{ kind, addr, order string }{
		{"sender", "from_address", "COUNT(*)"},
		{"receiver_count", "to_address", "COUNT(*)"},
		{"receiver_volume", "to_address", amountExpr},
	} {
		if _, err := tx.Exec(`
			INSERT INTO bank_top_rollup (network, kind, address, count, total)
			SELECT network, ?, `+r.addr+`, COUNT(*), `+amountExpr+`
			FROM bank_sends WHERE `+scope+`
			GROUP BY network, `+r.addr+`
			ORDER BY `+r.order+` DESC
			LIMIT `+strconv.Itoa(bankTopRollupLimit)+``, r.kind); err != nil {
			return err
		}
	}
	return nil
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
	d.db.QueryRow(`SELECT value FROM sync_state WHERE key = ?`, rollupComputedAtKey).Scan(&at)
	return true, at
}

// blockTimeSources are the tables recording a chain timestamp, with the column
// that holds it. NetworkDataStart takes the minimum across all of them.

func (d *DB) refreshActiveAddrRollup(tx *sql.Tx) error {
	if _, err := tx.Exec(`DELETE FROM active_addr_rollup`); err != nil {
		return err
	}

	scope := d.networkFilter("network", "")
	branches := make([]string, 0, len(activeAddrKinds))
	for _, k := range activeAddrKinds {
		branches = append(branches, fmt.Sprintf(
			"SELECT network, strftime('%%Y-%%m-%%dT%%H', block_time) AS bucket, '%s' AS kind, %s AS addr FROM %s WHERE %s",
			k.kind, k.column, k.table, scope))
	}

	// block_time is nullable, and rows the syncer stored before it knew the
	// block's timestamp produce a NULL bucket. The primary key would reject
	// those and take the whole refresh down with it — silently, because the read
	// path falls back to computing live and the endpoint merely stays slow. The
	// live query drops the same rows, by way of `block_time >= ?`.
	_, err := tx.Exec(`
		INSERT INTO active_addr_rollup (network, bucket, kind, addr)
		SELECT DISTINCT network, bucket, kind, addr
		FROM (` + strings.Join(branches, " UNION ALL ") + `)
		WHERE bucket IS NOT NULL`)
	return err
}

// activeAddrRollupBoundary reports the instant from which the series still has
// to read the source tables, and whether the rollup can be used at all.
//
// The rebuild runs on a timer, so between two builds the rollup holds part of
// the hour it was built in and nothing after it. Serving the newest bucket from
// it would make this endpoint lag by up to the refresh interval and disagree
// with the live transaction feed on the same page — the class of bug that makes
// every other number on the site suspect.
//
// So the boundary is the *hour containing* the build, not the build instant:
// tuples strictly before it cover complete hours, and everything from it onward
// is recomputed live over a window of at most one interval plus one hour, which
// the (network, block_time) indexes serve directly.

func (d *DB) activeAddrRollupBoundary() (time.Time, bool) {
	var any int
	if err := d.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM active_addr_rollup)`).Scan(&any); err != nil || any == 0 {
		return time.Time{}, false
	}
	var at string
	if err := d.db.QueryRow(`SELECT value FROM sync_state WHERE key = ?`, rollupComputedAtKey).Scan(&at); err != nil {
		return time.Time{}, false
	}
	built, err := time.Parse(time.RFC3339, at)
	if err != nil {
		return time.Time{}, false
	}
	return built.UTC().Truncate(time.Hour), true
}

// RealmSharePoint is one realm's value in one time bucket.
//
// Deliberately long-form — one row per (bucket, realm) rather than a bucket
// carrying a map — because the set of realms is not known until the query runs
// and varies between buckets. Choosing which realms to name and which to fold
// into "rest" is a presentation decision, so it is left to the caller.
