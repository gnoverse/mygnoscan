package store

import (
	"fmt"
	"testing"
	"time"
)

func TestNamespaceOf(t *testing.T) {
	tests := []struct {
		name string
		path string
		want string
	}{
		{"realm under a named namespace", "gno.land/r/gnoswap/gns", "gnoswap"},
		{"pure package", "gno.land/p/demo/avl", "demo"},
		{"system realm", "gno.land/r/sys/namereg/v0", "sys"},
		{"address namespace groups by the address", "gno.land/r/g1n4pl5uc4yt5r96m9w6fmdznx3x0jyg8l6arhmt/gnomi/pad", "g1n4pl5uc4yt5r96m9w6fmdznx3x0jyg8l6arhmt"},
		{"deep path keeps the first element", "gno.land/r/moul/x/daily/counter/v0", "moul"},
		{"realm directly under r", "gno.land/r/demo", "demo"},
		{"p inside an r namespace is not a marker", "gno.land/r/moul/p/thing", "moul"},
		{"unknown domain still finds r", "example.com/r/foo/bar", "foo"},
		{"no r or p marker falls back to the second element", "gno.land/x/foo", "x"},
		{"single element", "foo", "foo"},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NamespaceOf(tt.path); got != tt.want {
				t.Errorf("NamespaceOf(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}

// seedContracts writes a small two-network world:
//
//	mainnet: r/a/one, r/a/two, p/b/lib
//	pearl:   r/a/one (same path, different traffic)
//
// The duplicated path is the point. 193 package paths exist on more than one
// network in production, so any aggregate that joins on path alone silently
// merges two chains into one row.
func seedContracts(t *testing.T, db *DB) time.Time {
	t.Helper()
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

	pkgs := []struct {
		network string
		path    string
		name    string
		realm   bool
	}{
		{"mainnet", "gno.land/r/a/one", "one", true},
		{"mainnet", "gno.land/r/a/two", "two", true},
		{"mainnet", "gno.land/p/b/lib", "lib", false},
		{"pearl", "gno.land/r/a/one", "one", true},
	}
	for i, p := range pkgs {
		if err := db.UpsertPackage(p.network, p.path, p.name, "g1creator", "TX", 100+i,
			rfc3339(base.Add(time.Duration(i)*time.Hour)), p.realm, 2); err != nil {
			t.Fatalf("upsert %s: %v", p.path, err)
		}
	}

	// mainnet calls: alice uses one+two, bob uses one+lib, carol uses one only.
	calls := []struct {
		network string
		caller  string
		path    string
		at      time.Time
	}{
		{"mainnet", "g1alice", "gno.land/r/a/one", base.Add(1 * time.Hour)},
		{"mainnet", "g1alice", "gno.land/r/a/two", base.Add(2 * time.Hour)},
		{"mainnet", "g1bob", "gno.land/r/a/one", base.Add(3 * time.Hour)},
		{"mainnet", "g1bob", "gno.land/p/b/lib", base.Add(4 * time.Hour)},
		{"mainnet", "g1carol", "gno.land/r/a/one", base.Add(5 * time.Hour)},
		// Same address, same paths, other chain. Must never merge.
		{"pearl", "g1alice", "gno.land/r/a/one", base.Add(6 * time.Hour)},
	}
	for i, c := range calls {
		// A distinct hash per call: calls is unique on (network, tx_hash,
		// msg_index), so reusing one hash silently drops every call but the first.
		MustCall(t, db, c.network, fmt.Sprintf("TXC%d", i), 200+i, c.at, c.caller, c.path, "Fn")
	}

	if err := db.SetDependencies("mainnet", "gno.land/r/a/one", []string{"gno.land/p/b/lib"}); err != nil {
		t.Fatalf("deps one: %v", err)
	}
	if err := db.SetDependencies("mainnet", "gno.land/r/a/two", []string{"gno.land/p/b/lib", "gno.land/r/a/one"}); err != nil {
		t.Fatalf("deps two: %v", err)
	}
	if err := db.SetDependencies("pearl", "gno.land/r/a/one", []string{"gno.land/p/b/lib"}); err != nil {
		t.Fatalf("deps pearl: %v", err)
	}
	return base
}

func nodesByPath(nodes []ContractNode) map[string]ContractNode {
	m := make(map[string]ContractNode, len(nodes))
	for _, n := range nodes {
		m[n.Path] = n
	}
	return m
}

func TestContractMapNodes(t *testing.T) {
	db := NewTestDB(t)
	seedContracts(t, db)

	nodes, err := db.ContractMapNodes("mainnet", time.Time{})
	if err != nil {
		t.Fatalf("ContractMapNodes: %v", err)
	}
	if len(nodes) != 3 {
		t.Fatalf("got %d nodes, want 3 (pearl must not leak in)", len(nodes))
	}

	by := nodesByPath(nodes)
	one, ok := by["gno.land/r/a/one"]
	if !ok {
		t.Fatal("r/a/one missing")
	}
	// Three mainnet calls from three addresses. The pearl call from g1alice
	// must not be counted, and must not inflate the caller count either.
	if one.Calls != 3 {
		t.Errorf("calls = %d, want 3", one.Calls)
	}
	if one.UniqueCallers != 3 {
		t.Errorf("unique callers = %d, want 3", one.UniqueCallers)
	}
	if one.Namespace != "a" {
		t.Errorf("namespace = %q, want %q", one.Namespace, "a")
	}
	if !one.IsRealm {
		t.Error("r/a/one should be a realm")
	}
	if by["gno.land/p/b/lib"].IsRealm {
		t.Error("p/b/lib should not be a realm")
	}
	if one.Creator != "g1creator" {
		t.Errorf("creator = %q", one.Creator)
	}
	if one.DeployedAt == "" {
		t.Error("deployed_at empty, the map labels bubbles with it")
	}
	// r/a/one imports p/b/lib and is imported by r/a/two. Sizing the import
	// view by anything else hides the packages everything depends on, because
	// a pure package has no calls and no gas of its own.
	if one.Imports != 1 {
		t.Errorf("imports = %d, want 1", one.Imports)
	}
	if one.Importers != 1 {
		t.Errorf("importers = %d, want 1", one.Importers)
	}
	if got := by["gno.land/p/b/lib"].Importers; got != 2 {
		t.Errorf("p/b/lib importers = %d, want 2", got)
	}
	// The pearl rows import p/b/lib too, and must not be counted here.
	if got := by["gno.land/p/b/lib"].Imports; got != 0 {
		t.Errorf("p/b/lib imports = %d, want 0", got)
	}
}

func TestContractMapNodesWindow(t *testing.T) {
	db := NewTestDB(t)
	base := seedContracts(t, db)

	// Window opening after r/a/one's first two calls: one call left for it,
	// and the node must still be present with zero for anything older.
	nodes, err := db.ContractMapNodes("mainnet", base.Add(4*time.Hour+30*time.Minute))
	if err != nil {
		t.Fatalf("ContractMapNodes: %v", err)
	}
	if len(nodes) != 3 {
		t.Fatalf("got %d nodes, want 3: a window narrows metrics, never the node set", len(nodes))
	}
	by := nodesByPath(nodes)
	if got := by["gno.land/r/a/one"].Calls; got != 1 {
		t.Errorf("windowed calls = %d, want 1", got)
	}
	if got := by["gno.land/r/a/two"].Calls; got != 0 {
		t.Errorf("windowed calls for r/a/two = %d, want 0", got)
	}
}

func TestContractImportEdges(t *testing.T) {
	db := NewTestDB(t)
	seedContracts(t, db)

	edges, err := db.ContractImportEdges("mainnet")
	if err != nil {
		t.Fatalf("ContractImportEdges: %v", err)
	}
	// one -> lib, two -> lib, two -> one. The pearl edge must not appear.
	if len(edges) != 3 {
		t.Fatalf("got %d edges, want 3: %+v", len(edges), edges)
	}
	seen := map[string]bool{}
	for _, e := range edges {
		seen[e.Source+" -> "+e.Target] = true
		if e.Weight != 1 {
			t.Errorf("import edge weight = %d, want 1", e.Weight)
		}
	}
	for _, want := range []string{
		"gno.land/r/a/one -> gno.land/p/b/lib",
		"gno.land/r/a/two -> gno.land/p/b/lib",
		"gno.land/r/a/two -> gno.land/r/a/one",
	} {
		if !seen[want] {
			t.Errorf("missing edge %s", want)
		}
	}
}

// Import edges point at paths, and a path can be imported without ever having
// been deployed on that chain (a dependency on something only published
// elsewhere). Those dangling targets have no bubble to attach to.
func TestContractImportEdgesSkipUndeployedTargets(t *testing.T) {
	db := NewTestDB(t)
	seedContracts(t, db)
	if err := db.SetDependencies("mainnet", "gno.land/r/a/one",
		[]string{"gno.land/p/b/lib", "gno.land/p/never/deployed"}); err != nil {
		t.Fatalf("deps: %v", err)
	}

	edges, err := db.ContractImportEdges("mainnet")
	if err != nil {
		t.Fatalf("ContractImportEdges: %v", err)
	}
	for _, e := range edges {
		if e.Target == "gno.land/p/never/deployed" {
			t.Fatal("edge points at a package with no node on the map")
		}
	}
}

func TestContractCallerEdges(t *testing.T) {
	db := NewTestDB(t)
	seedContracts(t, db)

	// min=1 so the single-address overlaps in the fixture survive.
	edges, err := db.ContractCallerEdges("mainnet", time.Time{}, 1, 100, 100)
	if err != nil {
		t.Fatalf("ContractCallerEdges: %v", err)
	}
	// alice links one+two, bob links one+lib. carol touches one contract only
	// and links nothing.
	if len(edges) != 2 {
		t.Fatalf("got %d edges, want 2: %+v", len(edges), edges)
	}
	for _, e := range edges {
		if e.Source >= e.Target {
			t.Errorf("edge %s -> %s is not normalised source < target", e.Source, e.Target)
		}
		if e.Weight != 1 {
			t.Errorf("weight = %d, want 1", e.Weight)
		}
	}
}

func TestContractCallerEdgesMinWeight(t *testing.T) {
	db := NewTestDB(t)
	base := seedContracts(t, db)
	// Give one+two a second shared caller so that pair reaches weight 2 while
	// one+lib stays at 1.
	MustCall(t, db, "mainnet", "TXD", 400, base, "g1dave", "gno.land/r/a/one", "Fn")
	MustCall(t, db, "mainnet", "TXE", 401, base, "g1dave", "gno.land/r/a/two", "Fn")

	edges, err := db.ContractCallerEdges("mainnet", time.Time{}, 2, 100, 100)
	if err != nil {
		t.Fatalf("ContractCallerEdges: %v", err)
	}
	if len(edges) != 1 {
		t.Fatalf("got %d edges, want 1 at min weight 2: %+v", len(edges), edges)
	}
	if edges[0].Weight != 2 {
		t.Errorf("weight = %d, want 2", edges[0].Weight)
	}
	if edges[0].Source != "gno.land/r/a/one" || edges[0].Target != "gno.land/r/a/two" {
		t.Errorf("wrong pair survived: %+v", edges[0])
	}
}

// One address touching everything links everything to everything, which is how
// this map turns into mud. Callers above the fanout cap keep their node
// metrics and stop producing edges.
func TestContractCallerEdgesFanoutCap(t *testing.T) {
	db := NewTestDB(t)
	base := seedContracts(t, db)
	for i, p := range []string{"gno.land/r/a/one", "gno.land/r/a/two", "gno.land/p/b/lib"} {
		MustCall(t, db, "mainnet", fmt.Sprintf("TXBOT%d", i), 500+i, base, "g1bot", p, "Fn")
	}

	uncapped, err := db.ContractCallerEdges("mainnet", time.Time{}, 1, 100, 100)
	if err != nil {
		t.Fatalf("uncapped: %v", err)
	}
	if len(uncapped) != 3 {
		t.Fatalf("uncapped edges = %d, want 3 (the bot links all three pairs)", len(uncapped))
	}

	capped, err := db.ContractCallerEdges("mainnet", time.Time{}, 1, 100, 2)
	if err != nil {
		t.Fatalf("capped: %v", err)
	}
	if len(capped) != 2 {
		t.Fatalf("capped edges = %d, want 2 (the 3-contract bot is excluded): %+v", len(capped), capped)
	}
}

func TestContractCallerEdgesLimitTakesHeaviest(t *testing.T) {
	db := NewTestDB(t)
	base := seedContracts(t, db)
	MustCall(t, db, "mainnet", "TXD", 400, base, "g1dave", "gno.land/r/a/one", "Fn")
	MustCall(t, db, "mainnet", "TXE", 401, base, "g1dave", "gno.land/r/a/two", "Fn")

	edges, err := db.ContractCallerEdges("mainnet", time.Time{}, 1, 1, 100)
	if err != nil {
		t.Fatalf("ContractCallerEdges: %v", err)
	}
	if len(edges) != 1 {
		t.Fatalf("got %d edges, want 1", len(edges))
	}
	if edges[0].Weight != 2 {
		t.Errorf("limit kept weight %d, want the heaviest edge (2)", edges[0].Weight)
	}
}

func TestContractCallerEdgesWindow(t *testing.T) {
	db := NewTestDB(t)
	base := seedContracts(t, db)

	// After this instant alice has called r/a/two but not r/a/one, so the pair
	// she created all-time has no overlap inside the window.
	edges, err := db.ContractCallerEdges("mainnet", base.Add(90*time.Minute), 1, 100, 100)
	if err != nil {
		t.Fatalf("ContractCallerEdges: %v", err)
	}
	for _, e := range edges {
		if e.Source == "gno.land/r/a/one" && e.Target == "gno.land/r/a/two" {
			t.Fatal("edge survived a window that excludes one of its two calls")
		}
	}
}

// The failure this guards against is silent: an aggregate that joins on
// pkg_path alone still returns plausible edges, just ones belonging to no
// single chain.
func TestContractCallerEdgesNetworkIsolation(t *testing.T) {
	db := NewTestDB(t)
	base := seedContracts(t, db)
	// g1alice called r/a/one on pearl and r/a/two on mainnet. That is not an
	// overlap on either chain.
	MustCall(t, db, "pearl", "TXP", 600, base, "g1alice", "gno.land/r/a/two", "Fn")
	if err := db.UpsertPackage("pearl", "gno.land/r/a/two", "two", "g1creator", "TX", 601,
		rfc3339(base), true, 1); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	mainnetEdges, err := db.ContractCallerEdges("mainnet", time.Time{}, 1, 100, 100)
	if err != nil {
		t.Fatalf("mainnet: %v", err)
	}
	pearlEdges, err := db.ContractCallerEdges("pearl", time.Time{}, 1, 100, 100)
	if err != nil {
		t.Fatalf("pearl: %v", err)
	}
	if len(mainnetEdges) != 2 {
		t.Errorf("mainnet edges = %d, want 2", len(mainnetEdges))
	}
	// On pearl alice now touches one and two: exactly one pair.
	if len(pearlEdges) != 1 {
		t.Errorf("pearl edges = %d, want 1: %+v", len(pearlEdges), pearlEdges)
	}
}

func TestContractMapNodesEmptyNetwork(t *testing.T) {
	db := NewTestDB(t)
	seedContracts(t, db)

	nodes, err := db.ContractMapNodes("nosuchnet", time.Time{})
	if err != nil {
		t.Fatalf("ContractMapNodes: %v", err)
	}
	if len(nodes) != 0 {
		t.Errorf("got %d nodes for an unknown network, want 0", len(nodes))
	}
}
