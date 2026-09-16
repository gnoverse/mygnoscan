package httpapi

import (
	"net/http"
	"testing"

	"github.com/moul/mygnoscan/pkg/analyzer"
	"github.com/moul/mygnoscan/pkg/config"
	"github.com/moul/mygnoscan/pkg/indexer"
	"github.com/moul/mygnoscan/pkg/store"
)

// Liveness cannot be merged at all — not even wrongly, the way a denominated
// amount can be summed. There is no such thing as the height, or the last block
// time, of four chains at once. The page used to answer with an arbitrary entry
// of a Go map and present it under a global heading.
func TestSanityReportsLivenessPerNetwork(t *testing.T) {
	api, alpha, beta := newIndexerAPI(t)
	alpha.SetLatestHeight(3_100_024)
	beta.SetLatestHeight(400_024)

	t.Run("all networks reports every chain and no global height", func(t *testing.T) {
		rec, body := serve(t, api, "/api/sanity/overview")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, body)
		}

		var ov store.SanityOverview
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

		var ov store.SanityOverview
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

// An unreachable chain must be distinguishable from one sitting at genesis;
// both are height 0 in the fields that matter.
func TestSanityMarksAnUnreachableChain(t *testing.T) {
	api, alpha, _ := newIndexerAPI(t)
	alpha.Status = http.StatusInternalServerError

	rec, body := serve(t, api, "/api/sanity/overview")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, body)
	}

	var ov store.SanityOverview
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

// The sanity page must not silently omit a chain. fanOut skips networks with no
// client or an open breaker, but this is the page whose whole job is to report
// liveness — a chain missing from it is the one a reader most needs to see.
func TestSanityListsEveryConfiguredNetwork(t *testing.T) {
	db := store.NewTestDB(t)
	nets := []config.NetworkConfig{{ID: "alpha"}, {ID: "beta"}, {ID: "noclient"}}
	db.SetConfiguredNetworks(nets)

	alpha, alphaClient := indexer.NewFake(t)
	alpha.SeedChain(100, 5)

	// beta's breaker is already open, as it would be after a real outage.
	beta, betaClient := indexer.NewFake(t)
	beta.Status = http.StatusInternalServerError

	api := NewAPI(db, map[string]*indexer.Client{"alpha": alphaClient, "beta": betaClient},
		nets, analyzer.NewAnalyzer(db))
	for i := 0; i < breakerThreshold; i++ {
		api.health.record("beta", indexer.ErrUnavailable)
	}

	rec, body := serve(t, api, "/api/sanity/overview")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, body)
	}

	var ov store.SanityOverview
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

// seedCrossChainSends puts the same address on two chains with very different
// volumes, which is the shape that produced a leaderboard row summing two
// chains' ugnot into a figure describing nothing.
