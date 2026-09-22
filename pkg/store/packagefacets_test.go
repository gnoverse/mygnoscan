package store

import (
	"testing"

	"github.com/moul/mygnoscan/pkg/config"
)

func facetTestDB(t *testing.T) *DB {
	t.Helper()
	db := NewTestDB(t)
	db.SetConfiguredNetworks([]config.NetworkConfig{{ID: "alpha"}, {ID: "beta"}})
	return db
}

func addPkg(t *testing.T, db *DB, network, path string, isRealm bool) {
	t.Helper()
	if err := db.UpsertPackage(network, path, pathLeaf(path), "g1creator", "tx-"+path, 100, "2026-09-01T00:00:00Z", isRealm, 1); err != nil {
		t.Fatalf("UpsertPackage(%s): %v", path, err)
	}
}

func pathLeaf(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[i+1:]
		}
	}
	return p
}

// The SQL namespace expression and NamespaceOf are the same rule written twice,
// once in each language, and every other surface on the site uses the Go one.
// If they disagree, a facet silently lists a namespace whose rows are filed
// under a different name and clicking it returns nothing.
//
// Checked by evaluating both over the same paths rather than by reading them,
// and the path list is every shape that occurs on a real chain plus the odd
// ones NamespaceOf documents a fallback for.
func TestNamespaceSQLAgreesWithGo(t *testing.T) {
	db := facetTestDB(t)

	paths := []string{
		// The ordinary shapes.
		"gno.land/r/gnoswap/gns",
		"gno.land/p/demo/avl",
		"gno.land/r/gov/dao",
		"gno.land/p/nt/ufmt/v0",
		"gno.land/r/moul/config/v0",
		// Address-owned namespaces, which group per address on purpose.
		"gno.land/r/g1n4pl5uc4yt5r96m9w6fmdznx3x0jyg8l6arhmt/gnomi/pad",
		"gno.land/p/g17khqpukees4237dtn3astzapmp462vjhsz6st4/datasource",
		// A marker appearing again later in the path. The first one wins, in
		// both languages, and this is the case an instr() on the wrong marker
		// would get backwards.
		"gno.land/r/moul/p/thing",
		"gno.land/p/moul/r/thing",
		// A two-element path: the namespace is the leaf and there is no tail.
		"gno.land/r/leon",
		"gno.land/p/nt",
		// Shapes with no marker at all, which NamespaceOf documents a fallback
		// for. Neither occurs on gno.land today.
		"example.com/foo/bar",
		"example.com/foo",
		"standalone",
		// A hyphenated namespace, which exists on mainnet.
		"gno.land/r/nym-thegnomic001/gnomic_airdrop",
	}
	for _, p := range paths {
		addPkg(t, db, "alpha", p, true)
	}

	rows, err := db.db.Query(`SELECT p.path, ` + namespaceExpr("p") + ` FROM packages p ORDER BY p.path`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seen := 0
	for rows.Next() {
		var path, fromSQL string
		if err := rows.Scan(&path, &fromSQL); err != nil {
			t.Fatal(err)
		}
		if want := NamespaceOf(path); fromSQL != want {
			t.Errorf("namespace of %q: SQL says %q, NamespaceOf says %q", path, fromSQL, want)
		}
		seen++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if seen != len(paths) {
		t.Fatalf("checked %d paths, seeded %d", seen, len(paths))
	}
}

func TestPackageFacetCounts(t *testing.T) {
	db := facetTestDB(t)
	addPkg(t, db, "alpha", "gno.land/r/gnoswap/gns", true)
	addPkg(t, db, "alpha", "gno.land/r/gnoswap/router", true)
	addPkg(t, db, "alpha", "gno.land/p/gnoswap/uint256", false)
	addPkg(t, db, "alpha", "gno.land/r/moul/home", true)
	addPkg(t, db, "alpha", "gno.land/p/nt/ufmt/v0", false)

	kinds, ns, err := db.PackageFacets("alpha", PackageFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if kinds.All != 5 || kinds.Realm != 3 || kinds.Pure != 2 {
		t.Fatalf("kinds = %+v, want 5/3/2", kinds)
	}
	// Ordered by size, so the control lists the namespaces worth clicking first.
	if len(ns) != 3 || ns[0].Namespace != "gnoswap" || ns[0].Packages != 3 {
		t.Fatalf("namespaces = %+v", ns)
	}
	if ns[0].Realms != 2 || ns[0].Pure != 1 {
		t.Fatalf("gnoswap split = %+v, want 2 realms and 1 pure", ns[0])
	}
}

// Picking one facet has to narrow the other, or the control describes a listing
// the reader is not looking at. Picking a facet must not narrow *itself*, or
// the reader cannot see what switching to the other option would give them.
func TestFacetCountsNarrowEachOtherButNotThemselves(t *testing.T) {
	db := facetTestDB(t)
	addPkg(t, db, "alpha", "gno.land/r/gnoswap/gns", true)
	addPkg(t, db, "alpha", "gno.land/r/gnoswap/router", true)
	addPkg(t, db, "alpha", "gno.land/p/gnoswap/uint256", false)
	addPkg(t, db, "alpha", "gno.land/r/moul/home", true)

	// Namespace picked: the kind counts describe that namespace only.
	kinds, ns, err := db.PackageFacets("alpha", PackageFilter{Namespace: "gnoswap"})
	if err != nil {
		t.Fatal(err)
	}
	if kinds.All != 3 || kinds.Realm != 2 || kinds.Pure != 1 {
		t.Fatalf("kinds under namespace=gnoswap = %+v, want 3/2/1", kinds)
	}
	// And the namespace list still shows every namespace, so the reader can
	// switch away from the one they picked.
	if len(ns) != 2 {
		t.Fatalf("namespaces under namespace=gnoswap = %+v, want both listed", ns)
	}

	// Kind picked: the namespace counts describe that kind only.
	kinds, ns, err = db.PackageFacets("alpha", PackageFilter{Kind: KindPure})
	if err != nil {
		t.Fatal(err)
	}
	if len(ns) != 1 || ns[0].Namespace != "gnoswap" || ns[0].Packages != 1 {
		t.Fatalf("namespaces under kind=pure = %+v, want gnoswap only", ns)
	}
	// The kind counts still show both, for the same reason.
	if kinds.Realm != 3 || kinds.Pure != 1 {
		t.Fatalf("kinds under kind=pure = %+v, want both still counted", kinds)
	}
}

func TestPackageFacetsAreNetworkScoped(t *testing.T) {
	db := facetTestDB(t)
	addPkg(t, db, "alpha", "gno.land/r/onlyalpha/app", true)
	addPkg(t, db, "beta", "gno.land/r/onlybeta/app", true)

	for _, tt := range []struct {
		network string
		want    int
	}{{"alpha", 1}, {"beta", 1}, {"", 2}} {
		kinds, ns, err := db.PackageFacets(tt.network, PackageFilter{})
		if err != nil {
			t.Fatal(err)
		}
		if kinds.All != tt.want || len(ns) != tt.want {
			t.Fatalf("network %q: kinds %+v, %d namespaces, want %d of each", tt.network, kinds, len(ns), tt.want)
		}
	}
}

func TestParsePackageKind(t *testing.T) {
	for _, tt := range []struct {
		in   string
		want PackageKind
		ok   bool
	}{
		{"", KindAll, true},
		{"all", KindAll, true},
		{"realm", KindRealm, true},
		{"pure", KindPure, true},
		{"stdlib", "", false}, // proposal 5, not shipped: must not silently mean "all"
		{"REALM", "", false},
		{"nonsense", "", false},
	} {
		got, ok := ParsePackageKind(tt.in)
		if ok != tt.ok || (ok && got != tt.want) {
			t.Errorf("ParsePackageKind(%q) = (%q,%v), want (%q,%v)", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}
