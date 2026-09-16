package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/moul/mygnoscan/pkg/indexer"
	_ "modernc.org/sqlite"
)

func (d *DB) UpsertPackage(network, path, name, creator, txHash string, blockHeight int, blockTime string, isRealm bool, numFiles int) error {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	_, err := d.db.Exec(`
		INSERT OR REPLACE INTO packages (network, path, name, creator, tx_hash, block_height, block_time, is_realm, num_files)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, network, path, name, creator, txHash, blockHeight, blockTime, isRealm, numFiles)
	return err
}

// InsertPackageSubmission records one MsgAddPackage as its own permanent
// row, independent of whatever UpsertPackage does to packages' current-state
// row for the same path. msgIndex distinguishes multiple AddPackage messages
// in one multicall transaction, the same reason calls carries it (#152) —
// without it two submissions in the same tx would collide on (network,
// tx_hash) and INSERT OR IGNORE would silently drop the second.

func (d *DB) InsertPackageSubmission(network, txHash string, msgIndex int, path, name, creator string, blockHeight int, blockTime string, isRealm bool, numFiles int, success bool) error {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	_, err := d.db.Exec(`
		INSERT OR IGNORE INTO package_submissions
			(network, tx_hash, msg_index, path, name, creator, block_height, block_time, is_realm, num_files, success)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, network, txHash, msgIndex, path, name, creator, blockHeight, blockTime, isRealm, numFiles, success)
	return err
}

// InsertStorageEvent records one storage deposit or unlock.
//
// eventIndex is the event's position within the transaction's event list, which
// is what makes the key unique: one transaction routinely emits several storage
// events, and a transaction that touches two realms emits one per realm.
//
// INSERT OR IGNORE because the sync walk overlaps its own window on every pass —
// re-reading an event already stored must be a no-op, not a duplicate that
// doubles a realm's storage total.

func (d *DB) UpsertPackageFile(network, pkgPath, fileName, body string) error {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	_, err := d.db.Exec(`
		INSERT OR REPLACE INTO package_files (network, package_path, file_name, body)
		VALUES (?, ?, ?, ?)
	`, network, pkgPath, fileName, body)
	return err
}

// StoredPackageRef identifies a package whose source is held locally.

type StoredPackageRef struct {
	Network string
	Path    string
}

// StoredPackageRefs lists every package that has source in the database.
//
// Deliberately returns references rather than bodies, and takes no callback: a
// caller iterating packages will want to write as it goes, and d.mu is not
// reentrant — handing out source under the read lock would deadlock the first
// caller that tried. package_files is also the largest table on a busy chain, so
// not buffering every body is worth having anyway.

func (d *DB) StoredPackageRefs() ([]StoredPackageRef, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	rows, err := d.db.Query(`
		SELECT DISTINCT network, package_path
		FROM package_files
		ORDER BY network, package_path
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []StoredPackageRef
	for rows.Next() {
		var ref StoredPackageRef
		if err := rows.Scan(&ref.Network, &ref.Path); err != nil {
			return nil, err
		}
		out = append(out, ref)
	}
	return out, rows.Err()
}

// StoredPackageFiles returns one package's stored source.

func (d *DB) StoredPackageFiles(network, pkgPath string) ([]indexer.MemFile, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	rows, err := d.db.Query(`
		SELECT file_name, body FROM package_files
		WHERE network = ? AND package_path = ?
		ORDER BY file_name
	`, network, pkgPath)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []indexer.MemFile
	for rows.Next() {
		var f indexer.MemFile
		if err := rows.Scan(&f.Name, &f.Body); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// SetDependencies replaces all dependencies for a package.

func (d *DB) SetDependencies(network, pkgPath string, imports []string) error {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()

	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM dependencies WHERE network = ? AND package_path = ?`, network, pkgPath); err != nil {
		return err
	}

	stmt, err := tx.Prepare(`INSERT OR IGNORE INTO dependencies (network, package_path, import_path) VALUES (?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, imp := range imports {
		// A package cannot import itself, so an edge saying it does is wrong
		// wherever it came from. The analyzer already excludes the package's own
		// path during extraction, but this is the boundary the table is written
		// through and the cheapest place to make the invariant hold for every
		// caller — including a backfill replaying older, looser extraction.
		if imp == pkgPath {
			continue
		}
		if _, err := stmt.Exec(network, pkgPath, imp); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// InsertCall records one MsgCall message. msgIndex is that message's position
// within its transaction, which is what keeps repeat calls to the same
// function inside one multicall transaction from collapsing into a single
// stored row.

type GasRealm struct {
	Path    string `json:"path"`
	Gas     int    `json:"gas"`
	Fees    int    `json:"fees"`
	TxCount int    `json:"tx_count"`
}

// GasCaller is per-address gas consumption.

type PackageInfo struct {
	Network     string `json:"network,omitempty"`
	Path        string `json:"path"`
	Name        string `json:"name"`
	Creator     string `json:"creator"`
	BlockHeight int    `json:"block_height"`
	BlockTime   string `json:"block_time,omitempty"`
	TxHash      string `json:"tx_hash"`
	IsRealm     bool   `json:"is_realm"`
	NumFiles    int    `json:"num_files"`
	Calls       int    `json:"calls"`
	Importers   int    `json:"importers"`
	Imports     int    `json:"imports"`
	UniqueUsers int    `json:"unique_users"`
	// LastCallHeight/LastCallTime are the realm's most recent call, zero/empty
	// if it has never been called. Distinct from BlockHeight/BlockTime, which
	// are the deploy — a realm can be old and still busy, or new and dormant.
	LastCallHeight int    `json:"last_call_height,omitempty"`
	LastCallTime   string `json:"last_call_time,omitempty"`
	// GasUsed is the realm's all-time gas, from gas_realm_rollup. Zero for a
	// realm the rollup has not seen, which is not the same as a realm that
	// burned no gas — the rollup is rebuilt periodically, so a freshly deployed
	// realm reads zero until the next pass.
	GasUsed int `json:"gas_used"`
	// StorageDeposit is the realm's net storage cost in ugnot — deposits less
	// refunds — from storage_realm_rollup. Net rather than gross: a realm that
	// frees what it wrote has been refunded, and the gross figure would bill it
	// for storage it no longer holds.
	StorageDeposit int `json:"storage_deposit"`
	StorageBytes   int `json:"storage_bytes"`
}

type PackageDetail struct {
	PackageInfo
	// ExportedFuncs is what the realm makes callable, derived from its source
	// rather than from what has been called. Distinct from the calls tab, which
	// can only show functions someone has already invoked.
	ExportedFuncs []string   `json:"exported_funcs,omitempty"`
	Files         []FileInfo `json:"files"`
	Imports       []string   `json:"imports"`
	// Dependents carries each importer's creator, not just its path.
	//
	// A flat list of paths reads as adoption when it may be one project's
	// version churn: r/gnoswap/router has 25 dependents, 21 of which are one
	// deployer's sequential gnomemepad releases. The question a reader has is
	// "how many independent parties depend on this", and answering it from paths
	// alone meant cross-referencing creators by hand across pages.
	Dependents []Dependent  `json:"dependents"`
	Callers    []CallInfo   `json:"recent_calls"`
	MsgRunRefs []MsgRunInfo `json:"msgrun_refs"`
	CallCount  int          `json:"call_count"`
}

// Dependent is one realm that imports another.

type Dependent struct {
	Path    string `json:"path"`
	Creator string `json:"creator,omitempty"`
}

func (d *DB) CountPackages(network string, realmOnly bool) (int, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	// Scoped to the configured networks, so this agrees with the same count on
	// the home page. It did not: the stat tile read 321 realms while the
	// directory header read 508, because this branch counted retired chains and
	// that one did not. topaz alone accounts for the 187 between them.
	q := `SELECT COUNT(*) FROM packages WHERE is_realm = ? AND ` +
		d.networkFilter("network", network)
	var count int
	err := d.db.QueryRow(q, realmOnly).Scan(&count)
	return count, err
}

// ListPackages returns all packages, optionally filtered.
// packageSortClause maps a sort key to SQL. Whitelisted rather than
// interpolated: the value comes from a query parameter.

func packageSortClause(sortBy string) string {
	switch sortBy {
	case "calls":
		return "calls DESC, p.block_height DESC"
	case "importers":
		return "importers DESC, p.block_height DESC"
	case "imports":
		return "imports DESC, p.block_height DESC"
	case "users":
		return "unique_users DESC, p.block_height DESC"
	case "gas":
		return "gas_used DESC, p.block_height DESC"
	case "storage":
		return "storage_deposit DESC, p.block_height DESC"
	case "last_call":
		// A realm never called has no last_call_height (NULL), which SQLite's
		// default NULLS LAST already sorts after every real height on a DESC
		// ordering — no CASE needed to push the never-called to the bottom.
		return "last_call_height DESC, p.block_height DESC"
	case "name":
		return "p.path ASC"
	case "oldest":
		return "p.block_height ASC"
	default: // newest
		return "p.block_height DESC"
	}
}

func (d *DB) ListPackages(network string, realmOnly bool, limit, offset int, sortBy string) ([]PackageInfo, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	// Usage counts come from correlated subqueries rather than joins: a join on
	// path alone would mix networks together, and grouping four tables in one
	// query multiplies rows against each other.
	q := `SELECT p.network, p.path, p.name, p.creator, p.block_height, p.block_time, p.tx_hash, p.is_realm, p.num_files,
		(SELECT COUNT(*) FROM calls c
		   WHERE c.network = p.network AND c.pkg_path = p.path) AS calls,
		(SELECT COUNT(*) FROM dependencies d
		   WHERE d.network = p.network AND d.import_path = p.path) AS importers,
		(SELECT COUNT(*) FROM dependencies d
		   WHERE d.network = p.network AND d.package_path = p.path) AS imports,
		(SELECT COUNT(DISTINCT c.caller) FROM calls c
		   WHERE c.network = p.network AND c.pkg_path = p.path) AS unique_users,
		(SELECT c.block_height FROM calls c
		   WHERE c.network = p.network AND c.pkg_path = p.path
		   ORDER BY c.block_height DESC LIMIT 1) AS last_call_height,
		(SELECT c.block_time FROM calls c
		   WHERE c.network = p.network AND c.pkg_path = p.path
		   ORDER BY c.block_height DESC LIMIT 1) AS last_call_time,
		-- The rollup is already keyed by (network, path), so this is a primary-key
		-- lookup rather than the scan the other subqueries here do. COALESCE
		-- because a realm absent from the rollup must read 0, not NULL — a NULL
		-- would sort ahead of every real value on the DESC ordering and put the
		-- realms with no gas data at the top of a "most gas" sort.
		(SELECT COALESCE(g.gas_used, 0) FROM gas_realm_rollup g
		   WHERE g.network = p.network AND g.path = p.path) AS gas_used,
		(SELECT COALESCE(sr.fee_net, 0) FROM storage_realm_rollup sr
		   WHERE sr.network = p.network AND sr.path = p.path) AS storage_deposit,
		(SELECT COALESCE(sr.bytes_net, 0) FROM storage_realm_rollup sr
		   WHERE sr.network = p.network AND sr.path = p.path) AS storage_bytes
		FROM packages p WHERE p.is_realm = ? AND ` + d.networkFilter("p.network", network)
	args := []any{realmOnly}
	q += ` ORDER BY ` + packageSortClause(sortBy)
	if limit > 0 {
		q += fmt.Sprintf(` LIMIT %d OFFSET %d`, limit, offset)
	}

	rows, err := d.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var pkgs []PackageInfo
	for rows.Next() {
		var blockTime sql.NullString
		var lastCallHeight sql.NullInt64
		var lastCallTime sql.NullString
		var gasUsed, storageDeposit, storageBytes sql.NullInt64
		var p PackageInfo
		if err := rows.Scan(&p.Network, &p.Path, &p.Name, &p.Creator, &p.BlockHeight, &blockTime, &p.TxHash,
			&p.IsRealm, &p.NumFiles, &p.Calls, &p.Importers, &p.Imports, &p.UniqueUsers,
			&lastCallHeight, &lastCallTime, &gasUsed, &storageDeposit, &storageBytes); err != nil {
			return nil, err
		}
		p.GasUsed = int(gasUsed.Int64)
		p.StorageDeposit = int(storageDeposit.Int64)
		p.StorageBytes = int(storageBytes.Int64)
		p.BlockTime = blockTime.String
		p.LastCallHeight = int(lastCallHeight.Int64)
		p.LastCallTime = lastCallTime.String
		pkgs = append(pkgs, p)
	}
	return pkgs, rows.Err()
}

// GetPackageDetail returns full details for a package.

func (d *DB) GetPackageDetail(network, path string) (*PackageDetail, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	q := `SELECT network, path, name, creator, block_height, tx_hash, is_realm, num_files
	      FROM packages WHERE path = ? AND ` + d.networkFilter("network", network)
	args := []any{path}

	var p PackageDetail
	err := d.db.QueryRow(q, args...).Scan(&p.Network, &p.Path, &p.Name, &p.Creator, &p.BlockHeight, &p.TxHash, &p.IsRealm, &p.NumFiles)
	if err != nil {
		return nil, err
	}

	// Files
	filesQ := `SELECT file_name, body FROM package_files WHERE package_path = ? AND network = ?`
	rows, err := d.db.Query(filesQ, path, p.Network)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var f FileInfo
		if err := rows.Scan(&f.Name, &f.Body); err != nil {
			return nil, err
		}
		p.Files = append(p.Files, f)
	}

	// Imports (dependencies)
	// `import_path != package_path` guards the read as well as the write.
	//
	// Self-edges are already rejected on the way in, but rows written before
	// that guard existed are still in the table — six of them reached the live
	// graph from the old regex-based extraction, and r/gnoswap/router showed up
	// as both its own import and its own dependent. Filtering here fixes those
	// without a migration, and the graph tab has always excluded them, so this
	// is what makes the two views agree.
	impRows, err := d.db.Query(`SELECT import_path FROM dependencies
		WHERE package_path = ? AND network = ? AND import_path != package_path`, path, p.Network)
	if err != nil {
		return nil, err
	}
	defer impRows.Close()
	for impRows.Next() {
		var imp string
		if err := impRows.Scan(&imp); err != nil {
			return nil, err
		}
		p.Imports = append(p.Imports, imp)
	}

	// Dependents (who imports this), with each one's creator.
	//
	// LEFT JOIN rather than JOIN: an edge can point at a package this instance
	// has not synced the deploy for, and dropping those would understate the
	// count. Such a row simply has no creator.
	depRows, err := d.db.Query(`
		SELECT dep.package_path, COALESCE(pkg.creator, '')
		FROM dependencies dep
		LEFT JOIN packages pkg ON pkg.network = dep.network AND pkg.path = dep.package_path
		WHERE dep.import_path = ? AND dep.network = ? AND dep.package_path != dep.import_path
		ORDER BY dep.package_path ASC`, path, p.Network)
	if err != nil {
		return nil, err
	}
	defer depRows.Close()
	for depRows.Next() {
		var dep Dependent
		if err := depRows.Scan(&dep.Path, &dep.Creator); err != nil {
			return nil, err
		}
		p.Dependents = append(p.Dependents, dep)
	}

	// Recent calls
	callRows, err := d.db.Query(`
		SELECT tx_hash, block_height, caller, func_name, success
		FROM calls WHERE pkg_path = ? AND network = ?
		ORDER BY block_height DESC LIMIT 50
	`, path, p.Network)
	if err != nil {
		return nil, err
	}
	defer callRows.Close()
	for callRows.Next() {
		var c CallInfo
		if err := callRows.Scan(&c.TxHash, &c.BlockHeight, &c.Caller, &c.FuncName, &c.Success); err != nil {
			return nil, err
		}
		p.Callers = append(p.Callers, c)
	}

	// Call count
	d.db.QueryRow(`SELECT COUNT(*) FROM calls WHERE pkg_path = ? AND network = ?`, path, p.Network).Scan(&p.CallCount)

	// MsgRun references (where source contains import of this path)
	runRows, err := d.db.Query(`
		SELECT tx_hash, block_height, caller, success
		FROM msg_runs WHERE source LIKE ? AND network = ?
		ORDER BY block_height DESC LIMIT 50
	`, "%"+path+"%", p.Network)
	if err != nil {
		return nil, err
	}
	defer runRows.Close()
	for runRows.Next() {
		var r MsgRunInfo
		if err := runRows.Scan(&r.TxHash, &r.BlockHeight, &r.Caller, &r.Success); err != nil {
			return nil, err
		}
		p.MsgRunRefs = append(p.MsgRunRefs, r)
	}

	return &p, nil
}

// GovDAORelatedMsgRuns finds maketx-run scripts that plausibly created or
// touched a governance proposal. A proposal's own creation is a MsgRun (a
// `maketx run` script that imports gov/dao and calls
// dao.MustCreateProposal(...)), not a MsgCall, so it never appears in the
// `calls` table — this is the same "search msg_runs.source" technique
// GetPackageDetail's MsgRunRefs already uses for realm-to-script tracing,
// narrowed with a second predicate on the proposal's own executor package
// path (from its "Executor created in: `pkgpath`" line) since gov/dao alone
// matches every proposal ever created.
//
// This is a heuristic, not a guarantee: it finds scripts that reference both
// packages by name, which a script doing something else entirely with both
// imported could also match. With gov/dao's proposal volume (a handful, not
// thousands) that tradeoff favors recall over precision — a caller can read
// the matched script's source and judge for themselves.

func (d *DB) GetDependencyGraph(network, path string) (map[string][]string, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	graph := make(map[string][]string)
	visited := make(map[string]bool)

	var walk func(p string) error
	walk = func(p string) error {
		if visited[p] {
			return nil
		}
		visited[p] = true

		var rows *sql.Rows
		var err error
		rows, err = d.db.Query(`SELECT import_path FROM dependencies WHERE package_path = ? AND `+
			d.networkFilter("network", network), p)
		if err != nil {
			return err
		}
		defer rows.Close()

		var deps []string
		for rows.Next() {
			var dep string
			if err := rows.Scan(&dep); err != nil {
				return err
			}
			deps = append(deps, dep)
		}
		graph[p] = deps

		for _, dep := range deps {
			if strings.HasPrefix(dep, "gno.land/") {
				if err := walk(dep); err != nil {
					return err
				}
			}
		}
		return nil
	}

	if err := walk(path); err != nil {
		return nil, err
	}
	return graph, nil
}

// GetReverseGraph returns all packages that depend on path (recursive).

func (d *DB) GetReverseGraph(network, path string) (map[string][]string, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	graph := make(map[string][]string)
	visited := make(map[string]bool)

	var walk func(p string) error
	walk = func(p string) error {
		if visited[p] {
			return nil
		}
		visited[p] = true

		var rows *sql.Rows
		var err error
		rows, err = d.db.Query(`SELECT package_path FROM dependencies WHERE import_path = ? AND `+
			d.networkFilter("network", network), p)
		if err != nil {
			return err
		}
		defer rows.Close()

		var deps []string
		for rows.Next() {
			var dep string
			if err := rows.Scan(&dep); err != nil {
				return err
			}
			deps = append(deps, dep)
		}
		graph[p] = deps

		for _, dep := range deps {
			if err := walk(dep); err != nil {
				return err
			}
		}
		return nil
	}

	if err := walk(path); err != nil {
		return nil, err
	}
	return graph, nil
}

func (d *DB) GetTokenPackages(network string) ([]TokenInfo, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	// p.network comes along so the row can say which chain it is from. No two
	// chains currently share a package path, but nothing prevents it, and a
	// silent merge would be indistinguishable from a single deployment.
	q := `
		SELECT DISTINCT p.path, p.network, p.name, p.creator, COALESCE(c.cnt, 0)
		FROM packages p
		JOIN dependencies dep ON dep.package_path = p.path AND dep.network = p.network
		LEFT JOIN (SELECT pkg_path, network, COUNT(*) as cnt FROM calls GROUP BY network, pkg_path) c ON c.pkg_path = p.path AND c.network = p.network
		WHERE dep.import_path LIKE '%grc20%'`
	// Scoped like every other reader, so a retired network cannot reappear here.
	q += ` AND ` + d.networkFilter("p.network", network)
	args := []any{}
	q += ` ORDER BY p.block_height DESC`
	rows, err := d.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tokens := []TokenInfo{}
	for rows.Next() {
		var t TokenInfo
		if err := rows.Scan(&t.Path, &t.Network, &t.Name, &t.Creator, &t.CallCount); err != nil {
			return nil, err
		}
		tokens = append(tokens, t)
	}
	return tokens, rows.Err()
}

type RealmActivity struct {
	Network    string `json:"network,omitempty"`
	Path       string `json:"path"`
	Calls      int    `json:"calls"`
	Callers    int    `json:"callers"`
	Dependents int    `json:"dependents"`
	IsRealm    bool   `json:"is_realm"`
}

func (d *DB) GetPackageTimeSeries(network, granularity string, days int) ([]PkgTimePoint, error) {
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

	subq := func(typ string, isRealm int) string {
		return fmt.Sprintf(
			"SELECT strftime('%s', t.block_time) as bucket, '%s' as typ, COUNT(*) as cnt"+
				" FROM packages t"+
				" WHERE t.block_time >= ? AND t.is_realm = %d%s"+
				" GROUP BY bucket",
			sqlFmt, typ, isRealm, netFilter)
	}

	q := subq("packages", 0) +
		" UNION ALL " + subq("realms", 1) +
		" ORDER BY bucket ASC"

	args := []any{startTime, startTime}

	rows, err := d.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	buckets := make(map[string]*PkgTimePoint)
	for rows.Next() {
		var bucket, typ string
		var cnt int
		if err := rows.Scan(&bucket, &typ, &cnt); err != nil {
			return nil, err
		}
		pt, ok := buckets[bucket]
		if !ok {
			pt = &PkgTimePoint{Time: bucket}
			buckets[bucket] = pt
		}
		switch typ {
		case "packages":
			pt.Packages = cnt
		case "realms":
			pt.Realms = cnt
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return fillBuckets(buckets, days, granularity, step, truncFn,
		func(k string) PkgTimePoint { return PkgTimePoint{Time: k} },
		func(pt *PkgTimePoint) { pt.Total = pt.Packages + pt.Realms },
	), nil
}

// perChainBucketCount builds a per-bucket count of distinct addresses, keyed by
// (address, network) rather than by address alone.
//
// The same address on two chains is two actors with two histories, so counting
// it once undercounts. Every address figure in the project is counted this way;
// doing it here too is what keeps a series' components consistent with the
// totals computed beside them, rather than producing a page whose own numbers
// contradict each other.

const namespaceLabelMinPackages = 3

// namespaceLabelDominance is the share of a deployer's own packages that must sit
// under the namespace. Someone who publishes widely and happens to have three
// packages under a prefix is not that prefix's owner.

type WatchedRealm struct {
	Network    string `json:"network"`
	Path       string `json:"path"`
	Exists     bool   `json:"exists"`
	Calls      int    `json:"calls"`
	Calls24h   int    `json:"calls_24h"`
	Importers  int    `json:"importers"`
	LastHeight int    `json:"last_height"`
	LastTime   string `json:"last_time,omitempty"`
	// NewSince counts activity above the height the caller last saw. Height
	// rather than a timestamp because it is exact and monotonic per chain,
	// where a wall-clock comparison drifts against block time.
	//
	// Zero when the caller supplied no baseline. Reporting the entire history as
	// "new" in that case would be technically defensible and useless.
	NewSince int `json:"new_since"`
}

// WatchedAddress is one address's activity summary.

func (d *DB) WatchRealms(network string, items []WatchRequest) ([]WatchedRealm, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	if len(items) == 0 {
		return []WatchedRealm{}, nil
	}
	since24h := time.Now().UTC().Add(-24 * time.Hour).Format(time.RFC3339)
	netFilter := d.networkFilter("network", network)

	out := make([]WatchedRealm, 0, len(items))
	for _, item := range items {
		w := WatchedRealm{Path: item.ID}

		// The realm itself. A watched path can exist on several chains; report
		// the one the current view is scoped to, or the newest deploy otherwise.
		err := d.db.QueryRow(`SELECT network FROM packages WHERE path = ? AND `+netFilter+
			` ORDER BY block_height DESC LIMIT 1`, item.ID).Scan(&w.Network)
		if err == nil {
			w.Exists = true
		} else if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}

		d.db.QueryRow(`SELECT COUNT(*) FROM calls WHERE pkg_path = ? AND `+netFilter,
			item.ID).Scan(&w.Calls)
		d.db.QueryRow(`SELECT COUNT(*) FROM calls WHERE pkg_path = ? AND block_time >= ? AND `+netFilter,
			item.ID, since24h).Scan(&w.Calls24h)
		d.db.QueryRow(`SELECT COUNT(*) FROM dependencies WHERE import_path = ? AND `+netFilter,
			item.ID).Scan(&w.Importers)

		var height sql.NullInt64
		var when sql.NullString
		d.db.QueryRow(`SELECT block_height, block_time FROM calls WHERE pkg_path = ? AND `+netFilter+
			` ORDER BY block_height DESC LIMIT 1`, item.ID).Scan(&height, &when)
		w.LastHeight, w.LastTime = int(height.Int64), when.String

		if item.Since > 0 {
			d.db.QueryRow(`SELECT COUNT(*) FROM calls WHERE pkg_path = ? AND block_height > ? AND `+netFilter,
				item.ID, item.Since).Scan(&w.NewSince)
		}
		out = append(out, w)
	}
	return out, nil
}

// WatchAddresses summarises activity for a set of watched addresses.

const RealmsWithCallsMaxLimit = 100

// GetRealmsWithCalls lists the realms called in the window, busiest first.
// Feeds the function heatmap's realm selector.

func (d *DB) GetRealmsWithCalls(network string, days, limit int) ([]string, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	if limit <= 0 {
		limit = 30
	} else if limit > RealmsWithCallsMaxLimit {
		limit = RealmsWithCallsMaxLimit
	}
	// Midnight of day-(days-1), the same start instant GetFunctionCallHeatmap
	// uses. GetFunctionCallHeatmap's grid begins there, so a realm whose only
	// calls fall between that instant and time.Now().AddDate(0, 0, -days) would
	// otherwise appear in this selector and yield an empty heatmap.
	now := time.Now().UTC()
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).
		AddDate(0, 0, -(days - 1)).Format(time.RFC3339)

	netFilter := " AND " + d.networkFilter("network", network)
	args := []any{start}
	args = append(args, limit)

	rows, err := d.db.Query(fmt.Sprintf(
		"SELECT pkg_path, COUNT(*) AS n FROM calls WHERE block_time >= ?%s"+
			" GROUP BY pkg_path ORDER BY n DESC, pkg_path ASC LIMIT ?", netFilter), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var p string
		var n int
		if err := rows.Scan(&p, &n); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetFunctionCallHeatmap returns calls per (function, day) for one realm.
//
// The grid is zero-filled server-side over the full day range and the selected
// functions, so the caller never has to re-derive which days the window covers.
// Cells come back function-major, functions ordered busiest-first.
//
// Empty cell is 0 — "this function was not called that day" is a real zero, not
// missing data (§10.1).

type RealmSharePoint struct {
	Bucket  string `json:"bucket"`
	Network string `json:"network,omitempty"`
	Path    string `json:"path"`
	Value   int    `json:"value"`
}

// GetRealmShareTimeSeries buckets fee or storage spend per realm over time.
//
// The existing gas and storage rollups are all-time snapshots — "who has spent
// the most ever" — which cannot answer "where is activity concentrating now".
// A realm that dominated six months ago and has since gone quiet still tops
// every snapshot table on the site.
//
// metric is "fee" or "storage":
//
//   - fee attributes a transaction's gas fee to the realm it touched, the same
//     attribution gas_realm_rollup uses, so the two agree when summed over all
//     time. The DISTINCT matters for the same reason it does there: a multicall
//     hitting one realm several times must be charged its fee once.
//   - storage sums the signed storage events, so a bucket in which a realm
//     freed more than it wrote is negative. That is the honest reading and the
//     caller should render it as such rather than clamping to zero.

func (d *DB) GetRealmShareTimeSeries(network, metric, granularity string, days int) ([]RealmSharePoint, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	sqlFmt, _, _ := timeseriesFormat(granularity)
	startTime := time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339)

	var q string
	var args []any
	switch metric {
	case "storage":
		netFilter := " AND " + d.networkFilter("t.network", network)
		q = fmt.Sprintf(
			"SELECT strftime('%s', t.block_time) as bucket, t.network, t.pkg_path, SUM(t.fee)"+
				" FROM storage_events t"+
				" WHERE t.block_time >= ?%s AND t.pkg_path != ''"+
				" GROUP BY bucket, t.network, t.pkg_path"+
				" ORDER BY bucket ASC",
			sqlFmt, netFilter)
		args = []any{startTime}
	case "fee":
		netFilter := " AND " + d.networkFilter("t.network", network)
		// One row per (realm, transaction) before summing, so a transaction
		// touching a realm twice contributes its fee once.
		q = fmt.Sprintf(
			"SELECT bucket, network, path, SUM(gas_fee) FROM ("+
				" SELECT DISTINCT strftime('%s', t.block_time) as bucket, t.network, c.pkg_path as path, t.tx_hash, t.gas_fee"+
				"  FROM calls c JOIN transactions t"+
				"    ON t.network = c.network AND t.tx_hash = c.tx_hash"+
				" WHERE t.block_time >= ?%s"+
				" UNION"+
				" SELECT DISTINCT strftime('%s', t.block_time) as bucket, t.network, p.path, t.tx_hash, t.gas_fee"+
				"  FROM packages p JOIN transactions t"+
				"    ON t.network = p.network AND t.tx_hash = p.tx_hash"+
				" WHERE t.block_time >= ?%s"+
				") GROUP BY bucket, network, path ORDER BY bucket ASC",
			sqlFmt, netFilter, sqlFmt, netFilter)
		args = []any{startTime, startTime}
	default:
		return nil, fmt.Errorf("unknown metric %q: want fee or storage", metric)
	}

	rows, err := d.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []RealmSharePoint
	for rows.Next() {
		var p RealmSharePoint
		if err := rows.Scan(&p.Bucket, &p.Network, &p.Path, &p.Value); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
