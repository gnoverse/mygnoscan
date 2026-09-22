package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// The symbol index.
//
// Search used to answer only "which path, name or creator contains this
// string", which cannot find a package by what it declares. `IterateByOffset`
// is a real thing somebody types into the box, and the only page that knew
// about it was the docs tab of the one package that has it — reachable only by
// already knowing the answer.
//
// This is a projection of package_files and nothing else. It is rebuilt for a
// package whenever that package's source changes, and it is keyed on the
// package rather than on the submission, because package_files holds current
// bodies only: a redeploy overwrites them.

// SymbolRow is one declaration, flattened for storage.
//
// Methods are stored as their own rows with kind "method" and recv set, rather
// than nested under their type the way the docs tab renders them. A search hits
// a name, and a name is flat; the nesting is a presentation choice that the API
// layer can rebuild from these rows and that a WHERE clause cannot see through.
type SymbolRow struct {
	Kind      string
	Recv      string
	Name      string
	Signature string
	Doc       string
	File      string
	Line      int
	Exported  bool
}

// SymbolHit is a search result: the declaration, and enough of its package to
// render a row without a second query.
type SymbolHit struct {
	Network   string `json:"network,omitempty"`
	Path      string `json:"path"`
	IsRealm   bool   `json:"is_realm"`
	Kind      string `json:"kind"`
	Recv      string `json:"recv,omitempty"`
	Name      string `json:"name"`
	Signature string `json:"signature,omitempty"`
	Doc       string `json:"doc,omitempty"`
	File      string `json:"file,omitempty"`
	Line      int    `json:"line,omitempty"`
	Exported  bool   `json:"exported"`
}

// SymbolFingerprint returns what the index for one package was built from, or
// "" when it has never been built.
func (d *DB) SymbolFingerprint(network, pkgPath string) (string, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var fp string
	err := d.db.QueryRow(
		`SELECT fingerprint FROM symbol_index WHERE network = ? AND package_path = ?`,
		network, pkgPath).Scan(&fp)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return fp, err
}

// ReplaceSymbols swaps one package's symbols for a freshly extracted set.
//
// Delete-then-insert inside one transaction, rather than an upsert per row: a
// declaration that was removed from the source has to disappear from the index,
// and an upsert leaves it there forever. That is the failure mode where search
// keeps offering a function nobody can call any more.
func (d *DB) ReplaceSymbols(network, pkgPath, fingerprint string, rows []SymbolRow) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM symbols WHERE network = ? AND package_path = ?`,
		network, pkgPath); err != nil {
		return fmt.Errorf("clear symbols: %w", err)
	}
	ins, err := tx.Prepare(`INSERT OR REPLACE INTO symbols
		(network, package_path, kind, recv, name, signature, doc, file, line, exported)
		VALUES (?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer ins.Close()
	for _, r := range rows {
		if _, err := ins.Exec(network, pkgPath, r.Kind, r.Recv, r.Name,
			r.Signature, r.Doc, r.File, r.Line, r.Exported); err != nil {
			return fmt.Errorf("insert symbol %s: %w", r.Name, err)
		}
	}
	if _, err := tx.Exec(`INSERT INTO symbol_index
		(network, package_path, fingerprint, symbol_count, indexed_at)
		VALUES (?,?,?,?,CURRENT_TIMESTAMP)
		ON CONFLICT(network, package_path) DO UPDATE SET
		  fingerprint = excluded.fingerprint,
		  symbol_count = excluded.symbol_count,
		  indexed_at = excluded.indexed_at`,
		network, pkgPath, fingerprint, len(rows)); err != nil {
		return fmt.Errorf("record fingerprint: %w", err)
	}
	return tx.Commit()
}

// DeleteSymbolIndex drops one package's rows, for a package whose source has
// gone away entirely.
func (d *DB) DeleteSymbolIndex(network, pkgPath string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM symbols WHERE network = ? AND package_path = ?`, network, pkgPath); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM symbol_index WHERE network = ? AND package_path = ?`, network, pkgPath); err != nil {
		return err
	}
	return tx.Commit()
}

// maxSymbolResults bounds a search. The box shows a handful; anything past this
// is a query that wanted a different tool.
const maxSymbolResults = 50

// SearchSymbols finds declarations by name.
//
// Two passes, prefix then substring, and the reason is both ranking and cost.
// Somebody typing "Iter" means a symbol that *starts* with it, and the
// symbols_name index can answer that with a range scan; the substring fallback
// is a full scan and only runs when the prefix pass came back short. Merging
// them the other way round would bury the obvious answer under
// `unmarshalIterState`.
//
// Unexported symbols are searched but rank last: they are real and worth
// finding when you are reading the source, and they are never the thing a
// caller is looking for.
func (d *DB) SearchSymbols(network, q string, limit int) ([]SymbolHit, error) {
	q = strings.TrimSpace(q)
	if q == "" {
		return nil, nil
	}
	if limit <= 0 || limit > maxSymbolResults {
		limit = maxSymbolResults
	}
	d.mu.RLock()
	defer d.mu.RUnlock()

	seen := make(map[string]bool, limit)
	out := make([]SymbolHit, 0, limit)

	run := func(pattern string) error {
		if len(out) >= limit {
			return nil
		}
		rows, err := d.db.Query(`
			SELECT s.network, s.package_path, COALESCE(p.is_realm, 0),
			       s.kind, s.recv, s.name, s.signature, s.doc, s.file, s.line, s.exported
			FROM symbols s
			LEFT JOIN packages p ON p.network = s.network AND p.path = s.package_path
			WHERE s.name LIKE ? ESCAPE '\' AND `+d.networkFilter("s.network", network)+`
			ORDER BY s.exported DESC, LENGTH(s.name), s.name, s.package_path
			LIMIT ?`, pattern, limit*2)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var h SymbolHit
			if err := rows.Scan(&h.Network, &h.Path, &h.IsRealm, &h.Kind, &h.Recv, &h.Name,
				&h.Signature, &h.Doc, &h.File, &h.Line, &h.Exported); err != nil {
				return err
			}
			key := h.Network + "\x00" + h.Path + "\x00" + h.Kind + "\x00" + h.Recv + "\x00" + h.Name
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, h)
			if len(out) >= limit {
				return nil
			}
		}
		return rows.Err()
	}

	esc := escapeLike(q)
	if err := run(esc + "%"); err != nil {
		return nil, err
	}
	if err := run("%" + esc + "%"); err != nil {
		return nil, err
	}
	return out, nil
}

// escapeLike neutralises the wildcards a reader can type into a search box.
// Without it a query of "%" matches every symbol on the chain and a query of
// "_" matches every one-character name, which is a search box that answers a
// question nobody asked.
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// SymbolIndexStats is what the sanity page reports about the index.
type SymbolIndexStats struct {
	Packages int `json:"packages"`
	Symbols  int `json:"symbols"`
	Pending  int `json:"pending"`
}

// SymbolIndexStatus counts what is indexed against what has source.
func (d *DB) SymbolIndexStatus() (SymbolIndexStats, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var st SymbolIndexStats
	if err := d.db.QueryRow(`SELECT COUNT(*) FROM symbol_index`).Scan(&st.Packages); err != nil {
		return st, err
	}
	if err := d.db.QueryRow(`SELECT COUNT(*) FROM symbols`).Scan(&st.Symbols); err != nil {
		return st, err
	}
	// Packages with source and no index row yet. Not the same as "never
	// indexed": a package whose source changed has a row and a stale
	// fingerprint, and only the pass itself can tell.
	if err := d.db.QueryRow(`
		SELECT COUNT(*) FROM (
			SELECT DISTINCT network, package_path FROM package_files
			EXCEPT
			SELECT network, package_path FROM symbol_index
		)`).Scan(&st.Pending); err != nil {
		return st, err
	}
	return st, nil
}
