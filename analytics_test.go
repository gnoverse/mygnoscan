package main

import (
	"net/http"
	"testing"
)

// seedSharedRealm deploys the same package path on two chains and gives it very
// different traffic on each.
//
// This is not a contrived shape: 193 package paths now exist on more than one
// network, because pearl launched carrying the same demo realms gnoland1 has.
// When issue #86 was written the count was zero, which is why the collision was
// filed as hypothetical.
func seedSharedRealm(t *testing.T, db *DB) {
	t.Helper()

	const path = "gno.land/r/demo/boards"
	const when = "2026-08-01T00:00:00Z"

	for _, n := range []string{"busy", "quiet"} {
		if err := db.UpsertPackage(n, path, "boards", "g1creator", n+"-deploy", 100, when, true, 1); err != nil {
			t.Fatalf("UpsertPackage(%s): %v", n, err)
		}
	}

	// 50 calls on busy, 2 on quiet.
	for i := 0; i < 50; i++ {
		if err := db.InsertCall("busy", "busy-call-"+itoa(i), 100+i, when, "g1shared", path, "Post", true); err != nil {
			t.Fatalf("InsertCall: %v", err)
		}
	}
	for i := 0; i < 2; i++ {
		if err := db.InsertCall("quiet", "quiet-call-"+itoa(i), 100+i, when, "g1shared", path, "Post", true); err != nil {
			t.Fatalf("InsertCall: %v", err)
		}
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// Selecting a network must scope the rankings to it.
//
// The join filters were computed and then discarded with a blank assignment, so
// every ranking counted activity from every chain no matter what was selected.
// On production that put a realm with 22,789 calls at the top of pearl's
// leaderboard when pearl had none of them, and ordered the whole page by other
// chains' traffic.
func TestAnalyticsRankingsAreScopedToTheSelectedNetwork(t *testing.T) {
	db := newTestDB(t)
	db.SetConfiguredNetworks([]NetworkConfig{{ID: "busy"}, {ID: "quiet"}})
	seedSharedRealm(t, db)

	a, err := db.GetAnalytics("quiet")
	if err != nil {
		t.Fatalf("GetAnalytics: %v", err)
	}
	if len(a.TopRealms) == 0 {
		t.Fatal("no realms")
	}

	for _, r := range a.TopRealms {
		if r.Network != "quiet" {
			t.Errorf("realm %s is from %q in the quiet view", r.Path, r.Network)
		}
		if r.Calls != 2 {
			t.Errorf("%s shows %d calls; quiet has 2 and busy's 50 must not be counted here",
				r.Path, r.Calls)
		}
	}

	for _, c := range a.TopCallers {
		if c.Network != "quiet" {
			t.Errorf("caller %s is from %q in the quiet view", c.Address, c.Network)
		}
		if c.Calls != 2 {
			t.Errorf("caller %s shows %d calls, want quiet's 2", c.Address, c.Calls)
		}
	}
}

// In all-networks mode the same path appears once per chain, with each row
// carrying its own figures, rather than once with the two summed.
func TestAnalyticsSplitsASharedPathPerNetwork(t *testing.T) {
	db := newTestDB(t)
	db.SetConfiguredNetworks([]NetworkConfig{{ID: "busy"}, {ID: "quiet"}})
	seedSharedRealm(t, db)

	a, err := db.GetAnalytics("")
	if err != nil {
		t.Fatalf("GetAnalytics: %v", err)
	}

	byNetwork := map[string]int{}
	for _, r := range a.TopRealms {
		if r.Path != "gno.land/r/demo/boards" {
			continue
		}
		if r.Network == "" {
			t.Fatalf("merged ranking row has no network: %+v", r)
		}
		byNetwork[r.Network] = r.Calls
	}

	if len(byNetwork) != 2 {
		t.Fatalf("the shared path produced %d row(s), want one per chain: %v", len(byNetwork), byNetwork)
	}
	if byNetwork["busy"] != 50 || byNetwork["quiet"] != 2 {
		t.Errorf("call counts = %v, want busy 50 and quiet 2 kept apart", byNetwork)
	}
}

// An address active on two chains is two actors with two histories. Counting it
// once undercounts: on production, 68,506 blended against 68,806 honest.
func TestAnalyticsCountsAddressesPerNetwork(t *testing.T) {
	db := newTestDB(t)
	db.SetConfiguredNetworks([]NetworkConfig{{ID: "busy"}, {ID: "quiet"}})
	seedSharedRealm(t, db)

	a, err := db.GetAnalytics("")
	if err != nil {
		t.Fatalf("GetAnalytics: %v", err)
	}

	// g1shared calls on both chains and g1creator deploys on both, so four
	// (address, network) pairs exist where only two distinct strings do.
	if a.TotalAddresses != 4 {
		t.Errorf("total addresses = %d, want 4: two addresses seen on two chains each", a.TotalAddresses)
	}
}

// A network with no rows must not inherit another's.
func TestAnalyticsOnANetworkWithNoActivity(t *testing.T) {
	db := newTestDB(t)
	db.SetConfiguredNetworks([]NetworkConfig{{ID: "busy"}, {ID: "quiet"}, {ID: "empty"}})
	seedSharedRealm(t, db)

	a, err := db.GetAnalytics("empty")
	if err != nil {
		t.Fatalf("GetAnalytics: %v", err)
	}
	if a.TotalCalls != 0 || len(a.TopCallers) != 0 {
		t.Errorf("an empty network reported %d calls and %d callers", a.TotalCalls, len(a.TopCallers))
	}
	for _, r := range a.TopRealms {
		if r.Calls != 0 {
			t.Errorf("realm %s on an empty network shows %d calls", r.Path, r.Calls)
		}
	}
}

// Liveness cannot be merged at all — not even wrongly, the way a denominated
// amount can be summed. There is no such thing as the height, or the last block
// time, of four chains at once. The page used to answer with an arbitrary entry
// of a Go map and present it under a global heading.
func TestSanityReportsLivenessPerNetwork(t *testing.T) {
	api, alpha, beta := newIndexerAPI(t)
	alpha.latestHeight = 3_100_024
	beta.latestHeight = 400_024

	t.Run("all networks reports every chain and no global height", func(t *testing.T) {
		rec, body := serve(t, api, "/api/sanity/overview")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, body)
		}

		var ov SanityOverview
		mustJSON(t, body, &ov)

		if len(ov.ByNetwork) != 2 {
			t.Fatalf("by_network has %d entries, want one per chain: %+v", len(ov.ByNetwork), ov.ByNetwork)
		}
		if ov.ByNetwork["alpha"].ChainHeight != 3_100_024 || ov.ByNetwork["beta"].ChainHeight != 400_024 {
			t.Errorf("heights = alpha %d, beta %d; want each chain's own",
				ov.ByNetwork["alpha"].ChainHeight, ov.ByNetwork["beta"].ChainHeight)
		}
		// The top-level fields stay empty because no single value could be right.
		if ov.ChainHeight != 0 {
			t.Errorf("a global chain_height of %d was reported; it belongs to one chain and reads as all of them", ov.ChainHeight)
		}
	})

	t.Run("a single network fills the top-level fields and omits the split", func(t *testing.T) {
		rec, body := serve(t, api, "/api/sanity/overview?network=beta")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, body)
		}

		var ov SanityOverview
		mustJSON(t, body, &ov)

		if ov.ChainHeight != 400_024 {
			t.Errorf("chain_height = %d, want beta's 400024", ov.ChainHeight)
		}
		if ov.ByNetwork != nil {
			t.Errorf("by_network was sent for a single network, where it is just the total again: %+v", ov.ByNetwork)
		}
	})
}

// An unreachable chain must be distinguishable from one sitting at genesis;
// both are height 0 in the fields that matter.
func TestSanityMarksAnUnreachableChain(t *testing.T) {
	api, alpha, _ := newIndexerAPI(t)
	alpha.status = http.StatusInternalServerError

	rec, body := serve(t, api, "/api/sanity/overview")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, body)
	}

	var ov SanityOverview
	mustJSON(t, body, &ov)

	if live, ok := ov.ByNetwork["alpha"]; ok && live.Reachable {
		t.Errorf("a chain returning 500s was reported reachable: %+v", live)
	}
	if live := ov.ByNetwork["beta"]; !live.Reachable {
		t.Error("the healthy chain was dropped along with the broken one")
	}
}

// The sanity page must not silently omit a chain. fanOut skips networks with no
// client or an open breaker, but this is the page whose whole job is to report
// liveness — a chain missing from it is the one a reader most needs to see.
func TestSanityListsEveryConfiguredNetwork(t *testing.T) {
	db := newTestDB(t)
	nets := []NetworkConfig{{ID: "alpha"}, {ID: "beta"}, {ID: "noclient"}}
	db.SetConfiguredNetworks(nets)

	alpha, alphaClient := newFakeIndexer(t)
	alpha.seedChain(100, 5)

	// beta's breaker is already open, as it would be after a real outage.
	beta, betaClient := newFakeIndexer(t)
	beta.status = http.StatusInternalServerError

	api := NewAPI(db, map[string]*IndexerClient{"alpha": alphaClient, "beta": betaClient},
		nets, NewAnalyzer(db))
	for i := 0; i < breakerThreshold; i++ {
		api.health.record("beta", errIndexerUnavailable)
	}

	rec, body := serve(t, api, "/api/sanity/overview")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, body)
	}

	var ov SanityOverview
	mustJSON(t, body, &ov)

	for _, n := range nets {
		live, ok := ov.ByNetwork[n.ID]
		if !ok {
			t.Errorf("%s is missing from the liveness report entirely", n.ID)
			continue
		}
		if n.ID != "alpha" && live.Reachable {
			t.Errorf("%s was reported reachable: %+v", n.ID, live)
		}
	}
	if !ov.ByNetwork["alpha"].Reachable {
		t.Error("the healthy chain was reported unreachable")
	}
}
