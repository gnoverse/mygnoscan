package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/moul/mygnoscan/pkg/achievements"
)

// Achievements: who has done what on chain, precomputed.
//
// The catalog and the reasoning behind each badge live in pkg/achievements.
// This file is the half that touches the database: running each definition's
// query per network, storing the first unlock, and the three reads the API
// needs (one address's badges, one badge's holders, and the people directory
// that filters on both).
//
// Precomputed rather than live for the same reason as the gas rollups: each
// definition is a GROUP BY over a whole history table, twenty-odd of them, and
// an address page cannot wait for that. The rebuild is wholesale and idempotent,
// so a re-sync, a backfill or a chain reset cannot leave a stale badge behind —
// the one failure mode a "remember what you have awarded" design would have.

// AchievementInterval is how often the badge table is rebuilt.
//
// Longer than RollupInterval because nothing here is a figure a reader watches
// move: a badge is a fact about the past, and being ten minutes late to award
// one costs nobody anything. The recompute takes the write lock, so the gap is
// the point.
const AchievementInterval = 10 * time.Minute

const achievementsComputedAtKey = "achievements_at"

// Unlock is one badge an address holds, with the block that first earned it.
type Unlock struct {
	Slug   string `json:"slug"`
	Height int    `json:"block_height"`
	Time   string `json:"block_time,omitempty"`
	TxHash string `json:"tx_hash,omitempty"`
}

// Holder is one address that holds a given badge.
type Holder struct {
	Address string `json:"address"`
	Height  int    `json:"block_height"`
	Time    string `json:"block_time,omitempty"`
	TxHash  string `json:"tx_hash,omitempty"`
	Rank    int    `json:"rank"`
}

// Person is one row of the people directory.
type Person struct {
	Address string   `json:"address"`
	Name    string   `json:"name,omitempty"`
	Network string   `json:"network,omitempty"`
	Badges  []string `json:"badges"`
	Count   int      `json:"count"`
	// FirstHeight is the earliest block behind any of this address's badges,
	// which is the closest thing the index has to "since when". Not called
	// first_seen: an address can be received into existence before it ever
	// signs, and this only sees what it did.
	FirstHeight int    `json:"first_height"`
	FirstTime   string `json:"first_time,omitempty"`
	LastHeight  int    `json:"last_height"`
	LastTime    string `json:"last_time,omitempty"`
}

// RefreshAchievements recomputes the badge table for every configured network.
//
// Retried on a busy write lock the same way RefreshRollups is, and for the same
// reason: losing the whole pass to SQLITE_BUSY is silent, and here there is no
// live fallback at all — the badges would simply stop appearing.
func (d *DB) RefreshAchievements() error {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 2 * time.Second)
		}
		if lastErr = d.refreshAchievements(); lastErr == nil {
			return nil
		}
	}
	return lastErr
}

func (d *DB) refreshAchievements() error {
	d.mu.RLock()
	networks := append([]string(nil), d.configured...)
	d.mu.RUnlock()
	if len(networks) == 0 {
		return nil
	}

	d.writeMu.Lock()
	defer d.writeMu.Unlock()

	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	defs := achievements.Indexed()
	for _, network := range networks {
		// Per network rather than one DELETE for the lot: a network whose
		// definition query errors below aborts the whole transaction anyway, so
		// scoping the delete keeps the statement honest about what it replaces
		// rather than merely smaller.
		if _, err := tx.Exec(`DELETE FROM achievements WHERE network = ?`, network); err != nil {
			return err
		}
		for _, def := range defs {
			// The catalog's SQL supplies the four columns; the slug and network
			// are added here so a definition cannot get its own identity wrong.
			//
			// Every parameter is named, including the two this statement adds.
			// Mixing named and ordinal parameters in one statement leaves the
			// ordinal positions dependent on how many names precede them, which
			// is a footgun waiting for the next definition that binds something.
			stmt := `INSERT OR REPLACE INTO achievements (network, address, slug, block_height, block_time, tx_hash)
				SELECT @net, address, @slug, COALESCE(block_height, 0), COALESCE(block_time, ''), COALESCE(tx_hash, '')
				FROM (` + def.SQL + `) WHERE address <> ''`
			if _, err := tx.Exec(stmt, sql.Named("net", network), sql.Named("slug", def.Slug)); err != nil {
				return fmt.Errorf("achievement %s on %s: %w", def.Slug, network, err)
			}
		}
	}

	if _, err := tx.Exec(`INSERT OR REPLACE INTO sync_state (key, value) VALUES (?, ?)`,
		achievementsComputedAtKey, time.Now().UTC().Format(time.RFC3339)); err != nil {
		return err
	}
	return tx.Commit()
}

// AchievementsComputedAt is when the table was last rebuilt, RFC3339, or empty
// if it never has been. The API passes it through so a page can say how fresh
// the badges are rather than implying they are live.
func (d *DB) AchievementsComputedAt() string {
	var at string
	d.db.QueryRow(`SELECT value FROM sync_state WHERE key = ?`, achievementsComputedAtKey).Scan(&at)
	return at
}

// AddressAchievements returns every badge one address holds, oldest first.
func (d *DB) AddressAchievements(network, address string) ([]Unlock, error) {
	cond, args := d.networkParams("network", network)
	args = append(args, address)
	rows, err := d.db.Query(`
		SELECT slug, block_height, block_time, tx_hash
		  FROM achievements
		 WHERE `+cond+` AND address = ?
		 ORDER BY block_height, slug`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// One address can hold the same badge on two networks when no network is
	// selected. Collapsing to the earliest is what keeps the profile a profile
	// rather than a per-chain matrix; the page is network-scoped anyway when
	// a chain is chosen.
	seen := map[string]bool{}
	var out []Unlock
	for rows.Next() {
		var u Unlock
		if err := rows.Scan(&u.Slug, &u.Height, &u.Time, &u.TxHash); err != nil {
			return nil, err
		}
		if seen[u.Slug] {
			continue
		}
		seen[u.Slug] = true
		out = append(out, u)
	}
	return out, rows.Err()
}

// AchievementCounts is how many addresses hold each badge, which is what makes
// a badge readable as common or rare.
func (d *DB) AchievementCounts(network string) (map[string]int, error) {
	cond, args := d.networkParams("network", network)
	rows, err := d.db.Query(`
		SELECT slug, COUNT(DISTINCT address) FROM achievements WHERE `+cond+` GROUP BY slug`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var slug string
		var n int
		if err := rows.Scan(&slug, &n); err != nil {
			return nil, err
		}
		out[slug] = n
	}
	return out, rows.Err()
}

// AchievementHolders lists who holds one badge, earliest unlock first, which is
// also the order that makes the first few rows worth looking at.
func (d *DB) AchievementHolders(network, slug string, limit, offset int) ([]Holder, int, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	cond, args := d.networkParams("network", network)

	var total int
	countArgs := append(append([]any(nil), args...), slug)
	if err := d.db.QueryRow(`SELECT COUNT(DISTINCT address) FROM achievements WHERE `+cond+` AND slug = ?`,
		countArgs...).Scan(&total); err != nil {
		return nil, 0, err
	}

	qArgs := append(append([]any(nil), args...), slug, limit, offset)
	rows, err := d.db.Query(`
		SELECT address, MIN(block_height) AS h, block_time, tx_hash
		  FROM achievements
		 WHERE `+cond+` AND slug = ?
		 GROUP BY address
		 ORDER BY h ASC
		 LIMIT ? OFFSET ?`, qArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	out := []Holder{}
	rank := offset
	for rows.Next() {
		var h Holder
		if err := rows.Scan(&h.Address, &h.Height, &h.Time, &h.TxHash); err != nil {
			return nil, 0, err
		}
		rank++
		h.Rank = rank
		out = append(out, h)
	}
	return out, total, rows.Err()
}

// PeopleQuery is the directory's filter set.
type PeopleQuery struct {
	Network string

	// Q matches a registered username or an address, as a prefix on the
	// address and a substring on the name. Addresses are long and nobody types
	// the middle of one; names are short and people do.
	Q string

	// Has is an AND: every slug listed must be held. The alternative (OR) reads
	// as a bigger list the more you filter, which is the wrong direction for a
	// control whose whole job is narrowing.
	Has []string

	// NamedOnly restricts to addresses with a registered username. The
	// directory is otherwise every address that has ever acted, which is the
	// truthful default and also a long list.
	NamedOnly bool

	// Sort is "badges" (most badges first), "recent" (most recent unlock
	// first) or "oldest" (earliest first unlock first, so the people who were
	// here at the start come up).
	Sort string

	Limit, Offset int
}

// DirectoryPeople answers the people directory: addresses ranked by what they
// have done, narrowable by badge.
//
// The row set is "every address holding at least one badge", which is every
// address that has ever signed anything (first-tx covers that) plus the
// delegated keys and the addresses that only ever received. That is deliberately
// wider than "everyone with a username": a directory that only listed the 78
// registered names would hide almost everybody who actually builds here.
func (d *DB) DirectoryPeople(q PeopleQuery) ([]Person, int, error) {
	if q.Limit <= 0 || q.Limit > 200 {
		q.Limit = 50
	}
	cond, args := d.networkParams("a.network", q.Network)

	where := []string{cond}

	// Each required badge is its own EXISTS rather than a
	// "COUNT(*) FILTER (...) = n" over the group: the index on
	// (network, slug, block_height) serves an EXISTS directly, and the
	// alternative has to build every candidate's whole badge set first.
	for range q.Has {
		where = append(where, `EXISTS (SELECT 1 FROM achievements x
			WHERE x.network = a.network AND x.address = a.address AND x.slug = ?)`)
	}

	if q.NamedOnly {
		where = append(where, `EXISTS (SELECT 1 FROM users u
			WHERE u.network = a.network AND u.address = a.address AND u.deleted = 0)`)
	}
	if q.Q != "" {
		where = append(where, `(a.address LIKE ? OR EXISTS (SELECT 1 FROM users u
			WHERE u.network = a.network AND u.address = a.address AND u.deleted = 0 AND u.name LIKE ?))`)
	}

	// The filter arguments have to go in the order the conditions were appended
	// above, which is why they are collected here rather than beside each one.
	filterArgs := append([]any(nil), args...)
	for _, slug := range q.Has {
		filterArgs = append(filterArgs, slug)
	}
	if q.Q != "" {
		filterArgs = append(filterArgs, q.Q+"%", "%"+strings.ToLower(q.Q)+"%")
	}

	whereSQL := strings.Join(where, " AND ")

	var total int
	if err := d.db.QueryRow(`SELECT COUNT(*) FROM (
		SELECT a.address FROM achievements a WHERE `+whereSQL+` GROUP BY a.address)`, filterArgs...).Scan(&total); err != nil {
		return nil, 0, err
	}

	order := "count DESC, first_height ASC"
	switch q.Sort {
	case "recent":
		order = "last_height DESC"
	case "oldest":
		order = "first_height ASC"
	}

	rowArgs := append(append([]any(nil), filterArgs...), q.Limit, q.Offset)
	rows, err := d.db.Query(`
		SELECT a.address,
		       COUNT(DISTINCT a.slug) AS count,
		       MIN(a.block_height) AS first_height,
		       MAX(a.block_height) AS last_height,
		       GROUP_CONCAT(DISTINCT a.slug) AS slugs,
		       (SELECT u.name FROM users u
		         WHERE u.network = a.network AND u.address = a.address AND u.deleted = 0 AND u.alias = 0
		         ORDER BY u.block_height LIMIT 1) AS name,
		       -- Block times are RFC3339, so they sort lexicographically in the
		       -- same order as the heights beside them and MIN/MAX over the
		       -- string is the time of the first and last unlock. NULLIF keeps
		       -- a row whose time was never captured from sorting to the front
		       -- and claiming to be the earliest.
		       MIN(NULLIF(a.block_time, '')) AS first_time,
		       MAX(a.block_time) AS last_time
		  FROM achievements a
		 WHERE `+whereSQL+`
		 GROUP BY a.address
		 ORDER BY `+order+`
		 LIMIT ? OFFSET ?`, rowArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	out := []Person{}
	for rows.Next() {
		var p Person
		var slugs string
		var name, firstTime, lastTime sql.NullString
		if err := rows.Scan(&p.Address, &p.Count, &p.FirstHeight, &p.LastHeight, &slugs, &name, &firstTime, &lastTime); err != nil {
			return nil, 0, err
		}
		p.Name = name.String
		p.FirstTime = firstTime.String
		p.LastTime = lastTime.String
		if slugs != "" {
			p.Badges = strings.Split(slugs, ",")
		} else {
			p.Badges = []string{}
		}
		out = append(out, p)
	}
	return out, total, rows.Err()
}
