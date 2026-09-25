package store

import (
	"testing"
	"time"
)

func ts(day int) time.Time {
	return time.Date(2026, 9, day, 12, 0, 0, 0, time.UTC)
}

// The table's whole purpose: the earliest appearance, not the earliest one that
// happens to be scanned first, and not the most recent.
func TestFirstSeenRecordsTheEarliest(t *testing.T) {
	db := NewTestDB(t)

	// alice calls twice, out of height order in insertion order.
	MustCall(t, db, "alpha", "TX2", 200, ts(20), "g1alice", "gno.land/r/x/a", "Fn")
	MustCall(t, db, "alpha", "TX1", 100, ts(10), "g1alice", "gno.land/r/x/a", "Fn")
	MustCall(t, db, "alpha", "TX3", 300, ts(30), "g1bob", "gno.land/r/x/b", "Fn")

	if err := db.RefreshFirstSeen(); err != nil {
		t.Fatal(err)
	}

	row, ok, err := db.FirstSeenAt("alpha", FirstSeenAddress, "g1alice")
	if err != nil || !ok {
		t.Fatalf("alice not recorded: ok=%v err=%v", ok, err)
	}
	if row.Height != 100 {
		t.Errorf("alice first seen at height %d, want 100 (the earliest, not the first inserted)", row.Height)
	}

	// The package's first call is its own subject, and a/b are distinct.
	if row, ok, _ := db.FirstSeenAt("alpha", FirstSeenPackageCalled, "gno.land/r/x/a"); !ok || row.Height != 100 {
		t.Errorf("package a first call = %+v, want height 100", row)
	}
	if row, ok, _ := db.FirstSeenAt("alpha", FirstSeenPackageCalled, "gno.land/r/x/b"); !ok || row.Height != 300 {
		t.Errorf("package b first call = %+v, want height 300", row)
	}
}

// Running it twice must not change anything.
func TestFirstSeenIsIdempotent(t *testing.T) {
	db := NewTestDB(t)
	MustCall(t, db, "alpha", "TX1", 100, ts(10), "g1alice", "gno.land/r/x/a", "Fn")
	if err := db.RefreshFirstSeen(); err != nil {
		t.Fatal(err)
	}
	before, _, _ := db.FirstSeenAt("alpha", FirstSeenAddress, "g1alice")

	MustCall(t, db, "alpha", "TX9", 900, ts(28), "g1alice", "gno.land/r/x/a", "Fn")
	for i := 0; i < 3; i++ {
		if err := db.RefreshFirstSeen(); err != nil {
			t.Fatal(err)
		}
	}
	after, _, _ := db.FirstSeenAt("alpha", FirstSeenAddress, "g1alice")
	if after != before {
		t.Errorf("first seen moved from %+v to %+v after later calls", before, after)
	}
}

// The upsert's guard, exercised directly.
//
// The ordinary path cannot reach it: the source subquery is ROW_NUMBER() with
// rn = 1, so it only ever offers the earliest row and the guard never fires.
// That makes the guard defence against the case the query shape does not cover,
// a stored row that is already earlier than anything the source can see, which
// is what a partially deleted or re-walked calls table produces. Seeded through
// the SQL seam because no public method can create that state, and the first
// version of this test passed with the guard removed, which is exactly the
// failure this one exists to prevent.
func TestFirstSeenNeverMovesForward(t *testing.T) {
	db := NewTestDB(t)
	MustCall(t, db, "alpha", "TX1", 100, ts(20), "g1alice", "gno.land/r/x/a", "Fn")

	// A stored first appearance earlier than any row the source can offer.
	if _, err := db.SQL().Exec(
		`INSERT INTO first_seen (network, kind, subject, at, height) VALUES (?, ?, ?, ?, ?)`,
		"alpha", FirstSeenAddress, "g1alice", ts(5).Format(time.RFC3339), 10); err != nil {
		t.Fatal(err)
	}

	if err := db.RefreshFirstSeen(); err != nil {
		t.Fatal(err)
	}
	row, _, _ := db.FirstSeenAt("alpha", FirstSeenAddress, "g1alice")
	if row.Height != 10 {
		t.Errorf("first seen = height %d, want 10: the refresh overwrote an earlier record with a later one", row.Height)
	}
}

// A backfill that reveals older history has to correct the table, which is the
// only direction an upsert here is allowed to move a row.
func TestFirstSeenMovesEarlierWhenHistoryArrivesLate(t *testing.T) {
	db := NewTestDB(t)
	MustCall(t, db, "alpha", "TX5", 500, ts(25), "g1alice", "gno.land/r/x/a", "Fn")
	if err := db.RefreshFirstSeen(); err != nil {
		t.Fatal(err)
	}

	// The backfill walks older blocks and finds an earlier call.
	MustCall(t, db, "alpha", "TX1", 50, ts(5), "g1alice", "gno.land/r/x/a", "Fn")
	if err := db.RefreshFirstSeen(); err != nil {
		t.Fatal(err)
	}

	row, _, _ := db.FirstSeenAt("alpha", FirstSeenAddress, "g1alice")
	if row.Height != 50 {
		t.Errorf("first seen = height %d, want 50: a backfill must correct it downward", row.Height)
	}
}

// An untimed row is skipped rather than recorded with a later timestamp, and it
// is picked up once the block-time pass has stamped it. Recording the wrong
// time would be permanent, because the row would already exist.
func TestFirstSeenSkipsUntilTheEarliestRowIsTimed(t *testing.T) {
	db := NewTestDB(t)
	// Earliest call, no block_time yet.
	if err := db.InsertCall("alpha", "TX1", 100, 0, "", "g1alice", "gno.land/r/x/a", "Fn", true); err != nil {
		t.Fatal(err)
	}
	MustCall(t, db, "alpha", "TX2", 200, ts(20), "g1alice", "gno.land/r/x/a", "Fn")

	if err := db.RefreshFirstSeen(); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := db.FirstSeenAt("alpha", FirstSeenAddress, "g1alice"); ok {
		t.Fatal("recorded a first appearance from an untimed earliest row; it would have been the wrong time, permanently")
	}

	// The stamping pass fills it in.
	if _, err := db.SQL().Exec(
		`UPDATE calls SET block_time = ? WHERE network = 'alpha' AND tx_hash = 'TX1'`,
		ts(10).Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	if err := db.RefreshFirstSeen(); err != nil {
		t.Fatal(err)
	}
	row, ok, _ := db.FirstSeenAt("alpha", FirstSeenAddress, "g1alice")
	if !ok || row.Height != 100 {
		t.Errorf("after stamping, first seen = %+v, want height 100", row)
	}
}

// Deployers come from package_submissions, not packages: packages is written
// INSERT OR REPLACE and a redeploy destroys the first publication.
func TestFirstSeenDeployerUsesSubmissionsAndIgnoresFailures(t *testing.T) {
	db := NewTestDB(t)
	must := func(hash string, h int, day int, creator string, ok bool) {
		t.Helper()
		if err := db.InsertPackageSubmission("alpha", hash, 0, "gno.land/r/x/"+hash, "n",
			creator, h, ts(day).Format(time.RFC3339), true, 1, ok); err != nil {
			t.Fatal(err)
		}
	}
	must("TXfail", 50, 5, "g1dev", false) // earlier, but it published nothing
	must("TXok", 100, 10, "g1dev", true)
	must("TXlater", 300, 30, "g1dev", true)

	if err := db.RefreshFirstSeen(); err != nil {
		t.Fatal(err)
	}
	row, ok, _ := db.FirstSeenAt("alpha", FirstSeenDeployer, "g1dev")
	if !ok || row.Height != 100 {
		t.Errorf("deployer first seen = %+v, want height 100: a failed submission is not a publication", row)
	}
}

// Two chains, one address. The table is keyed by network for the same reason
// every ranking here is: the busiest caller on the site is active on several.
func TestFirstSeenIsScopedPerNetwork(t *testing.T) {
	db := NewTestDB(t)
	MustCall(t, db, "alpha", "TX1", 100, ts(10), "g1alice", "gno.land/r/x/a", "Fn")
	MustCall(t, db, "beta", "TX2", 5, ts(25), "g1alice", "gno.land/r/x/a", "Fn")
	if err := db.RefreshFirstSeen(); err != nil {
		t.Fatal(err)
	}

	a, _, _ := db.FirstSeenAt("alpha", FirstSeenAddress, "g1alice")
	b, _, _ := db.FirstSeenAt("beta", FirstSeenAddress, "g1alice")
	if a.Height != 100 || b.Height != 5 {
		t.Errorf("alpha=%d beta=%d, want 100 and 5: one address, two chains, two answers", a.Height, b.Height)
	}
}

// The range scan the table exists for.
func TestFirstSeenSince(t *testing.T) {
	db := NewTestDB(t)
	MustCall(t, db, "alpha", "TX1", 100, ts(1), "g1old", "gno.land/r/x/a", "Fn")
	MustCall(t, db, "alpha", "TX2", 200, ts(20), "g1new", "gno.land/r/x/b", "Fn")
	MustCall(t, db, "alpha", "TX3", 300, ts(22), "g1newer", "gno.land/r/x/c", "Fn")
	if err := db.RefreshFirstSeen(); err != nil {
		t.Fatal(err)
	}

	rows, err := db.FirstSeenSince("alpha", FirstSeenAddress, ts(15).Format(time.RFC3339), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2 (g1old is older than the window)", len(rows))
	}
	if rows[0].Subject != "g1newer" || rows[1].Subject != "g1new" {
		t.Errorf("order = %s, %s; want newest first", rows[0].Subject, rows[1].Subject)
	}
}

// A chain reset has to take first_seen with it, or every participant on the new
// chain looks like a returning one and "new this week" is permanently empty.
func TestFirstSeenGoesWithAChainReset(t *testing.T) {
	db := NewTestDB(t)
	MustCall(t, db, "alpha", "TX1", 100, ts(10), "g1alice", "gno.land/r/x/a", "Fn")
	MustCall(t, db, "beta", "TX2", 100, ts(10), "g1bob", "gno.land/r/x/b", "Fn")
	if err := db.RefreshFirstSeen(); err != nil {
		t.Fatal(err)
	}

	if _, err := db.DeleteNetworkData("alpha"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := db.FirstSeenAt("alpha", FirstSeenAddress, "g1alice"); ok {
		t.Error("alpha's first_seen survived a chain reset")
	}
	if _, ok, _ := db.FirstSeenAt("beta", FirstSeenAddress, "g1bob"); !ok {
		t.Error("beta's first_seen was taken by alpha's reset")
	}
}
