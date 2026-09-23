package store

import (
	"testing"
	"time"
)

// appsFixture seeds two chains that share a path, because that is the failure
// this file mostly exists to catch: the directory is chain-independent and its
// numbers are not, so a query that forgets the network attributes staging's
// traffic to mainnet and the card says an app is popular somewhere it is not
// even deployed.
func appsFixture(t *testing.T) (*DB, string, string) {
	t.Helper()
	db := NewTestDB(t)

	recent := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339)
	old := time.Now().UTC().AddDate(0, 0, -60).Format(time.RFC3339)

	pkg := func(network, path, creator string, isRealm bool) {
		t.Helper()
		if err := db.UpsertPackage(network, path, "x", creator, "tx-"+path, 10, old, isRealm, 1); err != nil {
			t.Fatalf("UpsertPackage: %v", err)
		}
	}
	call := func(network, hash string, idx int, when, caller, pkgPath string) {
		t.Helper()
		if err := db.InsertCall(network, hash, 11, idx, when, caller, pkgPath, "Post", true); err != nil {
			t.Fatalf("InsertCall: %v", err)
		}
	}

	pkg("live", "gno.land/r/demo/boards", "g1dev", true)
	pkg("live", "gno.land/r/demo/quiet", "g1dev", true)
	pkg("live", "gno.land/r/demo/busy", "g1other", true)
	pkg("live", "gno.land/p/demo/lib", "g1dev", false)
	pkg("other", "gno.land/r/demo/boards", "g1dev", true)

	// boards: two callers, one call outside the window.
	call("live", "c1", 0, recent, "g1a", "gno.land/r/demo/boards")
	call("live", "c2", 0, recent, "g1b", "gno.land/r/demo/boards")
	call("live", "c3", 0, old, "g1a", "gno.land/r/demo/boards")
	// busy: not in the directory, and the busiest thing on the chain.
	for i, c := range []string{"g1a", "g1b", "g1c", "g1d"} {
		call("live", "b"+string(rune('0'+i)), 0, recent, c, "gno.land/r/demo/busy")
	}
	// A pure package gets called too, and must never be offered as an app.
	call("live", "p1", 0, recent, "g1a", "gno.land/p/demo/lib")
	// The other chain's traffic on the same path, which must not leak.
	for i := 0; i < 99; i++ {
		call("other", "o"+string(rune('0'+i%10))+string(rune('a'+i/10)), 0, recent, "g1z", "gno.land/r/demo/boards")
	}
	return db, recent, old
}

func TestAppStats(t *testing.T) {
	db, _, _ := appsFixture(t)
	since := time.Now().UTC().AddDate(0, 0, -30).Format(time.RFC3339)
	paths := []string{
		"gno.land/r/demo/boards",
		"gno.land/r/demo/quiet",
		"gno.land/r/demo/absent",
	}

	stats, err := db.AppStats("live", paths, since)
	if err != nil {
		t.Fatalf("AppStats: %v", err)
	}

	tests := []struct {
		path     string
		deployed bool
		calls    int
		callers  int
		window   int
	}{
		// Three calls all time, two inside the 30d window. A card that showed
		// only one of those two numbers would call this app either busier or
		// deader than it is.
		{"gno.land/r/demo/boards", true, 3, 2, 2},
		// Deployed and never called is a real state, and distinct from the one
		// below it.
		{"gno.land/r/demo/quiet", true, 0, 0, 0},
		// Not on this chain at all: absent from the map, not a zeroed entry.
		{"gno.land/r/demo/absent", false, 0, 0, 0},
	}
	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			s, ok := stats[tc.path]
			if !tc.deployed {
				if ok {
					t.Fatalf("%s is not deployed on live, got an entry: %+v", tc.path, s)
				}
				return
			}
			if !ok {
				t.Fatalf("%s: no entry", tc.path)
			}
			if !s.Deployed {
				t.Errorf("Deployed = false, want true")
			}
			if s.Calls != tc.calls || s.Callers != tc.callers || s.CallsWindow != tc.window {
				t.Errorf("calls/callers/window = %d/%d/%d, want %d/%d/%d",
					s.Calls, s.Callers, s.CallsWindow, tc.calls, tc.callers, tc.window)
			}
		})
	}
}

// The other chain has 99 calls on the same path. If any of them reach the live
// figures the join lost its network, which is the documented way to break every
// aggregate in this package (AGENTS.md).
func TestAppStatsDoNotBlendChains(t *testing.T) {
	db, _, _ := appsFixture(t)
	since := time.Now().UTC().AddDate(0, 0, -30).Format(time.RFC3339)
	path := "gno.land/r/demo/boards"

	live, err := db.AppStats("live", []string{path}, since)
	if err != nil {
		t.Fatalf("AppStats live: %v", err)
	}
	if got := live[path].Calls; got != 3 {
		t.Errorf("live calls = %d, want 3 (the other chain's 99 must not count)", got)
	}
	other, err := db.AppStats("other", []string{path}, since)
	if err != nil {
		t.Fatalf("AppStats other: %v", err)
	}
	if got := other[path].Calls; got != 99 {
		t.Errorf("other calls = %d, want 99", got)
	}
}

// An empty window means "all of history", and CallsWindow then has to equal
// Calls. Returning zero would make every card read "quiet" on a page that never
// asked for a window.
func TestAppStatsEmptyWindowCountsEverything(t *testing.T) {
	db, _, _ := appsFixture(t)
	stats, err := db.AppStats("live", []string{"gno.land/r/demo/boards"}, "")
	if err != nil {
		t.Fatalf("AppStats: %v", err)
	}
	s := stats["gno.land/r/demo/boards"]
	if s.CallsWindow != s.Calls || s.Calls != 3 {
		t.Errorf("window/calls = %d/%d, want 3/3", s.CallsWindow, s.Calls)
	}
}

func TestAppCandidates(t *testing.T) {
	db, _, _ := appsFixture(t)
	since := time.Now().UTC().AddDate(0, 0, -30).Format(time.RFC3339)
	listed := []string{"gno.land/r/demo/boards", "gno.land/r/demo/quiet"}

	got, err := db.AppCandidates("live", listed, since, 10)
	if err != nil {
		t.Fatalf("AppCandidates: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d candidates, want 1: %+v", len(got), got)
	}
	c := got[0]
	if c.Path != "gno.land/r/demo/busy" {
		t.Errorf("path = %q, want the busiest unlisted realm", c.Path)
	}
	if c.Calls != 4 || c.Callers != 4 {
		t.Errorf("calls/callers = %d/%d, want 4/4", c.Calls, c.Callers)
	}
	if c.Creator != "g1other" {
		t.Errorf("creator = %q, want g1other", c.Creator)
	}
}

// Two rules the suggestion queue must not break: it never offers something the
// directory already lists, and it never offers a pure package, which has no
// page to launch and is not an app.
func TestAppCandidatesExcludeListedAndPurePackages(t *testing.T) {
	db, _, _ := appsFixture(t)
	since := time.Now().UTC().AddDate(0, 0, -30).Format(time.RFC3339)

	got, err := db.AppCandidates("live", nil, since, 10)
	if err != nil {
		t.Fatalf("AppCandidates: %v", err)
	}
	for _, c := range got {
		if c.Path == "gno.land/p/demo/lib" {
			t.Errorf("a pure package was offered as an app: %q", c.Path)
		}
	}

	got, err = db.AppCandidates("live", []string{"gno.land/r/demo/busy"}, since, 10)
	if err != nil {
		t.Fatalf("AppCandidates: %v", err)
	}
	for _, c := range got {
		if c.Path == "gno.land/r/demo/busy" {
			t.Errorf("an already-listed realm was offered again: %q", c.Path)
		}
	}
}

func TestCountRealmsIsPerChainAndExcludesPurePackages(t *testing.T) {
	db, _, _ := appsFixture(t)

	n, err := db.CountRealms("live")
	if err != nil {
		t.Fatalf("CountRealms: %v", err)
	}
	if n != 3 {
		t.Errorf("live realms = %d, want 3 (p/demo/lib is not one)", n)
	}
	if n, err = db.CountRealms("other"); err != nil || n != 1 {
		t.Errorf("other realms = %d (err %v), want 1", n, err)
	}
}
