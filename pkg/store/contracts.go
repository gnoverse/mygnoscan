package store

import (
	"database/sql"
	"sort"
	"strings"
	"time"
)

// Queries behind the contracts map: one bubble per deployed package, and two
// different ways of drawing a line between two of them.
//
// Everything here is network-scoped by a bound parameter and takes the network
// as a required argument rather than the store's usual "" means every
// configured network. A bubble is identified by its path, and 193 paths exist
// on more than one chain, so an all-networks map would draw one bubble
// carrying two chains' traffic. The API layer resolves "" to a concrete
// network before calling in.

// defaultEdgeLimit caps how many edges a map request can return when the
// caller does not say. 1500 is what a force layout can still lay out legibly
// at a thousand nodes; past that the picture is a texture, not a graph.
const defaultEdgeLimit = 1500

// ContractNode is one deployed package, with every metric the map can size a
// bubble by. All of them travel on first paint: switching the size metric is
// the main thing a reader does on this page, and refetching to do it would put
// a network round trip behind a dropdown.
type ContractNode struct {
	Path      string `json:"path"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	IsRealm   bool   `json:"is_realm"`
	Creator   string `json:"creator"`
	// Parked marks a package sitting in the chain's inert queue: submitted and
	// stored, but never enabled, and so invisible to every liveness probe on
	// chain. Left false here and stamped by the API layer, the same split
	// PackageInfo.Status follows: it is live RPC state, not something a query
	// over indexed history can answer.
	Parked bool `json:"parked"`
	// DeployedAt is the block time of the deploy, RFC 3339, or empty for rows
	// synced before block_time was stored.
	DeployedAt   string `json:"deployed_at"`
	DeployHeight int    `json:"deploy_height"`
	// Calls and UniqueCallers honour the window passed to ContractMapNodes.
	Calls         int `json:"calls"`
	UniqueCallers int `json:"unique_callers"`
	// Importers is how many deployed contracts import this one, and Imports how
	// many it imports. Importers is the quantity the import graph is actually
	// about: sizing that view by calls or gas hides the load-bearing packages
	// completely, because a pure package burns no gas and receives no calls of
	// its own. p/nt/ufmt/v0 has 91 dependents on mainnet and zero of everything
	// else.
	Importers int `json:"importers"`
	Imports   int `json:"imports"`
	// GasUsed and StorageBytes are all-time, read from the five-minute
	// rollups. They do not honour the window: gas attribution measured 3.4s
	// live on a mid-size chain, which is why the rollup exists, and there is
	// one rollup per chain rather than one per window.
	GasUsed      int64 `json:"gas_used"`
	StorageBytes int64 `json:"storage_bytes"`
}

// ContractEdge is one line on the map. Weight means different things per kind:
// always 1 for an import edge, and the number of shared caller addresses for a
// caller edge.
type ContractEdge struct {
	Source string `json:"source"`
	Target string `json:"target"`
	Weight int    `json:"weight"`
}

// NamespaceOf is the cluster key and the colour key: the first path element
// after the r/ or p/ marker.
//
//	gno.land/r/gnoswap/gns         -> gnoswap
//	gno.land/p/demo/avl            -> demo
//	gno.land/r/g1n4pl.../gnomi/pad -> g1n4pl...
//
// Address-owned namespaces group per address on purpose. A deployer's whole
// family of realms under r/g1.../ is one organism to a reader, and splitting
// it per realm would scatter the most interesting cluster on the map.
//
// The marker is found by scanning rather than by fixed position, so a path
// under a domain other than gno.land still resolves, and an r/ or p/ element
// appearing later in the path (gno.land/r/moul/p/thing) is not mistaken for
// the marker.
func NamespaceOf(path string) string {
	if path == "" {
		return ""
	}
	parts := strings.Split(path, "/")
	for i, p := range parts {
		if p != "r" && p != "p" {
			continue
		}
		if i+1 < len(parts) {
			return parts[i+1]
		}
		break
	}
	// No marker: fall back to the element after the domain, then the path
	// itself. Neither shape occurs on gno.land today; both are better as a
	// stable label than as an empty cluster that swallows everything odd.
	if len(parts) > 1 {
		return parts[1]
	}
	return parts[0]
}

// ContractMapNodes returns one node per package deployed on the network.
//
// A zero `since` means all time. A non-zero one narrows the call-derived
// metrics only: the node set is always every package ever deployed, because a
// contract that saw no traffic this week is a fact the map should show as a
// small bubble, not by disappearing.
func (d *DB) ContractMapNodes(network string, since time.Time) ([]ContractNode, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	// Correlated subqueries rather than joins, for the reason ListPackages
	// documents: joining calls and dependencies in one query multiplies rows
	// against each other, and joining on path alone mixes networks.
	//
	// The window predicate is concatenated in or left out entirely, never
	// written as `(? = '' OR block_time >= ?)`. That form reads as the tidier
	// one and costs the index: SQLite cannot prove the OR away, so it stops
	// using the covering index and builds a temp B-tree instead. Checked with
	// EXPLAIN QUERY PLAN, not assumed.
	window := ""
	args := []any{}
	cutoff := sinceParam(since)
	if cutoff != "" {
		window = " AND c.block_time >= ?"
	}
	for i := 0; i < 2; i++ {
		if cutoff != "" {
			args = append(args, cutoff)
		}
	}
	args = append(args, network)

	q := `SELECT p.path, p.name, p.creator, p.block_height, p.block_time, p.is_realm,
		(SELECT COUNT(*) FROM calls c
		   WHERE c.network = p.network AND c.pkg_path = p.path` + window + `) AS calls,
		(SELECT COUNT(DISTINCT c.caller) FROM calls c
		   WHERE c.network = p.network AND c.pkg_path = p.path` + window + `) AS unique_callers,
		(SELECT COALESCE(g.gas_used, 0) FROM gas_realm_rollup g
		   WHERE g.network = p.network AND g.path = p.path) AS gas_used,
		(SELECT COALESCE(sr.bytes_net, 0) FROM storage_realm_rollup sr
		   WHERE sr.network = p.network AND sr.path = p.path) AS storage_bytes,
		(SELECT COUNT(*) FROM dependencies d
		   WHERE d.network = p.network AND d.import_path = p.path) AS importers,
		(SELECT COUNT(*) FROM dependencies d
		   WHERE d.network = p.network AND d.package_path = p.path) AS imports
		FROM packages p WHERE p.network = ?
		ORDER BY p.block_height ASC`

	rows, err := d.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var nodes []ContractNode
	for rows.Next() {
		var n ContractNode
		var blockTime sql.NullString
		var gasUsed, storageBytes sql.NullInt64
		if err := rows.Scan(&n.Path, &n.Name, &n.Creator, &n.DeployHeight, &blockTime,
			&n.IsRealm, &n.Calls, &n.UniqueCallers, &gasUsed, &storageBytes,
			&n.Importers, &n.Imports); err != nil {
			return nil, err
		}
		n.DeployedAt = blockTime.String
		n.GasUsed = gasUsed.Int64
		n.StorageBytes = storageBytes.Int64
		n.Namespace = NamespaceOf(n.Path)
		nodes = append(nodes, n)
	}
	return nodes, rows.Err()
}

// ContractImportEdges returns the directed "source imports target" graph.
//
// Only edges whose target is itself deployed on this network are returned. An
// import can name a package that was never published here, and an edge to a
// package with no bubble would be a line into empty space.
func (d *DB) ContractImportEdges(network string) ([]ContractEdge, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	const q = `SELECT d.package_path, d.import_path
		FROM dependencies d
		JOIN packages src ON src.network = d.network AND src.path = d.package_path
		JOIN packages dst ON dst.network = d.network AND dst.path = d.import_path
		WHERE d.network = ?`

	rows, err := d.db.Query(q, network)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var edges []ContractEdge
	for rows.Next() {
		e := ContractEdge{Weight: 1}
		if err := rows.Scan(&e.Source, &e.Target); err != nil {
			return nil, err
		}
		edges = append(edges, e)
	}
	return edges, rows.Err()
}

// ContractCallerEdges links two contracts when the same addresses called both,
// weighted by how many addresses overlap.
//
// This is the map's answer to the edge bubblemaps draws between wallets that
// transferred to each other. Gno has no contract-to-contract call record to
// draw instead: calls.caller is the transaction signer, an externally-owned
// account, and a realm calling another realm inside the VM leaves no message
// in the indexer. Shared population is the honest structural substitute, and
// it is the one thing on this page no other view in the explorer shows.
//
// Three bounds keep it from rendering as mud, all of them server side so the
// client never has to decide what to drop:
//
//   - min: an edge needs at least this many shared callers. One address
//     touching two contracts once is not a relationship.
//   - maxFanout: an address that called more than this many distinct contracts
//     in the window produces no edges at all. A bot touching 50 contracts
//     contributes 1225 pairs by itself and links everything to everything. Its
//     calls still count toward every node's metrics; it just stops
//     manufacturing structure. Zero disables the cap.
//   - limit: at most this many edges, heaviest first.
//
// Pairs are emitted once, normalised to source < target.
//
// The pair counting happens in Go, not in SQL, and that is a measured decision
// rather than a stylistic one. The obvious formulation is a self-join over a
// CTE of distinct (caller, pkg_path) rows. Benchmarked against a synthetic
// mainnet (941 packages, 2,305 callers, 192,940 calls) it took 34s, and 6.5s
// with both CTEs forced MATERIALIZED, because the pair explosion happens
// inside the join before any bound can apply. Streaming the same rows out in
// caller order and counting pairs in a map is the same algorithm with the
// fanout cap applied *before* the pairs are generated: 190ms on the same data,
// and the worst case is bounded by maxFanout rather than by the busiest
// address on the chain.
//
// Calls are counted whether or not they succeeded, matching how every other
// call count in the explorer is computed. An address that tried a realm and
// reverted still chose to interact with it.
func (d *DB) ContractCallerEdges(network string, since time.Time, min, limit, maxFanout int) ([]ContractEdge, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	if min < 1 {
		min = 1
	}
	if limit <= 0 {
		limit = defaultEdgeLimit
	}
	if maxFanout <= 0 {
		maxFanout = 1 << 30 // no cap
	}

	deployed, paths, err := d.deployedPathIDs(network)
	if err != nil {
		return nil, err
	}

	// Ordered by caller so each address's contracts arrive as one contiguous
	// group and the pairs for it can be emitted without holding the whole
	// table. idx_calls_net_caller_pkg covers exactly this shape.
	//
	// The packages join this used to carry cost 100ms of the 190ms total, for
	// something a map lookup does for free: calls can name a path with no
	// package row (history synced from a height after the deploy), and an edge
	// to a package with no bubble would be a line into empty space.
	// Without a window this is a covering-index scan of
	// idx_calls_net_caller_pkg, which already holds (network, caller,
	// pkg_path) in that order: no temp B-tree for the DISTINCT, none for the
	// ORDER BY. Writing the window as `(? = '' OR block_time >= ?)` instead of
	// leaving it out costs both, because SQLite cannot see through the OR to
	// know the index still serves the query.
	q := `SELECT DISTINCT caller, pkg_path FROM calls WHERE network = ?`
	args := []any{network}
	if cutoff := sinceParam(since); cutoff != "" {
		q += ` AND block_time >= ?`
		args = append(args, cutoff)
	}
	q += ` ORDER BY caller`

	rows, err := d.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type pair struct{ a, b int32 }
	weights := map[pair]int{}
	var current string
	var group []int32

	flush := func() {
		// A caller of one contract creates no pair, and one over the cap
		// creates too many: it would link everything it touched to everything
		// else it touched, which is how this map turns into mud.
		if len(group) < 2 || len(group) > maxFanout {
			return
		}
		sort.Slice(group, func(i, j int) bool { return group[i] < group[j] })
		for i := 0; i < len(group); i++ {
			for j := i + 1; j < len(group); j++ {
				weights[pair{group[i], group[j]}]++
			}
		}
	}

	for rows.Next() {
		var caller, path string
		if err := rows.Scan(&caller, &path); err != nil {
			return nil, err
		}
		id, ok := deployed[path]
		if !ok {
			continue
		}
		if caller != current {
			flush()
			group = group[:0]
			current = caller
		}
		group = append(group, id)
	}
	flush()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	edges := make([]ContractEdge, 0, len(weights))
	for p, w := range weights {
		if w < min {
			continue
		}
		edges = append(edges, ContractEdge{Source: paths[p.a], Target: paths[p.b], Weight: w})
	}
	// Heaviest first, because that is the order the limit cuts on. Ties break
	// on path so the same database always returns the same map: a set of edges
	// that reshuffles between two reloads reads as the chain having changed.
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].Weight != edges[j].Weight {
			return edges[i].Weight > edges[j].Weight
		}
		if edges[i].Source != edges[j].Source {
			return edges[i].Source < edges[j].Source
		}
		return edges[i].Target < edges[j].Target
	})
	if len(edges) > limit {
		edges = edges[:limit]
	}
	return edges, nil
}

// deployedPathIDs interns every package path deployed on the network, so pair
// counting can key on two int32s instead of two strings. Roughly a thousand
// rows on the busiest chain today.
//
// IDs are assigned in path order, which is what makes `a < b` on the ID pair
// equivalent to `a < b` on the paths, so pairs normalise without a string
// comparison per pair.
func (d *DB) deployedPathIDs(network string) (map[string]int32, []string, error) {
	rows, err := d.db.Query(`SELECT path FROM packages WHERE network = ? ORDER BY path`, network)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	ids := map[string]int32{}
	var paths []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, nil, err
		}
		ids[p] = int32(len(paths))
		paths = append(paths, p)
	}
	return ids, paths, rows.Err()
}

// ActivePaths returns the set of package paths that saw activity on the
// network inside the window: called at least once, or deployed inside it.
//
// The one thing the contracts map computes for itself and every other graph
// has no way to ask for. A dependency graph is drawn from `dependencies`,
// which records what the source imports and knows nothing about whether
// anyone still calls it, so "hide what nobody has touched this week" needs
// this set handed to it separately.
//
// Unlike the rest of this file, an empty network means every configured one
// rather than being rejected. A path set carries no per-chain quantity to be
// mixed up: "called somewhere in the last day" is the right answer for a
// dependency graph that is itself the union of every chain, which is what
// /api/deps returns when the reader has not picked one.
//
// Deploys count because a realm published this morning and not yet called is
// the most interesting thing on a one-day view, and the one thing a filter
// built on calls alone would hide. The contracts map applies the same two
// tests client-side against data it already holds; this is the same definition
// for the graphs that hold nothing.
//
// A zero `since` means all time, and is a real answer here rather than a
// no-op: "called ever" is what the all-time reading of the filter asks for,
// and on mainnet it is already a strong filter, because 152 of 346 deployed
// packages have never been called once.
func (d *DB) ActivePaths(network string, since time.Time) ([]string, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	// Built by concatenation for the reason ContractMapNodes documents: the
	// tidier `(? = '' OR ...)` form defeats idx_calls_block_time and makes
	// SQLite build a temp B-tree over the whole calls table instead.
	cutoff := sinceParam(since)
	clause := func() (string, []any) {
		var where []string
		var args []any
		if network != "" {
			where = append(where, "network = ?")
			args = append(args, network)
		}
		if cutoff != "" {
			where = append(where, "block_time >= ?")
			args = append(args, cutoff)
		}
		if len(where) == 0 {
			return "", nil
		}
		return " WHERE " + strings.Join(where, " AND "), args
	}

	callWhere, callArgs := clause()
	pkgWhere, pkgArgs := clause()
	// A UNION rather than two round trips. EXPLAIN QUERY PLAN on a seeded
	// database: the call half SEARCHes calls USING idx_calls_block_time and
	// the deploy half SEARCHes packages USING idx_pkgs_block_time, both on
	// (network=? AND block_time>?). The one temp B-tree is the UNION's own
	// dedup, not a table scan.
	//
	// The deploy half is skipped entirely for an all-time window, where every
	// package qualifies and the union would just be every path on the chain.
	q := "SELECT DISTINCT pkg_path FROM calls" + callWhere
	args := callArgs
	if cutoff != "" {
		q += " UNION SELECT path FROM packages" + pkgWhere
		args = append(args, pkgArgs...)
	}

	rows, err := d.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	paths := []string{}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		paths = append(paths, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Sorted so the response is byte-identical between two calls with the same
	// arguments, which is what lets it be cached and diffed.
	sort.Strings(paths)
	return paths, nil
}

// sinceParam renders a window cutoff the way block_time is stored, or empty
// for "all time".
//
// The queries compare it as a string, which is only correct because every
// stored block_time is RFC 3339 in UTC: same length, same offset, so
// lexical order is chronological order. A local-zone timestamp would compare
// wrong rather than fail, so the conversion to UTC here is load-bearing.
func sinceParam(since time.Time) string {
	if since.IsZero() {
		return ""
	}
	return since.UTC().Format(time.RFC3339)
}
