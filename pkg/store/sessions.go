package store

import (
	"database/sql"
	"sort"
	"strconv"
	"strings"
)

// SessionGrant is one row of session_grants: a delegated signing key, the
// account it signs for, and what it was allowed to do.
//
// Revoked* are nil while the grant still stands. Expiry is a timestamp the
// reader compares against now; revocation is an event that happened at a
// height, and the two are different ways for a key to stop working.
type SessionGrant struct {
	Network       string   `json:"network,omitempty"`
	SessionAddr   string   `json:"session_addr"`
	Master        string   `json:"master"`
	AllowPaths    []string `json:"allow_paths,omitempty"`
	SpendLimit    string   `json:"spend_limit,omitempty"`
	SpendPeriod   int64    `json:"spend_period"`
	ExpiresAt     int64    `json:"expires_at"`
	GrantedHeight int      `json:"granted_height"`
	GrantedTime   string   `json:"granted_time,omitempty"`
	GrantedTx     string   `json:"granted_tx,omitempty"`
	RevokedHeight *int     `json:"revoked_height,omitempty"`
	RevokedTime   *string  `json:"revoked_time,omitempty"`
	RevokedTx     *string  `json:"revoked_tx,omitempty"`
}

// allowPathSep joins AllowPaths into one column. Newline, because an entry is
// "<route>/<type>[:<path>]" and a path may contain almost anything else; a
// comma or a space would be ambiguous against a realm path.
const allowPathSep = "\n"

func joinAllowPaths(paths []string) string { return strings.Join(paths, allowPathSep) }

func splitAllowPaths(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, allowPathSep)
}

// UpsertSessionGrant records a grant. Idempotent on (network, session_addr,
// granted_height), so a backfill re-walking a height it already covered
// rewrites the same row rather than duplicating it.
//
// The revoked columns are deliberately NOT overwritten: the backfill walks
// oldest first and the live pass walks newest first, so a re-walk of the grant
// height can easily happen after the revocation has already been recorded.
func (d *DB) UpsertSessionGrant(g SessionGrant) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.db.Exec(`
		INSERT INTO session_grants
			(network, session_addr, master, allow_paths, spend_limit, spend_period,
			 expires_at, granted_height, granted_time, granted_tx)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(network, session_addr, granted_height) DO UPDATE SET
			master       = excluded.master,
			allow_paths  = excluded.allow_paths,
			spend_limit  = excluded.spend_limit,
			spend_period = excluded.spend_period,
			expires_at   = excluded.expires_at,
			granted_time = excluded.granted_time,
			granted_tx   = excluded.granted_tx`,
		g.Network, g.SessionAddr, g.Master, joinAllowPaths(g.AllowPaths), g.SpendLimit,
		g.SpendPeriod, g.ExpiresAt, g.GrantedHeight, g.GrantedTime, g.GrantedTx)
	return err
}

// RevokeSessionGrant marks the grant a revoke_session names.
//
// Scoped to grants made at or before the revoking height, and to those not
// already revoked. Both matter once a key is granted twice: a revocation must
// close the grant that was open when it happened, not a later one, and
// re-running a backfill over the same height must not move an older
// revocation onto a newer grant.
func (d *DB) RevokeSessionGrant(network, sessionAddr string, height int, blockTime, txHash string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.db.Exec(`
		UPDATE session_grants
		   SET revoked_height = ?, revoked_time = ?, revoked_tx = ?
		 WHERE network = ? AND session_addr = ?
		   AND granted_height <= ? AND revoked_height IS NULL`,
		height, blockTime, txHash, network, sessionAddr, height)
	return err
}

// RevokeAllSessionGrants is the same for revoke_all_sessions, which names a
// master and no key.
func (d *DB) RevokeAllSessionGrants(network, master string, height int, blockTime, txHash string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.db.Exec(`
		UPDATE session_grants
		   SET revoked_height = ?, revoked_time = ?, revoked_tx = ?
		 WHERE network = ? AND master = ?
		   AND granted_height <= ? AND revoked_height IS NULL`,
		height, blockTime, txHash, network, master, height)
	return err
}

// scanGrants is the shared row reader: every query below selects these columns
// in this order.
const sessionGrantCols = `network, session_addr, master, allow_paths, spend_limit,
	spend_period, expires_at, granted_height, granted_time, granted_tx,
	revoked_height, revoked_time, revoked_tx`

func scanGrants(rows *sql.Rows) ([]SessionGrant, error) {
	out := []SessionGrant{}
	for rows.Next() {
		var g SessionGrant
		var paths string
		if err := rows.Scan(&g.Network, &g.SessionAddr, &g.Master, &paths, &g.SpendLimit,
			&g.SpendPeriod, &g.ExpiresAt, &g.GrantedHeight, &g.GrantedTime, &g.GrantedTx,
			&g.RevokedHeight, &g.RevokedTime, &g.RevokedTx); err != nil {
			return nil, err
		}
		g.AllowPaths = splitAllowPaths(paths)
		out = append(out, g)
	}
	return out, rows.Err()
}

// SessionGrantsByAddress answers the question the chain cannot: whose session
// is this address. Newest grant first, since a re-granted key has more than one.
func (d *DB) SessionGrantsByAddress(network, addr string) ([]SessionGrant, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	rows, err := d.db.Query(`SELECT `+sessionGrantCols+`
		  FROM session_grants
		 WHERE `+d.networkFilter("network", network)+` AND session_addr = ?
		 ORDER BY granted_height DESC`, addr)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanGrants(rows)
}

// SessionGrantsByMaster lists every grant an account has ever made, revoked and
// expired ones included. The live-state RPC read covers what still stands; this
// is the half that says what used to.
func (d *DB) SessionGrantsByMaster(network, master string) ([]SessionGrant, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	rows, err := d.db.Query(`SELECT `+sessionGrantCols+`
		  FROM session_grants
		 WHERE `+d.networkFilter("network", network)+` AND master = ?
		 ORDER BY granted_height DESC`, master)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanGrants(rows)
}

// SessionGrants lists grants chain-wide, newest first.
func (d *DB) SessionGrants(network string, limit, offset int) ([]SessionGrant, int, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var total int
	if err := d.db.QueryRow(`SELECT COUNT(*) FROM session_grants WHERE ` +
		d.networkFilter("network", network)).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := d.db.Query(`SELECT `+sessionGrantCols+`
		  FROM session_grants
		 WHERE `+d.networkFilter("network", network)+`
		 ORDER BY granted_height DESC
		 LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out, err := scanGrants(rows)
	return out, total, err
}

// SessionStats is the chain-wide summary the sessions page leads with.
//
// Live, Expired and Revoked partition Total, and they are computed against a
// caller-supplied `now` rather than SQLite's clock so a test can state a time
// instead of racing one.
type SessionStats struct {
	Total    int `json:"total"`
	Live     int `json:"live"`
	Expired  int `json:"expired"`
	Revoked  int `json:"revoked"`
	Masters  int `json:"masters"`
	Realms   int `json:"realms"`
	FirstAt  int `json:"first_height,omitempty"`
	LatestAt int `json:"latest_height,omitempty"`
}

// SessionStats counts grants by their state at time `now` (unix seconds).
//
// Revocation wins over expiry when both apply: a key that was revoked and would
// also have expired by now was stopped by the revocation, and counting it as
// merely expired would hide that somebody acted.
func (d *DB) SessionStats(network string, now int64) (SessionStats, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var st SessionStats
	var first, latest sql.NullInt64
	// COALESCE on the two SUMs, not decoration: SUM over zero rows is NULL, not
	// 0, so without it this fails to scan on a chain that has no grants yet.
	// That is the state every instance is in until the sweep finds its first
	// one, which made it the one case the page had to survive.
	err := d.db.QueryRow(`
		SELECT COUNT(*),
		       COALESCE(SUM(CASE WHEN revoked_height IS NOT NULL THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN revoked_height IS NULL AND expires_at != 0 AND expires_at <= ? THEN 1 ELSE 0 END), 0),
		       COUNT(DISTINCT master),
		       MIN(granted_height), MAX(granted_height)
		  FROM session_grants
		 WHERE `+d.networkFilter("network", network), now).
		Scan(&st.Total, &st.Revoked, &st.Expired, &st.Masters, &first, &latest)
	if err != nil {
		return st, err
	}
	st.Live = st.Total - st.Revoked - st.Expired
	if first.Valid {
		st.FirstAt = int(first.Int64)
	}
	if latest.Valid {
		st.LatestAt = int(latest.Int64)
	}
	return st, nil
}

// SessionRealm is one row of "which realms are delegated to".
type SessionRealm struct {
	Path   string `json:"path"`
	Grants int    `json:"grants"`
	// Masters is how many distinct accounts handed a key to this realm, which
	// is the adoption figure. Grants alone double-counts one account that
	// re-granted the same realm five times.
	Masters int `json:"masters"`
}

// SessionRealms ranks the realms sessions are scoped to.
//
// One grant contributes one row per AllowPaths entry, so a key scoped to five
// bubblerumble generations counts for each: the question is "which realms do
// people delegate to", and that key really does name five.
//
// Done in Go rather than SQL because allow_paths is a joined column, and
// splitting it in SQLite would mean a recursive CTE over a table this small.
func (d *DB) SessionRealms(network string, limit int) ([]SessionRealm, error) {
	d.mu.RLock()
	rows, err := d.db.Query(`SELECT allow_paths, master FROM session_grants WHERE ` +
		d.networkFilter("network", network))
	if err != nil {
		d.mu.RUnlock()
		return nil, err
	}
	type agg struct {
		grants  int
		masters map[string]bool
	}
	byPath := map[string]*agg{}
	for rows.Next() {
		var paths, master string
		if err := rows.Scan(&paths, &master); err != nil {
			rows.Close()
			d.mu.RUnlock()
			return nil, err
		}
		for _, entry := range splitAllowPaths(paths) {
			// The scope is what the reader recognises, so the route prefix is
			// dropped here and only here: "*" and "bank/send" carry no path
			// and stand for themselves.
			path := strings.TrimPrefix(entry, "vm/exec:")
			a := byPath[path]
			if a == nil {
				a = &agg{masters: map[string]bool{}}
				byPath[path] = a
			}
			a.grants++
			a.masters[master] = true
		}
	}
	err = rows.Err()
	rows.Close()
	d.mu.RUnlock()
	if err != nil {
		return nil, err
	}

	out := make([]SessionRealm, 0, len(byPath))
	for path, a := range byPath {
		out = append(out, SessionRealm{Path: path, Grants: a.grants, Masters: len(a.masters)})
	}
	sortSessionRealms(out)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// sessionBackfillCursorKey and sessionBackfillStopKey name the sync_state rows
// driving the historical walk.
//
// The stop height is recorded explicitly rather than inferred from the oldest
// stored grant, which is how the token ledger does it. That inference does not
// work here: session grants are rare enough (1 in 400 mainnet transactions on
// 2026-09-23) that a chain can legitimately have none, and "no rows yet" would
// then be indistinguishable from "the forward fill has not started", making the
// walk re-run over the whole chain on every pass forever.
//
// The cursor holds the lowest height swept so far, because this sweep runs
// NEWEST FIRST. See SessionBackfillRange for why that direction and not the
// other one.
// The key is versioned because the cursor's MEANING changed, not its format.
// It used to hold the highest height swept walking up; it now holds the lowest
// swept walking down. Any instance that ran the upward sweep has a low number
// under the old key, and reading that as a downward cursor would declare almost
// the whole chain already swept and skip the exact region where grants live.
// A new key makes such an instance start cleanly from the boundary instead.
func sessionBackfillCursorKey(network string) string {
	return "session_backfill_cursor_desc:" + network
}
func sessionBackfillStopKey(network string) string { return "session_backfill_stop:" + network }

// SessionBackfillRange returns the next batch of heights never walked for
// session messages, and whether any work is left.
//
// The forward fill rides the sync walk and so only ever covers blocks synced
// after this feature shipped. Everything below that boundary has to be swept,
// bounded per pass, resumable across restarts.
//
// **Newest first**, which is the opposite of how the token ledger sweeps, and
// deliberately so. Sessions are a recent chain feature: gnolang/gno#5307 landed
// on mainnet on 2026-09-17, around height 270,000 of 306,000, so the oldest
// 88% of the chain provably contains no grant at all. Sweeping upward from
// genesis spends roughly 25 hours at 100 blocks per 30s pass walking blocks
// that cannot contain a hit before reaching the ones that do, and the page
// shows an empty index that whole time. Measured on mainnet 2026-09-24, which
// is what prompted the direction change.
//
// Downward also degrades better in general: recent grants are the ones anyone
// is looking for, so an interrupted sweep has found the useful half.
//
// The cursor is therefore the LOWEST height swept so far, and it counts down
// from the pinned boundary toward the oldest stored block.
func (d *DB) SessionBackfillRange(network string, batch int) (from, to int, more bool, err error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var lowest *int
	if err = d.db.QueryRow(
		`SELECT MIN(height) FROM blocks WHERE ` + d.networkFilter("network", network)).Scan(&lowest); err != nil {
		return 0, 0, false, err
	}
	if lowest == nil {
		return 0, 0, false, nil // nothing stored yet, nothing to sweep
	}

	// The boundary is pinned the first time the sweep runs, to the tip as it
	// stood then, and never moves. Without it there is nothing to count down
	// from.
	stop := 0
	if v, serr := d.getSyncStateLocked(sessionBackfillStopKey(network)); serr == nil && v != "" {
		stop, _ = strconv.Atoi(v)
	}
	if stop == 0 {
		return 0, 0, false, nil // not pinned yet; PinSessionBackfillStop runs first
	}

	// to is exclusive and starts at the boundary, walking down.
	to = stop + 1
	if cur, cerr := d.getSyncStateLocked(sessionBackfillCursorKey(network)); cerr == nil && cur != "" {
		if n, perr := strconv.Atoi(cur); perr == nil && n < to {
			to = n
		}
	}
	if to <= *lowest {
		return 0, 0, false, nil // the sweep has reached the oldest block stored
	}
	from = to - batch
	if from < *lowest {
		from = *lowest
	}
	return from, to, true, nil
}

// PinSessionBackfillStop records the boundary the sweep runs up to, once.
// A second call is a no-op, so the boundary cannot drift onto a later tip.
//
// Reads the tip itself rather than accepting one from the caller. The obvious
// helper to reach for, LastBlockHeight, selects `block_height`, and the blocks
// table calls that column `height`, so asking it for the blocks tip returns an
// error. A caller that then skips pinning on error, which is the natural way to
// write it, leaves the boundary unset and the sweep unable to say it has
// finished. That shipped once; taking the argument away is what stops it
// shipping again.
func (d *DB) PinSessionBackfillStop(network string) error {
	existing, err := d.GetSyncState(sessionBackfillStopKey(network))
	if err != nil {
		return err
	}
	if existing != "" {
		return nil
	}

	d.mu.RLock()
	var tip *int
	err = d.db.QueryRow(
		`SELECT MAX(height) FROM blocks WHERE ` + d.networkFilter("network", network)).Scan(&tip)
	d.mu.RUnlock()
	if err != nil {
		return err
	}
	if tip == nil {
		// No blocks stored yet. Leaving it unpinned is right: pinning zero
		// would declare the sweep finished before it had anything to sweep.
		return nil
	}
	return d.SetSyncState(sessionBackfillStopKey(network), strconv.Itoa(*tip))
}

// SetSessionBackfillCursor records how far the sweep has got.
func (d *DB) SetSessionBackfillCursor(network string, height int) error {
	return d.SetSyncState(sessionBackfillCursorKey(network), strconv.Itoa(height))
}

// SessionBackfillProgress reports the sweep's position for the page footer, so
// a partial index says so instead of presenting itself as the whole chain.
//
// `at` and `stop` are reported as blocks *swept* out of blocks *to sweep*,
// counting up, even though the walk itself counts down. The reader wants a
// progress figure, not the cursor's raw value, and a number that fell as work
// progressed would read as going backwards.
func (d *DB) SessionBackfillProgress(network string) (done bool, at, stop int) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var boundary, cursor int
	if v, err := d.getSyncStateLocked(sessionBackfillStopKey(network)); err == nil && v != "" {
		boundary, _ = strconv.Atoi(v)
	}
	if boundary == 0 {
		return false, 0, 0 // never pinned, which is not the same as done
	}
	var lowest *int
	if err := d.db.QueryRow(
		`SELECT MIN(height) FROM blocks WHERE ` + d.networkFilter("network", network)).Scan(&lowest); err != nil || lowest == nil {
		return false, 0, 0
	}

	stop = boundary - *lowest + 1 // the whole span to sweep
	cursor = boundary + 1         // nothing swept yet
	if v, err := d.getSyncStateLocked(sessionBackfillCursorKey(network)); err == nil && v != "" {
		if n, perr := strconv.Atoi(v); perr == nil {
			cursor = n
		}
	}
	at = boundary - cursor + 1
	if at < 0 {
		at = 0
	}
	return cursor <= *lowest, at, stop
}

// sortSessionRealms orders by masters, then grants, then path.
//
// Masters first because it is the adoption figure: one account that granted the
// same realm five times must not outrank five accounts that granted it once.
// Path last so the order is total and a test can state it.
func sortSessionRealms(rs []SessionRealm) {
	sort.SliceStable(rs, func(i, j int) bool {
		if rs[i].Masters != rs[j].Masters {
			return rs[i].Masters > rs[j].Masters
		}
		if rs[i].Grants != rs[j].Grants {
			return rs[i].Grants > rs[j].Grants
		}
		return rs[i].Path < rs[j].Path
	})
}
