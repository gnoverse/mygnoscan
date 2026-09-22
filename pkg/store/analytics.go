package store

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

func (d *DB) InsertBankSend(network, txHash string, blockHeight int, blockTime, from, to, amount string, success bool) error {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	_, err := d.db.Exec(`INSERT OR IGNORE INTO bank_sends (network, tx_hash, block_height, block_time, from_address, to_address, amount, ugnot_amount, success) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		network, txHash, blockHeight, blockTime, from, to, amount, ParseUgnot(amount), success)
	return err
}

// GetSyncState reads a sync state value.

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
	TopCallers     []GasCaller
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
	//
	// Deduplicated on (network, tx_hash, path) within each branch before
	// summing: a transaction's gas is paid once, and since the multicall fix a
	// single tx_hash can carry several `calls` rows bundling different
	// functions on the same realm (one per message), which would otherwise
	// multiply that transaction's gas by how many of its messages targeted
	// this realm. Same reasoning as the gas-by-caller query below.
	realmWhere := " AND " + d.networkFilter("t.network", network)
	realmArgs := []any{}

	realmQuery := `
		SELECT path, SUM(gas_used), SUM(gas_fee), COUNT(*) FROM (
			SELECT DISTINCT c.pkg_path AS path, t.tx_hash, t.gas_used, t.gas_fee
			  FROM calls c JOIN transactions t
			    ON t.network = c.network AND t.tx_hash = c.tx_hash` + realmWhere + `
			UNION
			SELECT DISTINCT p.path AS path, t.tx_hash, t.gas_used, t.gas_fee
			  FROM package_submissions p JOIN transactions t
			    ON t.network = p.network AND t.tx_hash = p.tx_hash` + realmWhere + `
			UNION
			SELECT DISTINCT 'MsgRun by ' || m.caller AS path, t.tx_hash, t.gas_used, t.gas_fee
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

	// Same attribution, grouped by caller instead of realm. Deduplicated on
	// (network, tx_hash, caller) within each branch before summing: a
	// transaction's gas is paid once, and since the multicall fix a single
	// tx_hash can carry several `calls` rows (one per bundled message), which
	// would otherwise multiply that transaction's gas by how many messages it
	// held. bank_sends is included here but not in the realm attribution above
	// — a send touches no realm, but it is still gas its sender paid.
	callerQuery := `
		SELECT caller, SUM(gas_used), SUM(gas_fee), COUNT(*) FROM (
			SELECT DISTINCT c.caller AS caller, t.tx_hash, t.gas_used, t.gas_fee
			  FROM calls c JOIN transactions t
			    ON t.network = c.network AND t.tx_hash = c.tx_hash` + realmWhere + `
			UNION
			SELECT DISTINCT p.creator AS caller, t.tx_hash, t.gas_used, t.gas_fee
			  FROM package_submissions p JOIN transactions t
			    ON t.network = p.network AND t.tx_hash = p.tx_hash` + realmWhere + `
			UNION
			SELECT DISTINCT m.caller AS caller, t.tx_hash, t.gas_used, t.gas_fee
			  FROM msg_runs m JOIN transactions t
			    ON t.network = m.network AND t.tx_hash = m.tx_hash` + realmWhere + `
			UNION
			SELECT DISTINCT b.from_address AS caller, t.tx_hash, t.gas_used, t.gas_fee
			  FROM bank_sends b JOIN transactions t
			    ON t.network = b.network AND t.tx_hash = b.tx_hash` + realmWhere + `
		) GROUP BY caller ORDER BY SUM(gas_used) DESC LIMIT ?`
	if rollupReady {
		callerQuery = `
		SELECT caller, SUM(gas_used), SUM(gas_fee), SUM(tx_count)
		FROM gas_caller_rollup WHERE ` + d.networkFilter("network", network) + `
		GROUP BY caller ORDER BY SUM(gas_used) DESC LIMIT ?`
	}
	callerRows, err := d.db.Query(callerQuery, append(realmArgs, topN)...)
	if err != nil {
		return nil, fmt.Errorf("gas by caller: %w", err)
	}
	for callerRows.Next() {
		var c GasCaller
		if err := callerRows.Scan(&c.Address, &c.Gas, &c.Fees, &c.TxCount); err != nil {
			callerRows.Close()
			return nil, err
		}
		out.TopCallers = append(out.TopCallers, c)
	}
	callerRows.Close()
	if err := callerRows.Err(); err != nil {
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
		    (SELECT 'MsgAddPackage' FROM package_submissions p WHERE p.network = t.network AND p.tx_hash = t.tx_hash LIMIT 1),
		    (SELECT 'MsgRun' FROM msg_runs m WHERE m.network = t.network AND m.tx_hash = t.tx_hash LIMIT 1),
		    (SELECT 'BankMsgSend' FROM bank_sends b WHERE b.network = t.network AND b.tx_hash = t.tx_hash LIMIT 1),
		    ''),
		  COALESCE(
		    (SELECT c.pkg_path || '::' || c.func_name FROM calls c WHERE c.network = t.network AND c.tx_hash = t.tx_hash LIMIT 1),
		    (SELECT p.path FROM package_submissions p WHERE p.network = t.network AND p.tx_hash = t.tx_hash LIMIT 1),
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

type Stats struct {
	TotalTxs     int `json:"total_txs"`
	TotalCalls   int `json:"total_calls"`
	TotalDeploys int `json:"total_deploys"`
	// StorageDeposit is the chain's net storage cost in ugnot and StorageBytes
	// the data currently held, both net of unlocks. Zero is a real answer on a
	// chain whose realms have all been refunded; it is not "unknown".
	StorageDeposit int `json:"storage_deposit"`
	StorageBytes   int `json:"storage_bytes"`
	TotalMsgRuns   int `json:"total_msg_runs"`
	TotalSends     int `json:"total_sends"`
	TotalRealms    int `json:"total_realms"`
	TotalPackages  int `json:"total_packages"`
	UniqueCallers  int `json:"unique_callers"`
	LatestBlock    int `json:"latest_block"`
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
	d.db.QueryRow(`SELECT COUNT(*) FROM package_submissions` + nf).Scan(&s.TotalDeploys)
	d.db.QueryRow(`SELECT COUNT(*) FROM packages` + nf + ` AND is_realm = 1`).Scan(&s.TotalRealms)
	// Counted, not derived as TotalDeploys - TotalRealms.
	//
	// That subtraction held only while TotalDeploys was a row count over the
	// same collapsed table. Now that it counts submissions, a realm resubmitted
	// once would have appeared as a phantom non-realm package — the home page's
	// "packages" tile inventing an entry nobody deployed.
	d.db.QueryRow(`SELECT COUNT(*) FROM packages` + nf + ` AND is_realm = 0`).Scan(&s.TotalPackages)
	d.db.QueryRow(`SELECT COUNT(*) FROM msg_runs` + nf).Scan(&s.TotalMsgRuns)
	d.db.QueryRow(`SELECT COUNT(*) FROM bank_sends` + nf).Scan(&s.TotalSends)
	s.TotalTxs = s.TotalCalls + s.TotalDeploys + s.TotalMsgRuns + s.TotalSends
	// Per (caller, network): the same address on two chains is two actors, and
	// collapsing them undercounts. 19,054 blended against 19,117 on production.
	d.db.QueryRow(`SELECT COUNT(*) FROM (SELECT DISTINCT caller, network FROM calls` + nf + `)`).Scan(&s.UniqueCallers)
	d.db.QueryRow(`SELECT COALESCE(MAX(block_height), 0) FROM packages` + nf).Scan(&s.LatestBlock)
	// Summed from the events rather than the rollup: the rollup is rebuilt on a
	// timer, and the headline figure should not lag the realm pages by a
	// refresh interval. One SUM over an indexed table is cheap enough here.
	d.db.QueryRow(`SELECT COALESCE(SUM(fee), 0), COALESCE(SUM(bytes_delta), 0) FROM storage_events`+nf).
		Scan(&s.StorageDeposit, &s.StorageBytes)
	return &s, nil
}

// Search searches across packages and callers.

type TokenInfo struct {
	Path      string `json:"path"`
	Network   string `json:"network,omitempty"`
	Name      string `json:"name"`
	Creator   string `json:"creator"`
	CallCount int    `json:"call_count"`
}

type BankStats struct {
	// ComputedAt is when the rollups behind these figures were built, empty if
	// computed live.
	ComputedAt      string     `json:"computed_at,omitempty"`
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

// bankTopRead is how long each of the three /coins leaderboards is. Ten fit on
// one screen but answered almost nothing: the page is asked "who moves the
// coin", and the tail is where the answer stops being the two faucets everybody
// already knows. Kept well under bankTopRollupLimit so a per-network read is
// never short.

const bankTopRead = 50

func (d *DB) GetBankStats(network string) (*BankStats, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	nFilter := " WHERE " + d.networkFilter("network", network)

	var s BankStats

	// Prefer the rollup. Seven passes over bank_sends, three of them
	// COUNT(DISTINCT) over the whole table, measured at 5.4s and growing.
	if ready, at := d.bankRollupReady(); ready {
		s.ComputedAt = at
		d.db.QueryRow(`SELECT COALESCE(SUM(total_sends),0), COALESCE(SUM(total_volume),0)
			FROM bank_totals_rollup`+nFilter).Scan(&s.TotalSends, &s.TotalVolume)

		// Counted per (address, network) — the same address on two chains is
		// two actors — so these sum across the rollup's per-network rows.
		d.db.QueryRow(`SELECT COALESCE(SUM(unique_senders),0), COALESCE(SUM(unique_receivers),0),
			COALESCE(SUM(unique_addresses),0) FROM bank_totals_rollup`+nFilter).
			Scan(&s.UniqueSenders, &s.UniqueReceivers, &s.UniqueAddresses)

		if network == "" {
			if rows, err := d.db.Query(`SELECT network, total_sends, total_volume
				FROM bank_totals_rollup` + nFilter); err == nil {
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

		top := func(kind, order string) []AddrStat {
			return d.queryAddrStats(`SELECT network, address, count, total FROM bank_top_rollup
				WHERE kind = '` + kind + `' AND ` + d.networkFilter("network", network) + `
				ORDER BY ` + order + ` DESC LIMIT ` + strconv.Itoa(bankTopRead))
		}
		s.TopSenders = top("sender", "count")
		s.TopReceiversVol = top("receiver_volume", "total")
		s.TopReceiversCnt = top("receiver_count", "count")
		return &s, nil
	}

	// Not built yet — a fresh database, or the first start after this shipped.
	// Computing live is slow but right; zeros would read as "no transfers".
	d.db.QueryRow(`SELECT COUNT(*) FROM bank_sends` + nFilter).Scan(&s.TotalSends)
	d.db.QueryRow(`SELECT ` + amountExpr + ` FROM bank_sends` + nFilter).Scan(&s.TotalVolume)

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

	d.db.QueryRow(`SELECT COUNT(*) FROM (SELECT DISTINCT addr, network FROM (
		SELECT from_address as addr, network FROM bank_sends` + nFilter + `
		UNION ALL SELECT to_address, network FROM bank_sends` + nFilter + `))`).Scan(&s.UniqueAddresses)
	d.db.QueryRow(`SELECT COUNT(*) FROM (SELECT DISTINCT from_address, network FROM bank_sends` + nFilter + `)`).Scan(&s.UniqueSenders)
	d.db.QueryRow(`SELECT COUNT(*) FROM (SELECT DISTINCT to_address, network FROM bank_sends` + nFilter + `)`).Scan(&s.UniqueReceivers)

	lim := ` LIMIT ` + strconv.Itoa(bankTopRead)
	s.TopSenders = d.queryAddrStats(`SELECT network, from_address, COUNT(*), ` + amountExpr + ` FROM bank_sends` + nFilter + ` GROUP BY network, from_address ORDER BY COUNT(*) DESC` + lim)
	s.TopReceiversVol = d.queryAddrStats(`SELECT network, to_address, COUNT(*), ` + amountExpr + ` FROM bank_sends` + nFilter + ` GROUP BY network, to_address ORDER BY ` + amountExpr + ` DESC` + lim)
	s.TopReceiversCnt = d.queryAddrStats(`SELECT network, to_address, COUNT(*), ` + amountExpr + ` FROM bank_sends` + nFilter + ` GROUP BY network, to_address ORDER BY COUNT(*) DESC` + lim)

	return &s, nil
}

// bankRollupReady reports whether the bank rollups hold anything, and when they
// were built. Callers hold the read lock.

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
	d.db.QueryRow(`SELECT COUNT(*) FROM package_submissions` + nFilter).Scan(&a.TotalDeploys)
	d.db.QueryRow(`SELECT COUNT(*) FROM msg_runs` + nFilter).Scan(&a.TotalMsgRuns)
	d.db.QueryRow(`SELECT COUNT(*) FROM bank_sends` + nFilter).Scan(&a.TotalSends)

	// Addresses are counted per chain. The same string on two chains is two
	// actors with two histories, and collapsing them undercounts: 68,506
	// blended against 68,806 counted honestly.
	addrUnionFilter := " WHERE " + d.networkFilter("network", network)
	// UNION ALL, not UNION.
	//
	// The outer SELECT DISTINCT already dedups on the same key (or a coarser
	// one), so an inner UNION pays for the same deduplication twice. Measured on
	// production: the active-address series went 4.81s -> 2.58s with byte-
	// identical results.
	d.db.QueryRow(`SELECT COUNT(*) FROM (SELECT DISTINCT addr, network FROM (
		SELECT caller as addr, network FROM calls` + addrUnionFilter + ` UNION ALL SELECT creator, network FROM packages` + addrUnionFilter + `
		UNION ALL SELECT caller, network FROM msg_runs` + addrUnionFilter + ` UNION ALL SELECT from_address, network FROM bank_sends` + addrUnionFilter + `
		UNION ALL SELECT to_address, network FROM bank_sends` + addrUnionFilter + `
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
	deployQ := `SELECT network, creator, COUNT(*) as c, 0 FROM package_submissions` + nFilter + ` GROUP BY network, creator ORDER BY c DESC LIMIT 15`
	rows5, _ := d.db.Query(deployQ)
	if rows5 != nil {
		defer rows5.Close()
		for rows5.Next() {
			var c CallerActivity
			rows5.Scan(&c.Network, &c.Address, &c.Calls, &c.Realms)
			a.TopDeployers = append(a.TopDeployers, c)
		}
	}

	// Recent realms.
	//
	// `p`-aliased because pFilter is `AND p.network = ...`, written for the two
	// joined queries above. Unaliased this was `no such column: p.network`, the
	// error went into a `_`, and the handler answered `recent_realms: null` on
	// every network of every deployment. Renaming the alias needs all three
	// changed together.
	recentQ := `SELECT p.network, p.path, p.name, p.creator, p.block_height, COALESCE(p.block_time, ''), p.tx_hash, p.is_realm, p.num_files FROM packages p WHERE p.is_realm = 1` + pFilter + ` ORDER BY p.block_height DESC LIMIT 10`
	rows6, _ := d.db.Query(recentQ)
	if rows6 != nil {
		defer rows6.Close()
		for rows6.Next() {
			var p PackageInfo
			rows6.Scan(&p.Network, &p.Path, &p.Name, &p.Creator, &p.BlockHeight, &p.BlockTime, &p.TxHash, &p.IsRealm, &p.NumFiles)
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

func perChainBucketCount(sqlFmt, typ, column, from, netFilter string) string {
	return fmt.Sprintf(
		"SELECT bucket, '%s' as typ, COUNT(*) as cnt FROM ("+
			" SELECT DISTINCT strftime('%s', t.block_time) as bucket, %s, t.network"+
			" FROM %s WHERE t.block_time >= ?%s"+
			") GROUP BY bucket",
		typ, sqlFmt, column, from, netFilter)
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
	// UNION ALL, not UNION.
	//
	// The outer SELECT DISTINCT already dedups on the same key (or a coarser
	// one), so an inner UNION pays for the same deduplication twice. Measured on
	// production: the active-address series went 4.81s -> 2.58s with byte-
	// identical results.
	addrQuery := `SELECT COUNT(*) FROM (SELECT DISTINCT addr, network FROM (
		SELECT caller as addr, network FROM calls WHERE block_time >= ?` + addrFilter + `
		UNION ALL SELECT creator, network FROM package_submissions WHERE block_time >= ?` + addrFilter + `
		UNION ALL SELECT from_address, network FROM bank_sends WHERE block_time >= ?` + addrFilter + `
	))`
	d.db.QueryRow(addrQuery, since24h, since24h, since24h).Scan(&ov.ActiveAddresses24h)

	// "New" means first submitted in the window, which is not what either table
	// says on its own.
	//
	// packages.block_time is whichever submission is currently live, so a
	// package first deployed ten days ago and resubmitted two days ago counted
	// as new — the opposite failure to the undercounts elsewhere in this pass.
	// package_submissions keeps every attempt, so a blind table swap would
	// count each resubmission as another new package. Both are wrong; the
	// question is answered by the earliest submission per path.
	d.db.QueryRow(`SELECT COUNT(*) FROM (
		SELECT MIN(block_time) AS first_time
		FROM package_submissions WHERE `+d.networkFilter("network", network)+`
		GROUP BY network, path
	) WHERE first_time >= ?`, since7d).Scan(&ov.NewPackages7d)

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

func (d *DB) activeAddrSeriesLive(network, granularity string, days int, now time.Time) ([]ActiveAddressTimePoint, error) {
	sqlFmt, step, truncFn := timeseriesFormat(granularity)
	startTime := now.AddDate(0, 0, -days).Format(time.RFC3339)

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
	//
	// The four source tables are the same four activityMsgTables lists for the
	// activity heatmap, so the two endpoints report the same total for the same
	// network and window. Keep the two lists in sync by hand; nothing enforces
	// the agreement automatically.
	unionNetFilter := " AND " + d.networkFilter("network", network)
	unionQ := fmt.Sprintf(
		"SELECT bucket, COUNT(*) as cnt FROM ("+
			" SELECT DISTINCT strftime('%s', block_time) as bucket, addr, network FROM ("+
			"  SELECT caller as addr, block_time, network FROM calls WHERE block_time >= ?%s"+
			"  UNION ALL SELECT creator, block_time, network FROM package_submissions WHERE block_time >= ?%s"+
			"  UNION ALL SELECT caller, block_time, network FROM msg_runs WHERE block_time >= ?%s"+
			"  UNION ALL SELECT from_address, block_time, network FROM bank_sends WHERE block_time >= ?%s"+
			" )) GROUP BY bucket ORDER BY bucket ASC",
		sqlFmt, unionNetFilter, unionNetFilter, unionNetFilter, unionNetFilter)

	urows, err := d.db.Query(unionQ, startTime, startTime, startTime, startTime)
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

	return activeAddrPoints(buckets, days, granularity, step, truncFn, now), nil
}

// activeAddrSeriesRolledUp answers the same question from the stored tuples,
// merged with whatever is newer than the rollup.
//
// One statement, one shared source. The rolled-up hours and the live tail feed
// the same deduplication rather than being counted separately and added, because
// adding them would double-count every address that was active on both sides of
// the boundary — which, on any bucket wider than an hour, is most of them.
//
// Both counts come back from one round trip: the per-kind figures grouped by
// (bucket, kind), and the union total grouped by bucket, unioned into the
// (bucket, typ, count) shape the live path already returns. Callers hold the
// read lock.

func (d *DB) activeAddrSeriesRolledUp(network, granularity string, days int, boundary, now time.Time) ([]ActiveAddressTimePoint, error) {
	_, step, truncFn := timeseriesFormat(granularity)

	start := now.AddDate(0, 0, -days)
	netFilter := d.networkFilter("network", network)

	// The window opens at an instant — "now minus 30 days" — and the stored
	// grain is the hour, so the hour the window opens in cannot be answered from
	// the rollup: its tuples say the address was active somewhere in that hour,
	// not whether it was before or after 14:23. Taking the whole hour would make
	// the first bucket count activity from outside the window and disagree with
	// the live path by exactly the rows in between. So the rollup starts at the
	// next whole hour and that opening hour is read live.
	firstWholeHour := start.Truncate(time.Hour)
	if firstWholeHour.Before(start) {
		firstWholeHour = firstWholeHour.Add(time.Hour)
	}

	// Buckets are fixed-width UTC hours, so string comparison is chronological
	// and the primary key's leading (network, bucket) serves the range directly.
	branches := []string{fmt.Sprintf(
		"SELECT bucket AS hour, network, kind, addr FROM active_addr_rollup"+
			" WHERE bucket >= ? AND bucket < ? AND %s", netFilter)}
	args := []any{firstWholeHour.Format(activeAddrBucketLayout), boundary.Format(activeAddrBucketLayout)}

	// The two stretches the rollup does not cover: the hour the window opens in,
	// and everything newer than the build.
	//
	// They overlap whenever the last build is older than the window, and that is
	// harmless rather than lucky — every branch feeds the same SELECT DISTINCT,
	// and a row reaching it from two branches is identical in all four columns,
	// so it collapses. Counting the stretches separately and adding them up is
	// what would double.
	liveWindows := []struct{ from, until time.Time }{
		{start, firstWholeHour},
		{boundary, time.Time{}},
	}
	for _, k := range activeAddrKinds {
		for _, w := range liveWindows {
			cond := "block_time >= ?"
			args = append(args, w.from.Format(time.RFC3339))
			if !w.until.IsZero() {
				cond += " AND block_time < ?"
				args = append(args, w.until.Format(time.RFC3339))
			}
			branches = append(branches, fmt.Sprintf(
				"SELECT strftime('%%Y-%%m-%%dT%%H', block_time), network, '%s', %s FROM %s"+
					" WHERE %s AND %s", k.kind, k.column, k.table, cond, netFilter))
		}
	}

	// MATERIALIZED because src is read twice — once per grouping — and without
	// it SQLite is free to inline the union into both, paying for the whole scan
	// twice over.
	//
	// Chaining a second CTE, so that the total dedups the per-kind result rather
	// than the raw union again, looks like it should be strictly cheaper and
	// measured as noise on the wide windows and a regression on 24h: it trades
	// one scan of src for an extra materialisation and re-sort, and the two are
	// the same size whenever the bucket is the stored hour. Left as two passes.
	bucket := activeAddrBucketExpr(granularity)
	q := fmt.Sprintf(`
		WITH src(hour, network, kind, addr) AS MATERIALIZED (%s)
		SELECT bucket, kind AS typ, COUNT(*) AS cnt FROM (
			SELECT DISTINCT %s AS bucket, kind, network, addr FROM src
		) GROUP BY bucket, kind
		UNION ALL
		SELECT bucket, 'total' AS typ, COUNT(*) AS cnt FROM (
			SELECT DISTINCT %s AS bucket, network, addr FROM src
		) GROUP BY bucket`,
		strings.Join(branches, " UNION ALL "), bucket, bucket)

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
		case "total":
			pt.TotalActive = cnt
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return activeAddrPoints(buckets, days, granularity, step, truncFn, now), nil
}

// activeAddrBucketLayout is the stored bucket format: a UTC hour, fixed width so
// that lexical order is chronological order.

const activeAddrBucketLayout = "2006-01-02T15"

// activeAddrBucketExpr maps a stored hour bucket onto the requested
// granularity's key, and must agree with bucketKey for every granularity or the
// series returns buckets the fill loop then cannot find.
//
// Three of the four are a prefix of 'YYYY-MM-DDTHH' rather than a strftime call,
// which is not micro-optimisation: strftime parses a date string per row, and
// the 90d window puts 135k rows through it. Only the ISO week needs real
// calendar arithmetic.

func activeAddrBucketExpr(granularity string) string {
	switch granularity {
	case "hourly":
		return "hour"
	case "weekly":
		return "strftime('%G-W%V', hour || ':00:00')"
	case "monthly":
		return "substr(hour, 1, 7)"
	default:
		return "substr(hour, 1, 10)"
	}
}

// activeAddrPoints turns the bucket map into the dense series the API returns,
// filling the buckets nothing happened in.
//
// Shared by both read paths on purpose: an endpoint that answers with a
// different set of buckets depending on whether a rollup happens to be built is
// the same bug as answering with different numbers.

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

var activityMsgTables = []struct{ table, addrCol string }{
	{"calls", "caller"},
	{"package_submissions", "creator"},
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

	// "all networks" means every configured network, never "no filter": an
	// unfiltered query also counts chains that were retired but whose rows are
	// still stored.
	netFilter := " AND " + d.networkFilter("t.network", network)
	parts := make([]string, 0, len(activityMsgTables))
	args := make([]any, 0, len(activityMsgTables))
	for _, s := range activityMsgTables {
		// Table names come from the constant above, never from input.
		parts = append(parts, fmt.Sprintf(
			"SELECT strftime('%%H', t.block_time) AS h, strftime('%%w', t.block_time) AS w, COUNT(*) AS c"+
				" FROM %s t WHERE t.block_time >= ?%s GROUP BY h, w", s.table, netFilter))
		args = append(args, start)
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

func (d *DB) GetRollingActiveTimeSeries(network string, days int) ([]RollingActivePoint, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	if days < rollingMinDays {
		days = rollingMinDays
	}
	now := time.Now().UTC()
	firstDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -days)
	loadFrom := firstDay.AddDate(0, 0, -(mauDays - 1)).Format(time.RFC3339)

	// "all networks" means every configured network, never "no filter": an
	// unfiltered query also counts chains that were retired but whose rows are
	// still stored.
	netFilter := " AND " + d.networkFilter("t.network", network)
	parts := make([]string, 0, len(activityMsgTables))
	args := make([]any, 0, len(activityMsgTables))
	for _, s := range activityMsgTables {
		// Column and table names come from the constant above, never from input.
		parts = append(parts, fmt.Sprintf(
			"SELECT strftime('%%Y-%%m-%%d', t.block_time) AS day, t.%s AS addr FROM %s t"+
				" WHERE t.block_time >= ?%s", s.addrCol, s.table, netFilter))
		args = append(args, loadFrom)
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

	netFilter := " AND " + d.networkFilter("network", network)
	args := []any{start}
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
