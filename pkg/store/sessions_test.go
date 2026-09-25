package store

import "testing"

// A fixed clock, so "live" and "expired" are stated rather than raced.
const (
	testNow     = int64(1790000000) // 2026-09-21
	pastExpiry  = int64(1780000000) // well before testNow
	aheadExpiry = int64(1800000000) // well after
)

func seedGrant(t *testing.T, db *DB, g SessionGrant) {
	t.Helper()
	if g.Network == "" {
		g.Network = "mainnet"
	}
	if err := db.UpsertSessionGrant(g); err != nil {
		t.Fatalf("UpsertSessionGrant(%s): %v", g.SessionAddr, err)
	}
}

func TestSessionStatsPartitionsByState(t *testing.T) {
	db := NewTestDB(t)

	seedGrant(t, db, SessionGrant{SessionAddr: "g1live", Master: "g1alice",
		AllowPaths: []string{"vm/exec:gno.land/r/moul/faucet"}, ExpiresAt: aheadExpiry, GrantedHeight: 10})
	seedGrant(t, db, SessionGrant{SessionAddr: "g1expired", Master: "g1alice",
		AllowPaths: []string{"vm/exec:gno.land/r/gov/dao"}, ExpiresAt: pastExpiry, GrantedHeight: 11})
	seedGrant(t, db, SessionGrant{SessionAddr: "g1never", Master: "g1bob",
		AllowPaths: []string{"*"}, ExpiresAt: 0, GrantedHeight: 12})
	seedGrant(t, db, SessionGrant{SessionAddr: "g1revoked", Master: "g1bob",
		AllowPaths: []string{"vm/exec:gno.land/r/moul/faucet"}, ExpiresAt: aheadExpiry, GrantedHeight: 13})
	if err := db.RevokeSessionGrant("mainnet", "g1revoked", 20, "2026-09-22T00:00:00Z", "txrevoke"); err != nil {
		t.Fatalf("RevokeSessionGrant: %v", err)
	}

	st, err := db.SessionStats("mainnet", testNow)
	if err != nil {
		t.Fatalf("SessionStats: %v", err)
	}
	if st.Total != 4 {
		t.Errorf("total = %d, want 4", st.Total)
	}
	// expires_at 0 means never, which is live, not expired-at-the-epoch.
	if st.Live != 2 {
		t.Errorf("live = %d, want 2 (g1live and the never-expiring g1never)", st.Live)
	}
	if st.Expired != 1 {
		t.Errorf("expired = %d, want 1", st.Expired)
	}
	if st.Revoked != 1 {
		t.Errorf("revoked = %d, want 1", st.Revoked)
	}
	if st.Live+st.Expired+st.Revoked != st.Total {
		t.Errorf("the three states must partition total: %d+%d+%d != %d",
			st.Live, st.Expired, st.Revoked, st.Total)
	}
	if st.Masters != 2 {
		t.Errorf("masters = %d, want 2", st.Masters)
	}
}

// A key that was revoked AND would also have expired by now counts once, as
// revoked. Counting it as expired would hide that somebody acted.
func TestRevocationWinsOverExpiry(t *testing.T) {
	db := NewTestDB(t)
	seedGrant(t, db, SessionGrant{SessionAddr: "g1both", Master: "g1alice", ExpiresAt: pastExpiry, GrantedHeight: 5})
	if err := db.RevokeSessionGrant("mainnet", "g1both", 6, "", "tx"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	st, err := db.SessionStats("mainnet", testNow)
	if err != nil {
		t.Fatalf("SessionStats: %v", err)
	}
	if st.Revoked != 1 || st.Expired != 0 || st.Total != 1 {
		t.Errorf("revoked=%d expired=%d total=%d, want 1/0/1", st.Revoked, st.Expired, st.Total)
	}
}

func TestRevokeAllClosesOnlyThatMasters(t *testing.T) {
	db := NewTestDB(t)
	seedGrant(t, db, SessionGrant{SessionAddr: "g1a1", Master: "g1alice", GrantedHeight: 1})
	seedGrant(t, db, SessionGrant{SessionAddr: "g1a2", Master: "g1alice", GrantedHeight: 2})
	seedGrant(t, db, SessionGrant{SessionAddr: "g1b1", Master: "g1bob", GrantedHeight: 3})

	if err := db.RevokeAllSessionGrants("mainnet", "g1alice", 9, "", "txall"); err != nil {
		t.Fatalf("RevokeAllSessionGrants: %v", err)
	}
	alice, err := db.SessionGrantsByMaster("mainnet", "g1alice")
	if err != nil {
		t.Fatalf("by master: %v", err)
	}
	for _, g := range alice {
		if g.RevokedHeight == nil {
			t.Errorf("%s should be revoked", g.SessionAddr)
		}
	}
	bob, err := db.SessionGrantsByMaster("mainnet", "g1bob")
	if err != nil {
		t.Fatalf("by master: %v", err)
	}
	if len(bob) != 1 || bob[0].RevokedHeight != nil {
		t.Error("revoke-all must not touch another master's grants")
	}
}

// A grant made AFTER a revocation stays open: the revoke closed what was open
// when it happened, which is what re-granting the same key depends on.
func TestRevokeDoesNotCloseALaterGrant(t *testing.T) {
	db := NewTestDB(t)
	seedGrant(t, db, SessionGrant{SessionAddr: "g1key", Master: "g1alice", GrantedHeight: 10})
	if err := db.RevokeSessionGrant("mainnet", "g1key", 20, "", "tx"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	seedGrant(t, db, SessionGrant{SessionAddr: "g1key", Master: "g1alice", GrantedHeight: 30})

	grants, err := db.SessionGrantsByAddress("mainnet", "g1key")
	if err != nil {
		t.Fatalf("by address: %v", err)
	}
	if len(grants) != 2 {
		t.Fatalf("got %d grants for a key granted twice, want 2", len(grants))
	}
	// Newest first.
	if grants[0].GrantedHeight != 30 || grants[0].RevokedHeight != nil {
		t.Errorf("the re-grant must be open: height=%d revoked=%v",
			grants[0].GrantedHeight, grants[0].RevokedHeight)
	}
	if grants[1].RevokedHeight == nil || *grants[1].RevokedHeight != 20 {
		t.Errorf("the first grant must stay revoked at 20, got %v", grants[1].RevokedHeight)
	}
}

// Re-walking a height the backfill already covered must not resurrect a
// revoked grant. The sweep runs oldest first and the live pass newest first, so
// a re-walk of the grant height after the revocation landed is routine.
func TestUpsertDoesNotClearRevocation(t *testing.T) {
	db := NewTestDB(t)
	g := SessionGrant{SessionAddr: "g1key", Master: "g1alice", GrantedHeight: 10,
		AllowPaths: []string{"vm/exec:gno.land/r/moul/faucet"}}
	seedGrant(t, db, g)
	if err := db.RevokeSessionGrant("mainnet", "g1key", 20, "", "tx"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	seedGrant(t, db, g) // the backfill comes back around

	grants, err := db.SessionGrantsByAddress("mainnet", "g1key")
	if err != nil {
		t.Fatalf("by address: %v", err)
	}
	if len(grants) != 1 {
		t.Fatalf("re-upserting the same grant duplicated it: %d rows", len(grants))
	}
	if grants[0].RevokedHeight == nil {
		t.Error("a re-walk cleared the revocation, so a revoked key reads as live")
	}
}

func TestSessionRealmsRanksByMasters(t *testing.T) {
	db := NewTestDB(t)
	// One account granting the same realm three times must not outrank three
	// accounts granting it once: the question is adoption.
	for i, h := range []int{1, 2, 3} {
		_ = i
		seedGrant(t, db, SessionGrant{SessionAddr: "g1solo" + string(rune('a'+h)), Master: "g1alice",
			AllowPaths: []string{"vm/exec:gno.land/r/solo"}, GrantedHeight: h})
	}
	for i, m := range []string{"g1bob", "g1carol", "g1dave"} {
		seedGrant(t, db, SessionGrant{SessionAddr: "g1pop" + string(rune('a'+i)), Master: m,
			AllowPaths: []string{"vm/exec:gno.land/r/popular"}, GrantedHeight: 10 + i})
	}
	realms, err := db.SessionRealms("mainnet", 10)
	if err != nil {
		t.Fatalf("SessionRealms: %v", err)
	}
	if len(realms) != 2 {
		t.Fatalf("got %d realms, want 2: %+v", len(realms), realms)
	}
	if realms[0].Path != "gno.land/r/popular" {
		t.Errorf("top realm = %s, want gno.land/r/popular (3 masters beats 3 grants by one)", realms[0].Path)
	}
	if realms[0].Masters != 3 || realms[1].Masters != 1 {
		t.Errorf("masters = %d and %d, want 3 and 1", realms[0].Masters, realms[1].Masters)
	}
	// The route prefix is dropped for display; the realm is what a reader
	// recognises and clicks.
	for _, r := range realms {
		if len(r.Path) > 7 && r.Path[:7] == "vm/exec" {
			t.Errorf("realm path kept its route prefix: %q", r.Path)
		}
	}
}

// One grant naming five realms counts for each: that key really can call all
// five, and the realm ranking is about what is delegated to.
func TestSessionRealmsCountsEveryEntry(t *testing.T) {
	db := NewTestDB(t)
	seedGrant(t, db, SessionGrant{SessionAddr: "g1multi", Master: "g1alice", GrantedHeight: 1,
		AllowPaths: []string{
			"vm/exec:gno.land/r/x/bubblerumble",
			"vm/exec:gno.land/r/x/bubblerumble2",
			"vm/exec:gno.land/r/x/bubblerumble3",
		}})
	realms, err := db.SessionRealms("mainnet", 10)
	if err != nil {
		t.Fatalf("SessionRealms: %v", err)
	}
	if len(realms) != 3 {
		t.Errorf("got %d realms from a 3-entry grant, want 3", len(realms))
	}
}

func TestSessionGrantsScopeByNetwork(t *testing.T) {
	db := NewTestDB(t)
	seedGrant(t, db, SessionGrant{Network: "mainnet", SessionAddr: "g1k", Master: "g1alice", GrantedHeight: 1})
	seedGrant(t, db, SessionGrant{Network: "pearl", SessionAddr: "g1k", Master: "g1alice", GrantedHeight: 1})

	main, _, err := db.SessionGrants("mainnet", 50, 0)
	if err != nil {
		t.Fatalf("SessionGrants: %v", err)
	}
	if len(main) != 1 {
		t.Errorf("mainnet returned %d grants, want 1: the same key on two chains is two grants", len(main))
	}
	all, _, err := db.SessionGrants("", 50, 0)
	if err != nil {
		t.Fatalf("SessionGrants(all): %v", err)
	}
	if len(all) != 2 {
		t.Errorf("all-networks returned %d, want 2", len(all))
	}
}

// The sweep must terminate. Its boundary is pinned once, so a growing tip
// cannot keep moving the finish line the forward fill is already covering.
func TestSessionBackfillStopIsPinnedOnce(t *testing.T) {
	db := NewTestDB(t)
	if err := db.UpsertBlock("mainnet", 500, "2026-09-20T00:00:00Z", 0, 0); err != nil {
		t.Fatalf("seed block: %v", err)
	}
	if err := db.PinSessionBackfillStop("mainnet"); err != nil {
		t.Fatalf("pin: %v", err)
	}
	// The chain moves on; the finish line must not.
	if err := db.UpsertBlock("mainnet", 9000, "2026-09-21T00:00:00Z", 0, 0); err != nil {
		t.Fatalf("seed later block: %v", err)
	}
	if err := db.PinSessionBackfillStop("mainnet"); err != nil {
		t.Fatalf("pin again: %v", err)
	}
	// Asserted through the batch, which is where the boundary is actually used.
	// The sweep must still start just past 500, not chase the new tip at 9000.
	_, to, more, err := db.SessionBackfillRange("mainnet", 100)
	if err != nil || !more {
		t.Fatalf("batch: more=%v err=%v", more, err)
	}
	if to != 501 {
		t.Errorf("batch ends at %d, want 501: a second pin must not move the finish line", to)
	}
}

// The sweep runs newest first, so the first batch it hands back must be the one
// touching the boundary, not the one at genesis.
//
// This is the whole point of the direction: sessions landed on mainnet around
// height 270,000 of 306,000, so an upward sweep walks about a day of blocks
// that cannot hold a grant before reaching any. Measured on mainnet 2026-09-24.
func TestSessionBackfillSweepsNewestFirst(t *testing.T) {
	db := NewTestDB(t)
	for _, h := range []int{1, 500, 1000} {
		if err := db.UpsertBlock("mainnet", h, "2026-09-20T00:00:00Z", 0, 0); err != nil {
			t.Fatalf("seed block %d: %v", h, err)
		}
	}
	if err := db.PinSessionBackfillStop("mainnet"); err != nil {
		t.Fatalf("pin: %v", err)
	}

	from, to, more, err := db.SessionBackfillRange("mainnet", 100)
	if err != nil || !more {
		t.Fatalf("first batch: more=%v err=%v", more, err)
	}
	// Boundary is 1000, so the first batch is [901, 1001): the newest blocks.
	if to != 1001 || from != 901 {
		t.Errorf("first batch = [%d, %d), want [901, 1001): the newest blocks first", from, to)
	}

	// After sweeping it, the next batch is the one below.
	if err := db.SetSessionBackfillCursor("mainnet", from); err != nil {
		t.Fatalf("cursor: %v", err)
	}
	from2, to2, more2, err := db.SessionBackfillRange("mainnet", 100)
	if err != nil || !more2 {
		t.Fatalf("second batch: more=%v err=%v", more2, err)
	}
	if to2 != 901 || from2 != 801 {
		t.Errorf("second batch = [%d, %d), want [801, 901): counting down", from2, to2)
	}
}

// The sweep ends when it reaches the oldest block stored, and does not run off
// below it.
func TestSessionBackfillStopsAtTheOldestBlock(t *testing.T) {
	db := NewTestDB(t)
	for _, h := range []int{950, 1000} {
		if err := db.UpsertBlock("mainnet", h, "2026-09-20T00:00:00Z", 0, 0); err != nil {
			t.Fatalf("seed block: %v", err)
		}
	}
	if err := db.PinSessionBackfillStop("mainnet"); err != nil {
		t.Fatalf("pin: %v", err)
	}
	from, _, more, err := db.SessionBackfillRange("mainnet", 500)
	if err != nil || !more {
		t.Fatalf("batch: more=%v err=%v", more, err)
	}
	if from != 950 {
		t.Errorf("from = %d, want 950: the batch must clamp to the oldest stored block", from)
	}
	if err := db.SetSessionBackfillCursor("mainnet", from); err != nil {
		t.Fatalf("cursor: %v", err)
	}
	if _, _, more, err = db.SessionBackfillRange("mainnet", 500); err != nil || more {
		t.Errorf("after reaching the oldest block more=%v err=%v, want false/nil", more, err)
	}
	done, at, stop := db.SessionBackfillProgress("mainnet")
	if !done {
		t.Error("reaching the oldest block should report complete")
	}
	// Reported counting up, because a progress figure that fell as work
	// advanced would read as going backwards.
	if at != stop || stop != 51 {
		t.Errorf("progress = %d/%d, want 51/51 (blocks 950..1000 inclusive)", at, stop)
	}
}

// Nothing is swept before the boundary exists, or the sweep would count down
// from zero.
func TestSessionBackfillWaitsForThePin(t *testing.T) {
	db := NewTestDB(t)
	if err := db.UpsertBlock("mainnet", 1000, "2026-09-20T00:00:00Z", 0, 0); err != nil {
		t.Fatalf("seed block: %v", err)
	}
	if _, _, more, err := db.SessionBackfillRange("mainnet", 100); err != nil || more {
		t.Errorf("unpinned sweep offered work: more=%v err=%v", more, err)
	}
}

// Pinning before any block is stored must leave the boundary unset, not zero:
// a zero stop would declare the sweep finished before it had anything to sweep.
func TestPinIsSkippedWithNoBlocks(t *testing.T) {
	db := NewTestDB(t)
	if err := db.PinSessionBackfillStop("mainnet"); err != nil {
		t.Fatalf("pin with no blocks: %v", err)
	}
	done, _, stop := db.SessionBackfillProgress("mainnet")
	if stop != 0 || done {
		t.Errorf("stop=%d done=%v, want 0/false", stop, done)
	}
}

// The regression this whole change is about: the tip is read from the blocks
// table, whose height column is `height`. Reaching for LastBlockHeight, which
// selects `block_height`, errors, and a caller that skips on error leaves the
// boundary unset and the sweep never reporting complete.
func TestPinReadsTheBlocksTip(t *testing.T) {
	db := NewTestDB(t)
	for _, h := range []int{100, 250, 175} {
		if err := db.UpsertBlock("mainnet", h, "2026-09-20T00:00:00Z", 0, 0); err != nil {
			t.Fatalf("seed block %d: %v", h, err)
		}
	}
	if err := db.PinSessionBackfillStop("mainnet"); err != nil {
		t.Fatalf("pin: %v", err)
	}
	// Asserted through the first batch rather than through the progress figure,
	// which reports a span rather than a height: the sweep must start at the
	// tip, so its first batch is the one ending just past 250.
	_, to, more, err := db.SessionBackfillRange("mainnet", 100)
	if err != nil || !more {
		t.Fatalf("batch: more=%v err=%v", more, err)
	}
	if to != 251 {
		t.Errorf("first batch ends at %d, want 251 (just past the highest stored block)", to)
	}
}

func TestSessionBackfillProgressReportsIncomplete(t *testing.T) {
	db := NewTestDB(t)
	// Never pinned: not complete, and distinguishable from "swept and found none".
	if done, _, _ := db.SessionBackfillProgress("mainnet"); done {
		t.Error("an unrun sweep reports complete")
	}
	for _, h := range []int{1, 1000} {
		if err := db.UpsertBlock("mainnet", h, "2026-09-20T00:00:00Z", 0, 0); err != nil {
			t.Fatalf("seed block %d: %v", h, err)
		}
	}
	if err := db.PinSessionBackfillStop("mainnet"); err != nil {
		t.Fatalf("pin: %v", err)
	}

	// Pinned but nothing swept yet: zero of the whole span.
	done, at, stop := db.SessionBackfillProgress("mainnet")
	if done || at != 0 || stop != 1000 {
		t.Errorf("fresh pin = %v %d/%d, want false 0/1000", done, at, stop)
	}

	// The cursor counts DOWN from the boundary; progress is reported counting
	// up, because a figure that fell as work advanced would read as going
	// backwards.
	if err := db.SetSessionBackfillCursor("mainnet", 600); err != nil {
		t.Fatalf("cursor: %v", err)
	}
	done, at, stop = db.SessionBackfillProgress("mainnet")
	if done || at != 401 || stop != 1000 {
		t.Errorf("progress = %v %d/%d, want false 401/1000", done, at, stop)
	}

	if err := db.SetSessionBackfillCursor("mainnet", 1); err != nil {
		t.Fatalf("cursor: %v", err)
	}
	if done, _, _ = db.SessionBackfillProgress("mainnet"); !done {
		t.Error("a cursor at the oldest stored block should report complete")
	}
}

// The empty index is the state every instance is in until the sweep finds its
// first grant, so it is the one case the sessions page must survive. SUM over
// zero rows is NULL rather than 0, which made this a 500 rather than a zeroed
// summary: caught by a smoke test against a fresh database, not by any test
// above, because all of them seed rows first.
func TestSessionStatsOnAnEmptyIndex(t *testing.T) {
	db := NewTestDB(t)

	st, err := db.SessionStats("mainnet", testNow)
	if err != nil {
		t.Fatalf("SessionStats on an empty index: %v", err)
	}
	if st.Total != 0 || st.Live != 0 || st.Expired != 0 || st.Revoked != 0 || st.Masters != 0 {
		t.Errorf("expected an all-zero summary, got %+v", st)
	}

	grants, total, err := db.SessionGrants("mainnet", 50, 0)
	if err != nil {
		t.Fatalf("SessionGrants on an empty index: %v", err)
	}
	if len(grants) != 0 || total != 0 {
		t.Errorf("got %d grants / total %d, want 0/0", len(grants), total)
	}
	realms, err := db.SessionRealms("mainnet", 10)
	if err != nil {
		t.Fatalf("SessionRealms on an empty index: %v", err)
	}
	if len(realms) != 0 {
		t.Errorf("got %d realms, want 0", len(realms))
	}
}

// An instance that ran a previous sweep holds a cursor under an older key. It
// must be ignored rather than read as progress: under the upward key the number
// meant the opposite direction, and under the desc key it claims blocks were
// examined that the fixed decoder never actually read.
// Read as a downward cursor that would mean "almost everything is swept", and
// the sweep would skip the newest blocks, which is the only region session
// grants exist in. The key is versioned so such an instance starts clean.
func TestAnUpwardCursorIsNotMistakenForADownwardOne(t *testing.T) {
	db := NewTestDB(t)
	for _, h := range []int{1, 1000} {
		if err := db.UpsertBlock("mainnet", h, "2026-09-20T00:00:00Z", 0, 0); err != nil {
			t.Fatalf("seed block: %v", err)
		}
	}
	// What the previous build left behind: swept up to 50.
	if err := db.SetSyncState("session_backfill_cursor_desc:mainnet", "50"); err != nil {
		t.Fatalf("seed stale cursor: %v", err)
	}
	if err := db.PinSessionBackfillStop("mainnet"); err != nil {
		t.Fatalf("pin: %v", err)
	}

	_, to, more, err := db.SessionBackfillRange("mainnet", 100)
	if err != nil || !more {
		t.Fatalf("batch: more=%v err=%v", more, err)
	}
	if to != 1001 {
		t.Errorf("first batch ends at %d, want 1001: a stale upward cursor must not be read as downward progress", to)
	}
	done, at, _ := db.SessionBackfillProgress("mainnet")
	if done || at != 0 {
		t.Errorf("progress = %v at=%d, want false 0: nothing has been swept downward yet", done, at)
	}
}
