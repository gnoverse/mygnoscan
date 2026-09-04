package main

import (
	"fmt"
	"reflect"
	"testing"
	"time"
)

// seedActiveAt puts one address into all three activity tables at one instant.
//
// The three tables are what "active address" means: a caller, a deployer, a
// sender. Seeding through the public inserters rather than raw SQL keeps the
// test honest about the shape the syncer actually writes.
func seedActiveAt(t *testing.T, db *DB, network, addr string, when time.Time, id string) {
	t.Helper()
	ts := when.UTC().Format("2006-01-02T15:04:05Z")
	if err := db.InsertCall(network, "call-"+id, 1, ts, addr, "gno.land/r/demo/boards", "Post", true); err != nil {
		t.Fatalf("InsertCall: %v", err)
	}
	if err := db.UpsertPackage(network, "gno.land/r/"+network+"/"+id, id, addr, "pkg-"+id, 2, ts, true, 1); err != nil {
		t.Fatalf("UpsertPackage: %v", err)
	}
	if err := db.InsertBankSend(network, "send-"+id, 3, ts, addr, "g1recipient", "1ugnot", true); err != nil {
		t.Fatalf("InsertBankSend: %v", err)
	}
}

// An address active on several days is one weekly active address, not several.
//
// This is the specific error a count-per-day rollup would introduce: summing
// daily counts to answer a weekly question overcounts anyone who came back, and
// the error grows with the bucket width — worst on the 1y and all windows,
// which is exactly where the number matters most. Storing distinct tuples
// rather than counts is what makes the wider buckets re-deduplicate instead.
func TestWiderBucketsCountAReturningAddressOnce(t *testing.T) {
	db := newTestDB(t)
	db.SetConfiguredNetworks([]NetworkConfig{{ID: "a"}})

	// Three days inside one ISO week and one month. Anchored on a Wednesday so
	// that "three consecutive days" cannot straddle a week boundary whatever
	// day the test runs.
	base := time.Now().UTC().Add(-72 * time.Hour)
	for base.Weekday() != time.Wednesday {
		base = base.Add(-24 * time.Hour)
	}
	for i := 0; i < 3; i++ {
		seedActiveAt(t, db, "a", "g1regular", base.Add(time.Duration(i)*24*time.Hour), fmt.Sprintf("d%d", i))
	}

	if err := db.RefreshRollups(); err != nil {
		t.Fatalf("RefreshRollups: %v", err)
	}

	for _, tc := range []struct {
		granularity string
		want        int
	}{
		{"daily", 3},   // one per day it was active
		{"weekly", 1},  // still one address
		{"monthly", 1}, // still one address
	} {
		t.Run(tc.granularity, func(t *testing.T) {
			pts, err := db.GetActiveAddressTimeSeries("", tc.granularity, 30)
			if err != nil {
				t.Fatalf("GetActiveAddressTimeSeries: %v", err)
			}
			total := 0
			for _, p := range pts {
				total += p.TotalActive
			}
			if total != tc.want {
				t.Errorf("total active across %s buckets = %d, want %d", tc.granularity, total, tc.want)
			}
		})
	}
}

// seedMixedActivity lays down a spread of activity designed to make a wrong
// rollup disagree with the live query: several addresses, several chains
// including one that is not configured, several kinds, and returning visitors
// at hour, day, week and month distances.
func seedMixedActivity(t *testing.T, db *DB) {
	t.Helper()
	now := time.Now().UTC()
	n := 0
	id := func() string { n++; return fmt.Sprintf("m%d", n) }

	for _, net := range []string{"a", "b", "retired"} {
		for _, back := range []time.Duration{
			90 * time.Minute, 5 * time.Hour, 26 * time.Hour,
			3 * 24 * time.Hour, 9 * 24 * time.Hour, 40 * 24 * time.Hour,
		} {
			when := now.Add(-back)
			// Two addresses per instant: one that appears everywhere (so every
			// branch of the union overlaps) and one unique to this instant.
			seedActiveAt(t, db, net, "g1everywhere", when, id())
			seedActiveAt(t, db, net, "g1"+net+fmt.Sprint(int(back.Hours())), when, id())

			// A caller-only address, so the per-kind counts are not all equal.
			if err := db.InsertCall(net, "conly-"+id(), 4, when.Format("2006-01-02T15:04:05Z"),
				"g1calleronly", "gno.land/r/demo/boards", "Post", true); err != nil {
				t.Fatalf("InsertCall: %v", err)
			}
		}
	}
}

// The rollup must answer exactly what the live query answered.
//
// Same database, same windows, same granularities — the only difference is
// which path produced the numbers. Before the first refresh the read falls back
// to computing live, so capturing the series then and re-reading it after the
// refresh diffs the two implementations against one snapshot.
func TestRollupMatchesLiveComputationAtEveryGranularity(t *testing.T) {
	db := newTestDB(t)
	db.SetConfiguredNetworks([]NetworkConfig{{ID: "a"}, {ID: "b"}})
	seedMixedActivity(t, db)

	type key struct {
		network     string
		granularity string
		days        int
	}
	cases := []key{}
	for _, net := range []string{"", "a", "b"} {
		for _, g := range []string{"hourly", "daily", "weekly", "monthly"} {
			for _, d := range []int{1, 7, 30, 365} {
				cases = append(cases, key{net, g, d})
			}
		}
	}

	live := map[key][]ActiveAddressTimePoint{}
	for _, c := range cases {
		pts, err := db.GetActiveAddressTimeSeries(c.network, c.granularity, c.days)
		if err != nil {
			t.Fatalf("live %v: %v", c, err)
		}
		if len(pts) == 0 {
			t.Fatalf("live %v returned no buckets — the fixture is not exercising anything", c)
		}
		live[c] = pts
	}

	if err := db.RefreshRollups(); err != nil {
		t.Fatalf("RefreshRollups: %v", err)
	}

	for _, c := range cases {
		got, err := db.GetActiveAddressTimeSeries(c.network, c.granularity, c.days)
		if err != nil {
			t.Fatalf("rolled %v: %v", c, err)
		}
		if !reflect.DeepEqual(got, live[c]) {
			t.Errorf("network=%q granularity=%s days=%d\n rolled up: %+v\n live:      %+v",
				c.network, c.granularity, c.days, got, live[c])
		}
	}
}

// A rollup built five minutes ago must not hide what happened since.
//
// The rebuild runs on a timer, so between two builds the newest rows exist only
// in the source tables. Serving the series from the rollup alone would make the
// current bucket lag by up to the refresh interval — visible on the 24h chart,
// and enough to make this endpoint disagree with the live transaction feed
// sitting next to it on the same page. Whatever the rollup does not cover yet
// has to be read live and merged in.
func TestSeriesIncludesActivityNewerThanTheRollup(t *testing.T) {
	db := newTestDB(t)
	db.SetConfiguredNetworks([]NetworkConfig{{ID: "a"}})

	seedActiveAt(t, db, "a", "g1early", time.Now().UTC().Add(-30*time.Hour), "early")
	if err := db.RefreshRollups(); err != nil {
		t.Fatalf("RefreshRollups: %v", err)
	}

	// Arrives after the rollup was built, as a live chain constantly does.
	seedActiveAt(t, db, "a", "g1latecomer", time.Now().UTC(), "late")

	pts, err := db.GetActiveAddressTimeSeries("", "daily", 7)
	if err != nil {
		t.Fatalf("GetActiveAddressTimeSeries: %v", err)
	}
	total := 0
	for _, p := range pts {
		total += p.TotalActive
	}
	if total != 2 {
		t.Errorf("total active = %d, want 2 — the address that arrived after the rollup was built is missing", total)
	}
}

// The merge of rolled-up and live rows must not count the overlap twice.
//
// The rollup covers everything up to the moment it was built, which includes
// part of the hour it was built in. Reading that same hour live as well means
// the same address arrives from both sides, and the merge has to collapse it.
func TestMergingRollupAndLiveRowsDoesNotDoubleCount(t *testing.T) {
	db := newTestDB(t)
	db.SetConfiguredNetworks([]NetworkConfig{{ID: "a"}})

	// In the current hour, so it lands on both sides of the boundary.
	seedActiveAt(t, db, "a", "g1boundary", time.Now().UTC(), "boundary")
	if err := db.RefreshRollups(); err != nil {
		t.Fatalf("RefreshRollups: %v", err)
	}

	pts, err := db.GetActiveAddressTimeSeries("", "hourly", 1)
	if err != nil {
		t.Fatalf("GetActiveAddressTimeSeries: %v", err)
	}
	total := 0
	for _, p := range pts {
		total += p.TotalActive
	}
	if total != 1 {
		t.Errorf("total active = %d, want 1 — the rolled-up and live copies of the same address were both counted", total)
	}
}

// Rows belonging to a network nobody configured are not this explorer's data.
//
// An empty network filter means "every configured network", never "every
// network ever synced". Retired chains keep their rows, and counting them is
// how the realm-count mismatch in #115 happened; the rollup must not
// reintroduce it by aggregating over everything it can see.
func TestRollupExcludesUnconfiguredNetworks(t *testing.T) {
	db := newTestDB(t)
	db.SetConfiguredNetworks([]NetworkConfig{{ID: "a"}})

	when := time.Now().UTC().Add(-2 * time.Hour)
	seedActiveAt(t, db, "a", "g1configured", when, "cfg")
	seedActiveAt(t, db, "retired", "g1retired", when, "old")

	if err := db.RefreshRollups(); err != nil {
		t.Fatalf("RefreshRollups: %v", err)
	}

	pts, err := db.GetActiveAddressTimeSeries("", "daily", 7)
	if err != nil {
		t.Fatalf("GetActiveAddressTimeSeries: %v", err)
	}
	total := 0
	for _, p := range pts {
		total += p.TotalActive
	}
	if total != 1 {
		t.Errorf("total active = %d, want 1 — a retired network's rows were counted", total)
	}
}

// The same address on two chains is two actors, in the rollup as everywhere.
func TestRollupCountsAnAddressOncePerChain(t *testing.T) {
	db := newTestDB(t)
	db.SetConfiguredNetworks([]NetworkConfig{{ID: "a"}, {ID: "b"}})

	when := time.Now().UTC().Add(-2 * time.Hour)
	seedActiveAt(t, db, "a", "g1everywhere", when, "a1")
	seedActiveAt(t, db, "b", "g1everywhere", when, "b1")

	if err := db.RefreshRollups(); err != nil {
		t.Fatalf("RefreshRollups: %v", err)
	}

	pts, err := db.GetActiveAddressTimeSeries("", "daily", 7)
	if err != nil {
		t.Fatalf("GetActiveAddressTimeSeries: %v", err)
	}
	total := 0
	for _, p := range pts {
		total += p.TotalActive
	}
	if total != 2 {
		t.Errorf("total active = %d, want 2 — one address on two chains is two actors", total)
	}
}

// A chain reset drops the network's rows; the rollup's copy has to go too.
//
// DeleteNetworkData exists because a reset makes the stored history refer to
// blocks that no longer exist. A rollup row surviving that would keep counting
// an address whose activity has been deleted, until the next refresh — and the
// next refresh is up to five minutes away.
func TestChainResetClearsTheRolledUpTuples(t *testing.T) {
	db := newTestDB(t)
	db.SetConfiguredNetworks([]NetworkConfig{{ID: "a"}, {ID: "b"}})

	when := time.Now().UTC().Add(-2 * time.Hour)
	seedActiveAt(t, db, "a", "g1onreset", when, "r1")
	seedActiveAt(t, db, "b", "g1survivor", when, "r2")
	if err := db.RefreshRollups(); err != nil {
		t.Fatalf("RefreshRollups: %v", err)
	}

	if _, err := db.DeleteNetworkData("a"); err != nil {
		t.Fatalf("DeleteNetworkData: %v", err)
	}

	pts, err := db.GetActiveAddressTimeSeries("", "daily", 7)
	if err != nil {
		t.Fatalf("GetActiveAddressTimeSeries: %v", err)
	}
	total := 0
	for _, p := range pts {
		total += p.TotalActive
	}
	if total != 1 {
		t.Errorf("total active = %d, want 1 — the reset chain's rolled-up tuples outlived its rows", total)
	}
}

// The sanity page's 24h figure deliberately stays live, and the rollup must not
// quietly change it.
//
// It looks like the same question the series answers, and #137 suggests folding
// it in, but its window is "the last 24 hours" to the second, while the stored
// grain is the hour. Serving it from the rolled-up tuples would have to round
// the window out to an hour boundary and would count up to an extra hour of
// activity — a different number, arrived at by a change that was supposed to be
// about speed. It is also a single 24h window over an indexed block_time, which
// is not what was slow. This pins that it keeps answering exactly as before.
func TestSanityActiveAddressesStayLiveAcrossARefresh(t *testing.T) {
	db := newTestDB(t)
	db.SetConfiguredNetworks([]NetworkConfig{{ID: "a"}, {ID: "b"}})
	seedMixedActivity(t, db)

	liveOv, err := db.GetSanityOverview("")
	if err != nil {
		t.Fatalf("GetSanityOverview: %v", err)
	}
	if liveOv.ActiveAddresses24h == 0 {
		t.Fatal("live active addresses 24h = 0 — the fixture is not exercising anything")
	}

	if err := db.RefreshRollups(); err != nil {
		t.Fatalf("RefreshRollups: %v", err)
	}

	rolledOv, err := db.GetSanityOverview("")
	if err != nil {
		t.Fatalf("GetSanityOverview: %v", err)
	}
	if rolledOv.ActiveAddresses24h != liveOv.ActiveAddresses24h {
		t.Errorf("rolled up = %d, live = %d", rolledOv.ActiveAddresses24h, liveOv.ActiveAddresses24h)
	}
}

// The hour the window opens in belongs half outside the window.
//
// "The last 7 days" starts at an instant, not on an hour boundary, but the
// stored grain is the hour: a tuple says an address was active somewhere in that
// hour, never whether it was before or after 14:23. Reading that opening hour
// from the rollup would count activity from before the window and inflate the
// first bucket — invisibly, because the first bucket of a chart is exactly where
// nobody checks.
//
// Seeding one address per minute across the two hours around the boundary means
// some fall inside the window and some outside it whatever time the test runs,
// so the two paths can only agree if the opening hour is handled to the second.
func TestWindowOpeningHourMatchesLive(t *testing.T) {
	db := newTestDB(t)
	db.SetConfiguredNetworks([]NetworkConfig{{ID: "a"}})

	const days = 7
	openingHour := time.Now().UTC().AddDate(0, 0, -days).Truncate(time.Hour)
	for i := -60; i <= 60; i++ {
		when := openingHour.Add(time.Duration(i) * time.Minute)
		if err := db.InsertCall("a", fmt.Sprintf("edge%d", i), 1,
			when.Format("2006-01-02T15:04:05Z"), fmt.Sprintf("g1edge%03d", i+60),
			"gno.land/r/demo/boards", "Post", true); err != nil {
			t.Fatalf("InsertCall: %v", err)
		}
	}

	for _, granularity := range []string{"hourly", "daily"} {
		live, err := db.GetActiveAddressTimeSeries("", granularity, days)
		if err != nil {
			t.Fatalf("live %s: %v", granularity, err)
		}
		if err := db.RefreshRollups(); err != nil {
			t.Fatalf("RefreshRollups: %v", err)
		}
		rolled, err := db.GetActiveAddressTimeSeries("", granularity, days)
		if err != nil {
			t.Fatalf("rolled %s: %v", granularity, err)
		}
		if !reflect.DeepEqual(rolled, live) {
			t.Errorf("%s: first bucket disagrees across the window edge\n rolled up: %+v\n live:      %+v",
				granularity, rolled[0], live[0])
		}
	}
}

// The stored grain is one tuple per (network, hour, kind, address).
//
// Hourly rather than daily because the two measured within 15% of each other on
// production (110,073 hourly tuples against 95,615 daily, from 1.27M raw rows),
// and the finer grain removes the need for a second table or a live path for the
// 24h and 7d windows. Repeat activity inside an hour collapses; the next hour is
// a new tuple, which is what lets a wider bucket re-deduplicate exactly.
func TestRollupStoresOneTuplePerAddressPerHourPerKind(t *testing.T) {
	db := newTestDB(t)
	db.SetConfiguredNetworks([]NetworkConfig{{ID: "a"}})

	hour := time.Now().UTC().Truncate(time.Hour).Add(-3 * time.Hour)
	for i, offset := range []time.Duration{5 * time.Minute, 25 * time.Minute, 45 * time.Minute} {
		seedActiveAt(t, db, "a", "g1busy", hour.Add(offset), fmt.Sprintf("same%d", i))
	}
	// A different hour is a different tuple.
	seedActiveAt(t, db, "a", "g1busy", hour.Add(-2*time.Hour), "other")

	if err := db.RefreshRollups(); err != nil {
		t.Fatalf("RefreshRollups: %v", err)
	}

	var n int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM active_addr_rollup`).Scan(&n); err != nil {
		t.Fatalf("count rollup: %v", err)
	}
	// Two hours x three kinds x one address.
	if n != 6 {
		t.Errorf("rollup holds %d tuples, want 6 — the grain is not one row per (network, hour, kind, address)", n)
	}

	var earliest string
	if err := db.db.QueryRow(`SELECT MIN(bucket) FROM active_addr_rollup`).Scan(&earliest); err != nil {
		t.Fatalf("min bucket: %v", err)
	}
	if want := hour.Add(-2 * time.Hour).Format("2006-01-02T15"); earliest != want {
		t.Errorf("earliest bucket = %q, want %q", earliest, want)
	}
}

// Settled history is answered from the rollup, not recomputed from the union.
//
// Checked by mutation: the source rows are deleted after the refresh, leaving
// only the rolled-up tuples. A read that still reports the day it deleted can
// only have got the number from the rollup — which is the whole point of
// building one, and the part a correctness-only test cannot observe.
func TestSeriesReadsSettledHistoryFromTheRollup(t *testing.T) {
	db := newTestDB(t)
	db.SetConfiguredNetworks([]NetworkConfig{{ID: "a"}})

	seedActiveAt(t, db, "a", "g1historic", time.Now().UTC().Add(-30*time.Hour), "h1")
	if err := db.RefreshRollups(); err != nil {
		t.Fatalf("RefreshRollups: %v", err)
	}

	for _, table := range []string{"calls", "packages", "bank_sends"} {
		if _, err := db.db.Exec(`DELETE FROM ` + table); err != nil {
			t.Fatalf("delete %s: %v", table, err)
		}
	}

	pts, err := db.GetActiveAddressTimeSeries("", "daily", 7)
	if err != nil {
		t.Fatalf("GetActiveAddressTimeSeries: %v", err)
	}
	total := 0
	for _, p := range pts {
		total += p.TotalActive
	}
	if total != 1 {
		t.Errorf("total active = %d, want 1 — the read recomputed from the source tables instead of reading the rollup", total)
	}
}
