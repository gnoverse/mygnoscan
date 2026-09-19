package store

import (
	"testing"
	"time"
)

// The edge rollups are incremental, which is the whole risk: a pass folds rows
// above its own cursor and *adds* to the running totals, so a cursor that does
// not advance turns every re-run into a double count.
func TestEdgeRollupsAreIncrementalAndPerChain(t *testing.T) {
	db := NewTestDB(t)

	when := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)

	send := func(network, hash string, height int, from, to, amount string) {
		t.Helper()
		if err := db.InsertBankSend(network, hash, height, when, from, to, amount, true); err != nil {
			t.Fatalf("InsertBankSend: %v", err)
		}
	}
	call := func(network, hash string, height int, caller, pkg string) {
		t.Helper()
		if err := db.InsertCall(network, hash, height, 0, when, caller, pkg, "Post", true); err != nil {
			t.Fatalf("InsertCall: %v", err)
		}
	}

	// Same address pair on two chains: two distinct actors, two distinct edges.
	send("live", "l-1", 10, "g1a", "g1b", "100ugnot")
	send("live", "l-2", 11, "g1a", "g1b", "50ugnot")
	send("other", "o-1", 10, "g1a", "g1b", "900ugnot")
	call("live", "l-1", 10, "g1a", "gno.land/r/demo/boards")
	call("other", "o-1", 10, "g1a", "gno.land/r/demo/boards")

	roll := func(network string) {
		t.Helper()
		last, _, err := db.TransferEdgesLastHeight(network)
		if err != nil {
			t.Fatalf("TransferEdgesLastHeight: %v", err)
		}
		rows, err := db.RollupBankSendsSince(network, last)
		if err != nil {
			t.Fatalf("RollupBankSendsSince: %v", err)
		}
		if err := db.UpsertTransferEdges(network, rows); err != nil {
			t.Fatalf("UpsertTransferEdges: %v", err)
		}
		cLast, _, err := db.CallerEdgesLastHeight(network)
		if err != nil {
			t.Fatalf("CallerEdgesLastHeight: %v", err)
		}
		cRows, err := db.RollupCallsSince(network, cLast)
		if err != nil {
			t.Fatalf("RollupCallsSince: %v", err)
		}
		if err := db.UpsertCallerEdges(network, cRows); err != nil {
			t.Fatalf("UpsertCallerEdges: %v", err)
		}
	}
	roll("live")
	roll("other")

	edgeValue := func(network string) int64 {
		t.Helper()
		g, err := db.GetTransferGraph(network, 7, 0, 0, "")
		if err != nil {
			t.Fatalf("GetTransferGraph: %v", err)
		}
		var total int64
		for _, e := range g.Edges {
			total += e.Value
		}
		return total
	}

	if got := edgeValue("live"); got != 150 {
		t.Errorf("live transfer volume = %d, want 150", got)
	}
	if got := edgeValue("other"); got != 900 {
		t.Errorf("other transfer volume = %d, want 900; the two chains share an address pair and must not merge", got)
	}

	// Re-running with no new rows must change nothing.
	roll("live")
	if got := edgeValue("live"); got != 150 {
		t.Errorf("re-running the rollup double-counted: volume = %d, want 150", got)
	}

	// A new row above the cursor is folded in, and only it.
	send("live", "l-3", 12, "g1a", "g1b", "7ugnot")
	roll("live")
	if got := edgeValue("live"); got != 157 {
		t.Errorf("volume after one new send = %d, want 157", got)
	}
	// And again with nothing new. This is the pass that catches a cursor which
	// fails to advance on the ON CONFLICT path: the first re-run above inserts
	// a fresh row, so only a re-run *after* an incremental update re-reads the
	// whole history and doubles the totals.
	roll("live")
	if got := edgeValue("live"); got != 157 {
		t.Errorf("rollup double-counted after an incremental update: volume = %d, want 157", got)
	}

	// Node volumes, not just edges: the node query ranks addresses by summed
	// volume in its own statement, so an unscoped one blends the chains even
	// while the edge query stays correct.
	g, err := db.GetTransferGraph("live", 7, 0, 0, "")
	if err != nil {
		t.Fatalf("GetTransferGraph: %v", err)
	}
	vol := map[string]int64{}
	for _, n := range g.Nodes {
		vol[n.ID] = n.Volume
	}
	if vol["g1a"] != 157 {
		t.Errorf("g1a node volume = %d, want 157; the other chain's 900 must not be counted", vol["g1a"])
	}

	cg, err := db.GetCallerGraph("live", 7, 0, 0)
	if err != nil {
		t.Fatalf("GetCallerGraph: %v", err)
	}
	if len(cg.Edges) != 1 || cg.Edges[0].Calls != 1 {
		t.Errorf("caller edges = %+v, want one edge with 1 call on live only", cg.Edges)
	}
	var realms, callers int
	for _, n := range cg.Nodes {
		switch n.Type {
		case "realm":
			realms++
		case "caller":
			callers++
		}
	}
	if realms != 1 || callers != 1 {
		t.Errorf("caller graph has %d callers and %d realms, want 1 and 1", callers, realms)
	}

}
