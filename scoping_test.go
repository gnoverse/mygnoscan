package main

import (
	"fmt"
	"testing"
	"time"
)

// Every aggregate must be scoped to the configured networks and must count
// addresses per chain.
//
// Two distinct defects hid behind the same code shape, `netFilter := ""` when no
// network is selected:
//
//   - An empty filter is not "all networks", it is "every network ever synced".
//     Retired chains keep their rows — topaz has been gone for months and still
//     holds 3,093 transactions in production — and every unfiltered aggregate
//     was counting them.
//   - COUNT(DISTINCT addr) collapses an address seen on two chains into one
//     actor, which undercounts. On production, 63,404 active addresses reported
//     against 63,421 counted honestly.
//
// These queries also ignore their errors: a malformed one leaves the destination
// at zero rather than failing. So this asserts on values, not on error returns —
// a broken query shows up as a zero where a number belongs, which is exactly how
// it would reach a user.
func seedThreeNetworks(t *testing.T, db *DB) {
	t.Helper()

	when := time.Now().UTC().Add(-time.Hour).Format("2006-01-02T15:04:05Z")

	// "shared" acts on both live chains; "solo" on only one. "retired" is a
	// chain that is no longer configured but whose rows remain.
	for _, net := range []string{"live1", "live2", "retired"} {
		for i := 0; i < 3; i++ {
			hash := fmt.Sprintf("%s-tx-%d", net, i)
			if err := db.UpsertTransaction(net, hash, 100+i, when, 1000, 2000, 10, true); err != nil {
				t.Fatalf("UpsertTransaction: %v", err)
			}
			if err := db.InsertCall(net, hash, 100+i, when, "g1shared",
				"gno.land/r/demo/boards", "Post", true); err != nil {
				t.Fatalf("InsertCall: %v", err)
			}
		}
		if err := db.InsertCall(net, net+"-solo", 200, when, "g1"+net, "gno.land/r/demo/boards", "Post", true); err != nil {
			t.Fatalf("InsertCall: %v", err)
		}
		if err := db.UpsertPackage(net, "gno.land/r/"+net+"/pkg", "pkg", "g1shared",
			net+"-deploy", 300, when, true, 1); err != nil {
			t.Fatalf("UpsertPackage: %v", err)
		}
		if err := db.InsertBankSend(net, net+"-send", 400, when, "g1shared", "g1recv", "5ugnot", true); err != nil {
			t.Fatalf("InsertBankSend: %v", err)
		}
	}
}

func newScopedDB(t *testing.T) *DB {
	t.Helper()

	db := newTestDB(t)
	// "retired" is deliberately absent: its rows exist, its configuration does not.
	db.SetConfiguredNetworks([]NetworkConfig{{ID: "live1"}, {ID: "live2"}})
	seedThreeNetworks(t, db)
	return db
}

func TestStatsExcludeRetiredNetworksAndCountCallersPerChain(t *testing.T) {
	db := newScopedDB(t)

	s, err := db.GetStats("")
	if err != nil {
		t.Fatalf("GetStats: %v", err)
	}

	// 4 calls per network on two live networks.
	if s.TotalCalls != 8 {
		t.Errorf("total calls = %d, want 8; retired rows must not be counted", s.TotalCalls)
	}
	// g1shared on both chains plus g1live1 and g1live2 = four actors.
	if s.UniqueCallers != 4 {
		t.Errorf("unique callers = %d, want 4: g1shared counts once per chain", s.UniqueCallers)
	}
}

func TestSanityOverviewIsScopedAndCountsPerChain(t *testing.T) {
	db := newScopedDB(t)

	ov, err := db.GetSanityOverview("")
	if err != nil {
		t.Fatalf("GetSanityOverview: %v", err)
	}

	if ov.TxLast24h != 6 {
		t.Errorf("txs in 24h = %d, want 6 across the two live chains", ov.TxLast24h)
	}
	// Per (address, network): g1shared and the two solo callers on their own
	// chain, plus g1shared as a deployer — all already counted — over 2 chains.
	if ov.ActiveAddresses24h != 4 {
		t.Errorf("active addresses = %d, want 4 counted per chain", ov.ActiveAddresses24h)
	}
	if ov.NewPackages7d != 2 {
		t.Errorf("new packages = %d, want 2; the retired chain's deploy must not count", ov.NewPackages7d)
	}
}

// Each time series, exercised end to end. A query with the wrong argument count
// returns no rows rather than an error, so an empty series is the symptom.
func TestTimeSeriesAreScopedToConfiguredNetworks(t *testing.T) {
	db := newScopedDB(t)

	tests := []struct {
		name string
		run  func() (int, int, error) // points, total counted
	}{
		{
			name: "transactions",
			run: func() (int, int, error) {
				pts, err := db.GetTransactionTimeSeries("", "daily", 7)
				total := 0
				for _, p := range pts {
					total += p.Calls
				}
				return len(pts), total, err
			},
		},
		{
			name: "packages",
			run: func() (int, int, error) {
				pts, err := db.GetPackageTimeSeries("", "daily", 7)
				total := 0
				for _, p := range pts {
					total += p.Total
				}
				return len(pts), total, err
			},
		},
		{
			name: "callers",
			run: func() (int, int, error) {
				pts, err := db.GetCallerTimeSeries("", "daily", 7)
				total := 0
				for _, p := range pts {
					total += p.UniqueCallers
				}
				return len(pts), total, err
			},
		},
		{
			name: "health",
			run: func() (int, int, error) {
				pts, err := db.GetHealthTimeSeries("", "daily", 7)
				total := 0
				for _, p := range pts {
					total += p.Total
				}
				return len(pts), total, err
			},
		},
		{
			name: "active addresses",
			run: func() (int, int, error) {
				pts, err := db.GetActiveAddressTimeSeries("", "daily", 7)
				total := 0
				for _, p := range pts {
					total += p.TotalActive
				}
				return len(pts), total, err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			points, total, err := tt.run()
			if err != nil {
				t.Fatalf("%s series: %v", tt.name, err)
			}
			if points == 0 {
				t.Fatalf("%s series is empty; a query with the wrong argument count "+
					"returns no rows instead of failing", tt.name)
			}
			if total == 0 {
				t.Errorf("%s series has points but counts nothing", tt.name)
			}
		})
	}
}

// The retired chain's rows must be absent from the totals, not merely unlabelled.
func TestTimeSeriesDropRetiredNetworkRows(t *testing.T) {
	db := newScopedDB(t)

	scoped, err := db.GetHealthTimeSeries("", "daily", 7)
	if err != nil {
		t.Fatalf("GetHealthTimeSeries: %v", err)
	}
	scopedTotal := 0
	for _, p := range scoped {
		scopedTotal += p.Total
	}

	// Configuring the retired chain as well must add exactly its rows back,
	// which proves the difference was the filter rather than a coincidence.
	db.SetConfiguredNetworks([]NetworkConfig{{ID: "live1"}, {ID: "live2"}, {ID: "retired"}})
	withRetired, err := db.GetHealthTimeSeries("", "daily", 7)
	if err != nil {
		t.Fatalf("GetHealthTimeSeries: %v", err)
	}
	retiredTotal := 0
	for _, p := range withRetired {
		retiredTotal += p.Total
	}

	if scopedTotal != 6 {
		t.Errorf("scoped total = %d, want 6 from the two live chains", scopedTotal)
	}
	if retiredTotal != 9 {
		t.Errorf("total with the retired chain configured = %d, want 9", retiredTotal)
	}
}

// The per-bucket address counts must agree with the totals computed beside
// them. Counting components one way and the total another produces a page whose
// own numbers contradict each other.
func TestCallerSeriesCountsAddressesPerChain(t *testing.T) {
	db := newScopedDB(t)

	pts, err := db.GetCallerTimeSeries("", "daily", 7)
	if err != nil {
		t.Fatalf("GetCallerTimeSeries: %v", err)
	}
	if len(pts) == 0 {
		t.Fatal("empty series")
	}

	callers, senders := 0, 0
	for _, p := range pts {
		callers += p.UniqueCallers
		senders += p.UniqueSenders
	}

	// g1shared calls on both live chains, plus one solo caller each: four.
	if callers != 4 {
		t.Errorf("unique callers = %d, want 4: g1shared counts once per chain", callers)
	}
	// g1shared sends on both live chains: two.
	if senders != 2 {
		t.Errorf("unique senders = %d, want 2 counted per chain", senders)
	}
}

// The two series that report unique callers must agree. They are computed by
// different functions, and for a while one counted per chain while the other
// blended — production showed 4,492 on one page and 4,490 on the other.
func TestBothCallerSeriesAgree(t *testing.T) {
	db := newScopedDB(t)

	callerSeries, err := db.GetCallerTimeSeries("", "daily", 7)
	if err != nil {
		t.Fatalf("GetCallerTimeSeries: %v", err)
	}
	activeSeries, err := db.GetActiveAddressTimeSeries("", "daily", 7)
	if err != nil {
		t.Fatalf("GetActiveAddressTimeSeries: %v", err)
	}

	sum := func(get func(int) int, n int) int {
		total := 0
		for i := 0; i < n; i++ {
			total += get(i)
		}
		return total
	}
	a := sum(func(i int) int { return callerSeries[i].UniqueCallers }, len(callerSeries))
	b := sum(func(i int) int { return activeSeries[i].UniqueCallers }, len(activeSeries))

	if a != b {
		t.Errorf("unique callers = %d from the caller series but %d from the active-address series", a, b)
	}
	if a == 0 {
		t.Error("both series report zero callers")
	}
}

// The home page and the realms directory must answer the same question with the
// same number.
//
// They did not: the stat tile read 321 realms while the directory header read
// 508, in the same all-networks mode, with nothing on screen saying which was
// authoritative. The gap was exactly topaz's 187 retired realms, which one
// count included and the other did not.
//
// This is the most corrosive class of bug an explorer can have. Once it is
// caught disagreeing with itself on a basic count, every other number it shows
// becomes suspect.
func TestRealmCountsAgreeAcrossEndpoints(t *testing.T) {
	db := newScopedDB(t)

	// Each seeded network has one realm; only two of the three are configured.
	stats, err := db.GetStats("")
	if err != nil {
		t.Fatalf("GetStats: %v", err)
	}
	directory, err := db.CountPackages("", true)
	if err != nil {
		t.Fatalf("CountPackages: %v", err)
	}
	analytics, err := db.GetAnalytics("")
	if err != nil {
		t.Fatalf("GetAnalytics: %v", err)
	}

	if stats.TotalRealms != directory {
		t.Errorf("the home page reports %d realms and the directory reports %d for the same question",
			stats.TotalRealms, directory)
	}
	if analytics.TotalRealms != directory {
		t.Errorf("analytics reports %d realms and the directory reports %d",
			analytics.TotalRealms, directory)
	}
	if directory != 2 {
		t.Errorf("realm count = %d, want 2; the retired chain's realm must not be counted", directory)
	}
}

// The listed rows must match the total above them. A count that excludes a
// retired chain while the list includes it produces a page whose header
// disagrees with the rows under it.
func TestRealmListMatchesItsTotal(t *testing.T) {
	db := newScopedDB(t)

	total, err := db.CountPackages("", true)
	if err != nil {
		t.Fatalf("CountPackages: %v", err)
	}
	rows, err := db.ListPackages("", true, 100, 0, "")
	if err != nil {
		t.Fatalf("ListPackages: %v", err)
	}

	if len(rows) != total {
		t.Errorf("the list returned %d rows under a total of %d", len(rows), total)
	}
	for _, r := range rows {
		if r.Network == "retired" {
			t.Errorf("a retired chain's realm appears in the list: %s", r.Path)
		}
	}
}

// A package that exists only on a retired chain must not resolve. It is history
// from a chain that no longer exists, and rendering it as a live realm invites
// someone to try calling it.
func TestPackageDetailExcludesRetiredNetworks(t *testing.T) {
	db := newScopedDB(t)

	if _, err := db.GetPackageDetail("", "gno.land/r/retired/pkg"); err == nil {
		t.Error("a retired chain's package resolved in all-networks mode")
	}
	if _, err := db.GetPackageDetail("", "gno.land/r/live1/pkg"); err != nil {
		t.Errorf("a live package failed to resolve: %v", err)
	}
}
