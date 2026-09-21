package store

import (
	"strings"
	"time"
)

// Co-usage: for one realm, what else the addresses that called it also called.
//
// The contracts map already draws this chain-wide, as its "shared callers"
// edges, by counting every pair of contracts a caller touched
// (ContractCallerEdges). That whole-pair matrix is the wrong query to ask from
// a realm page, and not mainly because it is bigger.
//
// It is bounded by two constants that exist to keep a 348-bubble picture
// legible, and both are wrong at the scale of one realm. defaultContractMaxFanout
// drops any address that touched more than 30 contracts, because on the map it
// would link all 30 to each other and turn the picture to mud; from one realm's
// point of view that address is not mud, it is the answer. And
// defaultContractMinWeight needs two addresses in common before an edge exists,
// because chain-wide one shared address is a coincidence; on a realm with three
// callers in total it is most of the relationship.
//
// So this asks the targeted question instead: take the addresses that called
// this realm, and count what else each of them called. Two index-covered
// lookups rather than a matrix, no fanout cap, no minimum.
//
// The cost tracks how many of the chain's addresses called the subject, not how
// big the chain is. Measured 2026-09-22 at pearl's size (200k calls, 350
// contracts): 15ms on a small realm, 131ms on one shaped like r/gnoland/wugnot
// (416 callers of mainnet's 2305), and 457ms in the case that cannot be
// exceeded, every address on the chain having called this one realm. See
// BenchmarkRealmCoUsagePartners, which also records the index hint that looks
// obvious and is 12x slower.

// DefaultCoUsageLimit caps the partner list. Well past what a force graph lays
// out legibly, so the limit is a guard against a pathological chain rather than
// a number a reader is expected to hit.
const DefaultCoUsageLimit = 100

// CoUsagePartner is one other contract that the subject realm's callers also
// called.
type CoUsagePartner struct {
	Path string `json:"path"`
	// Shared is how many distinct addresses called both this contract and the
	// subject realm. The edge weight.
	Shared int `json:"shared"`
	// Calls is how many messages those shared addresses sent to this contract.
	// Not the contract's total: a partner that one shared address hammered a
	// thousand times and a partner twenty of them called once each are a
	// different kind of neighbour, and Shared alone cannot tell them apart.
	Calls int `json:"calls"`
	// Callers is this contract's own distinct caller count over the same
	// window, which is what makes Shared readable. "12 shared" means one thing
	// against a partner with 12 callers of its own and another against one with
	// 900, and on mainnet today that is the difference between the gnoswap
	// contracts (caller sets that are strict subsets of each other) and a
	// popular realm two of whose users happen to have passed through.
	Callers int `json:"callers"`
}

// RealmCoUsage is the whole answer for one realm in one window.
type RealmCoUsage struct {
	Path    string `json:"path"`
	Network string `json:"network"`
	Window  string `json:"window"`
	// Callers is the subject realm's own distinct caller count, the other
	// denominator: Shared can never exceed it.
	Callers   int              `json:"callers"`
	Partners  []CoUsagePartner `json:"partners"`
	Truncated bool             `json:"truncated"`
}

// RealmCoUsagePartners answers "what else did this realm's callers call".
//
// A zero `since` means all time. `limit` of 0 means DefaultCoUsageLimit.
func (d *DB) RealmCoUsagePartners(network, path string, since time.Time, limit int) (*RealmCoUsage, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	if limit <= 0 {
		limit = DefaultCoUsageLimit
	}

	// The window predicate is concatenated in or left out entirely, never
	// written as `(? = '' OR block_time >= ?)`. ContractMapNodes documents why:
	// SQLite cannot prove the OR away, so that form stops using the covering
	// index and builds a temp B-tree instead.
	cutoff := sinceParam(since)
	window := func(prefix string) string {
		if cutoff == "" {
			return ""
		}
		return " AND " + prefix + "block_time >= ?"
	}

	out := &RealmCoUsage{Path: path, Network: network}

	// The subject's own caller count. Covered by idx_calls_net_pkg_caller.
	ownArgs := []any{network, path}
	if cutoff != "" {
		ownArgs = append(ownArgs, cutoff)
	}
	if err := d.db.QueryRow(
		`SELECT COUNT(DISTINCT caller) FROM calls WHERE network = ? AND pkg_path = ?`+window(""),
		ownArgs...,
	).Scan(&out.Callers); err != nil {
		return nil, err
	}
	// No callers means no partners, and the two queries below would both scan
	// for an empty set. Return the empty answer rather than proving it twice.
	if out.Callers == 0 {
		return out, nil
	}

	// The partners. The inner SELECT is the subject's caller set
	// (idx_calls_net_pkg_caller); the outer grouping walks each of those
	// callers' other contracts (idx_calls_net_caller_pkg). `<> ?` drops the
	// subject itself, which is otherwise always the heaviest row in its own
	// result and means nothing.
	args := []any{network}
	if cutoff != "" {
		args = append(args, cutoff)
	}
	args = append(args, path, network, path)
	if cutoff != "" {
		args = append(args, cutoff)
	}
	// limit+1 so truncation is observed rather than inferred: asking for
	// exactly `limit` and getting `limit` back cannot distinguish "there were
	// exactly this many" from "there were more".
	args = append(args, limit+1)

	q := `SELECT c.pkg_path, COUNT(DISTINCT c.caller), COUNT(*)
		FROM calls c
		WHERE c.network = ?` + window("c.") + ` AND c.pkg_path <> ?
		  AND c.caller IN (
			SELECT caller FROM calls WHERE network = ? AND pkg_path = ?` + window("") + `
		  )
		GROUP BY c.pkg_path
		ORDER BY 2 DESC, 3 DESC, c.pkg_path
		LIMIT ?`

	rows, err := d.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var p CoUsagePartner
		if err := rows.Scan(&p.Path, &p.Shared, &p.Calls); err != nil {
			return nil, err
		}
		out.Partners = append(out.Partners, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out.Partners) > limit {
		out.Partners = out.Partners[:limit]
		out.Truncated = true
	}
	if len(out.Partners) == 0 {
		return out, nil
	}

	// Each partner's own caller count, in one grouped pass over the partners
	// that survived the limit.
	//
	// Two things this deliberately is not. Not a correlated subquery in the
	// SELECT list above: that is evaluated per group, and the groups are every
	// contract the callers touched, before the LIMIT cuts them down to the
	// hundred anybody will read. And not a pass over the whole network either,
	// which is what this was first written as and which spent its time on the
	// contracts that did not make the list: 113ms of a 630ms request in the
	// benchmark's worst case, 350 contracts scanned to decorate 100.
	paths := make([]string, len(out.Partners))
	for i, p := range out.Partners {
		paths[i] = p.Path
	}
	totals, err := d.callerTotals(network, cutoff, paths)
	if err != nil {
		return nil, err
	}
	for i := range out.Partners {
		out.Partners[i].Callers = totals[out.Partners[i].Path]
	}
	return out, nil
}

// callerTotals is the distinct caller count of each named contract on one
// network, one index range scan of idx_calls_net_pkg_caller per path.
func (d *DB) callerTotals(network, cutoff string, paths []string) (map[string]int, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	args := []any{network}
	q := `SELECT pkg_path, COUNT(DISTINCT caller) FROM calls WHERE network = ?`
	if cutoff != "" {
		q += ` AND block_time >= ?`
		args = append(args, cutoff)
	}
	q += ` AND pkg_path IN (?` + strings.Repeat(",?", len(paths)-1) + `) GROUP BY pkg_path`
	for _, p := range paths {
		args = append(args, p)
	}

	rows, err := d.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	totals := map[string]int{}
	for rows.Next() {
		var path string
		var n int
		if err := rows.Scan(&path, &n); err != nil {
			return nil, err
		}
		totals[path] = n
	}
	return totals, rows.Err()
}
