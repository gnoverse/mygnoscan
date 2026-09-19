package store

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Edge rollups behind the network graphs.
//
// These are folded incrementally from rows already in SQLite rather than
// recomputed wholesale like RefreshRollups: bank_sends and calls are the source,
// both are already synced, and an edge table keyed by day only ever gains rows.
// A pass reads everything above its own MAX(last_height) and adds to the running
// totals, so re-running one cannot double-count.

type TransferEdgeRow struct {
	FromAddress string
	ToAddress   string
	Day         string // 'YYYY-MM-DD'
	TotalValue  int64  // ugnot, parsed out of bank_sends' decorated amount string
	TxCount     int
	LastHeight  int
}

type CallerEdgeRow struct {
	Caller     string
	PkgPath    string
	Day        string
	Calls      int
	LastHeight int
}

func (d *DB) UpsertTransferEdges(network string, rows []TransferEdgeRow) error {
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
		INSERT INTO transfer_edges (network, from_address, to_address, day, total_value, tx_count, last_height)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (network, from_address, to_address, day) DO UPDATE SET
			total_value = total_value + excluded.total_value,
			tx_count    = tx_count + excluded.tx_count,
			last_height = MAX(last_height, excluded.last_height)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, r := range rows {
		if _, err := stmt.Exec(network, r.FromAddress, r.ToAddress, r.Day, r.TotalValue, r.TxCount, r.LastHeight); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (d *DB) UpsertCallerEdges(network string, rows []CallerEdgeRow) error {
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
		INSERT INTO caller_edges (network, caller, pkg_path, day, calls, last_height)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (network, caller, pkg_path, day) DO UPDATE SET
			calls       = calls + excluded.calls,
			last_height = MAX(last_height, excluded.last_height)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, r := range rows {
		if _, err := stmt.Exec(network, r.Caller, r.PkgPath, r.Day, r.Calls, r.LastHeight); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// edgesLastHeight is the rollup cursor for one edge table. The table name is a
// constant from the two callers below, never input.
func (d *DB) edgesLastHeight(table, network string) (int, bool, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var h sql.NullInt64
	if err := d.db.QueryRow(
		`SELECT MAX(last_height) FROM `+table+` WHERE network = ?`, network,
	).Scan(&h); err != nil {
		return 0, false, err
	}
	if !h.Valid {
		return 0, false, nil
	}
	return int(h.Int64), true, nil
}

func (d *DB) TransferEdgesLastHeight(network string) (int, bool, error) {
	return d.edgesLastHeight("transfer_edges", network)
}

func (d *DB) CallerEdgesLastHeight(network string) (int, bool, error) {
	return d.edgesLastHeight("caller_edges", network)
}

func (d *DB) RollupBankSendsSince(network string, sinceHeight int) ([]TransferEdgeRow, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	rows, err := d.db.Query(`
		SELECT from_address, to_address, date(block_time) AS day,
		       `+amountExpr+`, COUNT(*), MAX(block_height)
		FROM bank_sends
		WHERE network = ? AND block_height > ? AND success = 1
		GROUP BY from_address, to_address, day`, network, sinceHeight)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []TransferEdgeRow
	for rows.Next() {
		var r TransferEdgeRow
		var day sql.NullString
		if err := rows.Scan(&r.FromAddress, &r.ToAddress, &day, &r.TotalValue, &r.TxCount, &r.LastHeight); err != nil {
			return nil, err
		}
		// A row whose block_time will not parse yields a NULL day from date();
		// skip it rather than corrupting a real bucket or failing the pass.
		if !day.Valid || day.String == "" {
			continue
		}
		r.Day = day.String
		out = append(out, r)
	}
	return out, rows.Err()
}

func (d *DB) RollupCallsSince(network string, sinceHeight int) ([]CallerEdgeRow, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	rows, err := d.db.Query(`
		SELECT caller, pkg_path, date(block_time) AS day, COUNT(*), MAX(block_height)
		FROM calls
		WHERE network = ? AND block_height > ? AND success = 1
		GROUP BY caller, pkg_path, day`, network, sinceHeight)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []CallerEdgeRow
	for rows.Next() {
		var r CallerEdgeRow
		var day sql.NullString
		if err := rows.Scan(&r.Caller, &r.PkgPath, &day, &r.Calls, &r.LastHeight); err != nil {
			return nil, err
		}
		if !day.Valid || day.String == "" {
			continue
		}
		r.Day = day.String
		out = append(out, r)
	}
	return out, rows.Err()
}

// --- graphs read out of the edge tables ---
//
// Every query here is scoped to one network. transfer_edges holds denominated
// values, which are never summed across chains, and an address is a different
// actor on each chain — so the graphs are per-chain by construction and the
// handlers reject an all-networks request rather than silently answering for
// none.

type GraphNode struct {
	ID     string `json:"id"`
	Volume int64  `json:"volume"`
}

type GraphEdge struct {
	From    string `json:"from"`
	To      string `json:"to"`
	Value   int64  `json:"value"`
	TxCount int    `json:"tx_count"`
}

type TransferGraph struct {
	Nodes []GraphNode `json:"nodes"`
	Edges []GraphEdge `json:"edges"`
}

func (d *DB) GetTransferGraph(network string, days, topN int, minValue int64, ego string) (TransferGraph, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	start := time.Now().UTC().AddDate(0, 0, -days).Format("2006-01-02")

	if ego != "" {
		return d.egoTransferGraph(network, start, ego, minValue)
	}
	if topN <= 0 {
		topN = 100
	}
	if topN > 1000 {
		topN = 1000
	}
	return d.topNTransferGraph(network, start, topN, minValue)
}

func (d *DB) topNTransferGraph(network, start string, topN int, minValue int64) (TransferGraph, error) {
	addrRows, err := d.db.Query(`
		SELECT addr, SUM(vol) FROM (
			SELECT from_address AS addr, total_value AS vol FROM transfer_edges WHERE network = ? AND day >= ?
			UNION ALL
			SELECT to_address AS addr, total_value AS vol FROM transfer_edges WHERE network = ? AND day >= ?
		) GROUP BY addr ORDER BY SUM(vol) DESC LIMIT ?`,
		network, start, network, start, topN)
	if err != nil {
		return TransferGraph{}, err
	}
	nodeVol := map[string]int64{}
	var order []string
	for addrRows.Next() {
		var addr string
		var vol int64
		if err := addrRows.Scan(&addr, &vol); err != nil {
			addrRows.Close()
			return TransferGraph{}, err
		}
		nodeVol[addr] = vol
		order = append(order, addr)
	}
	if err := addrRows.Err(); err != nil {
		addrRows.Close()
		return TransferGraph{}, err
	}
	addrRows.Close()

	if len(order) == 0 {
		return TransferGraph{Nodes: []GraphNode{}, Edges: []GraphEdge{}}, nil
	}

	// Parallel-edge collapse: same-pair edges across multiple days in the
	// window sum at read time. Both endpoints must be in the top-N set, or a
	// high-volume node would drag in every low-volume address it ever touched.
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(order)), ",")
	args := make([]any, 0, 2+2*len(order)+1)
	args = append(args, network, start)
	for _, a := range order {
		args = append(args, a)
	}
	for _, a := range order {
		args = append(args, a)
	}
	args = append(args, minValue)

	q := fmt.Sprintf(`
		SELECT from_address, to_address, SUM(total_value), SUM(tx_count)
		FROM transfer_edges
		WHERE network = ? AND day >= ? AND from_address IN (%s) AND to_address IN (%s)
		GROUP BY from_address, to_address
		HAVING SUM(total_value) >= ?
		ORDER BY SUM(total_value) DESC`, placeholders, placeholders)

	edges, err := scanTransferEdges(d.db.Query(q, args...))
	if err != nil {
		return TransferGraph{}, err
	}

	nodes := make([]GraphNode, 0, len(order))
	for _, a := range order {
		nodes = append(nodes, GraphNode{ID: a, Volume: nodeVol[a]})
	}
	return TransferGraph{Nodes: nodes, Edges: edges}, nil
}

func (d *DB) egoTransferGraph(network, start, ego string, minValue int64) (TransferGraph, error) {
	edges, err := scanTransferEdges(d.db.Query(`
		SELECT from_address, to_address, SUM(total_value), SUM(tx_count)
		FROM transfer_edges
		WHERE network = ? AND day >= ? AND (from_address = ? OR to_address = ?)
		GROUP BY from_address, to_address
		HAVING SUM(total_value) >= ?
		ORDER BY SUM(total_value) DESC`, network, start, ego, ego, minValue))
	if err != nil {
		return TransferGraph{}, err
	}

	nodeVol := map[string]int64{}
	for _, e := range edges {
		other := e.To
		if e.From != ego {
			other = e.From
		}
		nodeVol[other] += e.Value
		nodeVol[ego] += e.Value
	}

	// The ego node renders even with no matching edges.
	nodes := []GraphNode{{ID: ego, Volume: nodeVol[ego]}}
	others := make([]string, 0, len(nodeVol))
	for addr := range nodeVol {
		if addr != ego {
			others = append(others, addr)
		}
	}
	// Map iteration order is random; sort so the response is stable between
	// identical requests.
	sort.Slice(others, func(i, j int) bool {
		if nodeVol[others[i]] != nodeVol[others[j]] {
			return nodeVol[others[i]] > nodeVol[others[j]]
		}
		return others[i] < others[j]
	})
	for _, addr := range others {
		nodes = append(nodes, GraphNode{ID: addr, Volume: nodeVol[addr]})
	}
	return TransferGraph{Nodes: nodes, Edges: edges}, nil
}

func scanTransferEdges(rows *sql.Rows, err error) ([]GraphEdge, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	edges := []GraphEdge{}
	for rows.Next() {
		var e GraphEdge
		if err := rows.Scan(&e.From, &e.To, &e.Value, &e.TxCount); err != nil {
			return nil, err
		}
		edges = append(edges, e)
	}
	return edges, rows.Err()
}

type CallerGraphNode struct {
	ID    string `json:"id"`
	Type  string `json:"type"` // "caller" | "realm"
	Calls int    `json:"calls"`
}

type CallerGraphEdge struct {
	Caller  string `json:"caller"`
	PkgPath string `json:"pkg_path"`
	Calls   int    `json:"calls"`
}

type CallerGraph struct {
	Nodes []CallerGraphNode `json:"nodes"`
	Edges []CallerGraphEdge `json:"edges"`
}

func (d *DB) GetCallerGraph(network string, days, topN, minCalls int) (CallerGraph, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	if topN <= 0 {
		topN = 200
	}
	if topN > 1000 {
		topN = 1000
	}
	start := time.Now().UTC().AddDate(0, 0, -days).Format("2006-01-02")

	callerRows, err := d.db.Query(`
		SELECT caller, SUM(calls) FROM caller_edges WHERE network = ? AND day >= ?
		GROUP BY caller ORDER BY SUM(calls) DESC LIMIT ?`, network, start, topN)
	if err != nil {
		return CallerGraph{}, err
	}
	callerCalls := map[string]int{}
	var callers []string
	for callerRows.Next() {
		var c string
		var n int
		if err := callerRows.Scan(&c, &n); err != nil {
			callerRows.Close()
			return CallerGraph{}, err
		}
		callerCalls[c] = n
		callers = append(callers, c)
	}
	if err := callerRows.Err(); err != nil {
		callerRows.Close()
		return CallerGraph{}, err
	}
	callerRows.Close()

	if len(callers) == 0 {
		return CallerGraph{Nodes: []CallerGraphNode{}, Edges: []CallerGraphEdge{}}, nil
	}

	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(callers)), ",")
	args := make([]any, 0, 2+len(callers)+1)
	args = append(args, network, start)
	for _, c := range callers {
		args = append(args, c)
	}
	args = append(args, minCalls)

	edgeRows, err := d.db.Query(fmt.Sprintf(`
		SELECT caller, pkg_path, SUM(calls)
		FROM caller_edges
		WHERE network = ? AND day >= ? AND caller IN (%s)
		GROUP BY caller, pkg_path
		HAVING SUM(calls) >= ?
		ORDER BY SUM(calls) DESC`, placeholders), args...)
	if err != nil {
		return CallerGraph{}, err
	}
	defer edgeRows.Close()

	realmCalls := map[string]int{}
	edges := []CallerGraphEdge{}
	for edgeRows.Next() {
		var e CallerGraphEdge
		if err := edgeRows.Scan(&e.Caller, &e.PkgPath, &e.Calls); err != nil {
			return CallerGraph{}, err
		}
		edges = append(edges, e)
		realmCalls[e.PkgPath] += e.Calls
	}
	if err := edgeRows.Err(); err != nil {
		return CallerGraph{}, err
	}

	nodes := make([]CallerGraphNode, 0, len(callers)+len(realmCalls))
	for _, c := range callers {
		nodes = append(nodes, CallerGraphNode{ID: c, Type: "caller", Calls: callerCalls[c]})
	}
	realms := make([]string, 0, len(realmCalls))
	for pkg := range realmCalls {
		realms = append(realms, pkg)
	}
	// Same reason as the transfer graph: stable output for identical requests.
	sort.Slice(realms, func(i, j int) bool {
		if realmCalls[realms[i]] != realmCalls[realms[j]] {
			return realmCalls[realms[i]] > realmCalls[realms[j]]
		}
		return realms[i] < realms[j]
	})
	for _, pkg := range realms {
		nodes = append(nodes, CallerGraphNode{ID: pkg, Type: "realm", Calls: realmCalls[pkg]})
	}
	return CallerGraph{Nodes: nodes, Edges: edges}, nil
}
