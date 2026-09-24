package store

import (
	"strings"
)

// The other half of the app directory.
//
// pkg/registry holds what a human says an app is for. This holds what the chain
// says about the same path: whether it is deployed on the network you are
// looking at, and whether anybody calls it. A directory that renders a realm
// nobody has touched since it was deployed identically to the busiest realm on
// the chain is worth less than no directory, and the difference is one GROUP BY.
//
// Both queries are per-network by construction. The same path is a different
// deployment on staging and on mainnet, so summing their call counts would
// invent a figure, the same reason the graph endpoints refuse an absent network
// (see api_graph.go). The handler decides what to do about that; this file just
// never blends.

// AppStat is what the chain knows about one directory entry.
type AppStat struct {
	Path string `json:"path"`
	// Deployed is whether this path exists on the network asked about. False
	// is a real answer for a curated entry: the directory is chain-independent
	// and several entries are not on every chain.
	Deployed   bool   `json:"deployed"`
	DeployedAt string `json:"deployed_at,omitempty"`
	// Calls and Callers are all-time; CallsWindow counts only since the cutoff
	// the caller passed, so a card can say "busy once" apart from "busy now".
	Calls       int    `json:"calls"`
	Callers     int    `json:"callers"`
	CallsWindow int    `json:"calls_window"`
	LastCall    string `json:"last_call,omitempty"`
}

// AppCandidate is a realm the chain says is busy and the directory does not
// mention. It is a suggestion queue, never an entry: curated means a human said
// so, and nothing here promotes itself.
type AppCandidate struct {
	Path     string `json:"path"`
	Calls    int    `json:"calls"`
	Callers  int    `json:"callers"`
	LastCall string `json:"last_call,omitempty"`
	Creator  string `json:"creator,omitempty"`
}

// sqlPlaceholders returns "?,?,?" for n.
func sqlPlaceholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// AppStats answers for every path at once.
//
// Two queries rather than one per app: ten cards must not mean twenty round
// trips, and the number of entries grows with the directory while the query
// count must not.
//
// `since` bounds CallsWindow only. Empty means the window is all of history, in
// which case CallsWindow equals Calls rather than zero: a card that says "0 in
// the window" when no window was asked for is a lie about the app.
func (d *DB) AppStats(network string, paths []string, since string) (map[string]AppStat, error) {
	out := make(map[string]AppStat, len(paths))
	if len(paths) == 0 {
		return out, nil
	}
	d.mu.RLock()
	defer d.mu.RUnlock()

	args := make([]any, 0, len(paths)+1)
	for _, p := range paths {
		args = append(args, p)
	}
	ph := sqlPlaceholders(len(paths))

	// Deploy state. MIN(block_time) because a path can be deployed more than
	// once on one chain (a redeploy keeps the row but the history has both),
	// and the date a reader wants is when it first appeared.
	rows, err := d.db.Query(`
		SELECT path, COALESCE(MIN(block_time), '')
		FROM packages
		WHERE `+d.networkFilter("network", network)+` AND path IN (`+ph+`)
		GROUP BY path`, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var path, at string
		if err := rows.Scan(&path, &at); err != nil {
			rows.Close()
			return nil, err
		}
		out[path] = AppStat{Path: path, Deployed: true, DeployedAt: at}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	// The window's placeholder sits in the SELECT list, which the driver binds
	// before the IN list further down. It therefore has to be first in args,
	// and cannot be appended to the slice the deploy query used: doing that
	// shifted every path by one and the whole query matched nothing.
	windowExpr := "COUNT(*)"
	callArgs := make([]any, 0, len(paths)+1)
	if since != "" {
		windowExpr = "SUM(CASE WHEN block_time >= ? THEN 1 ELSE 0 END)"
		callArgs = append(callArgs, since)
	}
	callArgs = append(callArgs, args...)
	callRows, err := d.db.Query(`
		SELECT pkg_path, COUNT(*), COUNT(DISTINCT caller), COALESCE(MAX(block_time), ''), `+windowExpr+`
		FROM calls
		WHERE `+d.networkFilter("network", network)+` AND pkg_path IN (`+ph+`)
		GROUP BY pkg_path`, callArgs...)
	if err != nil {
		return nil, err
	}
	defer callRows.Close()
	for callRows.Next() {
		var path, last string
		var calls, callers, window int
		if err := callRows.Scan(&path, &calls, &callers, &last, &window); err != nil {
			return nil, err
		}
		s := out[path]
		s.Path = path
		s.Calls, s.Callers, s.LastCall, s.CallsWindow = calls, callers, last, window
		out[path] = s
	}
	return out, callRows.Err()
}

// FamilyStat is what the chain says about several realms read as one app.
type FamilyStat struct {
	Calls   int `json:"calls"`
	Callers int `json:"callers"`
	// CallsWindow and CallersWindow are since the cutoff, and CallersWindow is
	// the one the ranking leans on.
	CallsWindow   int    `json:"calls_window"`
	CallersWindow int    `json:"callers_window"`
	LastCall      string `json:"last_call,omitempty"`
}

// AppFamilyStat reads a set of realms as one app.
//
// A query rather than a sum over AppStats, and that is the whole reason it
// exists: calls add up and callers do not. GnoSwap's router, staker and gns
// are largely the same people, so adding their caller counts would print a
// reach the DEX does not have. COUNT(DISTINCT caller) over the whole set is
// the exact answer, and the cost is one more query per folded card.
//
// `since` bounds the window columns only, and empty means all of history, for
// the same reason as in AppStats: a card that reports "0 in the window" when
// no window was asked for is lying about the app.
func (d *DB) AppFamilyStat(network string, paths []string, since string) (FamilyStat, error) {
	var out FamilyStat
	if len(paths) == 0 {
		return out, nil
	}
	d.mu.RLock()
	defer d.mu.RUnlock()

	// The window's placeholders sit in the SELECT list and therefore bind
	// before the IN list in the WHERE clause. Appending them after the paths is
	// how the same query silently matched nothing in AppStats once already.
	windowCalls, windowCallers := "COUNT(*)", "COUNT(DISTINCT caller)"
	args := []any{}
	if since != "" {
		windowCalls = "SUM(CASE WHEN block_time >= ? THEN 1 ELSE 0 END)"
		windowCallers = "COUNT(DISTINCT CASE WHEN block_time >= ? THEN caller END)"
		args = append(args, since, since)
	}
	for _, p := range paths {
		args = append(args, p)
	}
	row := d.db.QueryRow(`
		SELECT COUNT(*), COUNT(DISTINCT caller), `+windowCalls+`, `+windowCallers+`,
		       COALESCE(MAX(block_time), '')
		FROM calls
		WHERE `+d.networkFilter("network", network)+` AND pkg_path IN (`+sqlPlaceholders(len(paths))+`)`,
		args...)
	if err := row.Scan(&out.Calls, &out.Callers, &out.CallsWindow, &out.CallersWindow, &out.LastCall); err != nil {
		return FamilyStat{}, err
	}
	return out, nil
}

// AppCandidates ranks the realms nobody has written a blurb for.
//
// This is what makes the coverage gap actionable instead of merely true. The
// directory is curated and therefore always behind the chain; showing the
// busiest realms it does not mention turns curating from "stare at an empty
// JSON file" into a worklist in traffic order, and it stops the page implying
// that the entries it has are all there is.
//
// Realms only. A `p/` package has no page to launch and belongs in a directory
// of libraries, not of apps.
func (d *DB) AppCandidates(network string, exclude []string, since string, limit int) ([]AppCandidate, error) {
	if limit <= 0 {
		limit = 10
	}
	d.mu.RLock()
	defer d.mu.RUnlock()

	args := []any{}
	where := []string{d.networkFilter("c.network", network), "p.is_realm = 1"}
	if since != "" {
		where = append(where, "c.block_time >= ?")
		args = append(args, since)
	}
	if len(exclude) > 0 {
		where = append(where, "c.pkg_path NOT IN ("+sqlPlaceholders(len(exclude))+")")
		for _, p := range exclude {
			args = append(args, p)
		}
	}
	args = append(args, limit)

	// The join carries the network as well as the path. Joining on pkg_path
	// alone is the documented way to silently mix two chains here (AGENTS.md),
	// and it would attribute one chain's traffic to another chain's deploy.
	rows, err := d.db.Query(`
		SELECT c.pkg_path, COUNT(*) AS n, COUNT(DISTINCT c.caller),
		       COALESCE(MAX(c.block_time), ''), COALESCE(MAX(p.creator), '')
		FROM calls c
		JOIN packages p ON p.path = c.pkg_path AND p.network = c.network
		WHERE `+strings.Join(where, " AND ")+`
		GROUP BY c.pkg_path
		ORDER BY n DESC, c.pkg_path
		LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []AppCandidate{}
	for rows.Next() {
		var c AppCandidate
		if err := rows.Scan(&c.Path, &c.Calls, &c.Callers, &c.LastCall, &c.Creator); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CountRealms is the denominator the directory is measured against: ten
// curated entries means nothing until it sits next to how many realms exist.
func (d *DB) CountRealms(network string) (int, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	var n int
	err := d.db.QueryRow(`SELECT COUNT(*) FROM packages WHERE ` +
		d.networkFilter("network", network) + ` AND is_realm = 1`).Scan(&n)
	return n, err
}
