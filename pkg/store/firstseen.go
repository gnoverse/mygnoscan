package store

import (
	"database/sql"
	"fmt"
)

// first_seen: when each participant first appeared, maintained on the rollup
// tick rather than computed per request.
//
// "New this week" is three questions (a first-time caller, a first-time
// deployer, a package called for the first time) and each one answered live is
// a GROUP BY ... HAVING MIN(height) over the whole of calls or
// package_submissions. This table answers all three with one range scan on
// (network, kind, at), and it is the slowest-growing table in the schema: one
// row per participant ever, about 450 on mainnet against 20,000-odd calls.

// The kinds. Written as constants because they are also the values a caller
// filters on, and a typo in a string literal would silently return nothing
// rather than fail.
const (
	// FirstSeenAddress is the first call an address ever signed.
	FirstSeenAddress = "address"
	// FirstSeenDeployer is the first package an address ever published.
	FirstSeenDeployer = "deployer"
	// FirstSeenPackageCalled is the first call a package ever received, which
	// is not the same as when it was deployed.
	FirstSeenPackageCalled = "package_called"
)

// FirstSeenRow is one subject's first appearance.
type FirstSeenRow struct {
	Kind    string `json:"kind"`
	Subject string `json:"subject"`
	At      string `json:"at"`
	Height  int64  `json:"height"`
}

// firstSeenSources is the query per kind. Each picks the genuinely earliest row
// per subject by height, then keeps it only if that row carries a block time.
//
// The timestamp condition is the subtle half. block_time is stamped by a later
// pass than the one that writes the row, so the earliest row for a subject is
// sometimes present and untimed. Taking the earliest *timed* row instead would
// record a first appearance later than the truth and never correct it, because
// the row would already exist. Skipping the subject entirely until its earliest
// row is stamped costs one tick and cannot be wrong.
var firstSeenSources = map[string]string{
	FirstSeenAddress: `
		SELECT network, caller AS subject, block_time, block_height FROM (
			SELECT network, caller, block_time, block_height,
			       ROW_NUMBER() OVER (PARTITION BY network, caller
			                          ORDER BY block_height ASC) AS rn
			  FROM calls
			 WHERE caller <> ''
		) WHERE rn = 1`,

	FirstSeenPackageCalled: `
		SELECT network, pkg_path AS subject, block_time, block_height FROM (
			SELECT network, pkg_path, block_time, block_height,
			       ROW_NUMBER() OVER (PARTITION BY network, pkg_path
			                          ORDER BY block_height ASC) AS rn
			  FROM calls
			 WHERE pkg_path <> ''
		) WHERE rn = 1`,

	// package_submissions, not packages: packages is PRIMARY KEY (network, path)
	// written INSERT OR REPLACE, so a redeploy overwrites and the first
	// publication is gone. Only successful submissions count, since a failed
	// MsgAddPackage published nothing.
	FirstSeenDeployer: `
		SELECT network, creator AS subject, block_time, block_height FROM (
			SELECT network, creator, block_time, block_height,
			       ROW_NUMBER() OVER (PARTITION BY network, creator
			                          ORDER BY block_height ASC) AS rn
			  FROM package_submissions
			 WHERE creator <> '' AND success = 1
		) WHERE rn = 1`,
}

// RefreshFirstSeen brings first_seen up to date for every kind.
//
// Idempotent and safe to run on a partially-synced chain. The upsert only moves
// a row *earlier*, never later, so a backfill that reveals older history
// corrects the table and ordinary forward syncing leaves it alone.
func (d *DB) RefreshFirstSeen() error {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()

	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, kind := range []string{FirstSeenAddress, FirstSeenDeployer, FirstSeenPackageCalled} {
		src := firstSeenSources[kind]
		// The kind is a constant from this file, never from input.
		q := fmt.Sprintf(`
			INSERT INTO first_seen (network, kind, subject, at, height)
			SELECT network, %q, subject, block_time, block_height
			  FROM (%s)
			 WHERE block_time IS NOT NULL AND block_time <> ''
			ON CONFLICT(network, kind, subject) DO UPDATE
			   SET at = excluded.at, height = excluded.height
			 WHERE excluded.height < first_seen.height`, kind, src)
		if _, err := tx.Exec(q); err != nil {
			return fmt.Errorf("refresh first_seen %s: %w", kind, err)
		}
	}
	return tx.Commit()
}

// FirstSeenAt returns when a subject first appeared, and whether it ever has.
func (d *DB) FirstSeenAt(network, kind, subject string) (FirstSeenRow, bool, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	row := FirstSeenRow{Kind: kind, Subject: subject}
	err := d.db.QueryRow(
		`SELECT at, height FROM first_seen WHERE network = ? AND kind = ? AND subject = ?`,
		network, kind, subject).Scan(&row.At, &row.Height)
	if err == sql.ErrNoRows {
		return FirstSeenRow{}, false, nil
	}
	if err != nil {
		return FirstSeenRow{}, false, err
	}
	return row, true, nil
}

// FirstSeenSince lists the subjects of one kind that first appeared at or after
// `since`, newest first. This is the range scan the whole table exists for.
//
// `since` is compared as a string, which is correct only because every `at`
// here is RFC3339 UTC written by the same code path, so lexical order is
// chronological order. That holds for the same reason it holds elsewhere in
// this package and breaks the moment an offset other than Z appears.
func (d *DB) FirstSeenSince(network, kind, since string, limit int) ([]FirstSeenRow, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	d.mu.RLock()
	defer d.mu.RUnlock()

	rows, err := d.db.Query(`
		SELECT subject, at, height
		  FROM first_seen
		 WHERE network = ? AND kind = ? AND at >= ?
		 ORDER BY at DESC, height DESC
		 LIMIT ?`, network, kind, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []FirstSeenRow{}
	for rows.Next() {
		r := FirstSeenRow{Kind: kind}
		if err := rows.Scan(&r.Subject, &r.At, &r.Height); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
