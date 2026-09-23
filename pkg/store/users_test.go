package store

import "testing"

func seedUsers(t *testing.T, db *DB, network string, us []User) {
	t.Helper()
	for _, u := range us {
		if err := db.UpsertUser(network, u); err != nil {
			t.Fatalf("upsert user %q: %v", u.Name, err)
		}
	}
}

// The ranking is the whole point of SearchUsers: a plain substring match on
// mainnet answers "gno" with five namespaces and a pile of nym- names, and the
// exact registration has to be first.
func TestSearchUsersRanksExactThenPrefix(t *testing.T) {
	db := NewTestDB(t)
	seedUsers(t, db, "alpha", []User{
		{Name: "gno", Address: "g1gno"},
		{Name: "gnoland", Address: "g1gnoland"},
		{Name: "gnoswap", Address: "g1gnoswap"},
		{Name: "nym-ingnome001", Address: "g1nym"},
		{Name: "moul", Address: "g1manfred"},
	})

	got, err := db.SearchUsers("alpha", "gno", 10)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(got))
	for i, u := range got {
		names[i] = u.Name
	}
	if len(names) != 4 {
		t.Fatalf("got %v, want the four names containing gno", names)
	}
	if names[0] != "gno" {
		t.Errorf("first = %q, want the exact match", names[0])
	}
	// The substring-only match sorts behind all three prefixes.
	if names[3] != "nym-ingnome001" {
		t.Errorf("last = %q, want the substring-only match", names[3])
	}
}

// A search is case-insensitive on both sides: what a reader types and what the
// registry stored.
func TestSearchUsersIgnoresCase(t *testing.T) {
	db := NewTestDB(t)
	seedUsers(t, db, "alpha", []User{{Name: "Moul", Address: "g1manfred"}})
	got, err := db.SearchUsers("alpha", "MOUL", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "Moul" {
		t.Fatalf("got %+v, want the one registration", got)
	}
}

// An address is matched as a prefix, which is what a reader pasting a truncated
// g1... from somewhere else actually has.
func TestSearchUsersMatchesAddressPrefix(t *testing.T) {
	db := NewTestDB(t)
	seedUsers(t, db, "alpha", []User{
		{Name: "moul", Address: "g1manfred47kzduec920z88wfr64ylksmdcedlf5"},
		{Name: "other", Address: "g1aeddlftlfk27ret5rf750d7w5dume3kcsm8r8m"},
	})
	got, err := db.SearchUsers("alpha", "g1manfred", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "moul" {
		t.Fatalf("got %+v, want only moul", got)
	}
}

// A previous name still resolves -- r/sys/users keeps it deliberately -- but it
// must not outrank the address's current one.
func TestSearchUsersSortsAliasesAndTombstonesLast(t *testing.T) {
	db := NewTestDB(t)
	seedUsers(t, db, "alpha", []User{
		{Name: "aaa-current", Address: "g1a"},
		{Name: "aaa-old", Address: "g1a", Alias: true},
		{Name: "aaa-dead", Address: "g1b", Deleted: true},
	})
	got, err := db.SearchUsers("alpha", "aaa", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d rows, want all three: a name that is taken and dead is still an answer", len(got))
	}
	if got[0].Name != "aaa-current" {
		t.Errorf("first = %q, want the current name", got[0].Name)
	}
	if !got[2].Deleted {
		t.Errorf("last = %+v, want the tombstone", got[2])
	}
}

// Rows are network-scoped like every other table here, and the same name on two
// chains is two registrations.
func TestSearchUsersIsNetworkScoped(t *testing.T) {
	db := NewTestDB(t)
	seedUsers(t, db, "alpha", []User{{Name: "moul", Address: "g1alpha"}})
	seedUsers(t, db, "beta", []User{{Name: "moul", Address: "g1beta"}})

	got, err := db.SearchUsers("alpha", "moul", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Address != "g1alpha" {
		t.Fatalf("got %+v, want only alpha's registration", got)
	}
	all, err := db.SearchUsers("", "moul", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("got %d rows across all networks, want 2", len(all))
	}
}

// The package count rides the search query rather than costing a round trip per
// row, and it counts only the user's own network.
func TestSearchUsersCountsPackages(t *testing.T) {
	db := NewTestDB(t)
	seedUsers(t, db, "alpha", []User{{Name: "moul", Address: "g1manfred"}})
	for _, p := range []string{"gno.land/r/moul/home", "gno.land/p/moul/md"} {
		if err := db.UpsertPackage("alpha", p, "x", "g1manfred", "TX"+p, 1, "", true, 1); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.UpsertPackage("beta", "gno.land/r/moul/elsewhere", "x", "g1manfred", "TXB", 1, "", true, 1); err != nil {
		t.Fatal(err)
	}
	got, err := db.SearchUsers("alpha", "moul", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Packages != 2 {
		t.Fatalf("packages = %+v, want 2: the other chain's deploy is a different deployment", got)
	}
}

// Deleting is a tombstone, not a DELETE: r/sys/users never frees a name, and a
// row that vanished would let the search claim the name is available.
func TestMarkUserDeletedKeepsTheRow(t *testing.T) {
	db := NewTestDB(t)
	seedUsers(t, db, "alpha", []User{
		{Name: "one", Address: "g1a"},
		{Name: "two", Address: "g1a"},
		{Name: "other", Address: "g1b"},
	})
	if err := db.MarkUserDeleted("alpha", "g1a"); err != nil {
		t.Fatal(err)
	}
	n, err := db.UserCount("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("live count = %d, want 1", n)
	}
	got, err := db.SearchUsers("alpha", "one", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got[0].Deleted {
		t.Fatalf("got %+v, want the name still listed and marked deleted", got)
	}
}

// UserByAddress answers the other direction, and prefers the current name over
// an alias of the same address.
func TestUserByAddressPrefersTheCurrentName(t *testing.T) {
	db := NewTestDB(t)
	seedUsers(t, db, "alpha", []User{
		{Name: "old", Address: "g1a", BlockHeight: 10, Alias: true},
		{Name: "new", Address: "g1a", BlockHeight: 20},
	})
	got, err := db.UserByAddress("alpha", "g1a")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Name != "new" {
		t.Fatalf("got %+v, want the current name", got)
	}
	missing, err := db.UserByAddress("alpha", "g1nobody")
	if err != nil {
		t.Fatal(err)
	}
	if missing != nil {
		t.Fatalf("got %+v, want nil for an unregistered address", missing)
	}
}

// DemoteUserAliases is what an Updated event applies, and it must touch only
// the address that renamed.
func TestDemoteUserAliases(t *testing.T) {
	db := NewTestDB(t)
	seedUsers(t, db, "alpha", []User{
		{Name: "old", Address: "g1a"},
		{Name: "new", Address: "g1a"},
		{Name: "someone", Address: "g1b"},
	})
	if err := db.DemoteUserAliases("alpha", "g1a", "new"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name      string
		wantAlias bool
	}{
		{"old", true}, {"new", false}, {"someone", false},
	} {
		got, err := db.SearchUsers("alpha", tc.name, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Alias != tc.wantAlias {
			t.Errorf("%s: alias = %+v, want %v", tc.name, got, tc.wantAlias)
		}
	}
}
