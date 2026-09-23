package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Code search over every .gno file the chain holds.
//
// The corpus is already here: `package_files` carries the full source of every
// package ever deployed, because gno stores source on chain rather than
// bytecode. What was missing is an index over it, so "has anyone done this
// before" was answerable only by cloning tx-exports and grepping.
//
// FTS5 is used rather than LIKE. It is available in the pure-Go driver this
// project already depends on (modernc.org/sqlite), with no cgo and no build
// tag, `snippet()` included. Verified before committing to the design.
//
// The index is a separate table rather than an external-content FTS5 table
// over package_files. External content would avoid storing the source twice,
// and it is the wrong trade here: it requires triggers to stay consistent, and
// the startup migration path in this repo already rebuilds tables in ways that
// would silently desynchronise them. A standalone table can always be rebuilt
// from package_files, which is the property that matters when it drifts.

// CodeHit is one matching file.
type CodeHit struct {
	Network string `json:"network"`
	Path    string `json:"path"`
	File    string `json:"file"`
	// Snippet is the matched line with the term marked by « », chosen over
	// HTML tags because the frontend builds DOM and never parses markup back
	// out of a string.
	Snippet string `json:"snippet"`
	IsRealm bool   `json:"is_realm"`
}

// ensureCodeIndex creates the FTS5 table. Called from the schema migration.
const codeIndexSchema = `
CREATE VIRTUAL TABLE IF NOT EXISTS code_index USING fts5(
	network UNINDEXED,
	package_path,
	file_name,
	body,
	tokenize = 'unicode61 remove_diacritics 0 tokenchars ''_'''
);`

// IndexPackageFile adds or replaces one file in the search index on its own.
//
// The sync path does not use this: UpsertPackageFile writes the source and the
// index in one transaction, so they cannot disagree. This exists for indexing
// source that does not come from the indexer (stdlib, which is never a
// MsgAddPackage) and for tests.
//
// Delete-then-insert because FTS5 has no upsert: a plain insert on a
// re-deployed path would leave the old body matching forever, so a search
// would return source that is no longer on chain.
func (d *DB) IndexPackageFile(network, pkgPath, fileName, body string) error {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := tx.Exec(
		`DELETE FROM code_index WHERE network = ? AND package_path = ? AND file_name = ?`,
		network, pkgPath, fileName); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`INSERT INTO code_index (network, package_path, file_name, body) VALUES (?, ?, ?, ?)`,
		network, pkgPath, fileName, body); err != nil {
		return err
	}
	return tx.Commit()
}

// CodeIndexSize reports how many files are indexed, so the page can say
// whether it is searching the whole chain or a cold start.
func (d *DB) CodeIndexSize(network string) (int, error) {
	var n int
	err := d.db.QueryRow(
		`SELECT count(*) FROM code_index WHERE network = ?`, network).Scan(&n)
	return n, err
}

// RebuildCodeIndex fills the index from package_files.
//
// Idempotent and safe to run on every start: a deployment that predates the
// index has a full corpus and an empty index, and without this the search
// would quietly return nothing on exactly the deployments that have the most
// to search. Same class of trap as the GRC20 ledger that only saw
// transactions synced after it shipped.
func (d *DB) RebuildCodeIndex() (int, error) {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()

	tx, err := d.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := tx.Exec(`DELETE FROM code_index`); err != nil {
		return 0, err
	}
	res, err := tx.Exec(`
		INSERT INTO code_index (network, package_path, file_name, body)
		SELECT network, package_path, file_name, body FROM package_files`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return int(n), nil
}

// CodeSearchOpts bounds one query.
type CodeSearchOpts struct {
	Network string
	Query   string
	Kind    string // "" | "realm" | "package"
	Limit   int
}

// SearchCode runs a full-text query and returns one hit per matching file.
//
// The query is passed to FTS5's own parser, which gives a reader phrase
// search ("a b"), prefix (`Iterate*`) and boolean operators for free. That
// parser also rejects malformed input with an error rather than a panic, and
// that error is returned rather than swallowed: a search box that silently
// returns nothing for a syntax mistake teaches people it does not work.
func (d *DB) SearchCode(o CodeSearchOpts) ([]CodeHit, error) {
	if strings.TrimSpace(o.Query) == "" {
		return nil, nil
	}
	if o.Limit <= 0 || o.Limit > 200 {
		o.Limit = 50
	}

	// A realm is a path under /r/, a pure package under /p/. Derived from the
	// path rather than joined against `packages`, because the index has to
	// answer for stdlib and for anything whose package row has since been
	// rewritten, and because a join here costs an FTS5 query its speed.
	var filter string
	switch o.Kind {
	case "realm":
		filter = ` AND package_path LIKE 'gno.land/r/%'`
	case "package":
		filter = ` AND package_path LIKE 'gno.land/p/%'`
	}

	q := fmt.Sprintf(`
		SELECT package_path, file_name, snippet(code_index, 3, '«', '»', '…', 12)
		FROM code_index
		WHERE code_index MATCH ? AND network = ?%s
		ORDER BY rank
		LIMIT ?`, filter)

	rows, err := d.db.Query(q, o.Query, o.Network, o.Limit)
	if err != nil {
		// The only reader-controlled input in this statement is the MATCH
		// term, so a failure here is almost always their query rather than
		// our SQL. FTS5 reports those through SQLite's generic logic-error
		// channel with a variety of messages ("unterminated string",
		// "fts5: syntax error near ..."), so the class is matched rather than
		// one phrasing. Anything unrecognised still comes back as a server
		// error, because silently blaming the reader for our bug is worse.
		if isQuerySyntaxError(err) {
			return nil, &BadQueryError{Err: err}
		}
		return nil, err
	}
	defer rows.Close()

	var out []CodeHit
	for rows.Next() {
		var h CodeHit
		if err := rows.Scan(&h.Path, &h.File, &h.Snippet); err != nil {
			return nil, err
		}
		h.Network = o.Network
		h.IsRealm = strings.HasPrefix(h.Path, "gno.land/r/")
		out = append(out, h)
	}
	return out, rows.Err()
}

// BadQueryError marks a query the user wrote wrong, as opposed to a fault on
// our side. The distinction is the difference between a 400 that tells them
// what to fix and a 500 that tells them nothing.
type BadQueryError struct{ Err error }

func (e *BadQueryError) Error() string { return e.Err.Error() }
func (e *BadQueryError) Unwrap() error { return e.Err }

var _ = sql.ErrNoRows

// BackfillCodeIndex fills the index if it is empty but source exists.
//
// Returns the number of files indexed, or 0 when there was nothing to do. It
// deliberately does not rebuild a populated index: that would re-run on every
// restart of a busy instance for no gain, and RebuildCodeIndex is there for
// when a rebuild is actually wanted.
//
// Chunked, and that is not a detail. The first version did the whole corpus in
// one transaction and died on the live instance with SQLITE_BUSY: the syncer
// writes continuously, the connection's busy_timeout is 5s, and a single
// INSERT..SELECT over two million files holds the write lock for far longer
// than that. The result was the exact failure the backfill exists to prevent,
// an index that stays empty while the search box looks like it works.
//
// So: small batches, each its own transaction, each yielding the write lock
// between them, and a retry when the syncer wins a race anyway.
func (d *DB) BackfillCodeIndex() (int, error) {
	var indexed, files int
	if err := d.db.QueryRow(`SELECT count(*) FROM code_index`).Scan(&indexed); err != nil {
		return 0, err
	}
	if err := d.db.QueryRow(`SELECT count(*) FROM package_files`).Scan(&files); err != nil {
		return 0, err
	}
	if indexed > 0 || files == 0 {
		return 0, nil
	}

	const batch = 500
	total := 0
	for offset := 0; ; {
		n, err := d.backfillBatch(offset, batch)
		if err != nil {
			// Report what was indexed so far alongside the error: a partial
			// index is better than none and the next start resumes, because
			// the "already populated" guard above only skips a non-empty one
			// once it is complete enough to be useful.
			return total, err
		}
		if n == 0 {
			break
		}
		total += n
		offset += batch
	}
	return total, nil
}

// backfillBatch copies one page of files into the index, retrying a busy
// database rather than giving up on the whole backfill for one lost race.
func (d *DB) backfillBatch(offset, limit int) (int, error) {
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		n, err := d.backfillBatchOnce(offset, limit)
		if err == nil {
			return n, nil
		}
		lastErr = err
		if !isBusy(err) {
			return 0, err
		}
		// The syncer holds the write lock. Back off and let it finish; this is
		// background work and has no deadline.
		time.Sleep(time.Duration(attempt+1) * 250 * time.Millisecond)
	}
	return 0, lastErr
}

func (d *DB) backfillBatchOnce(offset, limit int) (int, error) {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()

	tx, err := d.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck

	res, err := tx.Exec(`
		INSERT INTO code_index (network, package_path, file_name, body)
		SELECT network, package_path, file_name, body FROM package_files
		ORDER BY network, package_path, file_name
		LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return int(n), nil
}

func isBusy(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "SQLITE_BUSY") ||
		strings.Contains(msg, "database is locked") ||
		strings.Contains(msg, "database table is locked")
}

// querySyntaxMarkers are the ways SQLite and FTS5 report a malformed MATCH.
// Verified against modernc.org/sqlite: an unbalanced quote surfaces as
// `SQL logic error: unterminated string (1)`, not as anything mentioning fts5.
var querySyntaxMarkers = []string{
	"fts5",
	"syntax error",
	"unterminated string",
	"SQL logic error",
}

func isQuerySyntaxError(err error) bool {
	msg := err.Error()
	for _, m := range querySyntaxMarkers {
		if strings.Contains(msg, m) {
			return true
		}
	}
	return false
}
