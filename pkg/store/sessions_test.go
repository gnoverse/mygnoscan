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
	if err := db.PinSessionBackfillStop("mainnet", 500); err != nil {
		t.Fatalf("pin: %v", err)
	}
	if err := db.PinSessionBackfillStop("mainnet", 9000); err != nil {
		t.Fatalf("pin again: %v", err)
	}
	_, _, stop := db.SessionBackfillProgress("mainnet")
	if stop != 500 {
		t.Errorf("stop = %d, want 500: a second pin must not move the finish line", stop)
	}
}

func TestSessionBackfillProgressReportsIncomplete(t *testing.T) {
	db := NewTestDB(t)
	// Never run: not complete, and distinguishable from "swept and found none".
	if done, _, _ := db.SessionBackfillProgress("mainnet"); done {
		t.Error("an unrun sweep reports complete")
	}
	if err := db.PinSessionBackfillStop("mainnet", 1000); err != nil {
		t.Fatalf("pin: %v", err)
	}
	if err := db.SetSessionBackfillCursor("mainnet", 400); err != nil {
		t.Fatalf("cursor: %v", err)
	}
	done, at, stop := db.SessionBackfillProgress("mainnet")
	if done || at != 400 || stop != 1000 {
		t.Errorf("progress = %v %d/%d, want false 400/1000", done, at, stop)
	}
	if err := db.SetSessionBackfillCursor("mainnet", 1000); err != nil {
		t.Fatalf("cursor: %v", err)
	}
	if done, _, _ := db.SessionBackfillProgress("mainnet"); !done {
		t.Error("cursor reaching stop should report complete")
	}
}
