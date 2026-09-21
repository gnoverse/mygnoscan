package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

func (d *DB) InsertCall(network, txHash string, blockHeight, msgIndex int, blockTime, caller, pkgPath, funcName string, success bool) error {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	_, err := d.db.Exec(`
		INSERT OR IGNORE INTO calls (network, tx_hash, msg_index, block_height, block_time, caller, pkg_path, func_name, success)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, network, txHash, msgIndex, blockHeight, blockTime, caller, pkgPath, funcName, success)
	return err
}

// ValoperRegistration is one validator's registration call, flattened. The
// moniker is the call's first argument.

func (d *DB) InsertMsgRun(network, txHash string, blockHeight int, blockTime, caller, source string, success bool) error {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	_, err := d.db.Exec(`
		INSERT OR IGNORE INTO msg_runs (network, tx_hash, block_height, block_time, caller, source, success)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, network, txHash, blockHeight, blockTime, caller, source, success)
	return err
}

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
	d.writeMu.Lock()
	defer d.writeMu.Unlock()

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
	d.writeMu.Lock()
	defer d.writeMu.Unlock()

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

type CallInfo struct {
	TxHash      string `json:"tx_hash"`
	BlockHeight int    `json:"block_height"`
	// BlockTime is nullable in the calls table and omitted when empty: rows
	// written before the syncer knew a block's timestamp carry none. A
	// consumer plotting these on a time axis has to say so rather than
	// treating a missing stamp as the epoch.
	BlockTime string `json:"block_time,omitempty"`
	Caller    string `json:"caller"`
	FuncName  string `json:"func_name"`
	Success   bool   `json:"success"`
}

type MsgRunInfo struct {
	TxHash      string `json:"tx_hash"`
	BlockHeight int    `json:"block_height"`
	BlockTime   string `json:"block_time,omitempty"`
	Caller      string `json:"caller"`
	Success     bool   `json:"success"`
}

func (d *DB) GovDAORelatedMsgRuns(network, executorPkgPath string) ([]MsgRunInfo, error) {
	if executorPkgPath == "" {
		return nil, nil
	}
	d.mu.RLock()
	defer d.mu.RUnlock()

	rows, err := d.db.Query(`
		SELECT tx_hash, block_height, COALESCE(block_time, ''), caller, success
		FROM msg_runs
		WHERE source LIKE ? AND source LIKE ? AND network = ?
		ORDER BY block_height ASC LIMIT 20
	`, "%"+GovDAOPathPrefix+"%", "%"+executorPkgPath+"%", network)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []MsgRunInfo
	for rows.Next() {
		var r MsgRunInfo
		if err := rows.Scan(&r.TxHash, &r.BlockHeight, &r.BlockTime, &r.Caller, &r.Success); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetDependencyGraph returns the full dependency graph for a package (recursive).

type TxTimePoint struct {
	Time    string `json:"time"`
	Calls   int    `json:"calls"`
	Deploys int    `json:"deploys"`
	MsgRuns int    `json:"msg_runs"`
	Sends   int    `json:"sends"`
	Total   int    `json:"total"`
}

func (d *DB) UpsertTransaction(network, txHash string, blockHeight int, blockTime string, gasUsed, gasWanted, gasFee int, success bool) error {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	_, err := d.db.Exec(`
		INSERT OR IGNORE INTO transactions (network, tx_hash, block_height, block_time, gas_used, gas_wanted, gas_fee, success)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, network, txHash, blockHeight, blockTime, gasUsed, gasWanted, gasFee, success)
	return err
}

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

type txSource struct {
	table   string
	caller  string
	detail  string
	success string
}

// MsgAddPackage sources from package_submissions, not packages: packages is
// a current-state projection (one row per path, overwritten by a later
// submission at the same path), so filtering /txs by MsgAddPackage against
// it silently hid every resubmission but the newest — see
// AddressTransactions' own comment on the identical bug (gno-meta#126).
// package_submissions carries its own success column, one row per attempt,
// so this no longer needs the join to `transactions` the old
// COALESCE(t.success, 1) depended on.

var TxSources = map[string]txSource{
	"MsgCall":       {table: "calls", caller: "caller", detail: "pkg_path || '::' || func_name", success: "e.success"},
	"MsgAddPackage": {table: "package_submissions", caller: "creator", detail: "path", success: "e.success"},
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
	src, ok := TxSources[msgType]
	if !ok {
		return nil, 0, fmt.Errorf("unknown message type: %s", msgType)
	}
	d.mu.RLock()
	defer d.mu.RUnlock()

	// Every txSource now carries its own success column directly (see
	// package_submissions), so no source needs a join out to transactions
	// for it. join stays as a variable, not inlined, so a future source that
	// does need one can reintroduce it the same way without restructuring
	// the query below.
	join := ""

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

// GovDAOPathPrefix is the realm the governance view is about.
//
// A prefix, not a substring: paths are `gno.land/r/gov/dao` and its versioned
// subpackages, and an anchored pattern can walk the (network, pkg_path) index
// instead of scanning. It also cannot pick up gnoswap's `gov/staker` and
// `gov/governance`, which a bare "gov" would.

func (d *DB) GovDAOCalls(network string, limit int) ([]StoredTx, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	rows, err := d.db.Query(`
		SELECT network, tx_hash, block_height, COALESCE(block_time, ''),
		       caller, pkg_path || '::' || func_name, success
		FROM calls
		WHERE pkg_path LIKE ? AND `+d.networkFilter("network", network)+`
		ORDER BY block_height DESC, tx_hash ASC
		LIMIT ?`, GovDAOPathPrefix+"%", limit)
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
	d.writeMu.Lock()
	defer d.writeMu.Unlock()

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

// BlockTick is one block, reduced to the two fields a cadence view needs.
type BlockTick struct {
	Height int    `json:"height"`
	Time   string `json:"time"`
	Txs    int    `json:"txs"`
}

// RecentBlockTimes returns this network's blocks since a cutoff, oldest first.
//
// For the heartbeat strip, which needs *when each block arrived* rather than
// how many arrived per bucket. Bucketing happens in the caller: a chain with
// ~3s blocks produces a couple of hundred rows over the windows this serves,
// which is cheaper to bucket in Go than to coax out of SQLite's date handling,
// and it lets one query serve every cell size.
//
// `limit` is a guard, not a feature. A caller asking for a huge window on a
// fast chain gets the newest rows rather than the whole table.
func (d *DB) RecentBlockTimes(network string, since time.Time, limit int) ([]BlockTick, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	// Ordered newest-first so the limit keeps the newest rows, then reversed
	// below: the strip reads left to right in time.
	rows, err := d.db.Query(
		`SELECT height, time, num_txs FROM blocks
		  WHERE network = ? AND time >= ?
		  ORDER BY height DESC LIMIT ?`,
		network, since.UTC().Format(time.RFC3339), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	ticks := []BlockTick{}
	for rows.Next() {
		var t BlockTick
		if err := rows.Scan(&t.Height, &t.Time, &t.Txs); err != nil {
			return nil, err
		}
		ticks = append(ticks, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i, j := 0, len(ticks)-1; i < j; i, j = i+1, j-1 {
		ticks[i], ticks[j] = ticks[j], ticks[i]
	}
	return ticks, nil
}

type BlockTimeBin struct {
	Bin    string `json:"bin"`
	Blocks int    `json:"blocks"`
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
		`SELECT value FROM sync_state WHERE key = ?`, BlocksBackfillDoneKey(network),
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
//
// package_submissions rather than packages, because both readings are about
// when something happened. packages keeps one row per path and stamps it with
// the latest submission, which moved a message out of the heatmap cell it
// actually occurred in, and — worse for first-seen — reported an address's
// debut as the time of a resubmission years later.

type FuncCallCell struct {
	Func  string `json:"func"`
	Day   string `json:"day"`
	Calls int    `json:"calls"`
}

// funcHeatmapMaxFuncs caps the rows of the function heatmap. A realm with 200
// exported functions would otherwise render rows one pixel tall.

func (d *DB) GetFunctionCallHeatmap(network, pkgPath string, days int) ([]FuncCallCell, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	if pkgPath == "" {
		return nil, nil
	}
	now := time.Now().UTC()
	firstDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -(days - 1))

	netFilter := " AND " + d.networkFilter("network", network)
	args := []any{pkgPath, firstDay.Format(time.RFC3339)}
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

// --- Active-address rollup ---------------------------------------------------

// activeAddrKinds are the three activities that make an address active, and the
// values stored in active_addr_rollup.kind.
//
// They are named exactly as the series has always named them, so the read path
// scans `kind` straight into the field it already had a case for. msg_runs is
// deliberately absent: the series has never counted it, and adding it here would
// change the numbers under cover of a performance change.
//
// deployers reads package_submissions: the rollup buckets by hour, and an
// address whose only activity in some hour was a submission later overwritten
// was absent from that hour entirely.

// LastBlockHeight is the highest block_height stored for a network in one of
// the per-message tables, or nil when that table holds nothing for it.
//
// Lives here rather than in the syncer that calls it. The syncer used to reach
// through to the store's *sql.DB and run this itself, which is the kind of
// layering violation a single package hides — the query knows the schema, so it
// belongs with the schema.
//
// tableName is interpolated, so it must never be caller-supplied; every call
// site names a table literally.
func (d *DB) LastBlockHeight(ctx context.Context, tableName, network string) (*int, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var lastHeight int
	err := d.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT block_height
		FROM %s
		WHERE network = $1
		ORDER BY block_height DESC
		LIMIT 1`, tableName), network).Scan(&lastHeight)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("last block height in %s: %w", tableName, err)
	}
	return &lastHeight, nil
}

// LastCallOrSendHeight is the highest block_height across calls and bank_sends
// for a network — where the transaction walk resumes from.
func (d *DB) LastCallOrSendHeight(ctx context.Context, network string) (*int, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var lastHeight int
	err := d.db.QueryRowContext(ctx, `SELECT block_height
		FROM calls
		WHERE network = $1
		UNION
		SELECT block_height
		FROM bank_sends
		WHERE network = $1
		ORDER BY block_height DESC
		LIMIT 1`, network).Scan(&lastHeight)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("last call or send height: %w", err)
	}
	return &lastHeight, nil
}
