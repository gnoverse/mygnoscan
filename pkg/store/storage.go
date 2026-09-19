package store

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

func (d *DB) InsertStorageEvent(network, txHash string, eventIndex int, pkgPath string,
	blockHeight int, blockTime, kind string, bytesDelta, fee int) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.db.Exec(`
		INSERT OR IGNORE INTO storage_events
			(network, tx_hash, event_index, pkg_path, block_height, block_time, kind, bytes_delta, fee)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, network, txHash, eventIndex, pkgPath, blockHeight, blockTime, kind, bytesDelta, fee)
	return err
}

// UpsertPackageFile inserts or updates a package file.

type StorageTimePoint struct {
	Time          string `json:"time"`
	BytesAdded    int    `json:"bytes_added"`
	FilesAdded    int    `json:"files_added"`
	PackagesAdded int    `json:"packages_added"`
}

func (d *DB) GetStorageTimeSeries(network, realmPath, granularity string, days int) ([]StorageTimePoint, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	sqlFmt, step, truncFn := timeseriesFormat(granularity)
	startTime := time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339)

	extraFilters := " AND " + d.networkFilter("p.network", network)
	args := []any{startTime}
	if realmPath != "" {
		extraFilters += " AND p.path = ?"
		args = append(args, realmPath)
	}

	q := fmt.Sprintf(
		"SELECT strftime('%s', p.block_time) as bucket,"+
			" SUM(LENGTH(pf.body)) as bytes_added,"+
			" COUNT(*) as files_added,"+
			" COUNT(DISTINCT p.path) as packages_added"+
			" FROM package_files pf"+
			" JOIN packages p ON p.network = pf.network AND p.path = pf.package_path"+
			" WHERE p.block_time >= ?%s"+
			" GROUP BY bucket ORDER BY bucket ASC",
		sqlFmt, extraFilters)

	rows, err := d.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type row struct {
		bucket        string
		bytesAdded    int
		filesAdded    int
		packagesAdded int
	}
	buckets := make(map[string]*row)
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.bucket, &r.bytesAdded, &r.filesAdded, &r.packagesAdded); err != nil {
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
	var out []StorageTimePoint
	for cur := start; !cur.After(end); cur = truncFn(cur.Add(step)) {
		k := bucketKey(cur, granularity)
		if r, ok := buckets[k]; ok {
			out = append(out, StorageTimePoint{
				Time:          k,
				BytesAdded:    r.bytesAdded,
				FilesAdded:    r.filesAdded,
				PackagesAdded: r.packagesAdded,
			})
		} else {
			out = append(out, StorageTimePoint{Time: k})
		}
	}
	return out, nil
}

func (d *DB) GetRealmsWithStorage(network string, days int) ([]string, error) {
	startTime := time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339)
	q := `SELECT DISTINCT p.path
		FROM package_files pf
		JOIN packages p ON p.network = pf.network AND p.path = pf.package_path
		WHERE p.block_time >= ? AND p.is_realm = 1`
	q += " AND " + d.networkFilter("p.network", network)
	q += " ORDER BY p.path ASC"
	rows, err := d.db.Query(q, startTime)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var paths []string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, err
		}
		paths = append(paths, path)
	}
	return paths, rows.Err()
}

// GetActiveAddressTimeSeries answers how many distinct addresses were active in
// each bucket, from the rollup where it can and from the source tables where it
// must.
//
// Before the first refresh there is no rollup — a fresh database, or the first
// start after this shipped — and the whole series is computed live rather than
// reported as zero, which would read as "nobody has ever used this chain".

// StorageDeltaPoint is on-chain storage movement per bucket, read from
// storage_events.
//
// This measures something different from StorageTimePoint: deposits and
// releases are what the chain actually charged and refunded, while bytes_added
// counts source bytes from package_files, a proxy that only ever grows. Both
// are useful, so both are served.
type StorageDeltaPoint struct {
	Time      string `json:"time"`
	Deposited int    `json:"deposited"`
	Released  int    `json:"released"` // negative, as the chain emits it
	Net       int    `json:"net"`
}

func (d *DB) GetStorageDeltaTimeSeries(network, realmPath, granularity string, days int) ([]StorageDeltaPoint, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	sqlFmt, step, truncFn := timeseriesFormat(granularity)
	start := time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339)

	filter := " AND " + d.networkFilter("network", network)
	args := []any{start}
	if realmPath != "" {
		filter += " AND pkg_path = ?"
		args = append(args, realmPath)
	}

	q := fmt.Sprintf(`
		SELECT strftime('%s', block_time) AS bucket,
		       COALESCE(SUM(CASE WHEN bytes_delta > 0 THEN bytes_delta ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN bytes_delta < 0 THEN bytes_delta ELSE 0 END), 0),
		       COALESCE(SUM(bytes_delta), 0)
		FROM storage_events
		WHERE block_time >= ?%s
		GROUP BY bucket ORDER BY bucket ASC`, sqlFmt, filter)

	rows, err := d.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	buckets := make(map[string]*StorageDeltaPoint)
	for rows.Next() {
		var bucket sql.NullString
		var dep, rel, net int
		if err := rows.Scan(&bucket, &dep, &rel, &net); err != nil {
			return nil, err
		}
		// A row whose block_time will not parse yields a NULL bucket: the window
		// filter is a string comparison, so garbage gets past it. Skip the row
		// rather than failing the whole chart.
		if !bucket.Valid || bucket.String == "" {
			continue
		}
		p := StorageDeltaPoint{Time: bucket.String, Deposited: dep, Released: rel, Net: net}
		buckets[p.Time] = &p
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return fillBuckets(buckets, days, granularity, step, truncFn,
		func(k string) StorageDeltaPoint { return StorageDeltaPoint{Time: k} },
		func(p *StorageDeltaPoint) {}), nil
}

// StorageConsumer ranks realms by how much storage they moved.
type StorageConsumer struct {
	Network   string `json:"network"`
	PkgPath   string `json:"pkg_path"`
	Deposited int    `json:"deposited"`
	Released  int    `json:"released"`
	Net       int    `json:"net"`
}

// GetStorageConsumers ranks realms by absolute net storage change.
//
// Keyed by (network, pkg_path), not pkg_path alone: the same realm path is
// deployed on more than one chain, and ranking by path would add a busy realm's
// mainnet and testnet storage into one row that describes neither.
func (d *DB) GetStorageConsumers(network string, days, topN int) ([]StorageConsumer, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	if topN <= 0 {
		topN = 20
	}
	if topN > 100 {
		topN = 100
	}
	start := time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339)

	q := fmt.Sprintf(`
		SELECT network, pkg_path,
		       COALESCE(SUM(CASE WHEN bytes_delta > 0 THEN bytes_delta ELSE 0 END), 0) AS deposited,
		       COALESCE(SUM(CASE WHEN bytes_delta < 0 THEN bytes_delta ELSE 0 END), 0) AS released,
		       COALESCE(SUM(bytes_delta), 0) AS net
		FROM storage_events
		WHERE block_time >= ? AND %s
		GROUP BY network, pkg_path
		ORDER BY ABS(net) DESC, network ASC, pkg_path ASC
		LIMIT ?`, d.networkFilter("network", network))

	rows, err := d.db.Query(q, start, topN)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []StorageConsumer
	for rows.Next() {
		var c StorageConsumer
		if err := rows.Scan(&c.Network, &c.PkgPath, &c.Deposited, &c.Released, &c.Net); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetRealmsWithStorageEvents lists realms that actually moved storage in the
// window.
//
// GetRealmsWithStorage answers the same question for the package_files series
// and cannot be reused here: it lists realms with recent *package rows*, which
// is a different set. A realm that only released state has storage events and
// no new files, so picking it from that list would draw an empty chart.
func (d *DB) GetRealmsWithStorageEvents(network string, days int) ([]string, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	start := time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339)
	rows, err := d.db.Query(`
		SELECT DISTINCT pkg_path FROM storage_events
		 WHERE block_time >= ? AND `+d.networkFilter("network", network)+`
		 ORDER BY pkg_path ASC`, start)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var paths []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		paths = append(paths, p)
	}
	return paths, rows.Err()
}
