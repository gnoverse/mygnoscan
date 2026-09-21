package store

import (
	"database/sql"
	"strings"
)

// Realm activity: who called a realm, how often, and which function.
//
// The realm detail page used to answer this with the 50 most recent rows
// GetPackageDetail carries, which cannot say how many distinct addresses have
// ever touched a realm, whether any of them came back, or which exported
// function they used. "54 calls" on a page showing 50 of them is the shape of
// the problem: the interesting questions (unique callers, their addresses,
// their transaction counts) are aggregates over the whole history, and an
// aggregate cannot be recovered from a truncated tail.
//
// So: aggregate in SQL over every row, and apply the reader's filters to the
// aggregate as well as to the feed. Filtering to `func=Bid` and reading
// "12 unique callers" is the point — it means twelve addresses bid, not twelve
// addresses touched the realm at all.

// RealmUsageFilter narrows a realm's message history.
//
// The zero value selects everything the realm has ever seen.
type RealmUsageFilter struct {
	// Caller is an exact address. Addresses are canonical bech32, so there is
	// no prefix or case matching to do.
	Caller string
	// Func is an exact exported function name. A MsgRun has none, so setting
	// this drops the MsgRun half of the union entirely rather than silently
	// keeping runs that match no function.
	Func string
	// Status is "ok", "fail", or "" for both.
	Status string
	// Kind is "call", "run", or "" for both.
	Kind string
	// Since is an RFC3339 lower bound on block_time, or "" for all time.
	//
	// Compared as a string, which is correct for the RFC3339 UTC stamps the
	// indexer writes: they are fixed-prefix and zero-padded, so lexical order
	// is chronological order down to the second. Rows with no block_time at
	// all (written before the syncer knew a block's timestamp, and genesis)
	// fall outside every window, which is the right answer for a window that
	// always ends now.
	Since string
	// Limit and Offset page the feed only. Every aggregate below is computed
	// over the full filtered set, never over the page.
	Limit  int
	Offset int
	// CallerLimit caps the callers table. 0 means DefaultRealmCallerLimit.
	CallerLimit int
}

// DefaultRealmCallerLimit caps the callers table at a length a browser can
// sort and filter in the page without help. A realm with more distinct callers
// than this is a rich list question, not a realm-page question.
const DefaultRealmCallerLimit = 500

// RealmUsage is the whole answer for one realm under one filter.
type RealmUsage struct {
	Path    string            `json:"path"`
	Network string            `json:"network"`
	Summary RealmUsageSummary `json:"summary"`
	// Callers is ordered by message count, descending. Truncated to
	// CallerLimit; CallersTruncated says whether that happened, so the page
	// can label a partial table instead of presenting it as the whole set.
	Callers          []RealmCaller   `json:"callers"`
	CallersTruncated bool            `json:"callers_truncated"`
	Functions        []RealmFunction `json:"functions"`
	// Rows is one page of the message feed, newest first.
	Rows []RealmUsageRow `json:"rows"`
}

// RealmUsageSummary counts the filtered set.
type RealmUsageSummary struct {
	Messages int `json:"messages"`
	Calls    int `json:"calls"`
	Runs     int `json:"runs"`
	// Txs is distinct transactions. Lower than Messages whenever someone
	// bundles several MsgCalls into one transaction, which is why both are
	// here: "54 calls from 30 transactions" is a different realm from
	// "54 calls from 54 transactions".
	Txs           int `json:"txs"`
	UniqueCallers int `json:"unique_callers"`
	// Returning is the callers with more than one message. One-shot traffic
	// and a habit look identical in a unique-caller count.
	Returning int    `json:"returning"`
	OK        int    `json:"ok"`
	Failed    int    `json:"failed"`
	Functions int    `json:"functions"`
	FirstTime string `json:"first_time,omitempty"`
	LastTime  string `json:"last_time,omitempty"`
	// GasUsed and GasFee are summed over distinct transactions, the same
	// attribution gas_realm_rollup uses: a transaction that touched this realm
	// contributes its gas once, however many messages it carried.
	GasUsed int `json:"gas_used"`
	GasFee  int `json:"gas_fee"`
}

// RealmCaller is one address's whole relationship with the realm.
type RealmCaller struct {
	Address  string `json:"address"`
	Messages int    `json:"messages"`
	Calls    int    `json:"calls"`
	Runs     int    `json:"runs"`
	Txs      int    `json:"txs"`
	OK       int    `json:"ok"`
	Failed   int    `json:"failed"`
	// Funcs is how many distinct functions this address called. One is a bot
	// or a single-purpose integration; several is someone exploring.
	Funcs       int    `json:"funcs"`
	FirstHeight int    `json:"first_height"`
	FirstTime   string `json:"first_time,omitempty"`
	LastHeight  int    `json:"last_height"`
	LastTime    string `json:"last_time,omitempty"`
	GasUsed     int    `json:"gas_used"`
	GasFee      int    `json:"gas_fee"`
}

// RealmFunction is one exported function's traffic.
type RealmFunction struct {
	Name    string `json:"name"`
	Calls   int    `json:"calls"`
	Callers int    `json:"callers"`
	OK      int    `json:"ok"`
	Failed  int    `json:"failed"`

	LastHeight int    `json:"last_height"`
	LastTime   string `json:"last_time,omitempty"`
}

// RealmUsageRow is one message in the feed.
type RealmUsageRow struct {
	Kind        string `json:"kind"`
	TxHash      string `json:"tx_hash"`
	MsgIndex    int    `json:"msg_index"`
	BlockHeight int    `json:"block_height"`
	BlockTime   string `json:"block_time,omitempty"`
	Caller      string `json:"caller"`
	FuncName    string `json:"func_name,omitempty"`
	Success     bool   `json:"success"`
	GasUsed     int    `json:"gas_used"`
	GasFee      int    `json:"gas_fee"`
}

// activitySource builds the `act` CTE: every message aimed at this realm that
// passes the filter, from both tables, in one shape.
//
// Both halves carry their own copy of the predicates rather than filtering the
// union afterward, so SQLite can use idx_calls_net_pkg_height instead of
// scanning the union. The MsgRun half has no such index and cannot get one —
// it matches on a LIKE over the script's source, the same heuristic
// GetPackageDetail's msgrun_refs has always used.
func activitySource(network, path string, f RealmUsageFilter) (string, []any) {
	var parts []string
	var args []any

	callWhere := []string{"network = ?", "pkg_path = ?"}
	callArgs := []any{network, path}
	if f.Caller != "" {
		callWhere = append(callWhere, "caller = ?")
		callArgs = append(callArgs, f.Caller)
	}
	if f.Func != "" {
		callWhere = append(callWhere, "func_name = ?")
		callArgs = append(callArgs, f.Func)
	}
	switch f.Status {
	case "ok":
		callWhere = append(callWhere, "success = 1")
	case "fail":
		callWhere = append(callWhere, "success = 0")
	}
	if f.Since != "" {
		callWhere = append(callWhere, "block_time >= ?")
		callArgs = append(callArgs, f.Since)
	}

	if f.Kind != "run" {
		parts = append(parts, `SELECT 'call' AS kind, tx_hash, msg_index, block_height,
			COALESCE(block_time, '') AS block_time, caller, func_name, success
			FROM calls WHERE `+strings.Join(callWhere, " AND "))
		args = append(args, callArgs...)
	}

	// A MsgRun has no function name, so a function filter excludes every run
	// by construction.
	if f.Kind != "call" && f.Func == "" {
		runWhere := []string{"network = ?", "source LIKE ?"}
		runArgs := []any{network, "%" + path + "%"}
		if f.Caller != "" {
			runWhere = append(runWhere, "caller = ?")
			runArgs = append(runArgs, f.Caller)
		}
		switch f.Status {
		case "ok":
			runWhere = append(runWhere, "success = 1")
		case "fail":
			runWhere = append(runWhere, "success = 0")
		}
		if f.Since != "" {
			runWhere = append(runWhere, "block_time >= ?")
			runArgs = append(runArgs, f.Since)
		}
		parts = append(parts, `SELECT 'run' AS kind, tx_hash, 0 AS msg_index, block_height,
			COALESCE(block_time, '') AS block_time, caller, '' AS func_name, success
			FROM msg_runs WHERE `+strings.Join(runWhere, " AND "))
		args = append(args, runArgs...)
	}

	// Both halves excluded (kind=run with a function filter) still has to be
	// valid SQL that selects nothing, rather than a query the caller has to
	// special-case at four call sites.
	if len(parts) == 0 {
		return `SELECT '' AS kind, '' AS tx_hash, 0 AS msg_index, 0 AS block_height,
			'' AS block_time, '' AS caller, '' AS func_name, 0 AS success WHERE 0`, nil
	}
	return strings.Join(parts, "\nUNION ALL\n"), args
}

// RealmUsage answers the calls tab: aggregates over the whole filtered
// history, plus one page of the message feed.
func (d *DB) RealmUsage(network, path string, f RealmUsageFilter) (*RealmUsage, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	// Resolve the package's own network before anything else. Every query
	// below binds it as a parameter, so an ambiguous `?network=all` cannot mix
	// two chains' calls into one caller count (AGENTS.md: everything is
	// network-scoped).
	var resolved string
	q := `SELECT network FROM packages WHERE path = ? AND ` + d.networkFilter("network", network) + ` LIMIT 1`
	if err := d.db.QueryRow(q, path).Scan(&resolved); err != nil {
		return nil, err
	}

	out := &RealmUsage{Path: path, Network: resolved}
	src, srcArgs := activitySource(resolved, path, f)
	with := "WITH act AS (\n" + src + "\n)\n"

	// Summary. First and last are read by height rather than by MIN/MAX over
	// the timestamp: the sub-second precision the indexer writes is not a
	// fixed width, so two stamps inside the same second do not compare
	// lexically. Height is the chain's own ordering and has no such edge.
	sum := with + `SELECT
		COUNT(*),
		SUM(CASE WHEN kind = 'call' THEN 1 ELSE 0 END),
		SUM(CASE WHEN kind = 'run' THEN 1 ELSE 0 END),
		COUNT(DISTINCT tx_hash),
		COUNT(DISTINCT caller),
		SUM(CASE WHEN success THEN 1 ELSE 0 END),
		COUNT(DISTINCT CASE WHEN kind = 'call' THEN func_name END),
		-- Counted in SQL rather than off the callers slice below, which is
		-- capped: deriving it there would silently mean "returning, among the
		-- top 500", and a realm with more callers than that is exactly the one
		-- whose retention is worth knowing.
		(SELECT COUNT(*) FROM (SELECT caller FROM act GROUP BY caller HAVING COUNT(*) > 1)),
		(SELECT block_time FROM act ORDER BY block_height ASC LIMIT 1),
		(SELECT block_time FROM act ORDER BY block_height DESC LIMIT 1)
		FROM act`
	var calls, runs, ok, returning sql.NullInt64
	var firstTime, lastTime sql.NullString
	// One set of arguments, not one per reference: the placeholders live in the
	// CTE's text, which appears once however many times the main query names
	// `act`.
	if err := d.db.QueryRow(sum, srcArgs...).Scan(
		&out.Summary.Messages, &calls, &runs, &out.Summary.Txs, &out.Summary.UniqueCallers,
		&ok, &out.Summary.Functions, &returning, &firstTime, &lastTime,
	); err != nil {
		return nil, err
	}
	out.Summary.Returning = int(returning.Int64)
	out.Summary.Calls = int(calls.Int64)
	out.Summary.Runs = int(runs.Int64)
	out.Summary.OK = int(ok.Int64)
	out.Summary.Failed = out.Summary.Messages - out.Summary.OK
	out.Summary.FirstTime = firstTime.String
	out.Summary.LastTime = lastTime.String

	// Gas per caller, over distinct transactions. Same attribution as
	// gas_realm_rollup: DISTINCT first so a multicall's transaction is not
	// billed once per message.
	gasUsed := map[string]int{}
	gasFee := map[string]int{}
	gasQ := with + `SELECT caller, SUM(gas_used), SUM(gas_fee) FROM (
		SELECT DISTINCT a.caller, a.tx_hash, t.gas_used, t.gas_fee
		  FROM act a JOIN transactions t ON t.network = ? AND t.tx_hash = a.tx_hash
	) GROUP BY caller`
	gasRows, err := d.db.Query(gasQ, append(append([]any{}, srcArgs...), resolved)...)
	if err != nil {
		return nil, err
	}
	for gasRows.Next() {
		var addr string
		var used, fee sql.NullInt64
		if err := gasRows.Scan(&addr, &used, &fee); err != nil {
			gasRows.Close()
			return nil, err
		}
		gasUsed[addr] = int(used.Int64)
		gasFee[addr] = int(fee.Int64)
		out.Summary.GasUsed += int(used.Int64)
		out.Summary.GasFee += int(fee.Int64)
	}
	gasRows.Close()
	if err := gasRows.Err(); err != nil {
		return nil, err
	}

	// Callers.
	callerLimit := f.CallerLimit
	if callerLimit <= 0 {
		callerLimit = DefaultRealmCallerLimit
	}
	callersQ := with + `SELECT caller,
		COUNT(*) AS messages,
		SUM(CASE WHEN kind = 'call' THEN 1 ELSE 0 END),
		SUM(CASE WHEN kind = 'run' THEN 1 ELSE 0 END),
		COUNT(DISTINCT tx_hash),
		SUM(CASE WHEN success THEN 1 ELSE 0 END),
		COUNT(DISTINCT CASE WHEN kind = 'call' THEN func_name END),
		MIN(block_height), MAX(block_height),
		(SELECT block_time FROM act i WHERE i.caller = o.caller ORDER BY i.block_height ASC LIMIT 1),
		(SELECT block_time FROM act i WHERE i.caller = o.caller ORDER BY i.block_height DESC LIMIT 1)
		FROM act o GROUP BY caller
		ORDER BY messages DESC, MAX(block_height) DESC
		LIMIT ?`
	cArgs := append(append([]any{}, srcArgs...), callerLimit+1)
	cRows, err := d.db.Query(callersQ, cArgs...)
	if err != nil {
		return nil, err
	}
	for cRows.Next() {
		var c RealmCaller
		var nCalls, nRuns, nOK, nFuncs sql.NullInt64
		var first, last sql.NullString
		if err := cRows.Scan(&c.Address, &c.Messages, &nCalls, &nRuns, &c.Txs, &nOK, &nFuncs,
			&c.FirstHeight, &c.LastHeight, &first, &last); err != nil {
			cRows.Close()
			return nil, err
		}
		c.Calls = int(nCalls.Int64)
		c.Runs = int(nRuns.Int64)
		c.OK = int(nOK.Int64)
		c.Failed = c.Messages - c.OK
		c.Funcs = int(nFuncs.Int64)
		c.FirstTime = first.String
		c.LastTime = last.String
		c.GasUsed = gasUsed[c.Address]
		c.GasFee = gasFee[c.Address]
		out.Callers = append(out.Callers, c)
	}
	cRows.Close()
	if err := cRows.Err(); err != nil {
		return nil, err
	}
	// One row over the cap was fetched precisely so this can tell "exactly at
	// the cap" from "more than the cap" without a second COUNT.
	if len(out.Callers) > callerLimit {
		out.Callers = out.Callers[:callerLimit]
		out.CallersTruncated = true
	}

	// Functions. MsgRuns are excluded: they have no function name, and a
	// bucket called "" next to Bid and Claim reads as a parsing failure.
	fnQ := with + `SELECT func_name,
		COUNT(*) AS calls,
		COUNT(DISTINCT caller),
		SUM(CASE WHEN success THEN 1 ELSE 0 END),
		MAX(block_height),
		(SELECT block_time FROM act i WHERE i.kind = 'call' AND i.func_name = o.func_name
		   ORDER BY i.block_height DESC LIMIT 1)
		FROM act o WHERE kind = 'call' GROUP BY func_name ORDER BY calls DESC, func_name ASC`
	fRows, err := d.db.Query(fnQ, srcArgs...)
	if err != nil {
		return nil, err
	}
	for fRows.Next() {
		var fn RealmFunction
		var nOK sql.NullInt64
		var last sql.NullString
		if err := fRows.Scan(&fn.Name, &fn.Calls, &fn.Callers, &nOK, &fn.LastHeight, &last); err != nil {
			fRows.Close()
			return nil, err
		}
		fn.OK = int(nOK.Int64)
		fn.Failed = fn.Calls - fn.OK
		fn.LastTime = last.String
		out.Functions = append(out.Functions, fn)
	}
	fRows.Close()
	if err := fRows.Err(); err != nil {
		return nil, err
	}

	// Feed page.
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	offset := f.Offset
	if offset < 0 {
		offset = 0
	}
	rowsQ := with + `SELECT a.kind, a.tx_hash, a.msg_index, a.block_height, a.block_time,
		a.caller, a.func_name, a.success,
		COALESCE(t.gas_used, 0), COALESCE(t.gas_fee, 0)
		FROM act a LEFT JOIN transactions t ON t.network = ? AND t.tx_hash = a.tx_hash
		ORDER BY a.block_height DESC, a.tx_hash ASC, a.msg_index ASC
		LIMIT ? OFFSET ?`
	rArgs := append(append([]any{}, srcArgs...), resolved, limit, offset)
	rRows, err := d.db.Query(rowsQ, rArgs...)
	if err != nil {
		return nil, err
	}
	defer rRows.Close()
	for rRows.Next() {
		var row RealmUsageRow
		if err := rRows.Scan(&row.Kind, &row.TxHash, &row.MsgIndex, &row.BlockHeight, &row.BlockTime,
			&row.Caller, &row.FuncName, &row.Success, &row.GasUsed, &row.GasFee); err != nil {
			return nil, err
		}
		out.Rows = append(out.Rows, row)
	}
	return out, rRows.Err()
}
