package store

import (
	"fmt"
	"testing"
	"time"

	"github.com/moul/mygnoscan/pkg/config"
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
		if err := db.InsertCall("busy", "busy-call-"+itoa(i), 100+i, 0, when, "g1shared", path, "Post", true); err != nil {
			t.Fatalf("InsertCall: %v", err)
		}
	}
	for i := 0; i < 2; i++ {
		if err := db.InsertCall("quiet", "quiet-call-"+itoa(i), 100+i, 0, when, "g1shared", path, "Post", true); err != nil {
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
	db := NewTestDB(t)
	db.SetConfiguredNetworks([]config.NetworkConfig{{ID: "busy"}, {ID: "quiet"}})
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
	db := NewTestDB(t)
	db.SetConfiguredNetworks([]config.NetworkConfig{{ID: "busy"}, {ID: "quiet"}})
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
	db := NewTestDB(t)
	db.SetConfiguredNetworks([]config.NetworkConfig{{ID: "busy"}, {ID: "quiet"}})
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
	db := NewTestDB(t)
	db.SetConfiguredNetworks([]config.NetworkConfig{{ID: "busy"}, {ID: "quiet"}, {ID: "empty"}})
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
func seedCrossChainSends(t *testing.T, db *DB) {
	t.Helper()

	const when = "2026-08-01T00:00:00Z"
	for i := 0; i < 10; i++ {
		if err := db.InsertBankSend("busy", itoa(i)+"-busy", 100+i, when,
			"g1shared", "g1receiver", "1000000ugnot", true); err != nil {
			t.Fatalf("InsertBankSend: %v", err)
		}
	}
	if err := db.InsertBankSend("quiet", "0-quiet", 100, when,
		"g1shared", "g1receiver", "5ugnot", true); err != nil {
		t.Fatalf("InsertBankSend: %v", err)
	}
}

// One chain's ugnot is not another's, so a row that sums them is meaningless.
// Rankings are keyed by (address, network) and each row carries its chain.
func TestBankRankingsAreKeyedByNetwork(t *testing.T) {
	db := NewTestDB(t)
	db.SetConfiguredNetworks([]config.NetworkConfig{{ID: "busy"}, {ID: "quiet"}})
	seedCrossChainSends(t, db)

	s, err := db.GetBankStats("")
	if err != nil {
		t.Fatalf("GetBankStats: %v", err)
	}

	byNetwork := map[string]int64{}
	for _, row := range s.TopSenders {
		if row.Address != "g1shared" {
			continue
		}
		if row.Network == "" {
			t.Fatalf("ranking row has no network: %+v", row)
		}
		byNetwork[row.Network] = row.Total
	}

	if len(byNetwork) != 2 {
		t.Fatalf("g1shared produced %d row(s), want one per chain: %v", len(byNetwork), byNetwork)
	}
	if byNetwork["busy"] != 10_000_000 || byNetwork["quiet"] != 5 {
		t.Errorf("volumes = %v, want busy 10000000 and quiet 5 kept apart", byNetwork)
	}
}

// The same address on two chains is two actors, so it counts twice.
func TestBankCountsAddressesPerNetwork(t *testing.T) {
	db := NewTestDB(t)
	db.SetConfiguredNetworks([]config.NetworkConfig{{ID: "busy"}, {ID: "quiet"}})
	seedCrossChainSends(t, db)

	s, err := db.GetBankStats("")
	if err != nil {
		t.Fatalf("GetBankStats: %v", err)
	}

	// g1shared and g1receiver, on two chains each.
	if s.UniqueSenders != 2 {
		t.Errorf("unique senders = %d, want 2: one address seen on two chains", s.UniqueSenders)
	}
	if s.UniqueReceivers != 2 {
		t.Errorf("unique receivers = %d, want 2", s.UniqueReceivers)
	}
	if s.UniqueAddresses != 4 {
		t.Errorf("unique addresses = %d, want 4: two addresses on two chains each", s.UniqueAddresses)
	}
}

// Selecting one chain scopes the rankings to it and drops the redundant label.
func TestBankStatsScopedToOneNetwork(t *testing.T) {
	db := NewTestDB(t)
	db.SetConfiguredNetworks([]config.NetworkConfig{{ID: "busy"}, {ID: "quiet"}})
	seedCrossChainSends(t, db)

	s, err := db.GetBankStats("quiet")
	if err != nil {
		t.Fatalf("GetBankStats: %v", err)
	}

	if s.TotalVolume != 5 {
		t.Errorf("total volume = %d, want quiet's 5 with busy's 10000000 excluded", s.TotalVolume)
	}
	for _, row := range s.TopSenders {
		if row.Network != "quiet" {
			t.Errorf("row from %q leaked into the quiet view: %+v", row.Network, row)
		}
	}
	if s.ByNetwork != nil {
		t.Errorf("by_network was sent for a single network: %+v", s.ByNetwork)
	}
}

// Namespace ownership is the one address name that can be proved from the data:
// the sole deployer of gno.land/r/gnoswap/* is gnoswap.
//
// The rule has to refuse more than it accepts. Seven namespaces on the live
// chains have several deployers, and picking one of them would present a guess
// as a fact — which is precisely the class of thing that costs an explorer its
// credibility.
func TestDerivedAddressLabels(t *testing.T) {
	db := NewTestDB(t)
	db.SetConfiguredNetworks([]config.NetworkConfig{{ID: "live"}, {ID: "retired"}})

	const when = "2026-08-01T00:00:00Z"
	deploy := func(net, path, creator string) {
		t.Helper()
		if err := db.UpsertPackage(net, path, "pkg", creator, net+path, 100, when, true, 1); err != nil {
			t.Fatalf("UpsertPackage: %v", err)
		}
	}

	// Sole deployer of a namespace, well past the minimum.
	for i := 0; i < 5; i++ {
		deploy("live", fmt.Sprintf("gno.land/r/gnoswap/pkg%d", i), "g1gnoswap")
	}
	// Two deployers under one namespace: nobody gets named.
	for i := 0; i < 4; i++ {
		deploy("live", fmt.Sprintf("gno.land/r/shared/a%d", i), "g1first")
		deploy("live", fmt.Sprintf("gno.land/r/shared/b%d", i), "g1second")
	}
	// Only two packages: below the minimum evidence.
	deploy("live", "gno.land/r/tiny/one", "g1tiny")
	deploy("live", "gno.land/r/tiny/two", "g1tiny")
	// A namespace that is itself an address carries no name.
	for i := 0; i < 4; i++ {
		deploy("live", fmt.Sprintf("gno.land/r/g1selfns/pkg%d", i), "g1selfns")
	}
	// Publishes widely, so no single namespace identifies it.
	for i := 0; i < 4; i++ {
		deploy("live", fmt.Sprintf("gno.land/r/spread%d/pkg", i), "g1spread")
	}
	// A retired chain must not contribute a name.
	for i := 0; i < 5; i++ {
		deploy("retired", fmt.Sprintf("gno.land/r/ghost/pkg%d", i), "g1ghost")
	}
	db.SetConfiguredNetworks([]config.NetworkConfig{{ID: "live"}})

	labels, err := db.DerivedAddressLabels("")
	if err != nil {
		t.Fatalf("DerivedAddressLabels: %v", err)
	}

	if got := labels["g1gnoswap"].Label; got != "@gnoswap" {
		t.Errorf("sole deployer label = %q, want @gnoswap", got)
	}
	if labels["g1gnoswap"].Why == "" {
		t.Error("a label with no stated evidence cannot be checked by a reader")
	}

	for addr, reason := range map[string]string{
		"g1first":  "shares its namespace with another deployer",
		"g1second": "shares its namespace with another deployer",
		"g1tiny":   "has too few packages to establish ownership",
		"g1selfns": "its namespace is an address, which carries no name",
		"g1spread": "publishes across namespaces, so none identifies it",
		"g1ghost":  "deployed only on a retired network",
	} {
		if l, ok := labels[addr]; ok {
			t.Errorf("%s was labelled %q but %s", addr, l.Label, reason)
		}
	}
}

// The gas page reads precomputed rollups.
//
// Its two aggregates scale with the chain and had reached 14s on sapphire,
// against a 30s server write timeout — the trajectory that broke the address
// page twice. Neither can be indexed away: attributing gas per realm means
// touching every call, and the totals sum every transaction.
func TestGasRollups(t *testing.T) {
	db := NewTestDB(t)
	db.SetConfiguredNetworks([]config.NetworkConfig{{ID: "live"}, {ID: "other"}})

	const when = "2026-08-01T00:00:00Z"
	seed := func(net, hash, path string, gas, fee int) {
		t.Helper()
		if err := db.UpsertTransaction(net, hash, 100, when, gas, gas*2, fee, true); err != nil {
			t.Fatalf("UpsertTransaction: %v", err)
		}
		if err := db.InsertCall(net, hash, 100, 0, when, "g1caller", path, "Post", true); err != nil {
			t.Fatalf("InsertCall: %v", err)
		}
	}
	seed("live", "a", "gno.land/r/demo/boards", 100, 10)
	seed("live", "b", "gno.land/r/demo/boards", 200, 20)
	seed("live", "c", "gno.land/r/demo/users", 50, 5)
	// The same path on another chain must stay a separate row in the rollup.
	seed("other", "d", "gno.land/r/demo/boards", 999, 99)

	t.Run("before any refresh it computes live rather than showing zeros", func(t *testing.T) {
		// Zeros would read as "this chain has used no gas".
		stats, err := db.GetGasStats("live", 10)
		if err != nil {
			t.Fatalf("GetGasStats: %v", err)
		}
		if stats.TotalGasUsed != 350 {
			t.Errorf("gas used = %d, want 350 computed live", stats.TotalGasUsed)
		}
		if stats.ComputedAt != "" {
			t.Errorf("computed_at = %q, want empty when computed live", stats.ComputedAt)
		}
	})

	if err := db.RefreshRollups(); err != nil {
		t.Fatalf("RefreshRollups: %v", err)
	}

	t.Run("the rollup gives the same answer", func(t *testing.T) {
		stats, err := db.GetGasStats("live", 10)
		if err != nil {
			t.Fatalf("GetGasStats: %v", err)
		}
		if stats.TotalGasUsed != 350 || stats.TotalTxs != 3 {
			t.Errorf("gas=%d txs=%d, want 350 and 3", stats.TotalGasUsed, stats.TotalTxs)
		}
		if stats.ComputedAt == "" {
			t.Error("no computed_at, so the page cannot say how fresh these are")
		}
		if len(stats.TopRealms) == 0 || stats.TopRealms[0].Path != "gno.land/r/demo/boards" {
			t.Errorf("top realm = %+v, want boards with 300 gas", stats.TopRealms)
		}
		if stats.TopRealms[0].Gas != 300 {
			t.Errorf("boards gas = %d, want 300", stats.TopRealms[0].Gas)
		}
	})

	t.Run("another chain's gas stays out", func(t *testing.T) {
		stats, err := db.GetGasStats("live", 10)
		if err != nil {
			t.Fatalf("GetGasStats: %v", err)
		}
		if stats.TotalGasUsed != 350 {
			t.Errorf("gas used = %d — the other chain's 999 leaked in", stats.TotalGasUsed)
		}
	})

	t.Run("all networks re-groups a shared path across chains", func(t *testing.T) {
		// The rollup stores (network, path); a path on two chains is two rows
		// there and must be summed on read when both are in scope.
		stats, err := db.GetGasStats("", 10)
		if err != nil {
			t.Fatalf("GetGasStats: %v", err)
		}
		if stats.TotalGasUsed != 1349 {
			t.Errorf("gas used = %d, want 1349 across both chains", stats.TotalGasUsed)
		}
		var boards int
		for _, r := range stats.TopRealms {
			if r.Path == "gno.land/r/demo/boards" {
				boards = r.Gas
			}
		}
		if boards != 1299 {
			t.Errorf("boards gas = %d, want 1299 (300 + 999) summed across chains", boards)
		}
	})

	t.Run("a refresh replaces rather than accumulates", func(t *testing.T) {
		// Whole-table replacement, so running it twice must not double anything.
		if err := db.RefreshRollups(); err != nil {
			t.Fatalf("RefreshRollups: %v", err)
		}
		stats, err := db.GetGasStats("live", 10)
		if err != nil {
			t.Fatalf("GetGasStats: %v", err)
		}
		if stats.TotalGasUsed != 350 {
			t.Errorf("gas used = %d after a second refresh, want 350", stats.TotalGasUsed)
		}
	})
}

// The bank rollup must give the same answers as computing live — including the
// per-chain keying, which is the part most easily lost when precomputing.
func TestBankRollupMatchesLiveComputation(t *testing.T) {
	db := NewTestDB(t)
	db.SetConfiguredNetworks([]config.NetworkConfig{{ID: "busy"}, {ID: "quiet"}})
	seedCrossChainSends(t, db)

	// Extra shape: an address that both sends and receives, so the distinct
	// address count cannot be a simple sum of senders and receivers.
	if err := db.InsertBankSend("busy", "back", 200, "2026-08-01T00:00:00Z",
		"g1receiver", "g1shared", "7ugnot", true); err != nil {
		t.Fatalf("InsertBankSend: %v", err)
	}

	live, err := db.GetBankStats("")
	if err != nil {
		t.Fatalf("GetBankStats live: %v", err)
	}
	if live.ComputedAt != "" {
		t.Error("computed_at set before any refresh")
	}

	if err := db.RefreshRollups(); err != nil {
		t.Fatalf("RefreshRollups: %v", err)
	}

	rolled, err := db.GetBankStats("")
	if err != nil {
		t.Fatalf("GetBankStats rolled: %v", err)
	}
	if rolled.ComputedAt == "" {
		t.Error("no computed_at, so the page cannot say how fresh these are")
	}

	for _, c := range []struct {
		name         string
		live, rolled int
	}{
		{"total sends", live.TotalSends, rolled.TotalSends},
		{"unique senders", live.UniqueSenders, rolled.UniqueSenders},
		{"unique receivers", live.UniqueReceivers, rolled.UniqueReceivers},
		{"unique addresses", live.UniqueAddresses, rolled.UniqueAddresses},
	} {
		if c.live != c.rolled {
			t.Errorf("%s: live=%d rolled=%d", c.name, c.live, c.rolled)
		}
	}
	if live.TotalVolume != rolled.TotalVolume {
		t.Errorf("volume: live=%d rolled=%d", live.TotalVolume, rolled.TotalVolume)
	}

	t.Run("the per-chain split survives", func(t *testing.T) {
		if len(rolled.ByNetwork) != len(live.ByNetwork) {
			t.Fatalf("by_network has %d entries rolled vs %d live", len(rolled.ByNetwork), len(live.ByNetwork))
		}
		for net, slice := range live.ByNetwork {
			if rolled.ByNetwork[net] != slice {
				t.Errorf("%s: live=%+v rolled=%+v", net, slice, rolled.ByNetwork[net])
			}
		}
	})

	t.Run("rankings stay keyed by network", func(t *testing.T) {
		// One chain's ugnot is not another's, so a row summing them would be
		// meaningless — the defect fixed in #111 and easy to reintroduce here.
		for _, r := range rolled.TopSenders {
			if r.Network == "" {
				t.Errorf("ranking row has no network: %+v", r)
			}
		}
		byNet := map[string]int64{}
		for _, r := range rolled.TopSenders {
			if r.Address == "g1shared" {
				byNet[r.Network] = r.Total
			}
		}
		if len(byNet) != 2 {
			t.Errorf("g1shared produced %d rows, want one per chain: %v", len(byNet), byNet)
		}
	})

	t.Run("a single network scopes and drops the split", func(t *testing.T) {
		one, err := db.GetBankStats("quiet")
		if err != nil {
			t.Fatalf("GetBankStats(quiet): %v", err)
		}
		if one.TotalVolume != 5 {
			t.Errorf("quiet volume = %d, want 5", one.TotalVolume)
		}
		if one.ByNetwork != nil {
			t.Errorf("by_network sent for a single network: %+v", one.ByNetwork)
		}
	})
}

// A leaderboard bounded by the wrong key silently loses its top entry.
//
// The rollup first stored the top receivers by *count* and re-sorted those by
// volume, so an address with one enormous transfer never reached the volume
// ranking. Every total still matched, which is exactly how this class of bug
// hides — it was caught by diffing against live computation on production.
//
// The fixture has to exceed the rollup's own LIMIT to exercise it. My first
// version used six receivers, so nothing was ever truncated and the test passed
// against the bug.
func TestBankRollupKeepsEachRankingsOwnTop(t *testing.T) {
	db := NewTestDB(t)
	db.SetConfiguredNetworks([]config.NetworkConfig{{ID: "n"}})

	const when = "2026-08-01T00:00:00Z"
	// More frequent receivers than the rollup keeps, each with several receipts,
	// so a single-receipt address cannot be in the count-ordered top slice.
	const frequent = bankTopRollupLimit + 50
	for who := 0; who < frequent; who++ {
		for i := 0; i < 3; i++ {
			if err := db.InsertBankSend("n", fmt.Sprintf("s-%d-%d", who, i), 100+i, when,
				"g1sender", fmt.Sprintf("g1frequent%05d", who), "1ugnot", true); err != nil {
				t.Fatalf("InsertBankSend: %v", err)
			}
		}
	}
	// One receipt, enormous: dominates by volume, invisible by count.
	if err := db.InsertBankSend("n", "whale", 500, when, "g1sender", "g1whale", "999999999ugnot", true); err != nil {
		t.Fatalf("InsertBankSend: %v", err)
	}

	if err := db.RefreshRollups(); err != nil {
		t.Fatalf("RefreshRollups: %v", err)
	}
	s, err := db.GetBankStats("n")
	if err != nil {
		t.Fatalf("GetBankStats: %v", err)
	}

	if len(s.TopReceiversVol) == 0 || s.TopReceiversVol[0].Address != "g1whale" {
		got := "none"
		if len(s.TopReceiversVol) > 0 {
			got = s.TopReceiversVol[0].Address
		}
		t.Errorf("top receiver by volume = %s, want g1whale — a one-off large transfer "+
			"must not be lost to a count-ordered truncation", got)
	}
	if len(s.TopReceiversCnt) == 0 || s.TopReceiversCnt[0].Count != 3 {
		t.Errorf("top receiver by count = %+v, want one of the frequent receivers", s.TopReceiversCnt)
	}
}

// Deduplicating twice costs twice and buys nothing.
//
// Five queries wrapped an inner UNION in an outer SELECT DISTINCT over the same
// key — or, for the time series, a coarser one. The inner dedup could only
// remove rows the outer one would remove anyway. Measured on production: the
// active-address series went 4.81s to 2.58s with byte-identical output.
//
// This pins the *results*, since that is what switching to UNION ALL could have
// changed. Duplicate rows are seeded deliberately so a missing outer DISTINCT
// would show up as inflated counts.
func TestDedupOnceGivesTheSameCounts(t *testing.T) {
	db := NewTestDB(t)
	db.SetConfiguredNetworks([]config.NetworkConfig{{ID: "a"}, {ID: "b"}})

	when := time.Now().UTC().Add(-2 * time.Hour).Format("2006-01-02T15:04:05Z")

	// One address reachable through every table, on both chains, several times —
	// so every branch of every union produces rows that overlap the others.
	for _, net := range []string{"a", "b"} {
		for i := 0; i < 3; i++ {
			if err := db.InsertCall(net, fmt.Sprintf("c-%s-%d", net, i), 100+i, 0, when,
				"g1everywhere", "gno.land/r/demo/boards", "Post", true); err != nil {
				t.Fatalf("InsertCall: %v", err)
			}
			if err := db.InsertBankSend(net, fmt.Sprintf("s-%s-%d", net, i), 200+i, when,
				"g1everywhere", "g1everywhere", "1ugnot", true); err != nil {
				t.Fatalf("InsertBankSend: %v", err)
			}
		}
		if err := db.UpsertPackage(net, "gno.land/r/"+net+"/pkg", "pkg", "g1everywhere",
			net+"-deploy", 300, when, true, 1); err != nil {
			t.Fatalf("UpsertPackage: %v", err)
		}
		if err := db.InsertMsgRun(net, net+"-run", 400, when, "g1everywhere", "package main", true); err != nil {
			t.Fatalf("InsertMsgRun: %v", err)
		}
	}

	t.Run("analytics counts the address once per chain", func(t *testing.T) {
		a, err := db.GetAnalytics("")
		if err != nil {
			t.Fatalf("GetAnalytics: %v", err)
		}
		// g1everywhere on two chains = 2. It appears in four tables and as both
		// sender and receiver; none of that should multiply it.
		if a.TotalAddresses != 2 {
			t.Errorf("total addresses = %d, want 2 — the dedup collapsed to the wrong key", a.TotalAddresses)
		}
	})

	t.Run("sanity counts the address once per chain", func(t *testing.T) {
		ov, err := db.GetSanityOverview("")
		if err != nil {
			t.Fatalf("GetSanityOverview: %v", err)
		}
		if ov.ActiveAddresses24h != 2 {
			t.Errorf("active addresses = %d, want 2", ov.ActiveAddresses24h)
		}
	})

	t.Run("the time series counts it once per bucket per chain", func(t *testing.T) {
		pts, err := db.GetActiveAddressTimeSeries("", "daily", 7)
		if err != nil {
			t.Fatalf("GetActiveAddressTimeSeries: %v", err)
		}
		total := 0
		for _, p := range pts {
			total += p.TotalActive
		}
		// One day of activity, one address, two chains.
		if total != 2 {
			t.Errorf("total active across buckets = %d, want 2", total)
		}
	})

	t.Run("bank stats count it once per chain, live and rolled up", func(t *testing.T) {
		live, err := db.GetBankStats("")
		if err != nil {
			t.Fatalf("GetBankStats: %v", err)
		}
		if err := db.RefreshRollups(); err != nil {
			t.Fatalf("RefreshRollups: %v", err)
		}
		rolled, err := db.GetBankStats("")
		if err != nil {
			t.Fatalf("GetBankStats rolled: %v", err)
		}
		if live.UniqueAddresses != 2 {
			t.Errorf("live unique addresses = %d, want 2", live.UniqueAddresses)
		}
		if rolled.UniqueAddresses != live.UniqueAddresses {
			t.Errorf("rolled=%d live=%d", rolled.UniqueAddresses, live.UniqueAddresses)
		}
	})
}

// Where activity is concentrating, and whether that is changing.
//
// Every existing rollup is an all-time snapshot — "who has spent the most
// ever" — so a realm that dominated months ago and has since gone quiet still
// tops every table on the site. This is the series that can tell them apart.
func TestRealmShareTimeSeries(t *testing.T) {
	db := NewTestDB(t)
	db.SetConfiguredNetworks([]config.NetworkConfig{{ID: "alpha"}})

	now := time.Now().UTC()
	day := func(n int) string { return now.AddDate(0, 0, -n).Format(time.RFC3339) }

	// "faded" was busy three days ago and has stopped; "rising" is busy today.
	// An all-time total cannot distinguish them; a bucketed series must.
	seed := func(hash string, height int, when, realm string, fee int) {
		t.Helper()
		if err := db.UpsertTransaction("alpha", hash, height, when, 100, 200, fee, true); err != nil {
			t.Fatalf("UpsertTransaction: %v", err)
		}
		if err := db.InsertCall("alpha", hash, height, 0, when, "g1caller", realm, "Post", true); err != nil {
			t.Fatalf("InsertCall: %v", err)
		}
	}
	seed("tx1", 101, day(3), "gno.land/r/a/faded", 500)
	seed("tx2", 102, day(3), "gno.land/r/a/faded", 500)
	seed("tx3", 103, day(1), "gno.land/r/a/rising", 300)

	pts, err := db.GetRealmShareTimeSeries("alpha", "fee", "daily", 7)
	if err != nil {
		t.Fatalf("GetRealmShareTimeSeries: %v", err)
	}
	if len(pts) == 0 {
		t.Fatal("empty series")
	}

	byRealm := map[string]int{}
	buckets := map[string]bool{}
	for _, p := range pts {
		byRealm[p.Path] += p.Value
		buckets[p.Bucket] = true
	}
	if byRealm["gno.land/r/a/faded"] != 1000 {
		t.Errorf("faded total = %d, want 1000", byRealm["gno.land/r/a/faded"])
	}
	if byRealm["gno.land/r/a/rising"] != 300 {
		t.Errorf("rising total = %d, want 300", byRealm["gno.land/r/a/rising"])
	}
	// The point of the series: the two realms are in different buckets. A
	// single-bucket result would be a snapshot wearing a chart's clothes.
	if len(buckets) < 2 {
		t.Errorf("series has %d bucket(s); the two realms were active on different days", len(buckets))
	}
}

// A transaction touching one realm several times is charged its fee once.
//
// Same reason gas_realm_rollup uses DISTINCT: a multicall carries one fee, and
// counting it per message would inflate exactly the busiest realms.
func TestRealmShareChargesAMulticallFeeOnce(t *testing.T) {
	db := NewTestDB(t)
	db.SetConfiguredNetworks([]config.NetworkConfig{{ID: "alpha"}})

	when := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339)
	if err := db.UpsertTransaction("alpha", "multi", 100, when, 100, 200, 900, true); err != nil {
		t.Fatalf("UpsertTransaction: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := db.InsertCall("alpha", "multi", 100, i, when, "g1caller", "gno.land/r/a/busy", "Post", true); err != nil {
			t.Fatalf("InsertCall: %v", err)
		}
	}

	pts, err := db.GetRealmShareTimeSeries("alpha", "fee", "daily", 7)
	if err != nil {
		t.Fatalf("GetRealmShareTimeSeries: %v", err)
	}
	total := 0
	for _, p := range pts {
		total += p.Value
	}
	if total != 900 {
		t.Errorf("total fee = %d, want 900; a three-message multicall still carries one fee", total)
	}
}

// Storage share is signed: a bucket in which a realm freed more than it wrote
// is negative, and clamping that to zero would quietly overstate what storage
// costs.
func TestRealmShareStorageIsSigned(t *testing.T) {
	db := NewTestDB(t)
	db.SetConfiguredNetworks([]config.NetworkConfig{{ID: "alpha"}})

	when := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339)
	if err := db.InsertStorageEvent("alpha", "txA", 0, "gno.land/r/a/x", 100, when, "deposit", 100, 400); err != nil {
		t.Fatalf("InsertStorageEvent: %v", err)
	}
	if err := db.InsertStorageEvent("alpha", "txB", 0, "gno.land/r/a/x", 101, when, "unlock", -300, -900); err != nil {
		t.Fatalf("InsertStorageEvent: %v", err)
	}

	pts, err := db.GetRealmShareTimeSeries("alpha", "storage", "daily", 7)
	if err != nil {
		t.Fatalf("GetRealmShareTimeSeries: %v", err)
	}
	total := 0
	for _, p := range pts {
		total += p.Value
	}
	if total != -500 {
		t.Errorf("storage total = %d, want -500: the realm was refunded more than it paid", total)
	}
}

func TestRealmShareRejectsAnUnknownMetric(t *testing.T) {
	db := NewTestDB(t)
	db.SetConfiguredNetworks([]config.NetworkConfig{{ID: "alpha"}})

	if _, err := db.GetRealmShareTimeSeries("alpha", "bananas", "daily", 7); err == nil {
		t.Error("an unknown metric was accepted; it would silently return an empty chart")
	}
}

// The rollup is bounded per network, not across all of them.
//
// The INSERT used one global LIMIT while its comment claimed "bounded per
// network", and the two only agreed while every network was small. A busy chain
// with more high-count addresses than the whole limit fills the rollup on its
// own, and the quiet chain beside it ends up with a leaderboard of nothing —
// which reads as "nobody moves the coin here", not as "this is truncated".
//
// The quiet network's counts are deliberately lower than every one of the busy
// network's, so a global ORDER BY puts all of them past the cut.
func TestBankRollupBoundsEachNetworkSeparately(t *testing.T) {
	db := NewTestDB(t)
	db.SetConfiguredNetworks([]config.NetworkConfig{{ID: "busy"}, {ID: "quiet"}})

	const when = "2026-08-01T00:00:00Z"
	send := func(net, addr string, n, seq int) {
		t.Helper()
		for i := 0; i < n; i++ {
			id := fmt.Sprintf("%s-%s-%d", net, addr, i)
			if err := db.InsertBankSend(net, id, seq*100+i, when, addr, "g1recv", "1ugnot", true); err != nil {
				t.Fatalf("InsertBankSend: %v", err)
			}
		}
	}
	for who := 0; who < bankTopRollupLimit+10; who++ {
		send("busy", fmt.Sprintf("g1busy%05d", who), 2, who)
	}
	for who := 0; who < bankTopRead+10; who++ {
		send("quiet", fmt.Sprintf("g1quiet%05d", who), 1, who)
	}

	if err := db.RefreshRollups(); err != nil {
		t.Fatalf("RefreshRollups: %v", err)
	}

	for _, tc := range []struct{ network string }{{"busy"}, {"quiet"}} {
		s, err := db.GetBankStats(tc.network)
		if err != nil {
			t.Fatalf("GetBankStats(%s): %v", tc.network, err)
		}
		if len(s.TopSenders) != bankTopRead {
			t.Errorf("%s: top senders = %d rows, want %d — each network gets its own slice of the rollup",
				tc.network, len(s.TopSenders), bankTopRead)
		}
		for _, row := range s.TopSenders {
			if row.Network != tc.network {
				t.Errorf("%s: leaked a %s row into the leaderboard", tc.network, row.Network)
			}
		}
	}
}

// The three /coins leaderboards are bankTopRead long whether they come from the
// rollup or from the live fallback a fresh database uses, so the page does not
// silently shrink to ten rows for the first five minutes after a restart.
func TestBankStatsLiveFallbackIsTheSameLength(t *testing.T) {
	db := NewTestDB(t)
	db.SetConfiguredNetworks([]config.NetworkConfig{{ID: "n"}})

	const when = "2026-08-01T00:00:00Z"
	for who := 0; who < bankTopRead+10; who++ {
		for i := 0; i < 2; i++ {
			id := fmt.Sprintf("s-%d-%d", who, i)
			if err := db.InsertBankSend("n", id, who*10+i, when,
				fmt.Sprintf("g1sender%05d", who), fmt.Sprintf("g1recv%05d", who), "1ugnot", true); err != nil {
				t.Fatalf("InsertBankSend: %v", err)
			}
		}
	}

	// No RefreshRollups: this is the path a fresh database takes.
	s, err := db.GetBankStats("n")
	if err != nil {
		t.Fatalf("GetBankStats: %v", err)
	}
	for _, tc := range []struct {
		name string
		rows []AddrStat
	}{
		{"top_senders", s.TopSenders},
		{"top_receivers_volume", s.TopReceiversVol},
		{"top_receivers_count", s.TopReceiversCnt},
	} {
		if len(tc.rows) != bankTopRead {
			t.Errorf("%s = %d rows, want %d", tc.name, len(tc.rows), bankTopRead)
		}
	}
}

// RecentRealms answered `null` on every network of every deployment, because
// its query was the one place pFilter (`AND p.network = ...`) was pasted onto
// an unaliased `FROM packages`. SQLite rejected it with `no such column:
// p.network`, the error went into a blank, and the handler reported an empty
// list rather than a failure. Table-driven over the scopes, because the bug was
// invisible in all three.
func TestAnalyticsRecentRealms(t *testing.T) {
	db := NewTestDB(t)
	db.SetConfiguredNetworks([]config.NetworkConfig{{ID: "busy"}, {ID: "quiet"}, {ID: "empty"}})
	seedSharedRealm(t, db)

	for _, tc := range []struct {
		name    string
		network string
		want    int
	}{
		{"one chain", "busy", 1},
		{"the other chain", "quiet", 1},
		{"all chains", "", 2},
		{"a chain with nothing on it", "empty", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, err := db.GetAnalytics(tc.network)
			if err != nil {
				t.Fatalf("GetAnalytics(%q): %v", tc.network, err)
			}
			if len(a.RecentRealms) != tc.want {
				t.Fatalf("recent realms = %d, want %d: %+v", len(a.RecentRealms), tc.want, a.RecentRealms)
			}
			for _, r := range a.RecentRealms {
				if tc.network != "" && r.Network != tc.network {
					t.Errorf("%s is from %q, which is not the selected chain", r.Path, r.Network)
				}
				// The time is what dates the height in the UI. A row that
				// carries a height and no time renders an undated block.
				if r.BlockHeight == 0 || r.BlockTime == "" {
					t.Errorf("%s has height %d and time %q; both are needed to draw a dated block",
						r.Path, r.BlockHeight, r.BlockTime)
				}
			}
		})
	}
}
