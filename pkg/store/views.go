package store

import (
	"fmt"
	"strings"
	"time"
)

// Reads, which the chain does not record.
//
// Everything else in this package is a fact the chain proves. This is not: it
// is a count of how many times somebody opened a realm *on this explorer*, and
// it exists because the chain gives no way to answer "is anybody looking at
// this".
//
// A realm is ranked on calls everywhere else here, which is the right signal
// for anything transactional and structurally blind to a whole class of realm:
// a blog written to three times, a profile page deployed once and then only
// read, a registry that answers vm/qrender and vm/qeval. Those are reads, and
// gno.land records none of them. What this explorer can honestly say is which
// realms its own readers opened, so that is what it says, in those words, and
// never as "popular".
//
// Kept deliberately thin: a count per realm per day, no address, no IP, no user
// agent, nothing that identifies a reader. The question is "did anyone look",
// not "who".

// RealmView is one realm's read count over a window.
type RealmView struct {
	Path  string `json:"path"`
	Views int    `json:"views"`
}

// ViewKey identifies one day's bucket for one realm.
type ViewKey struct {
	Network string
	Path    string
	Day     string // YYYY-MM-DD, UTC
}

// ViewDay is the bucket a timestamp falls in.
func ViewDay(t time.Time) string { return t.UTC().Format("2006-01-02") }

// AddRealmViews folds a batch of counts into the table.
//
// A batch rather than a row per request: a read is the cheapest thing this
// server does and paying for a write on each one would make looking at a realm
// more expensive than the realm. The caller buffers and flushes.
func (d *DB) AddRealmViews(batch map[ViewKey]int) error {
	if len(batch) == 0 {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	stmt, err := tx.Prepare(`
		INSERT INTO realm_views (network, path, day, views) VALUES (?, ?, ?, ?)
		ON CONFLICT(network, path, day) DO UPDATE SET views = views + excluded.views`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for k, n := range batch {
		if n <= 0 {
			continue
		}
		if _, err := stmt.Exec(k.Network, k.Path, k.Day, n); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// RealmViewsFor answers for a set of paths at once, keyed by path.
//
// `since` is a day string (YYYY-MM-DD); empty means every day on record.
func (d *DB) RealmViewsFor(network string, paths []string, since string) (map[string]int, error) {
	out := make(map[string]int, len(paths))
	if len(paths) == 0 {
		return out, nil
	}
	d.mu.RLock()
	defer d.mu.RUnlock()

	args := make([]any, 0, len(paths)+1)
	for _, p := range paths {
		args = append(args, p)
	}
	where := d.networkFilter("network", network) + " AND path IN (" + sqlPlaceholders(len(paths)) + ")"
	if since != "" {
		where += " AND day >= ?"
		args = append(args, since)
	}
	rows, err := d.db.Query(`SELECT path, COALESCE(SUM(views), 0) FROM realm_views
		WHERE `+where+` GROUP BY path`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var p string
		var n int
		if err := rows.Scan(&p, &n); err != nil {
			return nil, err
		}
		out[p] = n
	}
	return out, rows.Err()
}

// TopViewedRealms ranks what this explorer's readers opened.
func (d *DB) TopViewedRealms(network, since string, limit int) ([]RealmView, error) {
	if limit <= 0 {
		limit = 50
	}
	d.mu.RLock()
	defer d.mu.RUnlock()

	args := []any{}
	where := d.networkFilter("network", network)
	if since != "" {
		where += " AND day >= ?"
		args = append(args, since)
	}
	args = append(args, limit)
	rows, err := d.db.Query(fmt.Sprintf(`
		SELECT path, SUM(views) AS n FROM realm_views
		WHERE %s GROUP BY path ORDER BY n DESC, path ASC LIMIT ?`, where), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RealmView{}
	for rows.Next() {
		var v RealmView
		if err := rows.Scan(&v.Path, &v.Views); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ViewsSince turns a window string into the day a query should start at.
// Empty means all of it.
func ViewsSince(window string, now time.Time) string {
	var d time.Duration
	switch strings.TrimSpace(window) {
	case "24h":
		d = 24 * time.Hour
	case "7d":
		d = 7 * 24 * time.Hour
	case "30d":
		d = 30 * 24 * time.Hour
	case "90d":
		d = 90 * 24 * time.Hour
	default:
		return ""
	}
	return ViewDay(now.Add(-d))
}
