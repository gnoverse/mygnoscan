package store

import (
	"testing"
	"time"
)

func TestParseUgnot(t *testing.T) {
	tests := []struct {
		in   string
		want int64
	}{
		{"", 0},
		{"0ugnot", 0},
		{"100ugnot", 100},
		{"741332864386ugnot", 741332864386},
		// A coin list: only the native denom is rankable, because two chains'
		// non-native coins are not comparable and summing denominations would
		// invent a number.
		{"100ugnot,5foocoin", 100},
		{"5foocoin,100ugnot", 100},
		{"5foocoin", 0},
		{"not a balance", 0},
	}
	for _, tt := range tests {
		if got := ParseUgnot(tt.in); got != tt.want {
			t.Errorf("ParseUgnot(%q) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

func TestBalancesRoundTrip(t *testing.T) {
	db := NewTestDB(t)

	rows := []BalanceRow{
		{Address: "g1rich", Amount: "900ugnot", Height: 100},
		{Address: "g1poor", Amount: "5ugnot", Height: 100},
		{Address: "g1broke", Amount: "", Height: 100},
	}
	if err := db.UpsertBalances("alpha", rows); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, err := db.BalancesFor("alpha", []string{"g1rich", "g1poor", "g1missing"})
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if got["g1rich"].Amount != "900ugnot" || got["g1rich"].Ugnot != 900 {
		t.Errorf("g1rich = %+v, want the raw string and its parsed value", got["g1rich"])
	}
	// An address with nothing cached is absent, not zero. "unknown" and "has no
	// coins" are different claims and only one is safe to make about money.
	if _, ok := got["g1missing"]; ok {
		t.Error("an unswept address came back with a balance")
	}

	// Upserting again must update rather than duplicate: the sweeper runs every
	// ten minutes forever.
	if err := db.UpsertBalances("alpha", []BalanceRow{{Address: "g1rich", Amount: "950ugnot", Height: 200}}); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	got, _ = db.BalancesFor("alpha", []string{"g1rich"})
	if got["g1rich"].Ugnot != 950 || got["g1rich"].Height != 200 {
		t.Errorf("g1rich = %+v, want the refreshed figure", got["g1rich"])
	}
}

func TestRichListRanksAndScopes(t *testing.T) {
	db := NewTestDB(t)
	if err := db.UpsertBalances("alpha", []BalanceRow{
		{Address: "g1mid", Amount: "500ugnot"},
		{Address: "g1top", Amount: "900ugnot"},
		{Address: "g1low", Amount: "5ugnot"},
		// Zero balances are cached (so the sweeper stops retrying them) but not
		// ranked: a rich list of empty accounts is noise.
		{Address: "g1zero", Amount: ""},
	}); err != nil {
		t.Fatal(err)
	}
	// A different chain's balances must never appear in this ranking: ugnot is
	// denominated per chain and one column holding both belongs to neither.
	if err := db.UpsertBalances("beta", []BalanceRow{{Address: "g1beta", Amount: "9999ugnot"}}); err != nil {
		t.Fatal(err)
	}

	list, err := db.RichList("alpha", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"g1top", "g1mid", "g1low"}
	if len(list) != len(want) {
		t.Fatalf("got %d rows, want %d: %+v", len(list), len(want), list)
	}
	for i, addr := range want {
		if list[i].Address != addr {
			t.Errorf("rank %d = %s, want %s", i+1, list[i].Address, addr)
		}
	}

	page, err := db.RichList("alpha", 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 1 || page[0].Address != "g1mid" {
		t.Errorf("second page = %+v, want just g1mid", page)
	}
}

func TestBalanceCoverageReportsTheBoundary(t *testing.T) {
	db := NewTestDB(t)
	when := time.Now().UTC()
	MustCall(t, db, "alpha", "TX1", 1, when, "g1caller", "gno.land/r/x/y", "Fn")
	MustCall(t, db, "alpha", "TX2", 2, when, "g1other", "gno.land/r/x/y", "Fn")
	if err := db.UpsertBalances("alpha", []BalanceRow{{Address: "g1caller", Amount: "10ugnot"}}); err != nil {
		t.Fatal(err)
	}

	cov, err := db.BalanceCoverage("alpha")
	if err != nil {
		t.Fatal(err)
	}
	// One of two swept. The page prints this rather than implying it ranked
	// everyone, because gno offers no way to enumerate accounts at all.
	if cov.Swept != 1 {
		t.Errorf("swept = %d, want 1", cov.Swept)
	}
	if cov.Known < 2 {
		t.Errorf("known = %d, want at least the two addresses that called", cov.Known)
	}
	if cov.NewestFetch == "" {
		t.Error("newest fetch is empty, so the page cannot date the figures")
	}
}

// The sweeper has to make progress on a cold cache instead of refreshing the
// same head of the list forever, so never-fetched addresses come first.
func TestKnownAddressesPrefersTheUnswept(t *testing.T) {
	db := NewTestDB(t)
	when := time.Now().UTC()
	MustCall(t, db, "alpha", "TX1", 1, when, "g1swept", "gno.land/r/x/y", "Fn")
	MustCall(t, db, "alpha", "TX2", 2, when, "g1fresh", "gno.land/r/x/y", "Fn")
	if err := db.UpsertBalances("alpha", []BalanceRow{{Address: "g1swept", Amount: "1ugnot"}}); err != nil {
		t.Fatal(err)
	}

	addrs, err := db.KnownAddresses("alpha", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(addrs) < 2 {
		t.Fatalf("got %v, want both addresses", addrs)
	}
	if addrs[0] != "g1fresh" {
		t.Errorf("first = %s, want the never-swept address to be swept first", addrs[0])
	}
}

// Counts cannot be re-aggregated: an address active on three days of a week is
// one weekly active address, not three. That is why the rollup stores tuples.
func TestAccountPopulationDeduplicates(t *testing.T) {
	db := NewTestDB(t)
	now := time.Now().UTC()
	for i := 0; i < 3; i++ {
		MustCall(t, db, "alpha", "TXD"+string(rune('a'+i)), i+1,
			now.Add(-time.Duration(i)*2*time.Hour), "g1busy", "gno.land/r/x/y", "Fn")
	}
	MustCall(t, db, "alpha", "TXE", 9, now, "g1other", "gno.land/r/x/y", "Fn")

	pop, err := db.AccountPopulation("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if pop.Daily != 2 {
		t.Errorf("daily active = %d, want 2 distinct addresses rather than 4 calls", pop.Daily)
	}
	if pop.Known < 2 {
		t.Errorf("known = %d, want at least 2", pop.Known)
	}
}
