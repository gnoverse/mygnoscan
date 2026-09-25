package store

import (
	"sort"
	"testing"

	"github.com/moul/mygnoscan/pkg/achievements"
	"github.com/moul/mygnoscan/pkg/config"
)

// The catalog is twenty-odd hand-written queries against nine tables, and a
// query that references a column that does not exist fails at *run* time, on a
// timer, into a log line nobody reads — after which the badges simply never
// appear and nothing says why. So the first test here runs every definition
// against the real schema, and the second checks that each one awards the badge
// to the address it is supposed to and to nobody else.
//
// Verified the way AGENTS.md asks for: each definition below was checked to go
// red when the fact it depends on is removed from the fixture, not merely to be
// green with it there.

// seedAchievementWorld writes one small, deliberately asymmetric chain:
//
//	alice   deploys a /p/ package and a /r/ realm and a home realm, imports
//	        someone else's code, calls her own realm, gets called by bob,
//	        registers a name, wraps and unwraps, grants and revokes a session.
//	bob     only ever calls alice's realm and receives coin. He is the control:
//	        every builder badge must miss him.
//	carol   is a delegated key of alice's, and does nothing itself.
func seedAchievementWorld(t *testing.T, db *DB) {
	t.Helper()
	const net = "mainnet"
	db.SetConfiguredNetworks([]config.NetworkConfig{{ID: net}})

	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}

	// alice's packages. The /p/ one imports a path she does not own, which is
	// what first-import is about; bob's realm imports hers, which is what
	// imported-by-other is about.
	must("pkg", db.UpsertPackage(net, "gno.land/p/alice/util", "util", "g1alice", "TX1", 10, "2026-01-01T00:00:00Z", false, 1))
	must("sub", db.InsertPackageSubmission(net, "TX1", 0, "gno.land/p/alice/util", "util", "g1alice", 10, "2026-01-01T00:00:00Z", false, 1, true))
	must("deps", db.SetDependencies(net, "gno.land/p/alice/util", []string{"gno.land/p/demo/avl"}))

	must("pkg", db.UpsertPackage(net, "gno.land/r/alice/shop", "shop", "g1alice", "TX2", 11, "2026-01-02T00:00:00Z", true, 1))
	must("sub", db.InsertPackageSubmission(net, "TX2", 0, "gno.land/r/alice/shop", "shop", "g1alice", 11, "2026-01-02T00:00:00Z", true, 1, true))

	must("pkg", db.UpsertPackage(net, "gno.land/r/alice/home", "home", "g1alice", "TX3", 12, "2026-01-03T00:00:00Z", true, 1))
	must("sub", db.InsertPackageSubmission(net, "TX3", 0, "gno.land/r/alice/home", "home", "g1alice", 12, "2026-01-03T00:00:00Z", true, 1, true))

	// bob's realm, which imports alice's package.
	must("pkg", db.UpsertPackage(net, "gno.land/r/bob/app", "app", "g1bob", "TX4", 13, "2026-01-04T00:00:00Z", true, 1))
	must("deps", db.SetDependencies(net, "gno.land/r/bob/app", []string{"gno.land/p/alice/util"}))

	// dave imports only himself, and is imported only by himself. He is the
	// control for the two dependency badges specifically: without him both
	// queries pass with their "and it is not your own code" clause deleted,
	// because nobody else in this world imports themselves. Packages only, no
	// submission rows, so he does not move any other badge's expected set.
	must("pkg", db.UpsertPackage(net, "gno.land/p/dave/lib", "lib", "g1dave", "TX19", 14, "2026-01-19T00:00:00Z", false, 1))
	must("pkg", db.UpsertPackage(net, "gno.land/r/dave/app", "app", "g1dave", "TX20", 15, "2026-01-20T00:00:00Z", true, 1))
	must("deps", db.SetDependencies(net, "gno.land/r/dave/app", []string{"gno.land/p/dave/lib"}))

	// Calls. alice calls her own shop; bob calls it too.
	must("call", db.InsertCall(net, "TX5", 20, 0, "2026-01-05T00:00:00Z", "g1alice", "gno.land/r/alice/shop", "Buy", true))
	must("call", db.InsertCall(net, "TX6", 21, 0, "2026-01-06T00:00:00Z", "g1bob", "gno.land/r/alice/shop", "Buy", true))

	// wugnot, profile, govdao.
	must("call", db.InsertCall(net, "TX7", 22, 0, "2026-01-07T00:00:00Z", "g1alice", "gno.land/r/gnoland/wugnot", "Deposit", true))
	must("call", db.InsertCall(net, "TX8", 23, 0, "2026-01-08T00:00:00Z", "g1alice", "gno.land/r/gnoland/wugnot", "Withdraw", true))
	must("call", db.InsertCall(net, "TX9", 24, 0, "2026-01-09T00:00:00Z", "g1alice", "gno.land/r/demo/profile", "SetStringField", true))
	must("call", db.InsertCall(net, "TX10", 25, 0, "2026-01-10T00:00:00Z", "g1alice", "gno.land/r/gov/dao", "MustVoteOnProposalSimple", true))
	must("call", db.InsertCall(net, "TX11", 26, 0, "2026-01-11T00:00:00Z", "g1alice", "gno.land/r/gov/dao", "ExecuteProposal", true))

	// A run, a send, a receive.
	must("run", db.InsertMsgRun(net, "TX12", 27, "2026-01-12T00:00:00Z", "g1alice", "package main", true))
	must("send", db.InsertBankSend(net, "TX13", 28, "2026-01-13T00:00:00Z", "g1alice", "g1bob", "1000000ugnot", true))

	// Tokens: alice's shop issues one, and it moves from alice to bob.
	must("token", db.InsertTokenTransfer(net, "TX14", 0, TokenTransfer{
		Token: "gno.land/r/alice/shop.shop.0000001", From: "g1alice", To: "g1bob",
		Value: 5, BlockHeight: 29, BlockTime: "2026-01-14T00:00:00Z"}))

	// Identity.
	must("user", db.UpsertUser(net, User{Name: "alice", Address: "g1alice", TxHash: "TX15", BlockHeight: 30, BlockTime: "2026-01-15T00:00:00Z"}))

	// Sessions: alice grants carol a key, then revokes it.
	must("grant", db.UpsertSessionGrant(SessionGrant{Network: net, SessionAddr: "g1carol", Master: "g1alice",
		AllowPaths: []string{"vm/exec:gno.land/r/alice/shop"}, GrantedHeight: 31, GrantedTime: "2026-01-16T00:00:00Z", GrantedTx: "TX16"}))
	must("revoke", db.RevokeSessionGrant(net, "g1carol", 32, "2026-01-17T00:00:00Z", "TX17"))

	// A validator, which is nobody else in this world.
	must("valoper", db.InsertValoperRegistration(net, "TX18", 33, "2026-01-18T00:00:00Z", "g1val", "Register", "g1val", "val-1", true))
}

// holders returns the addresses holding one badge, sorted, so an assertion can
// compare whole sets rather than probe them one at a time. Probing one address
// is how a query that awards a badge to everybody passes.
func holders(t *testing.T, db *DB, slug string) []string {
	t.Helper()
	rows, total, err := db.AchievementHolders("mainnet", slug, 500, 0)
	if err != nil {
		t.Fatalf("AchievementHolders(%s): %v", slug, err)
	}
	if total != len(rows) {
		t.Fatalf("%s: total %d but %d rows, so the count and the page disagree", slug, total, len(rows))
	}
	out := make([]string, 0, len(rows))
	for _, h := range rows {
		out = append(out, h.Address)
	}
	sort.Strings(out)
	return out
}

func equalSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Every definition has to at least execute against the real schema. This is the
// test that catches a renamed column, and it is separate from the behaviour
// table below because a syntax error there would fail twenty assertions at once
// and say nothing about which query is broken.
func TestEveryAchievementQueryRuns(t *testing.T) {
	db := NewTestDB(t)
	db.SetConfiguredNetworks([]config.NetworkConfig{{ID: "mainnet"}})
	if err := db.RefreshAchievements(); err != nil {
		t.Fatalf("RefreshAchievements on an empty database: %v", err)
	}
	if len(achievements.Indexed()) == 0 {
		t.Fatal("no indexed definitions, so this test proves nothing")
	}
}

func TestAchievementsAwardTheRightAddresses(t *testing.T) {
	db := NewTestDB(t)
	seedAchievementWorld(t, db)
	if err := db.RefreshAchievements(); err != nil {
		t.Fatalf("RefreshAchievements: %v", err)
	}

	tests := []struct {
		slug string
		want []string
		why  string
	}{
		{"first-tx", []string{"g1alice", "g1bob"}, "both signed something; carol only ever received a grant"},
		{"first-gnot-sent", []string{"g1alice"}, "only alice sent"},
		{"first-gnot-received", []string{"g1bob"}, "only bob received"},
		{"first-call", []string{"g1alice", "g1bob"}, "both called a realm"},
		{"first-run", []string{"g1alice"}, "only alice ran a script"},
		{"first-package", []string{"g1alice"}, "bob deployed a realm, not a package"},
		{"first-realm", []string{"g1alice"}, "bob's realm has no submission row, only a package row"},
		{"home-realm", []string{"g1alice"}, "only alice deployed …/home"},
		{"first-import", []string{"g1alice", "g1bob"}, "alice imports p/demo/avl, bob imports alice's util; dave imports only his own lib"},
		{"imported-by-other", []string{"g1alice"}, "bob imports alice; nobody imports bob, and dave importing dave does not count"},
		{"own-realm-call", []string{"g1alice"}, "alice called her own shop"},
		{"called-by-other", []string{"g1alice"}, "bob called alice's shop"},
		{"username", []string{"g1alice"}, "only alice registered"},
		{"profile", []string{"g1alice"}, "only alice set a profile field"},
		{"wrap-wugnot", []string{"g1alice"}, "only alice deposited"},
		{"unwrap-wugnot", []string{"g1alice"}, "only alice withdrew"},
		{"grc20-sent", []string{"g1alice"}, "the transfer left alice"},
		{"grc20-received", []string{"g1bob"}, "the transfer reached bob"},
		{"token-issuer", []string{"g1alice"}, "the token's realm is alice's"},
		{"session-created", []string{"g1alice"}, "alice is the master"},
		{"session-revoked", []string{"g1alice"}, "alice revoked it"},
		{"session-key", []string{"g1carol"}, "carol is the delegated address, not a master"},
		{"govdao-vote", []string{"g1alice"}, "only alice voted"},
		{"govdao-execute", []string{"g1alice"}, "only alice executed"},
		{"validator", []string{"g1val"}, "only g1val registered"},
	}

	// Every indexed definition must appear above. A badge added to the catalog
	// with no row here would otherwise ship untested, which on a query written
	// by hand is the same as shipping it wrong.
	covered := map[string]bool{}
	for _, tc := range tests {
		covered[tc.slug] = true
	}
	for _, def := range achievements.Indexed() {
		if !covered[def.Slug] {
			t.Errorf("achievement %q has no case in this table", def.Slug)
		}
	}

	for _, tc := range tests {
		t.Run(tc.slug, func(t *testing.T) {
			got := holders(t, db, tc.slug)
			if !equalSet(got, tc.want) {
				t.Errorf("%s: holders %v, want %v (%s)", tc.slug, got, tc.want, tc.why)
			}
		})
	}
}

// The first unlock is the fact a badge carries, and "first" is the part a
// GROUP BY gets wrong quietly: without the MIN it would report whichever row
// the planner happened to reach last.
func TestAchievementRecordsTheFirstOccurrenceNotTheLatest(t *testing.T) {
	db := NewTestDB(t)
	const net = "mainnet"
	db.SetConfiguredNetworks([]config.NetworkConfig{{ID: net}})

	for _, c := range []struct {
		tx     string
		height int
		time   string
	}{
		{"TXLATE", 900, "2026-06-01T00:00:00Z"},
		{"TXFIRST", 100, "2026-01-01T00:00:00Z"},
		{"TXMID", 500, "2026-03-01T00:00:00Z"},
	} {
		if err := db.InsertCall(net, c.tx, c.height, 0, c.time, "g1alice", "gno.land/r/demo/foo", "Bar", true); err != nil {
			t.Fatalf("InsertCall: %v", err)
		}
	}
	if err := db.RefreshAchievements(); err != nil {
		t.Fatalf("RefreshAchievements: %v", err)
	}

	got, err := db.AddressAchievements(net, "g1alice")
	if err != nil {
		t.Fatalf("AddressAchievements: %v", err)
	}
	var call *Unlock
	for i := range got {
		if got[i].Slug == "first-call" {
			call = &got[i]
		}
	}
	if call == nil {
		t.Fatal("no first-call badge")
	}
	if call.Height != 100 || call.TxHash != "TXFIRST" || call.Time != "2026-01-01T00:00:00Z" {
		t.Errorf("first-call recorded %d/%s/%s, want the earliest: 100/TXFIRST/2026-01-01T00:00:00Z",
			call.Height, call.TxHash, call.Time)
	}
}

// Every table here is network-scoped, and AGENTS.md's first invariant is that a
// query which forgets that silently merges two chains. For achievements the
// merge is worse than a wrong number: it would award a badge on mainnet for
// something an address did on a testnet.
func TestAchievementsStayWithinTheirNetwork(t *testing.T) {
	db := NewTestDB(t)
	db.SetConfiguredNetworks([]config.NetworkConfig{{ID: "mainnet"}, {ID: "testnet"}})

	if err := db.InsertCall("testnet", "TX1", 10, 0, "2026-01-01T00:00:00Z", "g1alice", "gno.land/r/gnoland/wugnot", "Deposit", true); err != nil {
		t.Fatalf("InsertCall: %v", err)
	}
	if err := db.RefreshAchievements(); err != nil {
		t.Fatalf("RefreshAchievements: %v", err)
	}

	if got, _, err := db.AchievementHolders("mainnet", "wrap-wugnot", 10, 0); err != nil {
		t.Fatalf("AchievementHolders: %v", err)
	} else if len(got) != 0 {
		t.Errorf("mainnet has %d wrap-wugnot holders from a testnet deposit: %+v", len(got), got)
	}
	if got, _, err := db.AchievementHolders("testnet", "wrap-wugnot", 10, 0); err != nil {
		t.Fatalf("AchievementHolders: %v", err)
	} else if len(got) != 1 {
		t.Errorf("testnet has %d wrap-wugnot holders, want 1", len(got))
	}
}

// The rebuild replaces rather than accumulates. Without that, a re-sync or a
// chain reset would leave badges standing for facts the index no longer holds,
// and nothing would ever take them down again.
func TestRefreshAchievementsIsIdempotentAndDropsStaleBadges(t *testing.T) {
	db := NewTestDB(t)
	seedAchievementWorld(t, db)

	if err := db.RefreshAchievements(); err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	first, err := db.AddressAchievements("mainnet", "g1alice")
	if err != nil {
		t.Fatalf("AddressAchievements: %v", err)
	}
	if err := db.RefreshAchievements(); err != nil {
		t.Fatalf("second refresh: %v", err)
	}
	second, err := db.AddressAchievements("mainnet", "g1alice")
	if err != nil {
		t.Fatalf("AddressAchievements: %v", err)
	}
	if len(first) != len(second) {
		t.Fatalf("alice had %d badges then %d after an identical rebuild", len(first), len(second))
	}

	// Now take the fact away and rebuild. The badge must go with it.
	if _, err := db.db.Exec(`DELETE FROM calls WHERE pkg_path LIKE '%/wugnot'`); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := db.RefreshAchievements(); err != nil {
		t.Fatalf("third refresh: %v", err)
	}
	third, err := db.AddressAchievements("mainnet", "g1alice")
	if err != nil {
		t.Fatalf("AddressAchievements: %v", err)
	}
	for _, u := range third {
		if u.Slug == "wrap-wugnot" {
			t.Error("wrap-wugnot survived the deposit being removed, so the rebuild accumulates instead of replacing")
		}
	}
}

func TestDirectoryPeopleRanksAndFilters(t *testing.T) {
	db := NewTestDB(t)
	seedAchievementWorld(t, db)
	if err := db.RefreshAchievements(); err != nil {
		t.Fatalf("RefreshAchievements: %v", err)
	}

	t.Run("ranked by badge count", func(t *testing.T) {
		people, total, err := db.DirectoryPeople(PeopleQuery{Network: "mainnet", Limit: 10})
		if err != nil {
			t.Fatalf("DirectoryPeople: %v", err)
		}
		if total != len(people) {
			t.Fatalf("total %d, rows %d", total, len(people))
		}
		if len(people) == 0 {
			t.Fatal("nobody in the directory")
		}
		if people[0].Address != "g1alice" {
			t.Errorf("top of the directory is %s with %d badges, want g1alice", people[0].Address, people[0].Count)
		}
		if people[0].Name != "alice" {
			t.Errorf("alice's row carries the name %q, want \"alice\"", people[0].Name)
		}
		for i := 1; i < len(people); i++ {
			if people[i].Count > people[i-1].Count {
				t.Errorf("row %d has more badges than row %d, so the ranking is not sorted", i, i-1)
			}
		}
	})

	t.Run("has is an AND", func(t *testing.T) {
		// bob has first-call but not first-realm; alice has both. Filtering on
		// the pair must leave alice alone, which is the assertion an OR would
		// fail.
		people, _, err := db.DirectoryPeople(PeopleQuery{
			Network: "mainnet", Has: []string{"first-call", "first-realm"}, Limit: 10})
		if err != nil {
			t.Fatalf("DirectoryPeople: %v", err)
		}
		if len(people) != 1 || people[0].Address != "g1alice" {
			t.Errorf("filtering on first-call AND first-realm gave %d rows (%+v), want just g1alice", len(people), people)
		}
	})

	t.Run("named only", func(t *testing.T) {
		people, _, err := db.DirectoryPeople(PeopleQuery{Network: "mainnet", NamedOnly: true, Limit: 10})
		if err != nil {
			t.Fatalf("DirectoryPeople: %v", err)
		}
		if len(people) != 1 || people[0].Address != "g1alice" {
			t.Errorf("named-only gave %+v, want just g1alice", people)
		}
	})

	t.Run("q matches a name and an address prefix", func(t *testing.T) {
		byName, _, err := db.DirectoryPeople(PeopleQuery{Network: "mainnet", Q: "ali", Limit: 10})
		if err != nil {
			t.Fatalf("DirectoryPeople: %v", err)
		}
		if len(byName) != 1 || byName[0].Address != "g1alice" {
			t.Errorf("q=ali gave %+v, want g1alice", byName)
		}
		byAddr, _, err := db.DirectoryPeople(PeopleQuery{Network: "mainnet", Q: "g1bob", Limit: 10})
		if err != nil {
			t.Fatalf("DirectoryPeople: %v", err)
		}
		if len(byAddr) != 1 || byAddr[0].Address != "g1bob" {
			t.Errorf("q=g1bob gave %+v, want g1bob", byAddr)
		}
	})

	t.Run("badges come back with the row", func(t *testing.T) {
		people, _, err := db.DirectoryPeople(PeopleQuery{Network: "mainnet", Has: []string{"home-realm"}, Limit: 10})
		if err != nil {
			t.Fatalf("DirectoryPeople: %v", err)
		}
		if len(people) != 1 {
			t.Fatalf("want one row, got %d", len(people))
		}
		found := false
		for _, b := range people[0].Badges {
			if b == "home-realm" {
				found = true
			}
		}
		if !found {
			t.Errorf("the row matched on home-realm but its badges are %v", people[0].Badges)
		}
		if people[0].Count != len(people[0].Badges) {
			t.Errorf("count %d but %d badges listed", people[0].Count, len(people[0].Badges))
		}
	})
}
