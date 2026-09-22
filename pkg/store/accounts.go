package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

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
	d.writeMu.Lock()
	defer d.writeMu.Unlock()
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

type GasCaller struct {
	Address string `json:"address"`
	Gas     int    `json:"gas"`
	Fees    int    `json:"fees"`
	TxCount int    `json:"tx_count"`
}

// GasTx is a single expensive transaction.

type AccountInfo struct {
	Address     string `json:"address"`
	Network     string `json:"network,omitempty"`
	CallCount   int    `json:"call_count"`
	DeployCount int    `json:"deploy_count"`
	MsgRunCount int    `json:"msgrun_count"`
	SendCount   int    `json:"send_count"`
	// CallTxCount is the number of distinct transactions CallCount's messages
	// came from — always <= CallCount, and strictly less exactly when at
	// least one of them was a multicall. Since the msg_index fix (#152)
	// CallCount already counts every bundled message, so this is what lets a
	// reader tell "225 calls across 225 transactions" from "225 calls, one of
	// them a single 200-message multicall" — the two read identically without it.
	CallTxCount int `json:"call_tx_count"`
	// SentAmount and ReceivedAmount are ugnot sums over bank_sends where this
	// address was the sender or receiver, respectively.
	SentAmount     int64 `json:"sent_amount"`
	ReceivedAmount int64 `json:"received_amount"`
	// Balance is live RPC state, not something this query can answer — it
	// leaves this empty, and HandleAccounts fills it in per row afterward.
	// Unlike the address page, a row here already pins one concrete network,
	// so there is no "which chain" ambiguity to gate it on.
	Balance string `json:"balance,omitempty"`
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
	amountSum := `SUM(ugnot_amount)`
	q := `
		SELECT address, network, SUM(call_count), SUM(call_tx_count), SUM(deploy_count), SUM(run_count), SUM(send_count), SUM(sent_amount), SUM(received_amount)
		FROM (
			SELECT caller as address, network, COUNT(*) as call_count, COUNT(DISTINCT tx_hash) as call_tx_count, 0 as deploy_count, 0 as run_count, 0 as send_count, 0 as sent_amount, 0 as received_amount FROM calls` + nFilter + ` GROUP BY network, caller
			UNION ALL
			SELECT creator as address, network, 0, 0, COUNT(*), 0, 0, 0, 0 FROM package_submissions` + nFilter + ` GROUP BY network, creator
			UNION ALL
			SELECT caller as address, network, 0, 0, 0, COUNT(*), 0, 0, 0 FROM msg_runs` + nFilter + ` GROUP BY network, caller
			UNION ALL
			SELECT from_address as address, network, 0, 0, 0, 0, COUNT(*), ` + amountSum + `, 0 FROM bank_sends` + nFilter + ` GROUP BY from_address, network
			UNION ALL
			SELECT to_address as address, network, 0, 0, 0, 0, COUNT(*), 0, ` + amountSum + ` FROM bank_sends` + nFilter + ` GROUP BY to_address, network
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
		if err := rows.Scan(&a.Address, &a.Network, &a.CallCount, &a.CallTxCount, &a.DeployCount, &a.MsgRunCount, &a.SendCount, &a.SentAmount, &a.ReceivedAmount); err != nil {
			return nil, err
		}
		accounts = append(accounts, a)
	}
	return accounts, rows.Err()
}

type CallerActivity struct {
	Network string `json:"network,omitempty"`
	Address string `json:"address"`
	Calls   int    `json:"calls"`
	Realms  int    `json:"realms"`
}

type CallerTimePoint struct {
	Time            string `json:"time"`
	UniqueCallers   int    `json:"unique_callers"`
	UniqueDeployers int    `json:"unique_deployers"`
	UniqueSenders   int    `json:"unique_senders"`
}

// timeseriesFormat returns the SQLite strftime pattern, step duration, and truncation function
// for hourly/daily/weekly/monthly granularity.

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
		perChainBucketCount(sqlFmt, "deployers", "t.creator", "package_submissions t", netFilter),
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

type ActiveAddressTimePoint struct {
	Time            string `json:"time"`
	TotalActive     int    `json:"total_active"`
	UniqueCallers   int    `json:"unique_callers"`
	UniqueDeployers int    `json:"unique_deployers"`
	UniqueSenders   int    `json:"unique_senders"`
}

func (d *DB) GetActiveAddressTimeSeries(network, granularity string, days int) ([]ActiveAddressTimePoint, error) {
	return d.activeAddressTimeSeriesAt(network, granularity, days, time.Now().UTC())
}

// activeAddressTimeSeriesAt is the same read with the window's instant handed
// in rather than taken.
//
// A 7-day window opens at "now minus 7 days", to the second, and both paths
// used to call time.Now() for themselves, twice each counting the dense-series
// fill. Production never noticed: one request's reads land microseconds apart.
// A test that reads the series twice and compares them notices, because a
// rollup build runs between the two reads, and a minute boundary crossed in
// there moves the window a minute later and drops exactly one seeded address
// out of the first bucket.
//
// So the instant is a parameter, and every read inside one call derives from
// it. Behaviour is unchanged; what changes is that "the window" is now one
// value rather than four samples of a clock.
func (d *DB) activeAddressTimeSeriesAt(network, granularity string, days int, now time.Time) ([]ActiveAddressTimePoint, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	if boundary, ok := d.activeAddrRollupBoundary(); ok {
		return d.activeAddrSeriesRolledUp(network, granularity, days, boundary, now)
	}
	return d.activeAddrSeriesLive(network, granularity, days, now)
}

// activeAddressTimeSeriesLiveAt forces the reference path, whatever the rollup
// holds. Only the tests want this: the rolled-up path is correct exactly when
// it agrees with the live one, and once a rollup exists there is otherwise no
// way to ask for the thing it is being compared against.
func (d *DB) activeAddressTimeSeriesLiveAt(network, granularity string, days int, now time.Time) ([]ActiveAddressTimePoint, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	return d.activeAddrSeriesLive(network, granularity, days, now)
}

// activeAddrSeriesLive computes the series from calls, packages and bank_sends.
//
// This is what the endpoint used to do on every request: 15.9s on production,
// growing with the chain. It stays as the fallback for a database whose rollup
// has not been built yet, and as the reference the rollup path is diffed
// against. Callers hold the read lock.

type AddressLabel struct {
	Label string `json:"label"`
	Kind  string `json:"kind"`
	Why   string `json:"why"`
}

// namespaceLabelMinPackages is how many packages a deployer must have published
// under a namespace before that namespace names it. Below this the evidence is
// one or two deploys, which is as easily a guest as an owner.

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
		// "derived" is the provenance taxonomy the explorer renders by (see
		// pkg/registry): proved from chain data and recomputed on every
		// request, as opposed to a human's assertion or a heuristic. The
		// specific rule that proved it lives in Why, which is what a reader
		// checks it against.
		labels[creator] = AddressLabel{
			Label: "@" + best,
			Kind:  "derived",
			Why:   fmt.Sprintf("sole deployer of gno.land/*/%s/* (%d packages)", best, n),
		}
	}
	return labels, nil
}

// --- Watchlist ------------------------------------------------------------

// WatchedRealm is one realm's activity summary, for a watchlist digest.

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
		// package_submissions, not packages — see AddressTransactions' own
		// comment on the same swap: packages only ever keeps
		// the latest submission per path.
		d.db.QueryRow(`SELECT COUNT(*) FROM package_submissions WHERE creator = ? AND `+nf, item.ID).Scan(&w.Deploys)
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
			UNION ALL SELECT block_height, block_time, network FROM package_submissions WHERE creator = ? AND `+nf+`
			UNION ALL SELECT block_height, block_time, network FROM bank_sends WHERE from_address = ? AND `+nf+`
			UNION ALL SELECT block_height, block_time, network FROM bank_sends WHERE to_address = ? AND `+nf+`
		) ORDER BY block_height DESC LIMIT 1`,
			item.ID, item.ID, item.ID, item.ID).Scan(&height, &when, &w.Network)
		w.LastHeight, w.LastTime = int(height.Int64), when.String

		if item.Since > 0 {
			d.db.QueryRow(`SELECT COUNT(*) FROM (
				SELECT block_height FROM calls WHERE caller = ? AND block_height > ? AND `+nf+`
				UNION ALL SELECT block_height FROM package_submissions WHERE creator = ? AND block_height > ? AND `+nf+`
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
		// package_submissions, not packages: packages is a current-state
		// projection (one row per path, overwritten by a later submission at
		// the same path), so it silently drops every resubmission but the
		// newest from a creator's own history, which showed 187 MsgAddPackage
		// against 256 on-chain on the account that surfaced it. Real
		// per-submission success now, not a hardcoded 1.
		branch(`network, tx_hash, block_height, COALESCE(block_time,''), 'MsgAddPackage',
		        creator, path, success`, "package_submissions", "creator = ?"),
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
		"SELECT tx_hash FROM package_submissions WHERE creator = ? AND " + nf,
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

// WatchTransactions returns the most recent transactions touching any of the
// given realm paths or addresses, merged into one timeline — the row shape
// AddressTransactions uses, generalised from one address to a whole
// watchlist. Empty if both lists are empty.
//
// Same bounded-branch-then-union shape as AddressTransactions and for the
// same reason: unbounded, the union has to be fully materialised and sorted
// before the outer LIMIT can pick anything.
//
// A UNION (not UNION ALL) merges the branches: a watched address calling a
// watched realm matches both the realm-path and the caller branch of calls
// with an identical row, and UNION's own deduplication is what keeps that
// from showing up twice rather than needing a second pass here.

func (d *DB) WatchTransactions(network string, realms, addresses []string, limit int) ([]StoredTx, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	if len(realms) == 0 && len(addresses) == 0 {
		return []StoredTx{}, nil
	}

	nf := d.networkFilter("network", network)
	inClause := func(n int) string {
		return "(" + strings.TrimSuffix(strings.Repeat("?,", n), ",") + ")"
	}
	branch := func(sel, table, cond string) string {
		return fmt.Sprintf("SELECT * FROM (SELECT %s FROM %s WHERE %s AND %s"+
			" ORDER BY block_height DESC LIMIT %d)", sel, table, cond, nf, limit)
	}

	var branches []string
	var args []any

	if len(realms) > 0 {
		branches = append(branches, branch(`network, tx_hash, block_height, COALESCE(block_time,'') bt, 'MsgCall' typ,
		        caller who, pkg_path || '::' || func_name detail, success`,
			"calls", "pkg_path IN "+inClause(len(realms))))
		for _, r := range realms {
			args = append(args, r)
		}
		// package_submissions, not packages — see AddressTransactions'
		// comment on the same swap.
		branches = append(branches, branch(`network, tx_hash, block_height, COALESCE(block_time,'') bt, 'MsgAddPackage' typ,
		        creator who, path detail, success`,
			"package_submissions", "path IN "+inClause(len(realms))))
		for _, r := range realms {
			args = append(args, r)
		}
	}
	if len(addresses) > 0 {
		branches = append(branches, branch(`network, tx_hash, block_height, COALESCE(block_time,'') bt, 'MsgCall' typ,
		        caller who, pkg_path || '::' || func_name detail, success`,
			"calls", "caller IN "+inClause(len(addresses))))
		for _, a := range addresses {
			args = append(args, a)
		}
		branches = append(branches, branch(`network, tx_hash, block_height, COALESCE(block_time,'') bt, 'MsgAddPackage' typ,
		        creator who, path detail, success`,
			"package_submissions", "creator IN "+inClause(len(addresses))))
		for _, a := range addresses {
			args = append(args, a)
		}
		branches = append(branches, branch(`network, tx_hash, block_height, COALESCE(block_time,'') bt, 'MsgRun' typ,
		        caller who, '' detail, success`,
			"msg_runs", "caller IN "+inClause(len(addresses))))
		for _, a := range addresses {
			args = append(args, a)
		}
		branches = append(branches, branch(`network, tx_hash, block_height, COALESCE(block_time,'') bt, 'BankMsgSend' typ,
		        from_address who, to_address || ' ' || amount detail, success`,
			"bank_sends", "(from_address IN "+inClause(len(addresses))+" OR to_address IN "+inClause(len(addresses))+")"))
		for _, a := range addresses {
			args = append(args, a)
		}
		for _, a := range addresses {
			args = append(args, a)
		}
	}

	union := strings.Join(branches, " UNION ")

	rows, err := d.db.Query(`
		SELECT e.network, e.tx_hash, e.block_height, e.bt, e.typ, e.who, e.detail, e.success,
		       COALESCE(t.gas_used, 0), COALESCE(t.gas_fee, 0)
		FROM (`+union+`) e
		LEFT JOIN transactions t ON t.network = e.network AND t.tx_hash = e.tx_hash
		ORDER BY e.block_height DESC, e.tx_hash ASC LIMIT ?`,
		append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []StoredTx{}
	for rows.Next() {
		var t StoredTx
		if err := rows.Scan(&t.Network, &t.Hash, &t.BlockHeight, &t.BlockTime,
			&t.Type, &t.Caller, &t.Detail, &t.Success, &t.GasUsed, &t.GasFee); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// --- Gas rollups ------------------------------------------------------------

// rollupComputedAtKey records when the rollups were last recomputed, so the page
// can say how fresh its numbers are rather than presenting stale ones as live —
// and so the active-address series knows from which instant it has to stop
// trusting stored tuples and read the source tables instead.
//
// The stored key still says "gas" because it was written by the first rollup to
// need it and renaming it would reset every deployed instance's freshness stamp
// to "never built" for one refresh interval.

func (d *DB) InternProposer(network, address string) (int64, error) {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()

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

type ProposerCount struct {
	Address string `json:"address"`
	Blocks  int    `json:"blocks"`
}

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

	// "all networks" means every configured network, never "no filter": an
	// unfiltered query also counts chains that were retired but whose rows are
	// still stored.
	netFilter := " AND " + d.networkFilter("t.network", network)
	parts := make([]string, 0, len(activityMsgTables))
	args := make([]any, 0, len(activityMsgTables))
	for _, s := range activityMsgTables {
		// Column and table names come from the constant above, never from input.
		parts = append(parts, fmt.Sprintf(
			"SELECT t.%s AS addr, t.block_time AS bt FROM %s t"+
				" WHERE t.block_time IS NOT NULL AND t.block_time != ''%s",
			s.addrCol, s.table, netFilter))
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
