package store

import (
	"fmt"
	"strings"
)

// Faceting the package directory.
//
// A flat list of every deployed path answers "what exists" and nothing else. The
// two questions a reader actually arrives with are "what kind of thing is this"
// (a realm has a page, a pure package is a library other code imports) and
// "whose is it". Both are already derivable from what is stored; neither was
// exposed.

// PackageKind selects which half of the directory a query covers.
//
// The two existing routes are each one value of this: /api/realms was
// realmOnly=true and /api/packages was realmOnly=false. Neither could answer
// "everything", which is the listing a reader browsing the chain wants first.
type PackageKind string

const (
	// KindAll is both. It is not the default for the existing routes, because
	// changing what /api/realms answers would be a different endpoint wearing
	// the same name.
	KindAll   PackageKind = "all"
	KindRealm PackageKind = "realm"
	KindPure  PackageKind = "pure"
)

// ParsePackageKind accepts the query-string spelling.
func ParsePackageKind(s string) (PackageKind, bool) {
	switch PackageKind(s) {
	case KindAll, KindRealm, KindPure:
		return PackageKind(s), true
	case "":
		return KindAll, true
	}
	return "", false
}

// PackageFilter is what narrows a listing.
type PackageFilter struct {
	Kind PackageKind
	// Namespace is the element after the r/ or p/ marker, the same key
	// NamespaceOf derives in Go. Empty means every namespace.
	Namespace string
}

// where renders the filter as a SQL fragment plus its arguments.
func (f PackageFilter) where(col string) (string, []any) {
	var clauses []string
	var args []any
	switch f.Kind {
	case KindRealm:
		clauses = append(clauses, col+".is_realm = 1")
	case KindPure:
		clauses = append(clauses, col+".is_realm = 0")
	}
	if f.Namespace != "" {
		clauses = append(clauses, namespaceExpr(col)+" = ?")
		args = append(args, f.Namespace)
	}
	if len(clauses) == 0 {
		return "1=1", nil
	}
	return strings.Join(clauses, " AND "), args
}

// namespaceExpr derives the namespace from a path, in SQL.
//
// It has to agree with NamespaceOf, which is Go and is what every other surface
// uses, so the two are pinned against each other by a test over every distinct
// path in the corpus rather than by inspection.
//
// Deriving rather than storing is deliberate. A namespace column on packages
// would be faster to filter on, and it would also be a second copy of a fact
// the path already carries, kept in step by whoever remembers to update the
// syncer. The directory is hundreds of rows, not millions.
//
// instr() finds the first occurrence, which is the same scan-for-the-first-
// marker rule NamespaceOf uses: gno.land/r/moul/p/thing is moul, not thing.
func namespaceExpr(col string) string {
	p := col + ".path"
	// Position of the marker that comes first, or 0 when neither is present.
	marker := fmt.Sprintf(`CASE
		WHEN instr(%[1]s, '/r/') > 0 AND (instr(%[1]s, '/p/') = 0 OR instr(%[1]s, '/r/') < instr(%[1]s, '/p/'))
			THEN instr(%[1]s, '/r/')
		WHEN instr(%[1]s, '/p/') > 0 THEN instr(%[1]s, '/p/')
		ELSE 0 END`, p)
	// Everything after the marker, then up to the next slash.
	rest := fmt.Sprintf(`substr(%s, (%s) + 3)`, p, marker)
	tail := fmt.Sprintf(`CASE WHEN instr(%[1]s, '/') > 0 THEN substr(%[1]s, 1, instr(%[1]s, '/') - 1) ELSE %[1]s END`, rest)
	// No marker at all: NamespaceOf falls back to the element after the domain,
	// then to the whole path. Neither shape occurs on gno.land today, and both
	// are better as a stable label than as an empty bucket that swallows
	// everything odd.
	afterDomain := fmt.Sprintf(`CASE WHEN instr(%[1]s, '/') > 0
		THEN (SELECT CASE WHEN instr(r2, '/') > 0 THEN substr(r2, 1, instr(r2, '/') - 1) ELSE r2 END
		      FROM (SELECT substr(%[1]s, instr(%[1]s, '/') + 1) AS r2))
		ELSE %[1]s END`, p)
	return fmt.Sprintf(`CASE WHEN (%s) > 0 THEN (%s) ELSE (%s) END`, marker, tail, afterDomain)
}

// NamespaceFacet is one namespace and how much is in it.
type NamespaceFacet struct {
	Namespace string `json:"namespace"`
	Packages  int    `json:"packages"`
	Realms    int    `json:"realms"`
	Pure      int    `json:"pure"`
}

// KindFacet is how the directory splits by kind, for the counts a facet control
// shows beside each option.
type KindFacet struct {
	All   int `json:"all"`
	Realm int `json:"realm"`
	Pure  int `json:"pure"`
}

// PackageFacets returns the counts a facet control needs.
//
// One query for the namespaces and one for the kinds, both honouring the rest
// of the filter: picking a namespace has to narrow the kind counts, or the
// control reports totals for a listing the reader is not looking at.
func (d *DB) PackageFacets(network string, f PackageFilter) (KindFacet, []NamespaceFacet, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var kinds KindFacet
	// The kind counts ignore the kind filter on purpose: a control showing
	// "realm 210 / pure 159" has to keep showing both after one is picked, or
	// the reader cannot see what switching would give them.
	kindFilter := PackageFilter{Namespace: f.Namespace}
	where, args := kindFilter.where("p")
	q := `SELECT COUNT(*), COALESCE(SUM(p.is_realm = 1), 0), COALESCE(SUM(p.is_realm = 0), 0)
		FROM packages p WHERE ` + where + ` AND ` + d.networkFilter("p.network", network)
	if err := d.db.QueryRow(q, args...).Scan(&kinds.All, &kinds.Realm, &kinds.Pure); err != nil {
		return kinds, nil, fmt.Errorf("kind facets: %w", err)
	}

	// Namespace counts ignore the namespace filter, for the same reason.
	nsFilter := PackageFilter{Kind: f.Kind}
	where, args = nsFilter.where("p")
	q = `SELECT ` + namespaceExpr("p") + ` AS ns,
		COUNT(*), COALESCE(SUM(p.is_realm = 1), 0), COALESCE(SUM(p.is_realm = 0), 0)
		FROM packages p WHERE ` + where + ` AND ` + d.networkFilter("p.network", network) + `
		GROUP BY ns ORDER BY COUNT(*) DESC, ns ASC`
	rows, err := d.db.Query(q, args...)
	if err != nil {
		return kinds, nil, fmt.Errorf("namespace facets: %w", err)
	}
	defer rows.Close()
	var out []NamespaceFacet
	for rows.Next() {
		var n NamespaceFacet
		if err := rows.Scan(&n.Namespace, &n.Packages, &n.Realms, &n.Pure); err != nil {
			return kinds, nil, err
		}
		out = append(out, n)
	}
	return kinds, out, rows.Err()
}
