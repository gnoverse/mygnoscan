package store

import "testing"

// seedDiscover builds a chain where the two ranking signals disagree, which is
// the only interesting case: `loop` is the busiest realm by a mile and has one
// user, `social` is quieter and has many.
func seedDiscover(t *testing.T) *DB {
	t.Helper()
	db := NewTestDB(t)
	add := func(path string, isRealm bool) {
		t.Helper()
		if err := db.UpsertPackage("alpha", path, "x", "g1creator", "tx-"+path, 10,
			"2026-09-01T00:00:00Z", isRealm, 1); err != nil {
			t.Fatal(err)
		}
	}
	add("gno.land/r/x/loop", true)
	add("gno.land/r/x/social", true)
	add("gno.land/p/x/lib", false)

	n := 0
	call := func(path, caller, at string) {
		t.Helper()
		n++
		if err := db.InsertCall("alpha", "tx-"+caller+"-"+at, 20, n, at, caller, path, "Do", true); err != nil {
			t.Fatal(err)
		}
	}
	// One address, forty calls.
	for i := 0; i < 40; i++ {
		call("gno.land/r/x/loop", "g1bot", "2026-09-2"+string(rune('0'+i%10))+"T00:00:00Z")
	}
	// Eight addresses, eight calls.
	for i := 0; i < 8; i++ {
		call("gno.land/r/x/social", "g1user"+string(rune('a'+i)), "2026-09-20T00:00:00Z")
	}
	// A library is called too, and must never be offered as an app.
	call("gno.land/p/x/lib", "g1user", "2026-09-20T00:00:00Z")
	return db
}

// The claim the weights encode: reach outranks activity. One script in a loop
// is thousands of calls and one caller, and a directory that ranked on calls
// would put it first every time.
func TestDiscoverRanksReachOverActivity(t *testing.T) {
	db := seedDiscover(t)

	got, err := db.DiscoverApps("alpha", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) < 2 {
		t.Fatalf("got %d apps, want at least the two realms", len(got))
	}
	if got[0].Path != "gno.land/r/x/social" {
		t.Errorf("ranked %s first; 8 callers must beat 40 calls from one",
			got[0].Path)
	}
	if got[0].Callers != 8 || got[1].Calls != 40 {
		t.Errorf("counts are wrong: %+v / %+v", got[0], got[1])
	}
	// 8 callers * 10 + 8 calls
	if got[0].Score != 8*ScoreCallerWeight+8*ScoreCallWeight {
		t.Errorf("score = %d, want the documented formula", got[0].Score)
	}
}

// A `p/` package has no page to open and no user to count. It belongs in a
// directory of packages, which /packages already is.
func TestDiscoverOffersRealmsOnly(t *testing.T) {
	db := seedDiscover(t)

	got, err := db.DiscoverApps("alpha", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range got {
		if a.Path == "gno.land/p/x/lib" {
			t.Fatal("a library was offered as an app")
		}
	}
}

// The window bounds the score, not just a display figure: a realm that was busy
// last year and is dead now must not outrank one that is busy today.
func TestDiscoverWindowBoundsTheScore(t *testing.T) {
	db := seedDiscover(t)

	all, err := db.DiscoverApps("alpha", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	future, err := db.DiscoverApps("alpha", "2099-01-01T00:00:00Z", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) == 0 {
		t.Fatal("nothing discovered without a window")
	}
	// Everything scores zero inside an empty window, and HAVING drops it.
	if len(future) != 0 {
		t.Errorf("got %d apps in a window nothing falls into", len(future))
	}
}

// The doc travels with the package, and a realm without one is still a realm
// people use: the LEFT JOIN is what keeps the directory's contents from
// depending on whether a background indexing pass has caught up.
func TestDiscoverCarriesThePackageDocWithoutRequiringIt(t *testing.T) {
	db := seedDiscover(t)
	if err := db.ReplaceSymbols("alpha", "gno.land/r/x/social", "k",
		"Package social is a feed. It has more to say after the first sentence.", nil); err != nil {
		t.Fatal(err)
	}

	got, err := db.DiscoverApps("alpha", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	var social, loop DiscoveredApp
	for _, a := range got {
		switch a.Path {
		case "gno.land/r/x/social":
			social = a
		case "gno.land/r/x/loop":
			loop = a
		}
	}
	if social.PackageDoc == "" {
		t.Error("the indexed doc did not reach the discovery row")
	}
	if loop.Path == "" {
		t.Error("a realm with no indexed doc dropped out of the directory")
	}
}
