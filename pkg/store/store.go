package store

import (
	"database/sql"
	"fmt"
	"regexp"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

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
	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	_, err := d.db.Exec(`
		INSERT OR REPLACE INTO sync_state (key, value) VALUES (?, ?)
	`, key, value)
	return err
}

// NetworkScopedTables lists every table whose rows belong to a single network.

var NetworkScopedTables = []string{
	"packages",
	"package_files",
	"dependencies",
	"calls",
	"msg_runs",
	"bank_sends",
	"transactions",
	"blocks",
	"proposers",
	// The user registry is per chain: the same name can belong to different
	// accounts on two of them, and a reset chain that kept the old one would
	// have the search box answering with names nobody holds. The syncer replays
	// the whole registry from genesis on every pass, so wiping costs nothing to
	// recover.
	"users",
	// The edge rollups belong here for a reason their source tables do not make
	// obvious: their sync cursor is MAX(last_height) over their own rows. Left
	// behind by a reset, they would hold a dead chain's edges *and* a cursor
	// above the new chain's tip, so the replacement chain's transfers would
	// never be folded in and the graphs would show the old chain forever,
	// silently. Wiping them resets the cursor to zero as a side effect, which
	// is exactly what a re-sync from the new genesis needs.
	"transfer_edges",
	"caller_edges",
	// Read counts are this explorer's own measurement, but they are keyed by a
	// realm path on one chain. A reset means that chain's paths are gone, and
	// keeping the counts would hand a rebuilt chain the popularity of a dead
	// one, silently and in the direction of looking more used than it is.
	"realm_views",
	// The native coin ledger. A reset leaves a dead chain's legs behind, and
	// because the defi tab sums them against a *live* bank/balances read, the
	// two would disagree by the whole of the old chain's history and the page
	// would report the gap as an indexer problem. Its backfill markers go with
	// it, in DeleteNetworkData: a cursor above the new chain's tip would mark
	// the history closed before any of it had been read.
	"coin_transfers",
	// first_seen is a rollup of the tables above, in the same class as
	// caller_edges (963617b): a chain whose block-1 fingerprint changed has a
	// new set of participants, and keeping the old chain's first-appearance
	// dates would make every one of them look like a returning actor and the
	// "new this week" feed permanently empty.
	"first_seen",
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
	d.writeMu.Lock()
	defer d.writeMu.Unlock()

	tx, err := d.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	var total int64
	for _, table := range NetworkScopedTables {
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
		`DELETE FROM sync_state WHERE key = ?`, BlocksBackfillDoneKey(network),
	); err != nil {
		return 0, fmt.Errorf("clear blocks backfill flag: %w", err)
	}
	// Both coin-ledger markers, for the same reason and with the same failure
	// if forgotten: a done flag marks an emptied table as fully backfilled, and
	// a cursor left above the new chain's tip means the walk that would refill
	// it never starts.
	for _, key := range []string{CoinBackfillDoneKey(network), CoinBackfillCursorKey(network)} {
		if _, err := tx.Exec(`DELETE FROM sync_state WHERE key = ?`, key); err != nil {
			return 0, fmt.Errorf("clear coin backfill state: %w", err)
		}
	}

	// Derived rows go too, in the same transaction.
	//
	// Not counted in the total: the caller reports how much *data* a reset threw
	// away, and a rollup tuple is a restatement of rows already counted above.
	// Leaving them would keep the series reporting activity for a chain whose
	// history has just been deleted, until the next refresh up to five minutes
	// later — which is the stale-after-reset shape #19 was about.
	if _, err := tx.Exec(`DELETE FROM active_addr_rollup WHERE network = ?`, network); err != nil {
		return 0, fmt.Errorf("delete from active_addr_rollup: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return total, nil
}

// blockTimeTables lists tables whose rows carry a block_time worth backfilling.
// package_files and dependencies have no height of their own.

type FileInfo struct {
	Name string `json:"name"`
	Body string `json:"body"`
}

// searchKindLimit is how many rows each of the two kinds is guaranteed.
//
// The search box draws realms and packages as separate groups, so a single flat
// LIMIT is the wrong shape: one namespace's realms can fill it and leave the
// package group empty, which reads as "this namespace has no packages" rather
// than "you are looking at twenty realms". Ten each keeps both groups populated
// and the popup the same total size it was.
const searchKindLimit = 10

func (d *DB) Search(network, q string) ([]PackageInfo, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	// The counts are selected, not left to the scan: this used to read eight
	// columns into eleven destinations, so every search returned
	// "expected 8 destination arguments in Scan, not 11" and the site's search
	// box was dead for any query. Keep the SELECT list, the inner aliases and
	// the Scan destinations in step; unique_users was the one that got left
	// behind after that fix and answered 0 on every row for months.
	//
	// Windowed by is_realm so the two kinds are capped independently, and
	// ordered realms first: a realm is a thing a reader can open and use, a
	// package is a library it imports, and asked for "moul" the first answer
	// wanted is the former. Within a kind the order is still recency.
	qStr := `
		SELECT network, path, name, creator, block_height, tx_hash, is_realm, num_files,
		       calls, importers, imports, unique_users
		  FROM (
			SELECT p.network, p.path, p.name, p.creator, p.block_height, p.tx_hash,
			       p.is_realm, p.num_files,
			       (SELECT COUNT(*) FROM calls c WHERE c.network = p.network AND c.pkg_path = p.path) AS calls,
			       (SELECT COUNT(*) FROM dependencies d WHERE d.network = p.network AND d.import_path = p.path) AS importers,
			       (SELECT COUNT(*) FROM dependencies d WHERE d.network = p.network AND d.package_path = p.path) AS imports,
			       -- COUNT(DISTINCT caller), the same definition ListPackages uses.
			       -- Selected here because PackageInfo carries the field and a
			       -- column left unselected does not read as absent: it reads as
			       -- a confident zero, and a search row claiming a busy realm has
			       -- no users is worse than one that says nothing.
			       (SELECT COUNT(DISTINCT c.caller) FROM calls c WHERE c.network = p.network AND c.pkg_path = p.path) AS unique_users,
			       ROW_NUMBER() OVER (PARTITION BY p.is_realm ORDER BY p.block_height DESC) AS rn
			  FROM packages p
			 WHERE (p.path LIKE ? OR p.name LIKE ? OR p.creator LIKE ?)`
	args := []any{"%" + q + "%", "%" + q + "%", "%" + q + "%"}
	qStr += ` AND ` + d.networkFilter("p.network", network)
	qStr += `
		  )
		 WHERE rn <= ?
		 ORDER BY is_realm DESC, block_height DESC`
	args = append(args, searchKindLimit)

	rows, err := d.db.Query(qStr, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var pkgs []PackageInfo
	for rows.Next() {
		var p PackageInfo
		if err := rows.Scan(&p.Network, &p.Path, &p.Name, &p.Creator, &p.BlockHeight, &p.TxHash,
			&p.IsRealm, &p.NumFiles, &p.Calls, &p.Importers, &p.Imports, &p.UniqueUsers); err != nil {
			return nil, err
		}
		pkgs = append(pkgs, p)
	}
	return pkgs, rows.Err()
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

// amountExpr sums ugnot over bank_sends.
//
// It reads the column parsed at write time rather than the coin string. The
// string cannot be summed in SQL: there is no split, so the denom had to come
// out with REPLACE, and CAST(... AS INTEGER) takes the leading numeric prefix
// and silently discards the rest rather than erroring. That read "5foo,100ugnot"
// as 5 and "5foo" as 5, neither of which is a ugnot figure at all.
const amountExpr = `COALESCE(SUM(ugnot_amount), 0)`

type ImportRank struct {
	Network string `json:"network,omitempty"`
	Path    string `json:"path"`
	Imports int    `json:"imports"`
}

type PkgTimePoint struct {
	Time     string `json:"time"`
	Packages int    `json:"packages"`
	Realms   int    `json:"realms"`
	Total    int    `json:"total"`
}

type HealthTimePoint struct {
	Time        string  `json:"time"`
	Total       int     `json:"total"`
	Success     int     `json:"success"`
	Failed      int     `json:"failed"`
	SuccessRate float64 `json:"success_rate"`
}

func activeAddrPoints(
	buckets map[string]*ActiveAddressTimePoint,
	days int,
	granularity string,
	step time.Duration,
	truncFn func(time.Time) time.Time,
	now time.Time,
) []ActiveAddressTimePoint {
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
	return out
}

// AddressLabel is a display name for an address, derived from on-chain data.

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

const GovDAOPathPrefix = "gno.land/r/gov/dao"

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

func (d *DB) NetworkDataStart(network string) (time.Time, bool, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	parts := make([]string, 0, len(blockTimeSources))
	for _, s := range blockTimeSources {
		// "all networks" means every configured network. It matters here more
		// than most: this start date sizes the window=all range, so counting a
		// retired chain's earliest row would stretch every all-window request
		// back to a chain the instance no longer serves.
		filter := " AND " + d.networkFilter(s.table+".network", network)
		// Table and column names come from the constant above, never from input.
		parts = append(parts, fmt.Sprintf(
			"SELECT MIN(%[1]s.%[2]s) AS t FROM %[1]s WHERE %[1]s.%[2]s IS NOT NULL AND %[1]s.%[2]s != ''%[3]s",
			s.table, s.col, filter))
	}

	var earliest sql.NullString
	q := "SELECT MIN(t) FROM (" + strings.Join(parts, " UNION ALL ") + ")"
	if err := d.db.QueryRow(q).Scan(&earliest); err != nil {
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
	// RollingMaxDays caps the rolling series independently of the general
	// 365-day timeseries cap: it is always a daily series (the handler drops
	// granularity), so days and output points are the same number, and nothing
	// upstream bounds it — parseTimeseriesParams exempts "monthly" from its cap,
	// and window=all on an empty database falls back to a fixed multi-year
	// (allWindowDays, monthly) mapping that reaches this endpoint too.
	RollingMaxDays = 365
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

const funcHeatmapMaxFuncs = 20

// RealmsWithCallsMaxLimit caps ?limit= on the realm selector. It feeds a
// dropdown, not a paginated list, so there is no legitimate reason to ask for
// more than this many rows; without a cap an unvalidated limit is unbounded.

var activeAddrKinds = []struct{ kind, table, column string }{
	{"callers", "calls", "caller"},
	{"deployers", "package_submissions", "creator"},
	{"senders", "bank_sends", "from_address"},
}

// refreshActiveAddrRollup rebuilds the distinct (network, hour, kind, address)
// tuples for every configured network. Shares the caller's transaction, so
// readers see either the whole previous generation or the whole new one.
//
// A full rebuild rather than an append from a watermark. The append would stay
// flat as history grows, where this scales with all of it — 2.5s on production
// today — but it would need a reset hook to be exactly right across chain
// resets and backfills, and getting that wrong leaves tuples that no later pass
// revisits. The gas rollups made the same trade for the same reason; a periodic
// recompute is idempotent by construction. Revisit when the rebuild, not the
// query, is what costs.
