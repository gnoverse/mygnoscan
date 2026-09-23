package store

import (
	"testing"
	"time"

	"github.com/moul/mygnoscan/pkg/config"
)

// The dependency graph walks recursively from a root, which makes it the one
// query in the project that can loop forever on real data: gno packages import
// each other, and a cycle is legal.
//
// It is also where 232 phantom edges once lived — more than a tenth of the
// graph, including self-edges — because imports were extracted with a regex
// that matched any quoted gno.land path, comments and strings included.
func seedGraph(t *testing.T, db *DB) *DB {
	t.Helper()

	const when = "2026-08-01T00:00:00Z"
	// app -> lib -> util, plus a cycle between a and b, on two networks.
	edges := map[string][]string{
		"gno.land/r/demo/app":  {"gno.land/p/demo/lib"},
		"gno.land/p/demo/lib":  {"gno.land/p/demo/util"},
		"gno.land/p/demo/util": {},
		"gno.land/r/demo/a":    {"gno.land/r/demo/b"},
		"gno.land/r/demo/b":    {"gno.land/r/demo/a"},
	}

	for _, net := range []string{"alpha", "beta"} {
		for pkg, imports := range edges {
			if err := db.UpsertPackage(net, pkg, "pkg", "g1creator", net+"-"+pkg, 100, when, true, 1); err != nil {
				t.Fatalf("UpsertPackage: %v", err)
			}
			if len(imports) > 0 {
				if err := db.SetDependencies(net, pkg, imports); err != nil {
					t.Fatalf("SetDependencies: %v", err)
				}
			}
		}
	}
	return db
}

func newGraphDB(t *testing.T) *DB {
	t.Helper()

	db := NewTestDB(t)
	db.SetConfiguredNetworks([]config.NetworkConfig{{ID: "alpha"}, {ID: "beta"}})
	return seedGraph(t, db)
}

func TestDependencyGraphWalksTransitively(t *testing.T) {
	db := newGraphDB(t)

	graph, err := db.GetDependencyGraph("alpha", "gno.land/r/demo/app")
	if err != nil {
		t.Fatalf("GetDependencyGraph: %v", err)
	}

	if got := graph["gno.land/r/demo/app"]; len(got) != 1 || got[0] != "gno.land/p/demo/lib" {
		t.Errorf("app imports = %v, want [gno.land/p/demo/lib]", got)
	}
	// The walk has to follow the edge it just found, or the graph is one level
	// deep and the page shows a dependency with no dependencies.
	if got := graph["gno.land/p/demo/lib"]; len(got) != 1 || got[0] != "gno.land/p/demo/util" {
		t.Errorf("lib imports = %v, want [gno.land/p/demo/util]; the walk stopped at depth 1", got)
	}
}

// A cycle is legal in gno and must not hang the request.
func TestDependencyGraphTerminatesOnACycle(t *testing.T) {
	db := newGraphDB(t)

	type result struct {
		graph map[string][]string
		err   error
	}
	done := make(chan result, 1)
	go func() {
		g, err := db.GetDependencyGraph("alpha", "gno.land/r/demo/a")
		done <- result{g, err}
	}()

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("GetDependencyGraph: %v", r.err)
		}
		if len(r.graph) != 2 {
			t.Errorf("graph has %d nodes, want the two in the cycle: %v", len(r.graph), r.graph)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("GetDependencyGraph did not return: the walk is following the cycle forever")
	}
}

func TestReverseGraphFindsDependents(t *testing.T) {
	db := newGraphDB(t)

	graph, err := db.GetReverseGraph("alpha", "gno.land/p/demo/util", 0)
	if err != nil {
		t.Fatalf("GetReverseGraph: %v", err)
	}

	if got := graph["gno.land/p/demo/util"]; len(got) != 1 || got[0] != "gno.land/p/demo/lib" {
		t.Errorf("util dependents = %v, want [gno.land/p/demo/lib]", got)
	}
	if got := graph["gno.land/p/demo/lib"]; len(got) != 1 || got[0] != "gno.land/r/demo/app" {
		t.Errorf("lib dependents = %v, want [gno.land/r/demo/app]; the reverse walk stopped at depth 1", got)
	}
}

// depth 1 is what the deps tab draws, and the whole point is that the second
// hop is absent rather than merely undrawn: util's own dependents are a fact
// about util, and lib's are a fact about lib.
//
// The capped frontier still appears as a key with no edges. Without that, the
// funnel cannot tell a package it reached from one it never heard of, and lib
// would be missing from a picture that has an edge pointing at it.
func TestReverseGraphStopsAtTheRequestedDepth(t *testing.T) {
	db := newGraphDB(t)

	graph, err := db.GetReverseGraph("alpha", "gno.land/p/demo/util", 1)
	if err != nil {
		t.Fatalf("GetReverseGraph: %v", err)
	}

	if got := graph["gno.land/p/demo/util"]; len(got) != 1 || got[0] != "gno.land/p/demo/lib" {
		t.Errorf("util dependents = %v, want [gno.land/p/demo/lib]", got)
	}
	edges, ok := graph["gno.land/p/demo/lib"]
	if !ok {
		t.Fatal("lib is missing from the depth-1 graph; the node an edge points at has to exist")
	}
	if len(edges) != 0 {
		t.Errorf("lib dependents = %v at depth 1, want none: app is two hops from util", edges)
	}
	if _, found := graph["gno.land/r/demo/app"]; found {
		t.Errorf("app reached the depth-1 graph: %v", graph)
	}
}

// A cycle is legal in gno, and the reverse walk meets it the same way the
// forward one does. Unbounded, a -> b -> a has to terminate rather than queue
// forever.
func TestReverseGraphTerminatesOnACycle(t *testing.T) {
	db := newGraphDB(t)

	done := make(chan map[string][]string, 1)
	go func() {
		g, err := db.GetReverseGraph("alpha", "gno.land/r/demo/a", 0)
		if err != nil {
			close(done)
			return
		}
		done <- g
	}()

	select {
	case g, ok := <-done:
		if !ok {
			t.Fatal("GetReverseGraph errored on a cycle")
		}
		if _, found := g["gno.land/r/demo/b"]; !found {
			t.Errorf("b is missing from a's reverse graph: %v", g)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("GetReverseGraph did not return: the reverse walk is following the cycle forever")
	}
}

// Selecting a network must not pull in another chain's edges. The same paths
// exist on both here, which is the shape that made this worth checking: 193
// package paths now live on more than one chain.
func TestGraphIsScopedToOneNetwork(t *testing.T) {
	db := newGraphDB(t)

	// Give beta an extra edge that alpha does not have.
	if err := db.SetDependencies("beta", "gno.land/r/demo/app",
		[]string{"gno.land/p/demo/lib", "gno.land/p/beta/only"}); err != nil {
		t.Fatalf("SetDependencies: %v", err)
	}

	alpha, err := db.GetDependencyGraph("alpha", "gno.land/r/demo/app")
	if err != nil {
		t.Fatalf("GetDependencyGraph(alpha): %v", err)
	}
	for _, imp := range alpha["gno.land/r/demo/app"] {
		if imp == "gno.land/p/beta/only" {
			t.Errorf("beta's edge leaked into the alpha graph: %v", alpha["gno.land/r/demo/app"])
		}
	}

	beta, err := db.GetDependencyGraph("beta", "gno.land/r/demo/app")
	if err != nil {
		t.Fatalf("GetDependencyGraph(beta): %v", err)
	}
	if len(beta["gno.land/r/demo/app"]) != 2 {
		t.Errorf("beta app imports = %v, want both edges", beta["gno.land/r/demo/app"])
	}
}

// An unknown root is an empty graph, not an error: a typo in a URL should render
// an empty page rather than a 500.
func TestGraphOnAnUnknownPackage(t *testing.T) {
	db := newGraphDB(t)

	for _, tc := range []struct {
		name string
		run  func() (map[string][]string, error)
	}{
		{"forward", func() (map[string][]string, error) {
			return db.GetDependencyGraph("alpha", "gno.land/r/demo/nope")
		}},
		{"reverse", func() (map[string][]string, error) {
			return db.GetReverseGraph("alpha", "gno.land/r/demo/nope", 0)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			graph, err := tc.run()
			if err != nil {
				t.Fatalf("unknown package returned an error: %v", err)
			}
			for _, edges := range graph {
				if len(edges) != 0 {
					t.Errorf("unknown package produced edges: %v", graph)
				}
			}
		})
	}
}

// A package cannot import itself, so an edge saying it does is wrong wherever
// it came from.
//
// r/gnoswap/router appeared on its own deps tab as both its own import and its
// own dependent — a self-loop that cannot exist in gno. The graph tab already
// omitted it, so the two views disagreed about the same realm. The edges came
// from the old regex extraction, which matched any quoted gno.land path
// including the package's own; six of them reached the live graph.
//
// Guarded on both sides: SetDependencies refuses to store one, and the detail
// query filters them, which fixes rows written before the guard existed without
// needing a migration.
func TestSelfReferencesNeverReachTheDepsView(t *testing.T) {
	db := newGraphDB(t)

	const self = "gno.land/r/demo/app"

	// Offered a self-import alongside a real one, the store must keep only the
	// real one.
	if err := db.SetDependencies("alpha", self, []string{self, "gno.land/p/demo/lib"}); err != nil {
		t.Fatalf("SetDependencies: %v", err)
	}

	detail, err := db.GetPackageDetail("alpha", self)
	if err != nil {
		t.Fatalf("GetPackageDetail: %v", err)
	}
	for _, imp := range detail.Imports {
		if imp == self {
			t.Errorf("the realm is listed as its own import: %v", detail.Imports)
		}
	}
	if len(detail.Imports) != 1 || detail.Imports[0] != "gno.land/p/demo/lib" {
		t.Errorf("imports = %v, want just the real one; dropping the self-edge must not drop the rest", detail.Imports)
	}
	for _, dep := range detail.Dependents {
		if dep.Path == self {
			t.Errorf("the realm is listed as its own dependent: %v", dep)
		}
	}
}

// The read-side filter has to work on rows already in the table, because that
// is the case it exists for: the write-side guard cannot reach history.
func TestExistingSelfEdgesAreFilteredOnRead(t *testing.T) {
	db := newGraphDB(t)

	const self = "gno.land/r/demo/app"
	// Written straight to the table, the way the old extraction did.
	if _, err := db.db.Exec(
		`INSERT OR IGNORE INTO dependencies (network, package_path, import_path) VALUES (?, ?, ?)`,
		"alpha", self, self); err != nil {
		t.Fatalf("seed self-edge: %v", err)
	}

	detail, err := db.GetPackageDetail("alpha", self)
	if err != nil {
		t.Fatalf("GetPackageDetail: %v", err)
	}
	for _, imp := range detail.Imports {
		if imp == self {
			t.Error("a self-edge already in the table still reached the imports list")
		}
	}
	for _, dep := range detail.Dependents {
		if dep.Path == self {
			t.Error("a self-edge already in the table still reached the dependents list")
		}
	}
}
